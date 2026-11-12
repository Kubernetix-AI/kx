package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Token permissions.
const (
	PermDeploy = "deploy"
	PermRead   = "read"
	PermPush   = "registry:push"
)

// Deployment statuses reported by the API.
const (
	StatusPending    = "pending"
	StatusClaimed    = "claimed"
	StatusBuilding   = "building"
	StatusDeploying  = "deploying"
	StatusPromoting  = "promoting"
	StatusReady      = "ready"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled_back"
)

// App is an application.
type App struct {
	ID                   string      `json:"id"`
	Name                 string      `json:"name"`
	Status               string      `json:"status,omitempty"`
	URL                  string      `json:"url,omitempty"`
	SourceType           string      `json:"source_type,omitempty"`
	ImageURL             string      `json:"image_url,omitempty"`
	Port                 int         `json:"port"`
	Replicas             int         `json:"replicas"`
	LatestReleaseVersion int         `json:"latest_release_version"`
	Regions              []AppRegion `json:"regions,omitempty"`
	CreatedAt            time.Time   `json:"created_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
}

// AppRegion is one region of a multi-region app (GetApp only).
type AppRegion struct {
	Region  string `json:"region"`
	Status  string `json:"status"`
	Version int    `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
}

// MultiRegion reports whether the app runs in several regions. Only GetApp
// returns regions; ListApps does not.
func (a App) MultiRegion() bool { return len(a.Regions) > 0 }

// CreateAppRequest is the body of POST /apps for an image app.
type CreateAppRequest struct {
	Name       string `json:"name"`
	SourceType string `json:"source_type"` // "image"
	ImageURL   string `json:"image_url,omitempty"`
	Port       int    `json:"port,omitempty"`
}

// Deployment is one deploy of an app.
type Deployment struct {
	ID            string     `json:"id"`
	AppID         string     `json:"app_id,omitempty"`
	AppName       string     `json:"app_name,omitempty"`
	Version       int        `json:"version"`
	Status        string     `json:"status"`
	Kind          string     `json:"kind,omitempty"`
	ImageURL      string     `json:"image_url,omitempty"`
	CustomerError string     `json:"error_message_customer,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// Done reports whether the deployment has stopped changing.
func (d Deployment) Done() bool {
	switch d.Status {
	case StatusReady, StatusFailed, StatusRolledBack:
		return true
	}
	return false
}

// Phase is the word kx shows for the status: queued, starting, live,
// failed or rolled back.
func (d Deployment) Phase() string {
	switch d.Status {
	case StatusPending, StatusClaimed:
		return "queued"
	case StatusBuilding, StatusDeploying, StatusPromoting:
		return "starting"
	case StatusReady:
		return "live"
	case StatusRolledBack:
		return "rolled back"
	}
	return d.Status
}

// CreateDeploymentRequest is the body of POST /apps/{id}/deployments.
type CreateDeploymentRequest struct {
	ImageURL string `json:"image_url,omitempty"`
}

// DeploymentLogs are a deployment's build and rollout logs.
type DeploymentLogs struct {
	DeploymentID   string `json:"deployment_id"`
	AppID          string `json:"app_id"`
	Status         string `json:"status"`
	BuildLogs      string `json:"build_logs"`
	DeploymentLogs string `json:"deployment_logs"`
}

// Replica is one running instance of an app.
type Replica struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Ready     bool   `json:"ready"`
	StartedAt string `json:"started_at"`
	Region    string `json:"region,omitempty"`
}

// UsageItem is a current/limit pair; a limit of 0 means unlimited or unknown.
type UsageItem struct {
	Current int `json:"current"`
	Limit   int `json:"limit"`
}

// Usage is an organization's usage against its plan.
type Usage struct {
	OrgID string               `json:"org_id"`
	Plan  string               `json:"plan"`
	Usage map[string]UsageItem `json:"usage"`
}

// DeploysToday returns today's deploys and the daily limit.
func (u Usage) DeploysToday() (used, limit int, ok bool) {
	item, ok := u.Usage["deployments_today"]
	return item.Current, item.Limit, ok
}

// RegistryCredential is a short-lived push credential for the
// organization's image registry. Formatting it with fmt never shows the
// password.
type RegistryCredential struct {
	RegistryURL string    `json:"registry_url"`
	Username    string    `json:"username"`
	Password    string    `json:"password"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Host is the registry host to log in to, e.g. registry.kubernetix.ai.
func (c RegistryCredential) Host() string {
	host, _, _ := strings.Cut(c.RegistryURL, "/")
	return host
}

func (c RegistryCredential) String() string {
	return fmt.Sprintf("RegistryCredential{%s, user %s, expires %s, password redacted}", c.RegistryURL, c.Username, c.ExpiresAt.Format(time.RFC3339))
}

// GoString keeps %#v from printing the password.
func (c RegistryCredential) GoString() string { return c.String() }

// Org names an organization. The API may send it as a string or as an
// object with id and name; both decode.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// UnmarshalJSON accepts "acme-corp" or {"id": "acme-corp", "name": "Acme"}.
func (o *Org) UnmarshalJSON(b []byte) error {
	if s, ok := jsonString(b); ok {
		*o = Org{ID: s}
		return nil
	}
	type plain Org
	return json.Unmarshal(b, (*plain)(o))
}

// Principal is a user. The API may send an email string or an object.
type Principal struct {
	ID    string `json:"id,omitempty"`
	Email string `json:"email,omitempty"`
}

// UnmarshalJSON accepts "sam@acme.dev" or {"id": "...", "email": "..."}.
func (p *Principal) UnmarshalJSON(b []byte) error {
	if s, ok := jsonString(b); ok {
		if strings.Contains(s, "@") {
			*p = Principal{Email: s}
		} else {
			*p = Principal{ID: s}
		}
		return nil
	}
	type plain Principal
	return json.Unmarshal(b, (*plain)(p))
}

func jsonString(b []byte) (string, bool) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(b, &s) != nil {
		return "", false
	}
	return s, true
}

// WhoAmI is the answer to GET /whoami.
type WhoAmI struct {
	Kind        string    `json:"kind"` // "user" or "token"
	Org         Org       `json:"org"`
	User        Principal `json:"user"`
	Token       *Token    `json:"token,omitempty"`
	RegistryURL string    `json:"registry_url,omitempty"`
}

// Token is the view of an API token. It never holds the token itself.
type Token struct {
	ID                string     `json:"id"`
	OrgID             string     `json:"org_id,omitempty"`
	Name              string     `json:"name"`
	Prefix            string     `json:"token_prefix"`
	Scope             string     `json:"scope"` // "org" or "app"
	AppID             string     `json:"app_id,omitempty"`
	AppName           string     `json:"app_name,omitempty"`
	Permissions       []string   `json:"permissions"`
	CreatedBy         string     `json:"created_by,omitempty"`
	CreatedByEmail    string     `json:"created_by_email,omitempty"`
	CreatedVia        string     `json:"created_via,omitempty"`     // "user" or "token"
	ParentTokenID     string     `json:"parent_token_id,omitempty"` // the token that made this one
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         *time.Time `json:"expires_at"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	LastUsedUserAgent string     `json:"last_used_user_agent,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	Status            string     `json:"status"` // active, expired, revoked, owner_inactive
}

// Can reports whether the token has permission p.
func (t Token) Can(p string) bool {
	for _, have := range t.Permissions {
		if have == p {
			return true
		}
	}
	return false
}

// AppScoped reports whether the token is limited to one app.
func (t Token) AppScoped() bool { return t.Scope == "app" }

// CreateTokenRequest is the body of POST /orgs/{org}/tokens.
// ExpiresInDays 0 leaves the server default (90 days); NeverExpires sends
// null.
type CreateTokenRequest struct {
	Name          string
	AppID         string
	Permissions   []string
	ExpiresInDays int
	NeverExpires  bool
}

// MarshalJSON writes expires_in_days as a number, as null, or not at all.
func (r CreateTokenRequest) MarshalJSON() ([]byte, error) {
	type body struct {
		Name          string          `json:"name"`
		AppID         string          `json:"app_id,omitempty"`
		Permissions   []string        `json:"permissions"`
		ExpiresInDays json.RawMessage `json:"expires_in_days,omitempty"`
	}
	b := body{Name: r.Name, AppID: r.AppID, Permissions: r.Permissions}
	switch {
	case r.NeverExpires:
		b.ExpiresInDays = json.RawMessage("null")
	case r.ExpiresInDays > 0:
		b.ExpiresInDays = json.RawMessage(fmt.Sprint(r.ExpiresInDays))
	}
	return json.Marshal(b)
}

// CreatedToken is a new token: its view plus Secret, the only copy of the
// token. Formatting it with fmt never shows Secret.
type CreatedToken struct {
	Token
	Secret string `json:"token"`
}

func (c CreatedToken) String() string {
	return fmt.Sprintf("CreatedToken{%s %q, secret redacted}", c.Prefix, c.Name)
}

// GoString keeps %#v from printing the secret.
func (c CreatedToken) GoString() string { return c.String() }

// LogQuery selects runtime log lines.
type LogQuery struct {
	Limit   int       // 1-5000; 0 means the server default (100)
	Start   time.Time // zero: server default
	End     time.Time // zero: now
	Forward bool      // oldest first from Start; otherwise newest first
}

// LogLine is one runtime log line.
type LogLine struct {
	Time time.Time `json:"time"`
	NS   int64     `json:"-"` // Unix nanoseconds, exact, for resuming
	Line string    `json:"line"`
}
