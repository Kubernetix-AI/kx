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

	flags   config.Flags // what Cfg was resolved from, to resolve it again after a login
	command string       // the command's name under kx, like "deploy" or "token"

	client *api.Client
	who    *api.WhoAmI
}

// session resolves configuration for cmd. appFlag is the command's --app
// value ("" if it has none).
func (a *App) session(cmd *cobra.Command, appFlag string) (*Session, error) {
	flags := config.Flags{Org: a.orgFlag, App: appFlag, APIURL: a.apiURL, Dir: a.Dir}
	cfg, err := config.Resolve(flags, a.Getenv)
	if err != nil {
		return nil, err
	}
	return &Session{App: a, P: a.printer(), Cfg: cfg, CI: cfg.CI, flags: flags, command: topCommand(cmd)}, nil
}

// topCommand is the name of cmd's command right under kx: "token" for
// kx token list.
func topCommand(cmd *cobra.Command) string {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	return cmd.Name()
}

// Client returns an API client for the resolved token, after checking that
// there is one, that it has not ended, and that it is well formed. When
// there is no login, or it has ended, it logs in first if it can
// (canAutoLogin).
func (s *Session) Client(ctx context.Context) (*api.Client, error) {
	if s.client != nil {
		return s.client, nil
	}
	if err := s.Cfg.RequireToken(s.App.Now()); err != nil {
		if !needsLogin(err) || !s.canAutoLogin() {
			return nil, err
		}
		if err := s.autoLogin(ctx); err != nil {
			return nil, err
		}
		if err := s.Cfg.RequireToken(s.App.Now()); err != nil {
			return nil, err
		}
	}
	if err := s.useToken(); err != nil {
		return nil, err
	}
	return s.client, nil
}

// useToken checks that the resolved token is well formed and points the
// client at it. A client that already exists keeps its identity, so code
// holding it uses the new token too.
func (s *Session) useToken() error {
	if err := api.CheckToken(s.Cfg.Token); err != nil {
		var inv *kxerr.InvalidToken
		if errors.As(err, &inv) {
			inv.Source = s.Cfg.TokenSource
		}
		return err
	}
	if s.client == nil {
		s.client = s.App.newClient(s.Cfg.APIURL, s.Cfg.Token, s.CI)
	} else {
		s.client.SetToken(s.Cfg.Token)
	}
	s.client.Ctx = api.ErrContext{
		Org:         s.askedOrg(),
		App:         s.Cfg.App,
		TokenSource: s.Cfg.TokenSource,
	}
	return nil
}

// needsLogin reports the errors a login fixes: no login, an ended one, or
// none for the organization asked for.
func needsLogin(err error) bool {
	var ended *kxerr.LoginEnded
	var orgNot *kxerr.OrgNotLoggedIn
	return errors.Is(err, kxerr.ErrNotLoggedIn) || errors.As(err, &ended) || errors.As(err, &orgNot)
}

// noAutoLogin lists the commands that never log in by themselves: the
// ones that manage logins and tokens, and kx version.
var noAutoLogin = map[string]bool{"login": true, "logout": true, "whoami": true, "token": true, "version": true}

// canAutoLogin reports whether a command that has no usable login may log
// in by itself, the way kx login does. Only when someone is there to do
// it: never in CI, never unless both standard input and standard error are
// terminals, never with --json. Never when KX_TOKEN is set, so a token that
// stopped working fails with its own message instead of being swapped for
// another one. And at most once per run.
func (s *Session) canAutoLogin() bool {
	a := s.App
	return !s.CI.IsCI && s.P.InTTY && s.P.TTY && !a.jsonOut &&
		a.Getenv(config.EnvToken) == "" && !noAutoLogin[s.command] && !a.autoLoggedIn
}

// autoLogin logs in for the organization the command is for, if any,
// then resolves the configuration again so the command uses the new login.
// A token for another organization is saved, as kx login does, but the
// command stops: it never carries on somewhere it wasn't meant for, even
// when its organization was only the last one this computer logged in to.
func (s *Session) autoLogin(ctx context.Context) error {
	s.App.autoLoggedIn = true
	org := s.askedOrg()
	if !config.ValidOrgName(org) {
		org = ""
	}
	saved, err := s.App.interactiveLogin(ctx, s, org, "", true)
	if err != nil {
		return err
	}
	if org != "" && saved != org {
		return &kxerr.LoggedInElsewhere{Org: org, TokenOrg: saved}
	}
	cfg, err := config.Resolve(s.flags, s.App.Getenv)
	if err != nil {
		return err
	}
	s.Cfg, s.CI = cfg, cfg.CI
	return nil
}

// loginRefused reports a saved login that Kubernetix says has expired, was
// revoked or is unknown, which a new login fixes. A 401 without one of
// those reasons may not be the token's fault, so it isn't one.
func (s *Session) loginRefused(err error) bool {
	var apiErr *api.Error
	if s.Cfg.TokenSource != kxerr.SourceLogin || !errors.As(err, &apiErr) || apiErr.Status != 401 {
		return false
	}
	switch apiErr.Code {
	case "token_expired", "token_revoked":
		return true
	case "invalid_token":
		return strings.TrimRight(apiErr.Message, ". ") == "This API token is not valid"
	}
	return false
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
	c, err := s.Client(ctx)
	if err != nil {
		return nil, err
	}
	who, err := c.WhoAmI(ctx)
	if err != nil && s.loginRefused(err) && s.canAutoLogin() {
		if err := s.autoLogin(ctx); err != nil {
			return nil, err
		}
		if err := s.Cfg.RequireToken(s.App.Now()); err != nil {
			return nil, err
		}
		if err := s.useToken(); err != nil {
			return nil, err
		}
		who, err = c.WhoAmI(ctx)
	}
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
	c, err := s.Client(ctx)
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

// ResolveAppOrOnly is ResolveApp for commands that only look (status,
// logs, open): in a folder that isn't linked, they use the organization's
// only app (onlyApp). command names the command, for the fix line.
func (s *Session) ResolveAppOrOnly(ctx context.Context, command string) (*api.App, error) {
	app, err := s.ResolveApp(ctx)
	if errors.Is(err, kxerr.ErrNotLinked) {
		return s.onlyApp(ctx, command)
	}
	return app, err
}

// onlyApp is the app a command uses in a folder that isn't linked: the
// organization's only app, which it names. With none, or several, it says
// how to go on (kxerr.Unlinked).
func (s *Session) onlyApp(ctx context.Context, command string) (*api.App, error) {
	c, err := s.Client(ctx)
	if err != nil {
		return nil, err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := c.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	if len(apps) == 1 {
		s.P.Info("Using %s, the only app in %s.", apps[0].Name, org)
		c.Ctx.App = apps[0].Name
		return &apps[0], nil
	}
	names := make([]string, 0, len(apps))
	for _, a := range apps {
		names = append(names, a.Name)
	}
	return nil, &kxerr.Unlinked{Org: org, Command: command, Apps: names}
}

// FindApp looks an app up by name (or ID), trying the known ID first.
func (s *Session) FindApp(ctx context.Context, name, id string) (*api.App, error) {
	c, err := s.Client(ctx)
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
