package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

type explainCase struct {
	name string
	err  error
	want Problem
}

// The first thirteen cases are the README's error table, word for word.
func explainCases() []explainCase {
	evening := time.Date(2026, 11, 14, 20, 48, 0, 0, time.UTC)
	envCtx := ErrContext{Org: "acme-corp", TokenSource: kxerr.SourceEnv}
	loginCtx := ErrContext{Org: "acme-corp", TokenSource: kxerr.SourceLogin}
	pasteCtx := ErrContext{TokenSource: kxerr.SourcePaste}
	exp := time.Date(2027, 2, 9, 0, 0, 0, 0, time.UTC)
	apiErr := func(status int, code string, ctx ErrContext) *Error {
		return &Error{Status: status, Code: code, Message: "server says no", Method: "GET", Path: "/apps", Ctx: ctx}
	}
	withPath := func(e *Error, method, path string) *Error { e.Method, e.Path = method, path; return e }
	withExpiry := func(e *Error) *Error { e.TokenExpiresAt = &exp; return e }
	withName := func(e *Error) *Error { e.TokenPrefix, e.TokenName = "kx_01234567", "laptop"; return e }
	withRetry := func(e *Error, d time.Duration) *Error { e.RetryAfter = d; return e }
	withLimit := func(e *Error, name string) *Error { e.LimitName = name; return e }

	return []explainCase{
		// The error table.
		{"not logged in", kxerr.ErrNotLoggedIn, Problem{"not_logged_in", "You're not logged in to Kubernetix.", "Run: kx login", 3}},
		{"login ended", &kxerr.LoginEnded{Org: "acme-corp"}, Problem{"login_ended", "Your login to acme-corp has ended.", "Run: kx login --org acme-corp", 3}},
		{"KX_TOKEN expired", withExpiry(apiErr(401, "token_expired", envCtx)), Problem{"token_expired", "The KX_TOKEN secret holds a token that expired on 9 Feb 2027.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"creator removed", apiErr(401, "token_owner_removed", envCtx), Problem{"token_owner_removed", "This token stopped working because the person who created it is no longer a member of acme-corp.", "Ask an admin of acme-corp for a new token: Settings → API tokens", 3}},
		{"KX_TOKEN for another org (folder)", &kxerr.TokenOrgMismatch{TokenOrg: "acme-corp", Org: "acme-labs", FromFolder: true}, Problem{"token_org_mismatch", "KX_TOKEN belongs to acme-corp, but this folder deploys to acme-labs.", "Put a token for acme-labs in KX_TOKEN.", 3}},
		{"KX_TOKEN for another org (asked)", &kxerr.TokenOrgMismatch{TokenOrg: "acme-corp", Org: "acme-labs"}, Problem{"token_org_mismatch", "KX_TOKEN belongs to acme-corp, but you asked for acme-labs.", "Put a token for acme-labs in KX_TOKEN.", 3}},
		{"KX_TOKEN lacks read", &kxerr.TokenLacks{Permissions: []string{"read"}, Source: kxerr.SourceEnv}, Problem{"token_not_allowed", "The token in the KX_TOKEN secret doesn't have the Read permission, which kx deploy needs.", "Create a deploy token on the app's Deploy tab (it has Deploy, Read and Push images) and replace the KX_TOKEN secret.", 3}},
		{"login lacks deploy and push", &kxerr.TokenLacks{Permissions: []string{"deploy", "registry:push"}, Source: kxerr.SourceLogin, Org: "acme-corp"}, Problem{"token_not_allowed", "The token this computer uses for acme-corp doesn't have the Deploy and Push images permissions, which kx deploy needs.", "Log in with a token that can deploy: kx login --org acme-corp", 3}},
		{"server names the missing permission", withPath(&Error{Status: 403, Code: "token_not_allowed", Message: "This API token doesn't have the read permission.", Ctx: envCtx}, "GET", "/apps/a1"), Problem{"token_not_allowed", "The token in the KX_TOKEN secret doesn't have the Read permission.", "Create a token with the access you need on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"token create", kxerr.ErrTokenCreateMoved, Problem{"token_create_not_allowed", "kx can't create tokens; only people signed in to the dashboard can.", "Create one in the dashboard: Settings → API tokens (for CI, the app's Deploy tab)", 2}},
		{"app-scoped token, other app", &kxerr.TokenAppMismatch{TokenApp: "shop-api", App: "web"}, Problem{"token_app_mismatch", "This token can only deploy shop-api, and you asked to deploy web.", "Log in with a token that can deploy web: kx login", 3}},
		{"folder's org not logged in", &kxerr.OrgNotLoggedIn{Org: "acme-labs", LoggedIn: []string{"acme-corp"}, FromFolder: true}, Problem{"org_not_logged_in", "This folder deploys to acme-labs, but this computer is only logged in to acme-corp.", "Run: kx login --org acme-labs", 3}},
		{"cannot push (local)", kxerr.ErrNoPushPermission, Problem{"token_cannot_push", "This token can't upload images.", "Create a token with Push images on the app's Deploy tab, or deploy an image that's already pushed: kx deploy --image <image>", 3}},
		{"docker missing", kxerr.ErrDockerMissing, Problem{"docker_missing", "kx builds your app with Docker, and Docker isn't installed on this computer.", "Install Docker Desktop from docker.com and run kx deploy again.", 10}},
		{"docker stopped", kxerr.ErrDockerStopped, Problem{"docker_stopped", "Docker is installed but not running.", "Start Docker Desktop, then run kx deploy again.", 10}},
		{"no Dockerfile", &kxerr.NoDockerfile{Dir: "/Users/sam/code/shop-api"}, Problem{"no_dockerfile", "kx needs a Dockerfile to build your app, and there isn't one in /Users/sam/code/shop-api.", "Add a Dockerfile to this folder, or cd to the folder that has one. To have kx write one, run kx deploy in a terminal.", 2}},
		{"daily deploys used", &kxerr.DeployLimit{Org: "acme-corp", Plan: "free", Limit: 5, Now: evening}, Problem{"deploy_limit_reached", "acme-corp has used all 5 deploys the Free plan includes today; more are available at 00:00 UTC (in 3 h 12 min).", "Upgrade for more deploys a day: platform.kubernetix.ai/plans", 5}},
		{"push rate limited (local)", &kxerr.PushRateLimited{Wait: 6 * time.Minute}, Problem{"push_rate_limited", "This token has uploaded images too often in the last hour.", "Wait 6 minutes, then run kx deploy again.", 5}},
		{"deploy crashed", &kxerr.DeployFailed{App: "shop-api", Version: 15, ServingVersion: 14}, Problem{"deploy_failed", "shop-api v15 didn't go live: your container is crashing on start. Version 14 is still serving.", "See why: kx logs --deployment v15", 8}},

		// The same situations reported by the API.
		{"cannot push (API)", withPath(apiErr(403, "token_not_allowed", envCtx), "POST", "/registry/credentials"), Problem{"token_cannot_push", "This token can't upload images.", "Create a token with Push images on the app's Deploy tab, or deploy an image that's already pushed: kx deploy --image <image>", 3}},
		{"push rate limited (API)", withRetry(withPath(apiErr(429, "rate_limited", envCtx), "POST", "/registry/credentials"), 360*time.Second), Problem{"push_rate_limited", "This token has uploaded images too often in the last hour.", "Wait 6 minutes, then run kx deploy again.", 5}},
		{"login token expired (API)", apiErr(401, "token_expired", loginCtx), Problem{"login_ended", "Your login to acme-corp has ended.", "Run: kx login --org acme-corp", 3}},
		{"daily deploys used (API)", withLimit(apiErr(402, "tier_limit_exceeded", loginCtx), "max_deployments_per_day"), Problem{"deploy_limit_reached", "", "Upgrade for more deploys a day: platform.kubernetix.ai/plans", 5}},

		// Everything else kx can report.
		{"KX_TOKEN expired, date unknown", apiErr(401, "token_expired", envCtx), Problem{"token_expired", "The KX_TOKEN secret holds a token that has expired.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"pasted token expired", apiErr(401, "token_expired", pasteCtx), Problem{"token_expired", "That token has already expired.", "Create a new token and paste it: kx login", 3}},
		{"org not logged in via --org", &kxerr.OrgNotLoggedIn{Org: "acme-labs", LoggedIn: []string{"acme-corp", "acme-x"}}, Problem{"org_not_logged_in", "You asked for acme-labs, but this computer is only logged in to acme-corp and acme-x.", "Run: kx login --org acme-labs", 3}},
		{"login revoked", withName(apiErr(401, "token_revoked", loginCtx)), Problem{"token_revoked", "The token this computer uses for acme-corp (kx_01234567, \"laptop\") was revoked.", "Run: kx login --org acme-corp", 3}},
		{"KX_TOKEN revoked", withName(apiErr(401, "token_revoked", envCtx)), Problem{"token_revoked", "The KX_TOKEN secret holds a token (kx_01234567, \"laptop\") that was revoked.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"pasted token revoked", apiErr(401, "token_revoked", pasteCtx), Problem{"token_revoked", "That token was revoked.", "Create a new token and paste it: kx login", 3}},
		{"login token invalid (API)", apiErr(401, "invalid_token", loginCtx), Problem{"invalid_token", "Kubernetix doesn't recognise the token this computer uses for acme-corp.", "Run: kx login --org acme-corp", 3}},
		{"KX_TOKEN invalid (API)", apiErr(401, "invalid_token", envCtx), Problem{"invalid_token", "Kubernetix doesn't recognise the token in the KX_TOKEN secret.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"pasted token invalid (API)", apiErr(401, "invalid_token", pasteCtx), Problem{"invalid_token", "Kubernetix doesn't recognise that token.", "Create a new token and paste it: kx login", 3}},
		{"unauthenticated (API)", apiErr(401, "not_authenticated", loginCtx), Problem{"invalid_token", "Kubernetix doesn't recognise the token this computer uses for acme-corp.", "Run: kx login --org acme-corp", 3}},
		{"pasted token malformed", &kxerr.InvalidToken{Source: kxerr.SourcePaste, Reason: "checksum mismatch"}, Problem{"invalid_token", "That doesn't look like a whole Kubernetix token.", "Copy it again with the Copy button and paste it: kx login", 3}},
		{"KX_TOKEN malformed", &kxerr.InvalidToken{Source: kxerr.SourceEnv, Reason: "checksum mismatch"}, Problem{"invalid_token", "The KX_TOKEN secret doesn't hold a whole Kubernetix token.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"saved token damaged", &kxerr.InvalidToken{Source: kxerr.SourceLogin, Reason: "checksum mismatch"}, Problem{"invalid_token", "The token saved on this computer is damaged.", "Run: kx login", 3}},
		{"legacy key pasted", &kxerr.InvalidToken{Source: kxerr.SourcePaste, Legacy: true}, Problem{"legacy_token", "kx can't use the old ct_ API keys.", "Create a token in Settings → API tokens and paste it: kx login", 3}},
		{"legacy key in KX_TOKEN", &kxerr.InvalidToken{Source: kxerr.SourceEnv, Legacy: true}, Problem{"legacy_token", "The KX_TOKEN secret holds an old ct_ API key, which kx can't use.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"creator rejected", withName(apiErr(403, "token_owner_rejected", loginCtx)), Problem{"token_owner_rejected", "This token (kx_01234567, \"laptop\") stopped working because the account that created it was not approved.", "Ask an admin of acme-corp for a new token: Settings → API tokens", 3}},
		{"creator pending", apiErr(403, "token_owner_pending", loginCtx), Problem{"token_owner_pending", "This token can't be used until the account that created it is approved.", "Ask an admin of acme-corp for a new token: Settings → API tokens", 3}},
		{"creator can only view", apiErr(403, "token_owner_role", loginCtx), Problem{"token_owner_role", "This token can't deploy because the person who created it can only view acme-corp.", "Ask a developer or admin of acme-corp for a deploy token: Settings → API tokens", 3}},
		{"token not allowed", apiErr(403, "token_not_allowed", loginCtx), Problem{"token_not_allowed", "This token isn't allowed to do that.", "Log in with a token that can: kx login --org acme-corp", 3}},
		{"KX_TOKEN not allowed", apiErr(403, "token_not_allowed", envCtx), Problem{"token_not_allowed", "The token in the KX_TOKEN secret isn't allowed to do that.", "Create a token with the access you need on the app's Deploy tab and replace the KX_TOKEN secret.", 3}},
		{"role too low", apiErr(403, "insufficient_permissions", loginCtx), Problem{"insufficient_permissions", "Your role in acme-corp doesn't allow that.", "Ask an admin of acme-corp for access.", 3}},
		{"forbidden", apiErr(403, "forbidden", loginCtx), Problem{"insufficient_permissions", "Your role in acme-corp doesn't allow that.", "Ask an admin of acme-corp for access.", 3}},
		{"token limit", apiErr(409, "token_limit_reached", loginCtx), Problem{"token_limit_reached", "acme-corp already has as many active tokens as it can have.", "Revoke one you no longer use: kx token list, then kx token revoke <prefix>", 5}},
		{"token lookup down", apiErr(503, "token_lookup_unavailable", loginCtx), Problem{"token_lookup_unavailable", "Kubernetix couldn't check your token just now.", "Wait a minute, then try again.", 1}},
		{"app not found", apiErr(404, "app_not_found", ErrContext{Org: "acme-corp", App: "web"}), Problem{"app_not_found", "There's no app called web in acme-corp.", "See your apps: kx apps list", 4}},
		{"app not found, no name", apiErr(404, "app_not_found", ErrContext{}), Problem{"app_not_found", "That app doesn't exist.", "See your apps: kx apps list", 4}},
		{"deployment not found", apiErr(404, "deployment_not_found", loginCtx), Problem{"deployment_not_found", "That deployment doesn't exist.", "See recent deploys: kx status", 4}},
		{"org not found", apiErr(404, "org_not_found", ErrContext{Org: "acme-x"}), Problem{"org_not_found", "There's no organization called acme-x.", "Check the name, or see where you're logged in: kx whoami", 4}},
		{"unknown 404", apiErr(404, "", loginCtx), Problem{"not_found", "Kubernetix couldn't find that.", "Check the name and try again.", 4}},
		{"validation", &Error{Status: 400, Code: "validation_error", Message: "name must be 63 characters or fewer"}, Problem{"validation_error", "Kubernetix didn't accept the request: name must be 63 characters or fewer.", "Check the values you gave and run the command again.", 2}},
		{"duplicate app", apiErr(409, "duplicate_app", ErrContext{Org: "acme-corp", App: "shop-api"}), Problem{"duplicate_app", "An app called shop-api already exists in acme-corp.", "Deploy to it instead: kx link shop-api", 2}},
		{"app deleting", apiErr(409, "app_deleting", ErrContext{Org: "acme-corp", App: "shop-api"}), Problem{"app_deleting", "shop-api is being deleted, so it can't be deployed.", "Link this folder to another app: kx link", 2}},
		{"other plan limit", withLimit(apiErr(402, "tier_limit_exceeded", loginCtx), "max_apps"), Problem{"plan_limit_reached", "acme-corp has reached its plan's limit for apps.", "Upgrade for more: platform.kubernetix.ai/plans", 5}},
		{"plan feature", apiErr(402, "plan_upgrade_required", loginCtx), Problem{"plan_limit_reached", "acme-corp's plan doesn't include that.", "Upgrade: platform.kubernetix.ai/plans", 5}},
		{"billing unpaid", &Error{Status: 402, Code: "billing_unpaid", Message: billingOverdueMsg, Ctx: loginCtx}, Problem{"billing_unpaid", billingOverdueMsg, "Pay the overdue balance on acme-corp's Billing page in the dashboard: platform.kubernetix.ai/settings?tab=billing", 5}},
		{"subscription ended", &Error{Status: 402, Code: "subscription_ended", Message: billingEndedMsg, Ctx: envCtx}, Problem{"subscription_ended", billingEndedMsg, "Subscribe again on acme-corp's Billing page in the dashboard: platform.kubernetix.ai/settings?tab=billing", 5}},
		{"general rate limit", apiErr(429, "rate_limit_exceeded", loginCtx), Problem{"rate_limited", "Kubernetix is getting too many requests from acme-corp right now.", "Wait a minute, then try again.", 5}},
		{"registry not ready", apiErr(503, "registry_not_ready", loginCtx), Problem{"registry_not_ready", "acme-corp's image registry isn't ready yet.", "Wait a minute, then run kx deploy again.", 7}},
		{"rollback target not live", apiErr(400, "invalid_deployment_status", loginCtx), Problem{"invalid_deployment_status", "That version never went live, so kx can't roll back to it.", "Pick a version that went live: kx status", 2}},
		{"server error", apiErr(500, "database_error", loginCtx), Problem{"server_error", "Something went wrong on Kubernetix's side.", "Try again in a minute; if it keeps happening, open an issue at github.com/Kubernetix-AI/kx/issues", 1}},
		{"unknown 4xx", &Error{Status: 418, Code: "teapot", Message: "short and stout"}, Problem{"request_rejected", "Kubernetix didn't accept the request: short and stout.", "Check the values you gave and run the command again.", 2}},
		{"network", &NetworkError{Err: errors.New("dial tcp: connection refused")}, Problem{"network", "kx couldn't reach Kubernetix.", "Check your internet connection, then try again.", 1}},
		{"insecure URL", &kxerr.InsecureURL{URL: "http://x", Source: "KX_API_URL"}, Problem{"insecure_api_url", "kx only talks to Kubernetix over https, and KX_API_URL isn't an https address.", "Use an https:// address in KX_API_URL, or remove it.", 2}},
		{"insecure URL flag", &kxerr.InsecureURL{URL: "http://x", Source: "--api-url"}, Problem{"insecure_api_url", "kx only talks to Kubernetix over https, and --api-url isn't an https address.", "Use an https:// address with --api-url.", 2}},
		{"API URL with a password", &kxerr.InsecureURL{URL: "https://u:p@x", Source: "KX_API_URL", Reason: kxerr.URLCredentials}, Problem{"insecure_api_url", "KX_API_URL has a user name or password in it, and kx never sends those.", "Remove the user name and password from KX_API_URL.", 2}},
		{"API URL with a query", &kxerr.InsecureURL{URL: "https://x/?a", Source: "--api-url", Reason: kxerr.URLQueryOrFragment}, Problem{"insecure_api_url", "--api-url has a ? or # part, which an API address can't have.", "Remove everything from the ? or # onwards in --api-url.", 2}},
		{"API URL not a URL", &kxerr.InsecureURL{URL: "x", Source: "config", Reason: kxerr.URLInvalid}, Problem{"insecure_api_url", "The api_url in kx's settings file isn't a web address kx can use.", "Remove api_url from kx's settings file, or make it an https:// address.", 2}},
		{"API URL from elsewhere", &kxerr.InsecureURL{URL: "http://x"}, Problem{"insecure_api_url", "kx only talks to Kubernetix over https, and the API address isn't an https address.", "Use an https:// API address.", 2}},
		{"insecure URL config", &kxerr.InsecureURL{URL: "http://x", Source: "config"}, Problem{"insecure_api_url", "kx only talks to Kubernetix over https, and the api_url in kx's settings file isn't an https address.", "Remove api_url from kx's settings file, or make it an https:// address.", 2}},
		{"usage", &kxerr.Usage{What: "kx logs takes no arguments.", Fix: "See examples: kx logs --help"}, Problem{"usage", "kx logs takes no arguments.", "See examples: kx logs --help", 2}},
		{"not linked", kxerr.ErrNotLinked, Problem{"not_linked", "This folder isn't linked to an app yet.", "Run: kx link", 2}},
		{"multi-region", &kxerr.MultiRegion{App: "shop-api"}, Problem{"multi_region_app", "shop-api runs in several regions, and kx can't deploy those yet.", "Deploy it from the dashboard: platform.kubernetix.ai", 2}},
		{"login in CI", kxerr.ErrLoginInCI, Problem{"login_in_ci", "kx login saves a token on this computer, so it doesn't run in CI.", "Save a deploy token as the KX_TOKEN secret instead.", 2}},
		{"windows containers", kxerr.ErrDockerWindowsContainers, Problem{"docker_windows_containers", "Docker Desktop is set to Windows containers, and kx builds Linux images.", "Switch Docker Desktop to Linux containers, then run kx deploy again.", 10}},
		{"build failed", &kxerr.BuildFailed{Err: errors.New("exit status 1")}, Problem{"build_failed", "Docker couldn't build your app.", "Fix the error shown above, then run kx deploy again.", 6}},
		{"push failed", &kxerr.PushFailed{Err: errors.New("exit status 1")}, Problem{"push_failed", "kx couldn't upload your image to Kubernetix.", "Check your internet connection, then run kx deploy again.", 7}},
		{"wait timeout", &kxerr.WaitTimeout{App: "shop-api", Version: 15, After: 18 * time.Minute}, Problem{"wait_timeout", "shop-api v15 is still starting after 18 minutes, so kx stopped waiting; the deploy continues.", "Check on it: kx status", 9}},
		{"no room in the plan", &kxerr.DeployFailed{App: "shop-api", Version: 15, ServingVersion: 14, Code: "quota_blocked", Reason: quotaRestoredMsg},
			Problem{"quota_blocked", "shop-api v15 didn't go live: " + quotaRestoredMsg, quotaFix, 5}},
		{"no room in the plan, first deploy", &kxerr.DeployFailed{App: "shop-api", Version: 1, Code: "quota_blocked", Reason: quotaFirstMsg},
			Problem{"quota_blocked", "shop-api v1 didn't go live: " + quotaFirstMsg, quotaFix, 5}},
		{"no room in the plan, no message", &kxerr.DeployFailed{App: "shop-api", Version: 15, ServingVersion: 14, Code: "quota_blocked"},
			Problem{"quota_blocked", "shop-api v15 didn't go live: your plan has no room left for it. Version 14 is still serving.", quotaFix, 5}},
		// The platform's class beats what kx guessed from the replicas.
		{"no room in the plan, replicas looked stuck", &kxerr.DeployFailed{App: "shop-api", Version: 15, ServingVersion: 14, Code: "quota_blocked", Reason: quotaRestoredMsg, NotStarting: &kxerr.NotStarting{After: 2 * time.Minute, Path: "/", Port: 8080, HealthCheck: true}},
			Problem{"quota_blocked", "shop-api v15 didn't go live: " + quotaRestoredMsg, quotaFix, 5}},
		{"unknown failure class", &kxerr.DeployFailed{App: "shop-api", Version: 15, ServingVersion: 14, Code: "some_new_class", Reason: "your container is crashing on start"},
			Problem{"deploy_failed", "shop-api v15 didn't go live: your container is crashing on start. Version 14 is still serving.", "See why: kx logs --deployment v15", 8}},
		{"deploy failed with reason, no earlier version", &kxerr.DeployFailed{App: "shop-api", Version: 1, Reason: "the image could not be pulled."}, Problem{"deploy_failed", "shop-api v1 didn't go live: the image could not be pulled.", "See why: kx logs --deployment v1", 8}},
		{"never ready, platform failed it", &kxerr.DeployFailed{App: "shop-api", Version: 15, ServingVersion: 14, Reason: "Deployment failed.", NotStarting: &kxerr.NotStarting{After: 2*time.Minute + 10*time.Second, Path: "/", Port: 8080, HealthCheck: true}},
			Problem{"deploy_failed", "shop-api v15 didn't go live: its container kept crashing or didn't answer health checks on / at port 8080. Version 14 is still serving.", "Make your app start and answer on / at port 8080 (the log lines above may say why it didn't), then run kx deploy again.", 8}},
		{"never ready, path not known", &kxerr.DeployFailed{App: "shop-api", Version: 1, NotStarting: &kxerr.NotStarting{After: 5 * time.Minute, Port: 3000, HealthCheck: true}},
			Problem{"deploy_failed", "shop-api v1 didn't go live: its container kept crashing or didn't answer health checks at port 3000.", "Make your app start and answer health checks at port 3000 (the log lines above may say why it didn't), then run kx deploy again.", 8}},
		{"never ready, no health check", &kxerr.DeployFailed{App: "shop-api", Version: 1, NotStarting: &kxerr.NotStarting{After: 90 * time.Second}},
			Problem{"deploy_failed", "shop-api v1 didn't go live: its container kept stopping right after it started.", "Fix what stops your app from starting (the log lines above may say), then run kx deploy again.", 8}},
		{"never ready, rollback", &kxerr.DeployFailed{App: "shop-api", Version: 16, ServingVersion: 15, NotStarting: &kxerr.NotStarting{After: 2 * time.Minute, Path: "/", Port: 8080, HealthCheck: true, Rollback: true}},
			Problem{"deploy_failed", "shop-api v16 didn't go live: its container kept crashing or didn't answer health checks on / at port 8080. Version 15 is still serving.", "Pick another version to roll back to: kx status", 8}},
		{"timeout with a likely cause", &kxerr.WaitTimeout{App: "shop-api", Version: 15, After: 18 * time.Minute, NotStarting: &kxerr.NotStarting{After: 2 * time.Minute, Path: "/", Port: 8080, HealthCheck: true}},
			Problem{"wait_timeout", "shop-api v15 isn't live after 18 minutes: its container keeps crashing or isn't answering health checks on / at port 8080, so kx stopped waiting; the deploy continues.", "Check on it with kx status; to fix it, make your app start and answer on / at port 8080, then run kx deploy again.", 9}},
		{"timeout with a likely cause, no health check", &kxerr.WaitTimeout{App: "shop-api", Version: 15, After: 18 * time.Minute, NotStarting: &kxerr.NotStarting{After: 2 * time.Minute}},
			Problem{"wait_timeout", "shop-api v15 isn't live after 18 minutes: its container keeps stopping right after it starts, so kx stopped waiting; the deploy continues.", "Check on it with kx status; the log lines above may say what stops your app from starting.", 9}},
		{"timeout with a likely cause, rollback", &kxerr.WaitTimeout{App: "shop-api", Version: 16, After: 18 * time.Minute, NotStarting: &kxerr.NotStarting{After: 2 * time.Minute, Path: "/", Port: 8080, HealthCheck: true, Rollback: true}},
			Problem{"wait_timeout", "shop-api v16 isn't live after 18 minutes: its container keeps crashing or isn't answering health checks on / at port 8080, so kx stopped waiting; the deploy continues.", "Check on it with kx status; if it doesn't go live, pick another version to roll back to.", 9}},
		{"bad .kx.json", &kxerr.BadConfig{Path: "/code/shop/.kx.json", Project: true, Reason: "not valid JSON"}, Problem{"bad_project_file", "The .kx.json in " + filepath.FromSlash("/code/shop") + " can't be read: not valid JSON.", "Fix it, or delete it and run: kx link", 2}},
		{"bad settings", &kxerr.BadConfig{Path: "/home/sam/.config/kx/config.json", Reason: "not valid JSON"}, Problem{"bad_settings_file", "kx's settings file /home/sam/.config/kx/config.json can't be read: not valid JSON.", "Delete it and run: kx login", 2}},
		{"interrupted", context.Canceled, Problem{"interrupted", "kx stopped because it was interrupted.", "Run the command again when you're ready.", 1}},
		{"deadline", context.DeadlineExceeded, Problem{"timeout", "Kubernetix took too long to answer.", "Try again in a minute.", 1}},
		{"unexpected", errors.New("boom"), Problem{"unexpected", "kx hit an unexpected problem: boom.", "Run the command again; if it keeps happening, open an issue at github.com/Kubernetix-AI/kx/issues", 1}},
		{"not live", &kxerr.NotLive{App: "web"}, Problem{"not_live", "web isn't live yet, so it has no URL.", "Deploy it: kx deploy", 4}},
		{"no such version", &kxerr.NoVersion{App: "shop-api", Version: "v99"}, Problem{"version_not_found", "shop-api has no v99.", "See recent versions: kx status", 4}},
		{"no such token", &kxerr.NoToken{Which: "kx_nothere", Org: "acme-corp"}, Problem{"token_not_found", "There's no token kx_nothere in acme-corp.", "See your tokens: kx token list", 4}},
		{"no such token (API)", apiErr(404, "token_not_found", loginCtx), Problem{"token_not_found", "That token isn't in acme-corp any more.", "See your tokens: kx token list", 4}},
		{"wrapped", fmt.Errorf("deploying: %w", kxerr.ErrDockerStopped), Problem{"docker_stopped", "Docker is installed but not running.", "Start Docker Desktop, then run kx deploy again.", 10}},

		// kx deploy and kx rollback.
		{"rollback unconfirmed", &kxerr.RollbackUnconfirmed{App: "shop-api", Version: 13}, Problem{"rollback_unconfirmed", "kx couldn't confirm the rollback of shop-api to v13 because Kubernetix didn't answer, so it may or may not have started.", "Check: kx status", 1}},
		{"docker context unknown", &kxerr.DockerContextUnknown{Name: "colima"}, Problem{"docker_context_unknown", "kx couldn't read the address of the Docker context colima, so it can't upload the image from the engine that built it.", "Set DOCKER_HOST to that engine's address, then run kx deploy again.", 10}},
		{"docker context unknown, odd name", &kxerr.DockerContextUnknown{Name: "x\ny"}, Problem{"docker_context_unknown", "kx couldn't read the address of your active Docker context, so it can't upload the image from the engine that built it.", "Set DOCKER_HOST to that engine's address, then run kx deploy again.", 10}},
		{"tag exists twice", &kxerr.PushFailed{Err: kxerr.ErrTagExists}, Problem{"tag_exists", "The registry already has an image with that tag and won't replace it.", "Wait a minute, then run kx deploy again.", 7}},
		{"no digest", fmt.Errorf("push: %w", kxerr.ErrNoDigest), Problem{"no_digest", "Docker didn't report the digest of the image it uploaded, so kx can't be sure what it would deploy.", "Update Docker, then run kx deploy again.", 7}},
		{"no Dockerfile in CI", &kxerr.NoDockerfile{Dir: "/home/runner/work/shop-api", Why: kxerr.NoDockerfileCI}, Problem{"no_dockerfile", "kx needs a Dockerfile to build your app, and there isn't one in /home/runner/work/shop-api.", "Add a Dockerfile to this folder and commit it; kx deploy on your computer can write one.", 2}},
		{"no Dockerfile, app not recognised", &kxerr.NoDockerfile{Dir: "/Users/sam/code/shop-api", Why: kxerr.NoDockerfileUndetected}, Problem{"no_dockerfile", "kx needs a Dockerfile to build your app, and there isn't one in /Users/sam/code/shop-api.", "kx can write one for Node, Python, Go and static sites, but couldn't tell what this folder is. Add a Dockerfile, or cd to your app's folder.", 2}},
		{"no Dockerfile, .env not ignored", &kxerr.NoDockerfile{Dir: "/Users/sam/code/shop-api", Why: kxerr.NoDockerfileEnvExposed}, Problem{"no_dockerfile", ".dockerignore doesn't list .env, so a .env file in this folder would be copied into the image.", "Add the lines **/.env and **/.env.* to .dockerignore, then run kx deploy again.", 2}},
		{"no Dockerfile, .env in a subfolder", &kxerr.NoDockerfile{Dir: "/Users/sam/code/shop-api", Why: kxerr.NoDockerfileEnvExposed, Exposed: []string{"config/.env"}}, Problem{"no_dockerfile", "config/.env would be copied into the image, because .dockerignore doesn't exclude it.", "Add the lines **/.env and **/.env.* to .dockerignore, then run kx deploy again.", 2}},
		{"no Dockerfile, several .env files", &kxerr.NoDockerfile{Dir: "/Users/sam/code/shop-api", Why: kxerr.NoDockerfileEnvExposed, Exposed: []string{".env.local", "api/.env", "web/.env", "worker/.env", "x/.env"}}, Problem{"no_dockerfile", ".env.local, api/.env, web/.env and 2 more would be copied into the image, because .dockerignore doesn't exclude them.", "Add the lines **/.env and **/.env.* to .dockerignore, then run kx deploy again.", 2}},
		{"no Dockerfile, kx can't write one", &kxerr.NoDockerfile{Dir: "/Users/sam/code/shop-api", What: "This looks like a Node app, but package.json has no start script, so kx can't tell how to run it.", Fix: `Add a "start" script to package.json, or add a Dockerfile.`}, Problem{"no_dockerfile", "This looks like a Node app, but package.json has no start script, so kx can't tell how to run it.", `Add a "start" script to package.json, or add a Dockerfile.`, 2}},
		{"docker permission", kxerr.ErrDockerPermission, Problem{"docker_permission", "Docker is running, but this user isn't allowed to use it.", "Add yourself to the docker group (sudo usermod -aG docker $USER), then log in again and run kx deploy.", 10}},
		{"previous deploy still running", &kxerr.PreviousRunning{App: "shop-api", Version: 14, After: 18 * time.Minute}, Problem{"previous_deploy_running", "shop-api v14 is still deploying after 18 minutes, so kx didn't start another deploy.", "Check on it: kx status", 9}},
		{"stopped waiting", &kxerr.StoppedWaiting{App: "shop-api", Version: 15}, Problem{"interrupted", "kx stopped waiting for shop-api v15 because it was interrupted; the deploy continues.", "Check on it: kx status", 1}},
		{"nothing live", &kxerr.NothingLive{App: "shop-api"}, Problem{"nothing_live", "shop-api has no live version to roll back from.", "Deploy it: kx deploy", 4}},
		{"no rollback target", &kxerr.NoRollbackTarget{App: "shop-api"}, Problem{"no_rollback_target", "shop-api has no earlier live version with a different image to roll back to.", "See recent versions: kx status", 4}},
		{"already live", &kxerr.AlreadyLive{App: "shop-api", Version: 14}, Problem{"already_live", "shop-api v14 is already live.", "Pick another version: kx status", 2}},
		{"rollback target never went live", &kxerr.NotRollbackable{App: "shop-api", Version: 13}, Problem{"invalid_deployment_status", "shop-api v13 never went live, so kx can't roll back to it.", "Pick a version that went live: kx status", 2}},
	}
}

var (
	httpCodeRe = regexp.MustCompile(`\b(HTTP|[1-5][0-9]{2})\b`)
	traceRe    = regexp.MustCompile(`(goroutine|panic|\.go:[0-9]+|Traceback)`)
)

func TestExplain_EveryCodeOneSentenceOneFix(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range explainCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := Explain(tc.err)
			covered[got.Code] = true
			if got.Code != tc.want.Code || got.Fix != tc.want.Fix || got.Exit != tc.want.Exit ||
				(tc.want.What != "" && got.What != tc.want.What) {
				t.Fatalf("Explain() =\n  %+v\nwant\n  %+v", got, tc.want)
			}
			checkShape(t, got)
		})
	}
	for _, code := range allCodes {
		if !covered[code] {
			t.Errorf("code %q has no test case", code)
		}
	}
	if t.Failed() {
		return
	}
	for code := range covered {
		if !containsString(allCodes, code) {
			t.Errorf("code %q is produced but not listed in allCodes", code)
		}
	}
}

// checkShape enforces the two-line rule: line one is one plain sentence
// (the deploy-failure row adds a second one naming the version still
// serving), line
// two is one fix; neither has HTTP codes, stack traces or line breaks.
func checkShape(t *testing.T, p Problem) {
	t.Helper()
	for _, s := range []string{p.What, p.Fix} {
		if s == "" || strings.ContainsAny(s, "\r\n") {
			t.Errorf("not a single non-empty line: %q", s)
		}
		if httpCodeRe.MatchString(s) {
			t.Errorf("mentions an HTTP code: %q", s)
		}
		if traceRe.MatchString(s) {
			t.Errorf("looks like a stack trace: %q", s)
		}
	}
	if !strings.HasSuffix(p.What, ".") {
		t.Errorf("line one does not end a sentence: %q", p.What)
	}
	sentences := strings.Count(strings.TrimSuffix(p.What, "."), ". ") + 1
	maxSentences := 1
	switch p.Code {
	case "deploy_failed":
		maxSentences = 2
	case "billing_unpaid", "subscription_ended":
		// Line one is the platform's own customer message, which says
		// what is paused and that running apps are not affected.
		maxSentences = 4
	case "quota_blocked":
		// The platform's message: what is full, what runs, what to do.
		maxSentences = 4
	}
	if sentences > maxSentences {
		t.Errorf("line one has %d sentences: %q", sentences, p.What)
	}
	maxFixSentences := 1
	if p.Code == "no_dockerfile" {
		// "Add a Dockerfile …, or cd to the folder that has one." is the
		// fix; the second sentence says how kx can write one instead.
		maxFixSentences = 2
	}
	if strings.Count(p.Fix, ". ")+1 > maxFixSentences {
		t.Errorf("line two has more than one fix: %q", p.Fix)
	}
	if p.Exit < 1 || p.Exit > 10 {
		t.Errorf("exit code %d out of range", p.Exit)
	}
}

// The early signal: two lines while kx keeps waiting.
func TestNotLiveYet(t *testing.T) {
	tests := []struct {
		name      string
		ns        kxerr.NotStarting
		what, fix string
	}{
		{"health check", kxerr.NotStarting{After: 2*time.Minute + 5*time.Second, Path: "/", Port: 8080, HealthCheck: true},
			"shop-api v15 isn't live yet: for 2 minutes its container has kept crashing or hasn't answered health checks on / at port 8080.",
			"Make your app start and answer on / at port 8080; the log lines below may say why it hasn't."},
		{"no health check", kxerr.NotStarting{After: 95 * time.Second},
			"shop-api v15 isn't live yet: for 95 seconds its container has kept stopping right after it started.",
			"Fix what stops your app from starting; the log lines below may say what it is."},
		{"port not known", kxerr.NotStarting{After: 3 * time.Minute, Path: "/healthz", HealthCheck: true},
			"shop-api v15 isn't live yet: for 3 minutes its container has kept crashing or hasn't answered health checks on /healthz.",
			"Make your app start and answer on /healthz; the log lines below may say why it hasn't."},
		{"rollback", kxerr.NotStarting{After: 2 * time.Minute, Path: "/", Port: 8080, HealthCheck: true, Rollback: true},
			"shop-api v15 isn't live yet: for 2 minutes its container has kept crashing or hasn't answered health checks on / at port 8080.",
			"If it doesn't go live, pick another version to roll back to: kx status"},
	}
	for _, tt := range tests {
		what, fix := NotLiveYet("shop-api", 15, &tt.ns)
		if what != tt.what || fix != tt.fix {
			t.Errorf("%s:\n got %q\n     %q\nwant %q\n     %q", tt.name, what, fix, tt.what, tt.fix)
		}
		checkShape(t, Problem{Code: "x", What: what, Fix: fix, Exit: 1})
	}
}

func TestExplain_DeployLimitWording(t *testing.T) {
	tests := []struct {
		err  *kxerr.DeployLimit
		want string
		fix  string
	}{
		{&kxerr.DeployLimit{Org: "acme-corp", Plan: "pro", Limit: 50, Now: time.Date(2026, 11, 14, 23, 50, 0, 0, time.UTC)}, "acme-corp has used all 50 deploys the Pro plan includes today; more are available at 00:00 UTC (in 10 min).", "Upgrade for more deploys a day: platform.kubernetix.ai/plans"},
		{&kxerr.DeployLimit{Org: "acme-corp", Now: time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)}, "acme-corp has used all the deploys its plan includes today; more are available at 00:00 UTC (in 3 h).", "Upgrade for more deploys a day: platform.kubernetix.ai/plans"},
		{&kxerr.DeployLimit{Org: "acme-corp", Limit: 1, Now: time.Date(2026, 11, 14, 23, 59, 30, 0, time.UTC)}, "acme-corp has used the 1 deploy its plan includes today; more are available at 00:00 UTC (in 1 min).", "Upgrade for more deploys a day: platform.kubernetix.ai/plans"},
	}
	for _, tt := range tests {
		got := Explain(tt.err)
		if got.What != tt.want || got.Fix != tt.fix {
			t.Errorf("Explain() = %q / %q\nwant %q / %q", got.What, got.Fix, tt.want, tt.fix)
		}
	}
}

func TestExplain_PushWaitRoundsUp(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                  "Wait 6 minutes, then run kx deploy again.",
		30 * time.Second:   "Wait 1 minute, then run kx deploy again.",
		61 * time.Second:   "Wait 2 minutes, then run kx deploy again.",
		360 * time.Second:  "Wait 6 minutes, then run kx deploy again.",
		3600 * time.Second: "Wait 60 minutes, then run kx deploy again.",
	} {
		if got := Explain(&kxerr.PushRateLimited{Wait: d}).Fix; got != want {
			t.Errorf("Wait %v: %q, want %q", d, got, want)
		}
	}
}

func TestExplain_NeverShowsATokenOrRawControlCharacters(t *testing.T) {
	errs := []error{
		&Error{Status: 400, Code: "validation_error", Message: "bad " + testToken},
		errors.New("boom " + testToken),
		&Error{Status: 418, Message: "line\x1b[2Jone\ntwo"},
		&Error{Status: 418, Message: "gnp.\u202eexe and \u2066more\u200b"},
	}
	for _, err := range errs {
		p := Explain(err)
		if strings.Contains(p.What+p.Fix, testToken) {
			t.Errorf("Explain(%T) shows a token", err)
		}
		if strings.ContainsAny(p.What, "\x1b\n\u202e\u2066\u200b") {
			t.Errorf("Explain(%T) keeps control characters: %q", err, p.What)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestExplain_RealTokenErrorBodies sends the token error bodies exactly as
// the API writes them (the error envelope with token_prefix, token_name
// and, for token_expired, expires_at as top-level fields) through the
// client and checks the two lines kx prints for each kind of token.
func TestExplain_RealTokenErrorBodies(t *testing.T) {
	const (
		expiredBody = `{"error":"token_expired","expires_at":"2027-02-09T13:45:00Z","message":"This API token has expired.","token_name":"github-deploy","token_prefix":"kx_AbCdEfGh"}`
		revokedBody = `{"error":"token_revoked","message":"This API token has been revoked.","token_name":"github-deploy","token_prefix":"kx_AbCdEfGh"}`
		removedBody = `{"error":"token_owner_removed","message":"The person who created this API token is no longer a member of this organization.","token_name":"github-deploy","token_prefix":"kx_AbCdEfGh"}`
		pendingBody = `{"error":"token_owner_pending","message":"The account that created this API token is awaiting approval.","token_name":"github-deploy","token_prefix":"kx_AbCdEfGh"}`
		invalidBody = `{"error":"invalid_token","message":"This API token is not valid."}`
		lookupBody  = `{"error":"token_lookup_unavailable","message":"API tokens can't be checked right now. Please retry shortly.","token_name":"github-deploy","token_prefix":"kx_AbCdEfGh"}`
	)
	env := ErrContext{Org: "acme-corp", TokenSource: kxerr.SourceEnv}
	login := ErrContext{Org: "acme-corp", TokenSource: kxerr.SourceLogin}
	paste := ErrContext{TokenSource: kxerr.SourcePaste}
	tests := []struct {
		name   string
		status int
		body   string
		ctx    ErrContext
		want   Problem
	}{
		{"expired KX_TOKEN names the date", 401, expiredBody, env, Problem{"token_expired", "The KX_TOKEN secret holds a token that expired on 9 Feb 2027.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", ExitAuth}},
		{"expired laptop login", 401, expiredBody, login, Problem{"login_ended", "Your login to acme-corp has ended.", "Run: kx login --org acme-corp", ExitAuth}},
		{"expired pasted token", 401, expiredBody, paste, Problem{"token_expired", "That token has already expired.", "Create a new token and paste it: kx login", ExitAuth}},
		{"revoked KX_TOKEN names the token", 401, revokedBody, env, Problem{"token_revoked", `The KX_TOKEN secret holds a token (kx_AbCdEfGh, "github-deploy") that was revoked.`, "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", ExitAuth}},
		{"revoked laptop login names the token", 401, revokedBody, login, Problem{"token_revoked", `The token this computer uses for acme-corp (kx_AbCdEfGh, "github-deploy") was revoked.`, "Run: kx login --org acme-corp", ExitAuth}},
		{"creator removed", 401, removedBody, env, Problem{"token_owner_removed", "This token stopped working because the person who created it is no longer a member of acme-corp.", "Ask an admin of acme-corp for a new token: Settings → API tokens", ExitAuth}},
		{"creator pending names the token", 403, pendingBody, login, Problem{"token_owner_pending", `This token (kx_AbCdEfGh, "github-deploy") can't be used until the account that created it is approved.`, "Ask an admin of acme-corp for a new token: Settings → API tokens", ExitAuth}},
		{"unknown KX_TOKEN", 401, invalidBody, env, Problem{"invalid_token", "Kubernetix doesn't recognise the token in the KX_TOKEN secret.", "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.", ExitAuth}},
		{"unknown laptop login", 401, invalidBody, login, Problem{"invalid_token", "Kubernetix doesn't recognise the token this computer uses for acme-corp.", "Run: kx login --org acme-corp", ExitAuth}},
		{"lookup unavailable", 503, lookupBody, login, Problem{"token_lookup_unavailable", "Kubernetix couldn't check your token just now.", "Wait a minute, then try again.", ExitUnexpected}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tt.status == http.StatusServiceUnavailable {
					w.Header().Set("Retry-After", "2")
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body+"\n")
			})
			c.Ctx = tt.ctx
			_, err := c.WhoAmI(context.Background())
			got := Explain(err)
			if got != tt.want {
				t.Fatalf("Explain() =\n  %+v\nwant\n  %+v", got, tt.want)
			}
			checkShape(t, got)
		})
	}
}

// The platform's messages for a version its plan had no room for
// (error_code quota_blocked), word for word, and kx's fix line.
const (
	quotaRestoredMsg = "Your plan's memory is full. The new version was not started; the previous one still runs. Pick a smaller size, remove an app or database, or upgrade at platform.kubernetix.ai/plans."
	quotaFirstMsg    = "Your plan's app instances are full. This version was not started, so nothing is running yet. Use fewer replicas, remove an app or database, or upgrade at platform.kubernetix.ai/plans."
	quotaFix         = "Free up room in your plan or upgrade at platform.kubernetix.ai/plans, then try again."
)

// Until the platform reports error_code, a version its plan had no room
// for is a failed deploy like any other (README, below the error table).
func TestExplain_NoRoomBeforeThePlatformReportsIt(t *testing.T) {
	got := Explain(&kxerr.DeployFailed{App: "shop-api", Version: 15, ServingVersion: 14, Reason: quotaRestoredMsg})
	want := Problem{"deploy_failed", "shop-api v15 didn't go live: " + quotaRestoredMsg + " Version 14 is still serving.", "See why: kx logs --deployment v15", ExitDeploy}
	if got != want {
		t.Fatalf("Explain() =\n  %+v\nwant\n  %+v", got, want)
	}
}

// The platform's customer messages for a billing block (402), word for word.
const (
	billingOverdueMsg  = "Payment for this organization is overdue. New deployments and new resources are paused until the balance is paid. Your running apps are not affected."
	billingInvoiceMsg  = "This organization has an unpaid invoice from its cancelled subscription. New deployments and new resources are paused until it is paid. Your running apps are not affected."
	billingApprovalMsg = "Your bank needs you to approve the latest payment for this organization. New deployments and new resources are paused until it is approved. Your running apps are not affected."
	billingEndedMsg    = "Your subscription has ended. Your apps keep running, but new deployments are paused until you subscribe again or change your usage to fit the Free plan."
	billingReduceHint  = " You can still reduce it: a smaller size, a lower quality tier, fewer replicas, autoscaling or the Security Sandbox off."
)

// TestExplain_RealBillingBodies sends the billing blocks exactly as the API
// writes them (402 with error billing_unpaid, sometimes a reason, or
// subscription_ended, and a customer message) through the client and
// checks the two lines: the platform's message, then the dashboard's
// Billing page. Every one exits 5, like the plan limits.
func TestExplain_RealBillingBodies(t *testing.T) {
	const billing = "platform.kubernetix.ai/settings?tab=billing"
	body := func(code, reason, msg string) string {
		m := map[string]string{"error": code, "message": msg}
		if reason != "" {
			m["reason"] = reason
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	env := ErrContext{Org: "acme-corp", App: "shop-api", TokenSource: kxerr.SourceEnv}
	login := ErrContext{Org: "acme-corp", TokenSource: kxerr.SourceLogin}
	tests := []struct {
		name string
		body string
		ctx  ErrContext
		want Problem
	}{
		{"overdue", body("billing_unpaid", "", billingOverdueMsg), env,
			Problem{"billing_unpaid", billingOverdueMsg, "Pay the overdue balance on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"invoice left open by a cancelled subscription", body("billing_unpaid", "outstanding_invoice", billingInvoiceMsg), login,
			Problem{"billing_unpaid", billingInvoiceMsg, "Pay the open invoice on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"payment awaiting approval", body("billing_unpaid", "payment_approval_required", billingApprovalMsg), env,
			Problem{"billing_unpaid", billingApprovalMsg, "Approve the payment on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"subscription ended", body("subscription_ended", "", billingEndedMsg), env,
			Problem{"subscription_ended", billingEndedMsg, "Subscribe again on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"subscription ended, with the reduce hint", body("subscription_ended", "", billingEndedMsg+billingReduceHint), login,
			Problem{"subscription_ended", billingEndedMsg + billingReduceHint, "Subscribe again on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"org not known", body("billing_unpaid", "", billingOverdueMsg), ErrContext{TokenSource: kxerr.SourceEnv},
			Problem{"billing_unpaid", billingOverdueMsg, "Pay the overdue balance on the dashboard's Billing page: " + billing, ExitLimit}},
		{"unknown reason", body("billing_unpaid", "some_new_reason", billingOverdueMsg), env,
			Problem{"billing_unpaid", billingOverdueMsg, "Pay the overdue balance on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"no message", `{"error":"billing_unpaid"}`, env,
			Problem{"billing_unpaid", "Payment for acme-corp is overdue, so new deploys are paused.", "Pay the overdue balance on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"no message, invoice", `{"error":"billing_unpaid","reason":"outstanding_invoice"}`, env,
			Problem{"billing_unpaid", "acme-corp has an unpaid invoice, so new deploys are paused.", "Pay the open invoice on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"no message, approval", `{"error":"billing_unpaid","reason":"payment_approval_required","message":""}`, env,
			Problem{"billing_unpaid", "The latest payment for acme-corp needs your approval, so new deploys are paused.", "Approve the payment on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
		{"no message, ended, org not known", `{"error":"subscription_ended","message":"  "}`, ErrContext{},
			Problem{"subscription_ended", "Your organization's subscription has ended, so new deploys are paused.", "Subscribe again on the dashboard's Billing page: " + billing, ExitLimit}},
		{"message without a full stop", body("billing_unpaid", "", "Payment is overdue"), env,
			Problem{"billing_unpaid", "Payment is overdue.", "Pay the overdue balance on acme-corp's Billing page in the dashboard: " + billing, ExitLimit}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/apps/app-1/deployments" {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPaymentRequired)
				_, _ = io.WriteString(w, tt.body+"\n")
			})
			c.Ctx = tt.ctx
			_, err := c.CreateDeployment(context.Background(), "app-1", CreateDeploymentRequest{ImageURL: "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat("ab", 32)}, "key-1")
			got := Explain(err)
			if got != tt.want {
				t.Fatalf("Explain() =\n  %+v\nwant\n  %+v", got, tt.want)
			}
			checkShape(t, got)
		})
	}
}

// Server text in a billing block is cleaned like any other: no control or
// bidi characters, no tokens, no workflow commands.
func TestExplain_BillingMessageIsCleaned(t *testing.T) {
	for _, msg := range []string{
		"Payment\x1b[2J is \u202eoverdue ##[warning]y ##vso[task.setvariable]z " + testToken + "\nnext",
		" \u3000::error::Payment is overdue",
	} {
		p := Explain(&Error{Status: 402, Code: "billing_unpaid", Message: msg})
		if strings.ContainsAny(p.What, "\x1b\n\u202e") || strings.Contains(p.What, testToken) ||
			strings.HasPrefix(strings.TrimSpace(p.What), "::") || strings.Contains(p.What, "##[") || strings.Contains(p.What, "##vso[") {
			t.Errorf("What = %q", p.What)
		}
		if !strings.Contains(p.What, "Payment") || p.Exit != ExitLimit {
			t.Errorf("Problem = %+v", p)
		}
	}
	if p := Explain(&Error{Status: 402, Code: "billing_unpaid", Message: "Overdue " + testToken}); !strings.Contains(p.What, "kx_01234567…") {
		t.Errorf("token not shortened to its prefix: %q", p.What)
	}
}

func TestExplain_ServerTextCannotIssueWorkflowCommands(t *testing.T) {
	for _, msg := range []string{"name ##[warning]bad", "x ##vso[task.prependpath]/tmp", "::error::top"} {
		p := Explain(&Error{Status: 400, Code: "validation_error", Message: msg})
		if strings.Contains(p.What, "##[") || strings.Contains(strings.ToLower(p.What), "##vso[") {
			t.Errorf("Explain kept a workflow command: %q", p.What)
		}
	}
}
