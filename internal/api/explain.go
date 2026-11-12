package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// Exit codes. They are part of kx's public interface.
const (
	ExitOK         = 0
	ExitUnexpected = 1
	ExitUsage      = 2
	ExitAuth       = 3
	ExitNotFound   = 4
	ExitLimit      = 5
	ExitBuild      = 6
	ExitPush       = 7
	ExitDeploy     = 8
	ExitTimeout    = 9
	ExitDocker     = 10
)

// Problem is what kx shows when a command fails: What happened (one plain
// sentence), the one Fix, a stable Code for --json, and the exit code.
type Problem struct {
	Code string `json:"code"`
	What string `json:"message"`
	Fix  string `json:"fix"`
	Exit int    `json:"-"`
}

// allCodes lists every Code Explain can return; a test keeps it honest.
var allCodes = []string{
	"not_logged_in", "login_ended", "token_expired", "token_revoked", "invalid_token", "legacy_token",
	"token_owner_removed", "token_owner_rejected", "token_owner_pending", "token_owner_role",
	"token_app_mismatch", "token_org_mismatch", "token_create_not_allowed", "org_not_logged_in", "token_cannot_push", "token_not_allowed",
	"insufficient_permissions", "token_limit_reached", "token_lookup_unavailable",
	"app_not_found", "deployment_not_found", "org_not_found", "not_found", "not_live", "version_not_found", "token_not_found",
	"validation_error", "request_rejected", "duplicate_app", "app_deleting", "invalid_deployment_status",
	"deploy_limit_reached", "plan_limit_reached", "push_rate_limited", "rate_limited", "registry_not_ready",
	"docker_missing", "docker_stopped", "docker_windows_containers", "docker_permission", "no_dockerfile",
	"build_failed", "push_failed", "deploy_failed", "wait_timeout", "previous_deploy_running",
	"nothing_live", "no_rollback_target", "already_live", "tag_exists", "no_digest", "rollback_unconfirmed", "docker_context_unknown",
	"multi_region_app", "not_linked", "usage", "login_in_ci", "insecure_api_url",
	"bad_project_file", "bad_settings_file", "network", "server_error", "timeout", "interrupted", "unexpected",
}

// dockerContextNameRe is what Docker allows as a context name.
var dockerContextNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]{0,99}$`)

const (
	plansURL       = "platform.kubernetix.ai/plans"
	dashboardURL   = "platform.kubernetix.ai"
	issuesURL      = "github.com/Kubernetix-AI/kx/issues"
	newSecretFix   = "Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret." //nolint:gosec // G101: advice text, not a credential
	newPastedFix   = "Create a new token and paste it: kx login"
	retryLater     = "Wait a minute, then try again."
	checkValuesFix = "Check the values you gave and run the command again."
	appsListFix    = "See your apps: kx apps list"
)

// Explain turns any error kx can return into a Problem.
func Explain(err error) Problem {
	if err == nil {
		return Problem{Exit: ExitOK}
	}
	var (
		apiErr   *Error
		netErr   *NetworkError
		ended    *kxerr.LoginEnded
		orgNot   *kxerr.OrgNotLoggedIn
		mismatch *kxerr.TokenAppMismatch
		orgMis   *kxerr.TokenOrgMismatch
		refused  *kxerr.TokenCreateRefused
		invalid  *kxerr.InvalidToken
		insecure *kxerr.InsecureURL
		usage    *kxerr.Usage
		noDocker *kxerr.NoDockerfile
		limit    *kxerr.DeployLimit
		pushRate *kxerr.PushRateLimited
		failed   *kxerr.DeployFailed
		timeout  *kxerr.WaitTimeout
		build    *kxerr.BuildFailed
		push     *kxerr.PushFailed
		multi    *kxerr.MultiRegion
		badCfg   *kxerr.BadConfig
		notLive  *kxerr.NotLive
		noVer    *kxerr.NoVersion
		noTok    *kxerr.NoToken
		prevRun  *kxerr.PreviousRunning
		stopped  *kxerr.StoppedWaiting
		noLive   *kxerr.NothingLive
		noTarget *kxerr.NoRollbackTarget
		already  *kxerr.AlreadyLive
		notReady *kxerr.NotRollbackable
		unknown  *kxerr.RollbackUnconfirmed
		dockCtx  *kxerr.DockerContextUnknown
	)
	switch {
	case errors.As(err, &apiErr):
		return explainAPI(apiErr)
	case errors.As(err, &netErr):
		return Problem{"network", "kx couldn't reach Kubernetix.", "Check your internet connection, then try again.", ExitUnexpected}
	case errors.Is(err, kxerr.ErrNotLoggedIn):
		return Problem{"not_logged_in", "You're not logged in to Kubernetix.", "Run: kx login", ExitAuth}
	case errors.As(err, &ended):
		return loginEnded(ended.Org)
	case errors.As(err, &orgNot):
		what := "You asked for " + orgNot.Org
		if orgNot.FromFolder {
			what = "This folder deploys to " + orgNot.Org
		}
		return Problem{"org_not_logged_in", what + ", but this computer is only logged in to " + joinAnd(orgNot.LoggedIn) + ".", "Run: kx login --org " + orgNot.Org, ExitAuth}
	case errors.As(err, &refused):
		return explainCreateRefused(refused)
	case errors.As(err, &orgMis):
		what := "KX_TOKEN belongs to " + orgMis.TokenOrg + ", but you asked for " + orgMis.Org + "."
		if orgMis.FromFolder {
			what = "KX_TOKEN belongs to " + orgMis.TokenOrg + ", but this folder deploys to " + orgMis.Org + "."
		}
		return Problem{"token_org_mismatch", what, "Put a token for " + orgMis.Org + " in KX_TOKEN.", ExitAuth}
	case errors.As(err, &mismatch):
		return Problem{"token_app_mismatch", "This token can only deploy " + mismatch.TokenApp + ", and you asked to deploy " + mismatch.App + ".", "Log in with a token that can deploy " + mismatch.App + ": kx login", ExitAuth}
	case errors.As(err, &invalid):
		return explainInvalidToken(invalid)
	case errors.Is(err, kxerr.ErrNoPushPermission):
		return cannotPush()
	case errors.Is(err, kxerr.ErrDockerMissing):
		return Problem{"docker_missing", "kx builds your app with Docker, and Docker isn't installed on this computer.", "Install Docker Desktop from docker.com and run kx deploy again.", ExitDocker}
	case errors.Is(err, kxerr.ErrDockerStopped):
		return Problem{"docker_stopped", "Docker is installed but not running.", "Start Docker Desktop, then run kx deploy again.", ExitDocker}
	case errors.Is(err, kxerr.ErrDockerWindowsContainers):
		return Problem{"docker_windows_containers", "Docker Desktop is set to Windows containers, and kx builds Linux images.", "Switch Docker Desktop to Linux containers, then run kx deploy again.", ExitDocker}
	case errors.Is(err, kxerr.ErrDockerPermission):
		return Problem{"docker_permission", "Docker is running, but this user isn't allowed to use it.", "Add yourself to the docker group (sudo usermod -aG docker $USER), then log in again and run kx deploy.", ExitDocker}
	case errors.As(err, &noDocker):
		return Problem{"no_dockerfile", "There's no Dockerfile in " + noDocker.Dir + ".", "Run kx deploy in the folder that has your Dockerfile.", ExitUsage}
	case errors.As(err, &limit):
		return deployLimit(limit.Org, limit.Plan, limit.Limit, limit.Now)
	case errors.As(err, &pushRate):
		return pushRateLimited(pushRate.Wait)
	case errors.As(err, &failed):
		return deployFailed(failed)
	case errors.As(err, &timeout):
		return Problem{"wait_timeout", fmt.Sprintf("%s v%d is still starting after %s, so kx stopped waiting; the deploy continues.", timeout.App, timeout.Version, minutes(timeout.After)), "Check on it: kx status", ExitTimeout}
	case errors.As(err, &prevRun):
		return Problem{"previous_deploy_running", fmt.Sprintf("%s v%d is still deploying after %s, so kx didn't start another deploy.", prevRun.App, prevRun.Version, minutes(prevRun.After)), "Check on it: kx status", ExitTimeout}
	case errors.As(err, &stopped):
		return Problem{"interrupted", fmt.Sprintf("kx stopped waiting for %s v%d because it was interrupted; the deploy continues.", stopped.App, stopped.Version), "Check on it: kx status", ExitUnexpected}
	case errors.As(err, &noLive):
		return Problem{"nothing_live", noLive.App + " has no live version to roll back from.", "Deploy it: kx deploy", ExitNotFound}
	case errors.As(err, &noTarget):
		return Problem{"no_rollback_target", noTarget.App + " has no earlier live version with a different image to roll back to.", "See recent versions: kx status", ExitNotFound}
	case errors.As(err, &already):
		return Problem{"already_live", fmt.Sprintf("%s v%d is already live.", already.App, already.Version), "Pick another version: kx status", ExitUsage}
	case errors.As(err, &notReady):
		return Problem{"invalid_deployment_status", fmt.Sprintf("%s v%d never went live, so kx can't roll back to it.", notReady.App, notReady.Version), "Pick a version that went live: kx status", ExitUsage}
	case errors.As(err, &dockCtx):
		name := "your active Docker context"
		if dockerContextNameRe.MatchString(dockCtx.Name) {
			name = "the Docker context " + dockCtx.Name
		}
		return Problem{"docker_context_unknown", "kx couldn't read the address of " + name + ", so it can't upload the image from the engine that built it.", "Set DOCKER_HOST to that engine's address, then run kx deploy again.", ExitDocker}
	case errors.As(err, &unknown):
		// Exit 1 (network or server trouble), not 8: the rollback may well
		// have started, so "the deploy failed" would be wrong.
		return Problem{"rollback_unconfirmed", fmt.Sprintf("kx couldn't confirm the rollback of %s to v%d because Kubernetix didn't answer, so it may or may not have started.", unknown.App, unknown.Version), "Check: kx status", ExitUnexpected}
	case errors.Is(err, kxerr.ErrTagExists):
		return Problem{"tag_exists", "The registry already has an image with that tag and won't replace it.", "Wait a minute, then run kx deploy again.", ExitPush}
	case errors.Is(err, kxerr.ErrNoDigest):
		return Problem{"no_digest", "Docker didn't report the digest of the image it uploaded, so kx can't be sure what it would deploy.", "Update Docker, then run kx deploy again.", ExitPush}
	case errors.As(err, &build):
		return Problem{"build_failed", "Docker couldn't build your app.", "Fix the error shown above, then run kx deploy again.", ExitBuild}
	case errors.As(err, &push):
		return Problem{"push_failed", "kx couldn't upload your image to Kubernetix.", "Check your internet connection, then run kx deploy again.", ExitPush}
	case errors.As(err, &multi):
		return Problem{"multi_region_app", multi.App + " runs in several regions, and kx can't deploy those yet.", "Deploy it from the dashboard: " + dashboardURL, ExitUsage}
	case errors.As(err, &notLive):
		return Problem{"not_live", notLive.App + " isn't live yet, so it has no URL.", "Deploy it: kx deploy", ExitNotFound}
	case errors.As(err, &noVer):
		return Problem{"version_not_found", noVer.App + " has no " + noVer.Version + ".", "See recent versions: kx status", ExitNotFound}
	case errors.As(err, &noTok):
		return Problem{"token_not_found", "There's no token " + noTok.Which + " in " + orgOr(noTok.Org, "this organization") + ".", "See your tokens: kx token list", ExitNotFound}
	case errors.Is(err, kxerr.ErrNotLinked):
		return Problem{"not_linked", "This folder isn't linked to an app yet.", "Run: kx link", ExitUsage}
	case errors.Is(err, kxerr.ErrLoginInCI):
		return Problem{"login_in_ci", "kx login saves a token on this computer, so it doesn't run in CI.", "Save a deploy token as the KX_TOKEN secret instead.", ExitUsage}
	case errors.As(err, &usage):
		return Problem{"usage", usage.What, usage.Fix, ExitUsage}
	case errors.As(err, &insecure):
		return explainInsecure(insecure)
	case errors.As(err, &badCfg):
		if badCfg.Project {
			return Problem{"bad_project_file", "The .kx.json in " + filepath.Dir(badCfg.Path) + " can't be read: " + sentence(badCfg.Reason), "Fix it, or delete it and run: kx link", ExitUsage}
		}
		return Problem{"bad_settings_file", "kx's settings file " + badCfg.Path + " can't be read: " + sentence(badCfg.Reason), "Delete it and run: kx login", ExitUsage}
	case errors.Is(err, context.Canceled):
		return Problem{"interrupted", "kx stopped because it was interrupted.", "Run the command again when you're ready.", ExitUnexpected}
	case errors.Is(err, context.DeadlineExceeded):
		return Problem{"timeout", "Kubernetix took too long to answer.", "Try again in a minute.", ExitUnexpected}
	}
	return Problem{"unexpected", "kx hit an unexpected problem: " + sentence(clean(err.Error(), 200)), "Run the command again; if it keeps happening, open an issue at " + issuesURL, ExitUnexpected}
}

func explainAPI(e *Error) Problem {
	src, org := e.Ctx.TokenSource, e.Ctx.Org
	named := tokenLabel(e)

	switch e.Code {
	case "token_expired":
		switch src {
		case kxerr.SourceLogin:
			return loginEnded(org)
		case kxerr.SourcePaste:
			return Problem{"token_expired", "That token has already expired.", newPastedFix, ExitAuth}
		}
		if e.TokenExpiresAt != nil {
			return Problem{"token_expired", "The KX_TOKEN secret holds a token that expired on " + e.TokenExpiresAt.UTC().Format("2 Jan 2006") + ".", newSecretFix, ExitAuth}
		}
		return Problem{"token_expired", "The KX_TOKEN secret holds a token that has expired.", newSecretFix, ExitAuth}

	case "token_revoked":
		switch src {
		case kxerr.SourceLogin:
			return Problem{"token_revoked", "The token this computer uses for " + orgOr(org, "Kubernetix") + parens(named) + " was revoked.", loginFix(org), ExitAuth}
		case kxerr.SourcePaste:
			return Problem{"token_revoked", "That token was revoked.", newPastedFix, ExitAuth}
		}
		return Problem{"token_revoked", "The KX_TOKEN secret holds a token" + parens(named) + " that was revoked.", newSecretFix, ExitAuth}

	case "invalid_token", "not_authenticated", "authentication_failed", "invalid_authorization",
		"missing_authorization", "legacy_token_disabled":
		switch src {
		case kxerr.SourceLogin:
			return Problem{"invalid_token", "Kubernetix doesn't recognise the token this computer uses for " + orgOr(org, "Kubernetix") + ".", loginFix(org), ExitAuth}
		case kxerr.SourcePaste:
			return Problem{"invalid_token", "Kubernetix doesn't recognise that token.", newPastedFix, ExitAuth}
		}
		return Problem{"invalid_token", "Kubernetix doesn't recognise the token in the KX_TOKEN secret.", newSecretFix, ExitAuth}

	case "token_owner_removed":
		return Problem{"token_owner_removed", "This token stopped working because the person who created it is no longer a member of " + orgOr(org, "its organization") + ".", askAdmin(org, "a new token"), ExitAuth}
	case "token_owner_rejected":
		return Problem{"token_owner_rejected", "This token" + parens(named) + " stopped working because the account that created it was not approved.", askAdmin(org, "a new token"), ExitAuth}
	case "token_owner_pending":
		return Problem{"token_owner_pending", "This token" + parens(named) + " can't be used until the account that created it is approved.", askAdmin(org, "a new token"), ExitAuth}
	case "token_owner_role":
		return Problem{"token_owner_role", "This token" + parens(named) + " can't deploy because the person who created it can only view " + orgOr(org, "the organization") + ".", "Ask a developer or admin of " + orgOr(org, "your organization") + " for a deploy token: Settings → API tokens", ExitAuth}

	case "token_not_allowed":
		if e.Path == "/registry/credentials" {
			return cannotPush()
		}
		if src == kxerr.SourceEnv {
			return Problem{"token_not_allowed", "The token in the KX_TOKEN secret isn't allowed to do that.", "Create a token with the access you need on the app's Deploy tab and replace the KX_TOKEN secret.", ExitAuth}
		}
		return Problem{"token_not_allowed", "This token isn't allowed to do that.", "Log in with a token that can: " + strings.TrimPrefix(loginFix(org), "Run: "), ExitAuth}

	case "insufficient_permissions", "forbidden", "insufficient_role", "not_member", "no_org_context":
		return Problem{"insufficient_permissions", "Your role in " + orgOr(org, "this organization") + " doesn't allow that.", "Ask an admin of " + orgOr(org, "your organization") + " for access.", ExitAuth}

	case "token_limit_reached":
		return Problem{"token_limit_reached", orgOr(org, "Your organization") + " already has as many active tokens as it can have.", "Revoke one you no longer use: kx token list, then kx token revoke <prefix>", ExitLimit}
	case "token_lookup_unavailable", "approval_unavailable":
		return Problem{"token_lookup_unavailable", "Kubernetix couldn't check your token just now.", retryLater, ExitUnexpected}

	case "app_not_found":
		switch {
		case e.Ctx.App != "" && org != "":
			return Problem{"app_not_found", "There's no app called " + e.Ctx.App + " in " + org + ".", appsListFix, ExitNotFound}
		case e.Ctx.App != "":
			return Problem{"app_not_found", "There's no app called " + e.Ctx.App + ".", appsListFix, ExitNotFound}
		}
		return Problem{"app_not_found", "That app doesn't exist.", appsListFix, ExitNotFound}
	case "token_not_found":
		return Problem{"token_not_found", "That token isn't in " + orgOr(org, "this organization") + " any more.", "See your tokens: kx token list", ExitNotFound}
	case "deployment_not_found":
		return Problem{"deployment_not_found", "That deployment doesn't exist.", "See recent deploys: kx status", ExitNotFound}
	case "org_not_found":
		if org != "" {
			return Problem{"org_not_found", "There's no organization called " + org + ".", "Check the name, or see where you're logged in: kx whoami", ExitNotFound}
		}
		return Problem{"org_not_found", "That organization doesn't exist.", "Check the name, or see where you're logged in: kx whoami", ExitNotFound}

	case "duplicate_app":
		app := orgOr(e.Ctx.App, "that name")
		return Problem{"duplicate_app", "An app called " + app + " already exists in " + orgOr(org, "this organization") + ".", "Deploy to it instead: kx link " + e.Ctx.App, ExitUsage}
	case "app_deleting", "deleting":
		return Problem{"app_deleting", orgOr(e.Ctx.App, "This app") + " is being deleted, so it can't be deployed.", "Link this folder to another app: kx link", ExitUsage}
	case "invalid_deployment_status":
		return Problem{"invalid_deployment_status", "That version never went live, so kx can't roll back to it.", "Pick a version that went live: kx status", ExitUsage}
	case "validation_error", "invalid_request", "invalid_name":
		return Problem{"validation_error", "Kubernetix didn't accept the request: " + sentence(clean(e.Message, maxMessageLen)), checkValuesFix, ExitUsage}

	case "tier_limit_exceeded":
		if e.LimitName == "max_deployments_per_day" {
			return deployLimit(org, "", 0, time.Time{})
		}
		return Problem{"plan_limit_reached", orgOr(org, "Your organization") + " has reached its plan's limit for " + limitLabel(e.LimitName) + ".", "Upgrade for more: " + plansURL, ExitLimit}
	case "plan_upgrade_required", "free_quality_limit":
		return Problem{"plan_limit_reached", possessive(orgOr(org, "Your organization")) + " plan doesn't include that.", "Upgrade: " + plansURL, ExitLimit}

	case "rate_limited", "rate_limit_exceeded":
		if e.Path == "/registry/credentials" {
			return pushRateLimited(e.RetryAfter)
		}
		return Problem{"rate_limited", "Kubernetix is getting too many requests from " + orgOr(org, "you") + " right now.", retryLater, ExitLimit}

	case "registry_not_ready", "registry_not_setup":
		return Problem{"registry_not_ready", possessive(orgOr(org, "Your organization")) + " image registry isn't ready yet.", "Wait a minute, then run kx deploy again.", ExitPush}
	}

	switch {
	case e.Status == 401:
		return explainAPI(&Error{Status: 401, Code: "invalid_token", Ctx: e.Ctx})
	case e.Status == 403:
		return explainAPI(&Error{Status: 403, Code: "forbidden", Ctx: e.Ctx})
	case e.Status == 404:
		return Problem{"not_found", "Kubernetix couldn't find that.", "Check the name and try again.", ExitNotFound}
	case e.Status == 402:
		return explainAPI(&Error{Status: 402, Code: "plan_upgrade_required", Ctx: e.Ctx})
	case e.Status == 429:
		return explainAPI(&Error{Status: 429, Code: "rate_limit_exceeded", Ctx: e.Ctx})
	case e.Status >= 400 && e.Status < 500:
		msg := clean(e.Message, maxMessageLen)
		if msg == "" {
			msg = "it was not valid"
		}
		return Problem{"request_rejected", "Kubernetix didn't accept the request: " + sentence(msg), checkValuesFix, ExitUsage}
	}
	return Problem{"server_error", "Something went wrong on Kubernetix's side.", "Try again in a minute; if it keeps happening, open an issue at " + issuesURL, ExitUnexpected}
}

func explainCreateRefused(e *kxerr.TokenCreateRefused) Problem {
	who := "This token"
	switch e.Source {
	case kxerr.SourceLogin:
		who = "This login"
	case kxerr.SourceEnv:
		who = "The token in KX_TOKEN"
	}
	const code, dashboard = "token_create_not_allowed", "Settings → API tokens"
	switch e.Reason {
	case kxerr.CreateExpiry:
		if e.Until != nil {
			return Problem{code, who + " can only create tokens that end by " + e.Until.UTC().Format("2 Jan 2006") + ".", "Create longer-lived tokens in the dashboard: " + dashboard, ExitAuth}
		}
		return Problem{code, who + " can only create tokens with an end date.", "Choose one with --expires, up to 365d, or create a token that never expires in the dashboard: " + dashboard, ExitAuth}
	case kxerr.CreatePermissions:
		words := make([]string, 0, len(e.Permissions))
		for _, p := range e.Permissions {
			if p == PermPush {
				p = "push images"
			}
			words = append(words, p)
		}
		return Problem{code, who + " can only create tokens that can " + joinAnd(words) + ".", "Create tokens with more permissions in the dashboard: " + dashboard, ExitAuth}
	case kxerr.CreateApp:
		return Problem{code, who + " can only create tokens for " + e.App + ".", "Run: kx token create --app " + e.App, ExitAuth}
	}
	return Problem{code, who + " was made by another token, so it can't create tokens.", "Create tokens in the dashboard: " + dashboard, ExitAuth}
}

func explainInvalidToken(e *kxerr.InvalidToken) Problem {
	if e.Legacy {
		if e.Source == kxerr.SourceEnv {
			return Problem{"legacy_token", "The KX_TOKEN secret holds an old ct_ API key, which kx can't use.", newSecretFix, ExitAuth}
		}
		return Problem{"legacy_token", "kx can't use the old ct_ API keys.", "Create a token in Settings → API tokens and paste it: kx login", ExitAuth}
	}
	switch e.Source {
	case kxerr.SourceEnv:
		return Problem{"invalid_token", "The KX_TOKEN secret doesn't hold a whole Kubernetix token.", newSecretFix, ExitAuth}
	case kxerr.SourceLogin:
		return Problem{"invalid_token", "The token saved on this computer is damaged.", "Run: kx login", ExitAuth}
	}
	return Problem{"invalid_token", "That doesn't look like a whole Kubernetix token.", "Copy it again with the Copy button and paste it: kx login", ExitAuth}
}

func explainInsecure(e *kxerr.InsecureURL) Problem {
	// where names the setting in a sentence (Where at its start); in names
	// it after "in"; fixURL is the fix for an address that isn't usable.
	where, Where, in, fixURL := "the API address", "The API address", "the API address", "Use an https:// API address."
	switch e.Source {
	case "KX_API_URL":
		where, Where, in, fixURL = "KX_API_URL", "KX_API_URL", "KX_API_URL", "Use an https:// address in KX_API_URL, or remove it."
	case "--api-url":
		where, Where, in, fixURL = "--api-url", "--api-url", "--api-url", "Use an https:// address with --api-url."
	case "config":
		where, Where, in, fixURL = "the api_url in kx's settings file", "The api_url in kx's settings file", "api_url in kx's settings file",
			"Remove api_url from kx's settings file, or make it an https:// address."
	}
	var what, fix string
	switch e.Reason {
	case kxerr.URLCredentials:
		what, fix = Where+" has a user name or password in it, and kx never sends those.", "Remove the user name and password from "+in+"."
	case kxerr.URLQueryOrFragment:
		what, fix = Where+" has a ? or # part, which an API address can't have.", "Remove everything from the ? or # onwards in "+in+"."
	case kxerr.URLInvalid:
		what, fix = Where+" isn't a web address kx can use.", fixURL
	default:
		what, fix = "kx only talks to Kubernetix over https, and "+where+" isn't an https address.", fixURL
	}
	return Problem{"insecure_api_url", what, fix, ExitUsage}
}

func loginEnded(org string) Problem {
	if org == "" {
		return Problem{"login_ended", "Your login to Kubernetix has ended.", "Run: kx login", ExitAuth}
	}
	return Problem{"login_ended", "Your login to " + org + " has ended.", "Run: kx login --org " + org, ExitAuth}
}

func cannotPush() Problem {
	return Problem{"token_cannot_push", "This token can't upload images.", "Create a token with Push images on the app's Deploy tab, or deploy an image that's already pushed: kx deploy --image <image>", ExitAuth}
}

func pushRateLimited(wait time.Duration) Problem {
	mins := int((wait + time.Minute - 1) / time.Minute)
	if wait <= 0 {
		mins = 6
	}
	unit := "minutes"
	if mins == 1 {
		unit = "minute"
	}
	return Problem{"push_rate_limited", "This token has uploaded images too often in the last hour.", fmt.Sprintf("Wait %d %s, then run kx deploy again.", mins, unit), ExitLimit}
}

// deployLimit explains a used-up daily deploy allowance. The fix names no
// number: kx can't see the other plans' limits.
func deployLimit(org, plan string, limit int, now time.Time) Problem {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	left := now.Truncate(24 * time.Hour).Add(24 * time.Hour).Sub(now)
	who := orgOr(org, "Your organization")
	planName := "its plan"
	if plan != "" {
		planName = "the " + strings.ToUpper(plan[:1]) + plan[1:] + " plan"
	}
	var used string
	switch {
	case limit == 1:
		used = "the 1 deploy"
	case limit > 1:
		used = fmt.Sprintf("all %d deploys", limit)
	default:
		used = "all the deploys"
	}
	what := fmt.Sprintf("%s has used %s %s includes today; more are available at 00:00 UTC (in %s).", who, used, planName, untilText(left))
	return Problem{"deploy_limit_reached", what, "Upgrade for more deploys a day: " + plansURL, ExitLimit}
}

func deployFailed(e *kxerr.DeployFailed) Problem {
	reason := e.Reason
	if reason == "" {
		reason = "your container is crashing on start"
	}
	what := fmt.Sprintf("%s v%d didn't go live: %s", e.App, e.Version, sentence(clean(reason, maxMessageLen)))
	if e.ServingVersion > 0 {
		what += fmt.Sprintf(" Version %d is still serving.", e.ServingVersion)
	}
	return Problem{"deploy_failed", what, fmt.Sprintf("See why: kx logs --deployment v%d", e.Version), ExitDeploy}
}

// untilText renders a wait as "3 h 12 min", "3 h" or "12 min" (at least 1 min).
func untilText(d time.Duration) string {
	mins := int((d + time.Minute - 1) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	h, m := mins/60, mins%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m == 0:
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

func minutes(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m == 1 {
		return "1 minute"
	}
	if m < 1 {
		return d.Round(time.Second).String()
	}
	return fmt.Sprintf("%d minutes", m)
}

func tokenLabel(e *Error) string {
	switch {
	case e.TokenPrefix != "" && e.TokenName != "":
		return fmt.Sprintf("%s, %q", e.TokenPrefix, e.TokenName)
	case e.TokenPrefix != "":
		return e.TokenPrefix
	case e.TokenName != "":
		return fmt.Sprintf("%q", e.TokenName)
	}
	return ""
}

func parens(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func loginFix(org string) string {
	if org == "" {
		return "Run: kx login"
	}
	return "Run: kx login --org " + org
}

func askAdmin(org, what string) string {
	return "Ask an admin of " + orgOr(org, "your organization") + " for " + what + ": Settings → API tokens"
}

func orgOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func possessive(s string) string { return s + "'s" }

func limitLabel(name string) string {
	switch name {
	case "max_apps":
		return "apps"
	case "max_members":
		return "members"
	case "max_custom_domains":
		return "custom domains"
	case "max_secrets":
		return "secrets"
	case "max_replicas_per_app":
		return "replicas per app"
	case "":
		return "this"
	}
	return strings.ReplaceAll(strings.TrimPrefix(name, "max_"), "_", " ")
}

// sentence trims s and makes sure it ends with exactly one period.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".!? ")
	if s == "" {
		return "no details were given."
	}
	return s + "."
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return "no organization"
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
