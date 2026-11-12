// Package config works out which API, organization, token and app a kx
// command uses, from (highest first) flags, environment variables, the
// folder's .kx.json, kx's own settings file and built-in defaults.
//
// kx's settings file lives at os.UserConfigDir()/kx/config.json (or
// $KX_CONFIG_DIR/config.json). It holds one token per organization and is
// written atomically with mode 0600. In CI it is never read or written.
//
// .kx.json holds {"app", "app_id", "org"} and no secret, so it can be
// committed. It is found by searching from the working folder up to the git
// repository root. It cannot change the API URL, so a cloned repository
// can never send your token somewhere else.
package config

import (
	"os"
	"sort"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// DefaultAPIURL is the public Kubernetix API.
const DefaultAPIURL = "https://platform.kubernetix.ai/api/v1"

// Environment variables kx reads.
const (
	EnvToken     = "KX_TOKEN"
	EnvOrg       = "KX_ORG"
	EnvApp       = "KX_APP"
	EnvAPIURL    = "KX_API_URL"
	EnvConfigDir = "KX_CONFIG_DIR"
)

// Source says where a resolved value came from.
type Source string

// Value sources, highest precedence first.
const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceProject Source = "project"
	SourceConfig  Source = "config"
	SourceDefault Source = "default"
	SourceUnset   Source = ""
)

// Flags are the command-line values that take part in resolution. There is
// deliberately no token flag: command lines end up in shell history and
// process lists. Dir is the folder to search for .kx.json ("" = working
// directory).
type Flags struct {
	Org    string
	App    string
	APIURL string
	Dir    string
}

// Resolved is the outcome of Resolve.
type Resolved struct {
	APIURL       string
	APIURLSource Source

	Token          string
	TokenSource    kxerr.TokenSource
	TokenExpiresAt *time.Time // known only for tokens saved by kx login

	Org       string
	OrgSource Source

	App       string
	AppID     string // set only when App is the folder's linked app in the same org
	AppSource Source

	Dir         string   // folder the command runs in
	Project     *Project // nearest .kx.json, nil if none
	ProjectPath string

	User     *User  // kx's settings; nil in CI
	UserPath string // "" in CI

	CI ci.Env
}

// Resolve applies the precedence rules. getenv is usually os.Getenv.
func Resolve(f Flags, getenv func(string) string) (Resolved, error) {
	r := Resolved{CI: ci.Detect(getenv), Dir: f.Dir}
	if r.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return r, err
		}
		r.Dir = wd
	}

	path, project, err := FindProject(r.Dir)
	if err != nil {
		return r, err
	}
	r.Project, r.ProjectPath = project, path

	if !r.CI.IsCI {
		up, err := UserPath(getenv)
		if err != nil {
			return r, err
		}
		u, err := LoadUser(up)
		if err != nil {
			return r, err
		}
		r.User, r.UserPath = u, up
	}

	// API URL: flag, env, user config, default. Never .kx.json.
	switch {
	case f.APIURL != "":
		r.APIURL, r.APIURLSource = f.APIURL, SourceFlag
	case getenv(EnvAPIURL) != "":
		r.APIURL, r.APIURLSource = getenv(EnvAPIURL), SourceEnv
	case r.User != nil && r.User.APIURL != "":
		r.APIURL, r.APIURLSource = r.User.APIURL, SourceConfig
	default:
		r.APIURL, r.APIURLSource = DefaultAPIURL, SourceDefault
	}
	clean, err := api.CheckBaseURL(r.APIURL, urlSourceName(r.APIURLSource))
	if err != nil {
		return r, err
	}
	r.APIURL = clean

	// Org: flag, env, .kx.json, default_org.
	switch {
	case f.Org != "":
		r.Org, r.OrgSource = f.Org, SourceFlag
	case getenv(EnvOrg) != "":
		r.Org, r.OrgSource = getenv(EnvOrg), SourceEnv
	case project != nil && project.Org != "":
		r.Org, r.OrgSource = project.Org, SourceProject
	case r.User != nil && r.User.DefaultOrg != "":
		r.Org, r.OrgSource = r.User.DefaultOrg, SourceConfig
	}

	// App: flag, env, .kx.json. The app ID only travels with the linked app.
	switch {
	case f.App != "":
		r.App, r.AppSource = f.App, SourceFlag
	case getenv(EnvApp) != "":
		r.App, r.AppSource = getenv(EnvApp), SourceEnv
	case project != nil:
		r.App, r.AppSource = project.App, SourceProject
	}
	if project != nil && project.AppID != "" && r.App == project.App &&
		(project.Org == "" || project.Org == r.Org) {
		r.AppID = project.AppID
	}

	// Token: KX_TOKEN, then the login saved for the org.
	if tok := getenv(EnvToken); tok != "" {
		r.Token, r.TokenSource = tok, kxerr.SourceEnv
	} else if r.User != nil && r.Org != "" {
		if l, ok := r.User.Orgs[r.Org]; ok && l.Token != "" {
			r.Token, r.TokenSource, r.TokenExpiresAt = l.Token, kxerr.SourceLogin, l.ExpiresAt
		}
	}
	return r, nil
}

// RequireToken explains why there is no usable token, or returns nil.
// It does not validate the token's format; api.CheckToken does that.
func (r Resolved) RequireToken(now time.Time) error {
	if r.Token != "" {
		if r.TokenSource == kxerr.SourceLogin && r.TokenExpiresAt != nil && !now.Before(*r.TokenExpiresAt) {
			return &kxerr.LoginEnded{Org: r.Org}
		}
		return nil
	}
	if r.Org != "" && r.User != nil {
		if others := r.User.OrgNames(); len(others) > 0 {
			return &kxerr.OrgNotLoggedIn{Org: r.Org, LoggedIn: others, FromFolder: r.OrgSource == SourceProject}
		}
	}
	return kxerr.ErrNotLoggedIn
}

func urlSourceName(s Source) string {
	switch s {
	case SourceFlag:
		return "--api-url"
	case SourceEnv:
		return EnvAPIURL
	case SourceConfig:
		return "config"
	default:
		return "default"
	}
}

// OrgNames lists the organizations with a saved login, sorted.
func (u *User) OrgNames() []string {
	names := make([]string, 0, len(u.Orgs))
	for name := range u.Orgs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
