package cmd

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
	"github.com/kubernetix-ai/kx/internal/version"
)

// Session is one command's resolved world: configuration, printer, CI
// facts, and (on first use) an API client and the token's identity.
// Commands build one with App.session.
type Session struct {
	App *App
	P   *ui.Printer
	Cfg config.Resolved
	CI  ci.Env

	client *api.Client
	who    *api.WhoAmI
}

// session resolves configuration for cmd. appFlag is the command's --app
// value ("" if it has none).
func (a *App) session(cmd *cobra.Command, appFlag string) (*Session, error) {
	cfg, err := config.Resolve(config.Flags{Org: a.orgFlag, App: appFlag, APIURL: a.apiURL, Dir: a.Dir}, a.Getenv)
	if err != nil {
		return nil, err
	}
	return &Session{App: a, P: a.printer(), Cfg: cfg, CI: cfg.CI}, nil
}

// Client returns an API client for the resolved token, after checking that
// there is one, that it has not ended, and that it is well formed.
func (s *Session) Client() (*api.Client, error) {
	if s.client != nil {
		return s.client, nil
	}
	if err := s.Cfg.RequireToken(s.App.Now()); err != nil {
		return nil, err
	}
	if err := api.CheckToken(s.Cfg.Token); err != nil {
		var inv *kxerr.InvalidToken
		if errors.As(err, &inv) {
			inv.Source = s.Cfg.TokenSource
		}
		return nil, err
	}
	s.client = s.App.newClient(s.Cfg.APIURL, s.Cfg.Token, s.CI)
	s.client.Ctx = api.ErrContext{
		Org:         s.askedOrg(),
		App:         s.Cfg.App,
		TokenSource: s.Cfg.TokenSource,
	}
	return s.client, nil
}

// askedOrg is the org the command was asked to act on. With KX_TOKEN, the
// org of this computer's last login (default_org) is not a request for
// that org: the token's own org applies.
func (s *Session) askedOrg() string {
	if s.Cfg.TokenSource == kxerr.SourceEnv && s.Cfg.OrgSource == config.SourceConfig {
		return ""
	}
	return s.Cfg.Org
}

func (a *App) newClient(baseURL, token string, env ci.Env) *api.Client {
	c := api.New(baseURL, token)
	if a.HTTPClient != nil {
		c.HTTPClient = a.HTTPClient
	}
	c.Sleep = a.Sleep
	c.UserAgent = api.UserAgent(version.Get().Version, env.Name)
	return c
}

// WhoAmI returns (and caches) the token's identity.
func (s *Session) WhoAmI(ctx context.Context) (*api.WhoAmI, error) {
	if s.who != nil {
		return s.who, nil
	}
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	who, err := c.WhoAmI(ctx)
	if err != nil {
		return nil, err
	}
	s.who = who
	return who, nil
}

// Org returns the organization the command acts on. For a saved login it
// is the resolved org; for KX_TOKEN it is the token's own org, and a
// different --org, KX_ORG or .kx.json org is refused.
func (s *Session) Org(ctx context.Context) (string, error) {
	if s.Cfg.TokenSource == kxerr.SourceLogin && s.Cfg.Org != "" {
		return s.Cfg.Org, nil
	}
	who, err := s.WhoAmI(ctx)
	if err != nil {
		return "", err
	}
	org := who.Org.ID
	if asked := s.askedOrg(); asked != "" && asked != org {
		return "", &kxerr.TokenOrgMismatch{TokenOrg: org, Org: asked, FromFolder: s.Cfg.OrgSource == config.SourceProject}
	}
	s.client.Ctx.Org = org
	return org, nil
}

// ResolveApp finds the app the command acts on: the token's app for an
// app-scoped token, otherwise --app, KX_APP or the folder's .kx.json.
func (s *Session) ResolveApp(ctx context.Context) (*api.App, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	who, err := s.WhoAmI(ctx)
	if err != nil {
		return nil, err
	}
	if who.Token != nil && who.Token.AppScoped() {
		if s.Cfg.App != "" && s.Cfg.App != who.Token.AppName {
			return nil, &kxerr.TokenAppMismatch{TokenApp: who.Token.AppName, App: s.Cfg.App}
		}
		c.Ctx.App = who.Token.AppName
		return c.GetApp(ctx, who.Token.AppID)
	}
	if s.Cfg.App == "" {
		return nil, kxerr.ErrNotLinked
	}
	if _, err := s.Org(ctx); err != nil {
		return nil, err
	}
	return s.FindApp(ctx, s.Cfg.App, s.Cfg.AppID)
}

// FindApp looks an app up by name (or ID), trying the known ID first.
func (s *Session) FindApp(ctx context.Context, name, id string) (*api.App, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	c.Ctx.App = name
	if id != "" {
		app, err := c.GetApp(ctx, id)
		if err == nil && app.Name == name {
			return app, nil
		}
		var apiErr *api.Error
		notFound := errors.As(err, &apiErr) && apiErr.Status == 404
		if err != nil && !notFound {
			return nil, err
		}
	}
	apps, err := c.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	for i := range apps {
		if apps[i].Name == name || apps[i].ID == name {
			return &apps[i], nil
		}
	}
	return nil, &api.Error{Status: 404, Code: "app_not_found", Method: "GET", Path: "/apps", Ctx: c.Ctx}
}

// DashboardURL returns the dashboard address for path, derived from the
// API URL (https://platform.kubernetix.ai/api/v1 -> https://platform.kubernetix.ai).
func (s *Session) DashboardURL(path string) string { return dashboardURL(s.Cfg.APIURL, path) }

func dashboardURL(apiURL, path string) string {
	u, err := url.Parse(apiURL)
	if err != nil {
		return "https://platform.kubernetix.ai" + path
	}
	base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1"), "/api")
	return u.Scheme + "://" + u.Host + base + path
}

// DashboardHost is the dashboard's host name, for messages.
func (s *Session) DashboardHost() string {
	u, err := url.Parse(s.DashboardURL(""))
	if err != nil {
		return "platform.kubernetix.ai"
	}
	return u.Host
}
