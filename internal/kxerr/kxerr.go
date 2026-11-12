// Package kxerr holds the errors kx raises itself, before or instead of an
// API call. Each one names a situation the error table in the README
// describes; api.Explain turns them (and API errors) into the two lines kx
// prints and the exit code it returns.
//
// The Error strings are for logs and tests only. They never contain a token.
package kxerr

import (
	"errors"
	"fmt"
	"time"
)

// TokenSource says where the token in use came from, which changes the fix
// kx suggests when the token stops working.
type TokenSource string

const (
	// SourceNone means no token was found.
	SourceNone TokenSource = ""
	// SourceLogin is a token saved on this computer by kx login.
	SourceLogin TokenSource = "login"
	// SourceEnv is the KX_TOKEN environment variable (usually a CI secret).
	SourceEnv TokenSource = "env"
	// SourcePaste is a token being entered during kx login.
	SourcePaste TokenSource = "paste"
)

// ErrNotLoggedIn means there is no token for kx to use.
var ErrNotLoggedIn = errors.New("not logged in")

// ErrNoPushPermission means the token cannot mint registry credentials.
var ErrNoPushPermission = errors.New("token cannot push images")

// ErrDockerMissing means the docker command is not installed.
var ErrDockerMissing = errors.New("docker is not installed")

// ErrDockerStopped means docker is installed but its engine is not running.
var ErrDockerStopped = errors.New("docker is not running")

// ErrDockerWindowsContainers means Docker Desktop is in Windows-containers mode.
var ErrDockerWindowsContainers = errors.New("docker is in windows containers mode")

// ErrNotLinked means no app was given and the folder has no .kx.json.
var ErrNotLinked = errors.New("folder is not linked to an app")

// ErrLoginInCI means kx login was run inside a CI job.
var ErrLoginInCI = errors.New("kx login does not run in CI")

// LoginEnded means the token saved for Org has expired.
type LoginEnded struct{ Org string }

func (e *LoginEnded) Error() string { return "login to " + e.Org + " has ended" }

// OrgNotLoggedIn means the folder (FromFolder) or --org/KX_ORG names Org,
// but this computer only holds tokens for LoggedIn.
type OrgNotLoggedIn struct {
	Org        string
	LoggedIn   []string
	FromFolder bool
}

func (e *OrgNotLoggedIn) Error() string { return "not logged in to " + e.Org }

// TokenOrgMismatch means KX_TOKEN belongs to TokenOrg, but --org, KX_ORG
// or the folder's .kx.json (FromFolder) names Org.
type TokenOrgMismatch struct {
	TokenOrg   string
	Org        string
	FromFolder bool
}

func (e *TokenOrgMismatch) Error() string {
	return "KX_TOKEN is for " + e.TokenOrg + ", not " + e.Org
}

// Why the platform refused to let a token create a token
// (TokenCreateRefused.Reason). A token can only make tokens narrower than
// itself: within its permissions, app and expiry, and never from a token
// that another token made.
const (
	CreateExpiry      = "expiry"      // would outlive the token, or never expire
	CreatePermissions = "permissions" // asks for permissions the token lacks
	CreateApp         = "app"         // an app token asked for another app or every app
	CreateDerived     = "derived"     // the token was itself made by a token
)

// TokenCreateRefused means the token in use (from Source) was not allowed
// to create the token asked for. Until is when it ends (nil: never),
// Permissions what it can do, App its app if it is limited to one.
type TokenCreateRefused struct {
	Reason      string
	Source      TokenSource
	Until       *time.Time
	Permissions []string
	App         string
}

func (e *TokenCreateRefused) Error() string {
	return "token may not create that token (" + e.Reason + ")"
}

// TokenAppMismatch means an app-scoped token for TokenApp was asked to act on App.
type TokenAppMismatch struct{ TokenApp, App string }

func (e *TokenAppMismatch) Error() string {
	return "token is for app " + e.TokenApp + ", not " + e.App
}

// InvalidToken means a token failed the local format or checksum check, or
// is a legacy ct_ key.
type InvalidToken struct {
	Source TokenSource
	Legacy bool
	Reason string
}

func (e *InvalidToken) Error() string {
	if e.Legacy {
		return "legacy ct_ keys are not supported"
	}
	return "invalid token: " + e.Reason
}

// Why an API URL was refused (InsecureURL.Reason).
const (
	URLNotHTTPS        = "not_https"   // not https, and not http to a loopback address
	URLCredentials     = "credentials" // has a user name or password
	URLQueryOrFragment = "query"       // has a ?query or #fragment
	URLInvalid         = "invalid"     // not a URL with a host
)

// InsecureURL means kx refused an API URL: it is not https (and not http
// to a loopback address), or it carries credentials, a query or a fragment,
// or it is not a URL at all.
type InsecureURL struct {
	URL    string
	Source string // "KX_API_URL", "--api-url", "config", or "" when unknown
	Reason string // one of the URL* constants; "" means URLNotHTTPS
}

func (e *InsecureURL) Error() string {
	return "refusing API URL from " + e.Source + " (" + e.Reason + ")"
}

// Usage is a mistake in how kx was called. What and Fix are shown as-is.
type Usage struct{ What, Fix string }

func (e *Usage) Error() string { return e.What }

// NoDockerfile means Dir has no Dockerfile.
type NoDockerfile struct{ Dir string }

func (e *NoDockerfile) Error() string { return "no Dockerfile in " + e.Dir }

// DeployLimit means Org has used today's deploys. Plan is optional; Now
// defaults to the current time.
type DeployLimit struct {
	Org   string
	Plan  string // e.g. "free"
	Limit int
	Now   time.Time
}

func (e *DeployLimit) Error() string { return fmt.Sprintf("daily deploy limit of %d reached", e.Limit) }

// PushRateLimited means the token minted registry credentials too often.
type PushRateLimited struct{ Wait time.Duration }

func (e *PushRateLimited) Error() string { return "registry credential rate limit reached" }

// DeployFailed means version Version of App did not go live.
// ServingVersion is the version still serving, 0 if none. Reason is the
// platform's customer-facing message, if any.
type DeployFailed struct {
	App            string
	Version        int
	ServingVersion int
	Reason         string
}

func (e *DeployFailed) Error() string { return fmt.Sprintf("%s v%d failed", e.App, e.Version) }

// WaitTimeout means kx stopped waiting for App vVersion after After.
type WaitTimeout struct {
	App     string
	Version int
	After   time.Duration
}

func (e *WaitTimeout) Error() string { return fmt.Sprintf("%s v%d still starting", e.App, e.Version) }

// BuildFailed wraps a failed docker build.
type BuildFailed struct{ Err error }

func (e *BuildFailed) Error() string { return "build failed: " + errString(e.Err) }
func (e *BuildFailed) Unwrap() error { return e.Err }

// PushFailed wraps a failed docker push.
type PushFailed struct{ Err error }

func (e *PushFailed) Error() string { return "push failed: " + errString(e.Err) }
func (e *PushFailed) Unwrap() error { return e.Err }

// MultiRegion means App runs in several regions, which kx does not deploy.
type MultiRegion struct{ App string }

func (e *MultiRegion) Error() string { return e.App + " is a multi-region app" }

func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// BadConfig means a settings file could not be read. Project is true for a
// folder's .kx.json and false for kx's own settings file.
type BadConfig struct {
	Path    string
	Project bool
	Reason  string
}

func (e *BadConfig) Error() string { return "cannot read " + e.Path + ": " + e.Reason }

// NotLive means App has no live URL yet.
type NotLive struct{ App string }

func (e *NotLive) Error() string { return e.App + " is not live" }

// NoVersion means App has no deployment called Version (e.g. "v99").
type NoVersion struct{ App, Version string }

func (e *NoVersion) Error() string { return e.App + " has no " + e.Version }

// NoToken means no token in Org matches Which (a prefix, ID or name).
type NoToken struct{ Which, Org string }

func (e *NoToken) Error() string { return "no token " + e.Which }
