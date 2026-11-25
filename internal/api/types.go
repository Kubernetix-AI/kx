package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Token permissions.
const (
	PermDeploy = "deploy"
	PermRead   = "read"
	PermPush   = "registry:push"
	// PermConfigure lets a token change an app's environment variables and
	// replicas.
	PermConfigure = "configure"
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
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      string       `json:"status,omitempty"`
	URL         string       `json:"url,omitempty"`
	SourceType  string       `json:"source_type,omitempty"`
	ImageURL    string       `json:"image_url,omitempty"`
	Port        int          `json:"port"`
	Replicas    int          `json:"replicas"`
	Autoscaling *Autoscaling `json:"autoscaling,omitempty"`
	// EnvVars holds the names of the app's environment variables. Tokens
	// never see the values: each is "" and EnvVarsHidden is set. kx never
	// sends this map back, since every "" would overwrite a real value.
	EnvVars              map[string]string `json:"env_vars,omitempty"`
	EnvVarsHidden        bool              `json:"env_vars_hidden,omitempty"`
	LatestReleaseVersion int               `json:"latest_release_version"`
	Regions              []AppRegion       `json:"regions,omitempty"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

// Autoscaling is an app's autoscaling setting: it runs between Min() and
// MaxReplicas replicas, whatever its replica count says.
type Autoscaling struct {
	MinReplicas *int `json:"min_replicas,omitempty"`
	MaxReplicas int  `json:"max_replicas"`
}

// Min is the fewest replicas autoscaling runs (1 when not set).
func (a *Autoscaling) Min() int {
	if a == nil || a.MinReplicas == nil || *a.MinReplicas < 1 {
		return 1
	}
	return *a.MinReplicas
}

// Autoscales reports whether autoscaling decides how many replicas run.
func (a App) Autoscales() bool { return a.Autoscaling != nil && a.Autoscaling.MaxReplicas > 0 }

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
	ID            string `json:"id"`
	AppID         string `json:"app_id,omitempty"`
	AppName       string `json:"app_name,omitempty"`
	Version       int    `json:"version"`
	Status        string `json:"status"`
	Kind          string `json:"kind,omitempty"`
	ImageURL      string `json:"image_url,omitempty"`
	CustomerError string `json:"error_message_customer,omitempty"`
	// ErrorCode is the platform's class for a failure, e.g. quota_blocked
	// (the plan had no room for the version); new classes may appear.
	ErrorCode string `json:"error_code,omitempty"`
	// ErrorMessage is the platform's own text for a failure, which can
	// hold internal detail and log lines: kx only reads the class at its
	// start (FailureClass) and never prints it.
	ErrorMessage string `json:"error_message,omitempty"`
	// RetryCount is how many times Kubernetix has started this deployment
	// again after an attempt that didn't go live.
	RetryCount int `json:"retry_count,omitempty"`
	// Rollback is set on a deployment that rolled back to an earlier one;
	// Automatic when Kubernetix made it after a failed deploy.
	Rollback *DeploymentRollback `json:"rollback,omitempty"`
	// Replicas and Autoscaling are what the deployment runs with, copied
	// from the app when it was made (0 and nil from an older API).
	Replicas    int          `json:"replicas,omitempty"`
	Autoscaling *Autoscaling `json:"autoscaling,omitempty"`
	Port        int          `json:"port,omitempty"`
	HealthCheck *HealthCheck `json:"health_check,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	CompletedAt *time.Time   `json:"completed_at,omitempty"`
	// Replayed is set by CreateDeployment when Kubernetix handed back a
	// deployment made earlier with the same idempotency key.
	Replayed bool `json:"-"`
}

// FailureClasses are the classes Kubernetix gives a failed version it
// knows the cause of, in error_code: a plan with no room (quota_blocked),
// or a version that can't run.
var FailureClasses = []string{
	"quota_blocked", "crash_loop", "image_pull_failed", "oom_killed", "image_not_found", "image_not_allowed",
	"image_access_denied", "image_credential_rejected", "image_platform_unsupported", "invalid_image_name",
	"container_config_error", "container_create_error",
}

// failurePrefixes wrap the class at the start of the worker's own message
// ("deployment failed: crash_loop: …").
var failurePrefixes = []string{"deployment failed: ", "failed to prepare image: "}

// FailureClass is d's failure class: error_code when Kubernetix sends it,
// else a known class at the start of error_message, else "".
func (d Deployment) FailureClass() string {
	if d.ErrorCode != "" {
		return d.ErrorCode
	}
	msg := d.ErrorMessage
	for trimmed := true; trimmed; {
		trimmed = false
		for _, p := range failurePrefixes {
			if rest, ok := strings.CutPrefix(msg, p); ok {
				msg, trimmed = rest, true
			}
		}
	}
	class, _, found := strings.Cut(msg, ":")
	if found && slices.Contains(FailureClasses, class) {
		return class
	}
	return ""
}

// DeploymentRollback says what a rollback deployment restored: the
// deployment (and version) whose configuration it runs, and for one
// Kubernetix made itself (Automatic), the deployment that had failed.
type DeploymentRollback struct {
	Automatic          bool   `json:"automatic"`
	TargetDeploymentID string `json:"target_deployment_id,omitempty"`
	TargetVersion      int    `json:"target_version,omitempty"`
	FailedDeploymentID string `json:"failed_deployment_id,omitempty"`
	FailedVersion      int    `json:"failed_version,omitempty"`
}

// HealthCheck is how Kubernetix checks an app's containers: liveness
// (restart it when failing), readiness (send it traffic when passing) and
// startup (wait this long before the others count).
type HealthCheck struct {
	Liveness  *Probe `json:"liveness_probe,omitempty"`
	Readiness *Probe `json:"readiness_probe,omitempty"`
	Startup   *Probe `json:"startup_probe,omitempty"`
}

// Probe is one health check.
type Probe struct {
	HTTPGet             *HTTPGet `json:"http_get,omitempty"`
	InitialDelaySeconds int      `json:"initial_delay_seconds"`
	PeriodSeconds       int      `json:"period_seconds"`
	TimeoutSeconds      int      `json:"timeout_seconds"`
	FailureThreshold    int      `json:"failure_threshold"`
}

// HTTPGet is a health check that requests Path on Port.
type HTTPGet struct {
	Path string `json:"path"`
	Port int    `json:"port"`
}

// Window is how long the probe gives a container before it counts as
// failing: the initial delay plus the allowed failures. Nil is 0.
func (p *Probe) Window() time.Duration {
	if p == nil {
		return 0
	}
	return time.Duration(max(p.InitialDelaySeconds, 0)+max(p.FailureThreshold, 1)*max(p.PeriodSeconds, 1)) * time.Second
}

// Done reports whether the deployment has stopped changing: it is ready
// or failed. A rolled_back status is not enough on its own: Kubernetix can
// show it for a moment, with a generic message, before the version fails
// with its real reason or is tried again. Only a rolled_back that lasts is
// final; the deploy package decides that over several looks.
func (d Deployment) Done() bool {
	return d.Status == StatusReady || d.Status == StatusFailed
}

// RolledBack reports a rolled_back status, which may be momentary (see Done).
func (d Deployment) RolledBack() bool { return d.Status == StatusRolledBack }

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
	Pod  string    `json:"-"` // the replica that printed it, when the API says
}
