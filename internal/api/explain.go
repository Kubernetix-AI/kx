package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	"token_not_accepted", "token_app_mismatch", "token_org_mismatch", "token_create_not_allowed", "org_not_logged_in", "token_cannot_push", "token_not_allowed",
	"insufficient_permissions", "token_limit_reached", "token_lookup_unavailable",
	"app_not_found", "deployment_not_found", "org_not_found", "not_found", "not_live", "version_not_found", "token_not_found",
	"validation_error", "request_rejected", "duplicate_app", "app_deleting", "invalid_deployment_status",
	"deploy_limit_reached", "plan_limit_reached", "push_rate_limited", "rate_limited", "registry_not_ready",
	"billing_unpaid", "subscription_ended",
	"docker_missing", "docker_stopped", "docker_windows_containers", "docker_permission", "no_dockerfile",
	"build_failed", "push_failed", "deploy_failed", "quota_blocked", "wait_timeout", "previous_deploy_running",
	"nothing_live", "no_rollback_target", "already_live", "tag_exists", "tag_taken", "tag_check_failed", "no_digest", "rollback_unconfirmed", "docker_context_unknown",
	"multi_region_app", "not_linked", "usage", "invalid_env_var",
	"image_not_found", "image_refused", "database_not_ready", "env_prefix_conflict",
	"crash_loop", "oom_killed", "image_pull_failed", "container_config_error", "container_create_error", "login_in_ci", "insecure_api_url",
	"bad_project_file", "bad_settings_file", "network", "server_error", "timeout", "interrupted", "unexpected",
}

// digestRe matches an image digest.
var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// dockerContextNameRe is what Docker allows as a context name.
var dockerContextNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]{0,99}$`)

const (
	plansURL       = "platform.kubernetix.ai/plans"
	billingURL     = "platform.kubernetix.ai/settings?tab=billing"
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
		apiErr    *Error
		netErr    *NetworkError
		ended     *kxerr.LoginEnded
		orgNot    *kxerr.OrgNotLoggedIn
		elsewhere *kxerr.LoggedInElsewhere
		unlinked  *kxerr.Unlinked
		mismatch  *kxerr.TokenAppMismatch
		lacks     *kxerr.TokenLacks
		orgMis    *kxerr.TokenOrgMismatch
		invalid   *kxerr.InvalidToken
		insecure  *kxerr.InsecureURL
		usage     *kxerr.Usage
		noDocker  *kxerr.NoDockerfile
		limit     *kxerr.DeployLimit
		pushRate  *kxerr.PushRateLimited
		failed    *kxerr.DeployFailed
		timeout   *kxerr.WaitTimeout
		build     *kxerr.BuildFailed
		push      *kxerr.PushFailed
		multi     *kxerr.MultiRegion
		badCfg    *kxerr.BadConfig
		notLive   *kxerr.NotLive
		noVer     *kxerr.NoVersion
		noTok     *kxerr.NoToken
		prevRun   *kxerr.PreviousRunning
		stopped   *kxerr.StoppedWaiting
		noLive    *kxerr.NothingLive
		noTarget  *kxerr.NoRollbackTarget
		already   *kxerr.AlreadyLive
		notReady  *kxerr.NotRollbackable
		unknown   *kxerr.RollbackUnconfirmed
		dockCtx   *kxerr.DockerContextUnknown
		taken     *kxerr.TagTaken
		unchecked *kxerr.TagCheckFailed
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
	case errors.As(err, &elsewhere) && elsewhere.LastLogin:
		// The new login is now the last one, so running the command again
		// uses it.
		return Problem{"org_not_logged_in",
			"The token you pasted is for " + elsewhere.TokenOrg + ", but kx was going to use " + elsewhere.Org + ", where this computer was last logged in.",
			"Run " + elsewhere.Command + " again to use " + elsewhere.TokenOrg + ", or log in to " + elsewhere.Org + ": kx login --org " + elsewhere.Org, ExitAuth}
	case errors.As(err, &elsewhere):
		return Problem{"org_not_logged_in", "The token you pasted is for " + elsewhere.TokenOrg + ", but this command is for " + elsewhere.Org + ".", "Run: kx login --org " + elsewhere.Org, ExitAuth}
	case errors.Is(err, kxerr.ErrTokenCreateMoved):
		return Problem{"token_create_not_allowed", "kx can't create tokens; only people signed in to the dashboard can.", "Create one in the dashboard: Settings → API tokens (for CI, the app's Deploy tab)", ExitUsage}
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
	case errors.As(err, &lacks):
		return tokenLacks(lacks)
	case errors.Is(err, kxerr.ErrDockerMissing):
		return Problem{"docker_missing", "kx builds your app with Docker, and Docker isn't installed on this computer.", "Install Docker Desktop from docker.com and run kx deploy again.", ExitDocker}
	case errors.Is(err, kxerr.ErrDockerStopped):
		return Problem{"docker_stopped", "Docker is installed but not running.", "Start Docker Desktop, then run kx deploy again.", ExitDocker}
	case errors.Is(err, kxerr.ErrDockerWindowsContainers):
		return Problem{"docker_windows_containers", "Docker Desktop is set to Windows containers, and kx builds Linux images.", "Switch Docker Desktop to Linux containers, then run kx deploy again.", ExitDocker}
	case errors.Is(err, kxerr.ErrDockerPermission):
		return Problem{"docker_permission", "Docker is running, but this user isn't allowed to use it.", "Add yourself to the docker group (sudo usermod -aG docker $USER), then log in again and run kx deploy.", ExitDocker}
	case errors.As(err, &noDocker):
		return noDockerfile(noDocker)
	case errors.As(err, &limit):
		return deployLimit(limit.Org, limit.Plan, limit.Limit, limit.Now)
	case errors.As(err, &pushRate):
		return pushRateLimited(pushRate.Wait)
	case errors.As(err, &failed):
		return deployFailed(failed)
	case errors.As(err, &timeout):
		if ns := timeout.NotStarting; ns != nil {
			return Problem{"wait_timeout", fmt.Sprintf("%s v%d isn't live after %s: %s, so kx stopped waiting; the deploy continues.", timeout.App, timeout.Version, minutes(timeout.After), cause(ns, present)), stillWaitingFix(ns), ExitTimeout}
		}
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
	case errors.As(err, &taken):
		return tagTaken(taken)
	case errors.As(err, &unchecked):
		fix := "Check your internet connection, then run kx deploy again."
		switch unchecked.Cause {
		case kxerr.CheckRefused:
			fix = "Run kx deploy again in a minute; the registry didn't accept kx's upload credential."
		case kxerr.CheckUnavailable:
			fix = "The registry is having trouble; run kx deploy again in a few minutes."
		}
		return Problem{"tag_check_failed", "kx couldn't check whether " + unchecked.Image + ":" + unchecked.Tag + " already exists, so nothing was deployed.", fix, ExitPush}
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
	case errors.As(err, &unlinked):
		return explainUnlinked(unlinked)
	case errors.Is(err, kxerr.ErrNotLinked):
		return Problem{"not_linked", "This folder isn't linked to an app yet.", "Run: kx link", ExitUsage}
	case errors.Is(err, kxerr.ErrLoginInCI):
		return Problem{"login_in_ci", "kx login saves a token on this computer, so it doesn't run in CI.", "Save a deploy token as the KX_TOKEN secret instead.", ExitUsage}
	case errors.As(err, &usage):
		code := "usage"
		if usage.Code != "" {
			code = usage.Code
		}
		return Problem{code, usage.What, usage.Fix, ExitUsage}
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

// tagTaken explains a tag that already holds another image: kx never
// replaces it, and says how to deploy that image if the person knows it.
func tagTaken(e *kxerr.TagTaken) Problem {
	ref := e.Image + ":" + e.Tag
	held := ref // by digest when kx knows it, so that image and no other is deployed
	if digestRe.MatchString(e.Digest) {
		held = e.Image + "@" + e.Digest
	}
	// Anyone who can push to the organization's registry can write a tag,
	// so its image is only worth deploying to someone who knows it.
	fix := "Tag the commit with a new version, or, if you know that image, deploy it: kx deploy --image " + held
	if e.FromFlag {
		fix = "Choose another --tag, or, if you know that image, deploy it: kx deploy --image " + held
	}
	what := ref + " already holds another image, so kx left the tag alone and deployed nothing."
	if e.Raced {
		what = ref + " was given another image while kx was tagging it, so kx deployed nothing."
	}
	return Problem{"tag_taken", what, fix, ExitPush}
}

func explainAPI(e *Error) Problem {
	src, org := e.Ctx.TokenSource, e.Ctx.Org
	named := tokenLabel(e)

	if notAccepted(e) {
		return tokenNotAccepted(src, org)
	}
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
		if strings.HasSuffix(e.Path, "/rollback") && !missingPermissionRe.MatchString(e.Message) {
			return rollbackRefused(e.Message)
		}
		if m := missingPermissionRe.FindStringSubmatch(e.Message); m != nil {
			what := " doesn't have the " + permissionName(m[1]) + " permission."
			if m[1] == PermConfigure {
				who := "This token"
				if src == kxerr.SourceEnv {
					who = "The token in the KX_TOKEN secret"
				}
				return Problem{"token_not_allowed", who + what, configureFix(src, org), ExitAuth}
			}
			if src == kxerr.SourceEnv {
				return Problem{"token_not_allowed", "The token in the KX_TOKEN secret" + what, "Create a token with the access you need on the app's Deploy tab and replace the KX_TOKEN secret.", ExitAuth}
			}
			return Problem{"token_not_allowed", "This token" + what, "Log in with a token that can: " + strings.TrimPrefix(loginFix(org), "Run: "), ExitAuth}
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
		return Problem{"validation_error", "Kubernetix didn't accept the request: " + sentence(clean(e.Message, maxMessageLen)), hintOr(e, checkValuesFix), ExitUsage}

	// Kubernetix checks an image (that it exists, may be used, runs on
	// linux/amd64) before it deploys it or creates an app with it.
	case "image_not_found":
		return Problem{"image_not_found", platformSentence(e, "Kubernetix didn't find that image."), hintOr(e, "Check the image name and tag, then run the command again."), ExitNotFound}
	case "image_not_allowed", "image_access_denied", "image_credential_rejected", "image_platform_unsupported",
		"image_reference_invalid", "image_reference_too_long", "invalid_image_name", "ambiguous_image_tag":
		return Problem{"image_refused", platformSentence(e, "Kubernetix can't use that image."), hintOr(e, "Use another image, then run the command again."), ExitUsage}
	case "database_not_ready":
		return Problem{"database_not_ready", platformSentence(e, "A database the app uses isn't running yet."),
			"Wait until the database is running (the dashboard's Databases page shows it), then run the command again.", ExitUsage}
	case "env_prefix_conflict":
		fix := "Remove that variable from the app, or rebind the database with another prefix in the dashboard."
		if name, ok := strings.CutPrefix(e.Field, "env_vars."); ok && name != "" {
			fix = fmt.Sprintf("Remove %s from the app (kx env unset %s), or rebind the database with another prefix in the dashboard.", name, name)
		}
		return Problem{"env_prefix_conflict", platformSentence(e, "A variable of the app collides with one a database injects."), fix, ExitUsage}

	case "tier_limit_exceeded":
		if e.LimitName == "max_deployments_per_day" && e.Reason == "too_many_tries" {
			return tooManyTries(org, e.TriesLimit)
		}
		if e.LimitName == "max_deployments_per_day" {
			return deployLimit(org, "", 0, time.Time{})
		}
		return Problem{"plan_limit_reached", orgOr(org, "Your organization") + " has reached its plan's limit for " + limitLabel(e.LimitName) + ".", "Upgrade for more: " + plansURL, ExitLimit}
	case "billing_unpaid", "subscription_ended":
		return billingBlocked(e)
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
		return Problem{"request_rejected", "Kubernetix didn't accept the request: " + sentence(msg), hintOr(e, checkValuesFix), ExitUsage}
	}
	return Problem{"server_error", "Something went wrong on Kubernetix's side.", "Try again in a minute; if it keeps happening, open an issue at " + issuesURL, ExitUnexpected}
}

// notAccepted reports a 401 that carries no reason, or the generic one,
// which doesn't say whether the token itself is at fault.
func notAccepted(e *Error) bool {
	if e.Status != 401 {
		return false
	}
	return e.Code == "" || (e.Code == "invalid_token" && strings.TrimRight(e.Message, ". ") == "Invalid or expired token")
}

// tokenNotAccepted words notAccepted. The token may well still be good, so
// the fix is to try again, never to make a new token or log in again.
func tokenNotAccepted(src kxerr.TokenSource, org string) Problem {
	const again = "If it worked before, Kubernetix may be being updated: try again in a few minutes."
	switch src {
	case kxerr.SourceLogin:
		return Problem{"token_not_accepted", "Kubernetix didn't accept the token this computer uses for " + orgOr(org, "Kubernetix") + ".", again, ExitAuth}
	case kxerr.SourcePaste:
		return Problem{"token_not_accepted", "Kubernetix didn't accept that token.", "If you just created it, Kubernetix may be being updated: try again in a few minutes.", ExitAuth}
	}
	return Problem{"token_not_accepted", "Kubernetix didn't accept the token in the KX_TOKEN secret.", again, ExitAuth}
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

// shownApps is how many app names the fix for an unlinked folder lists.
const shownApps = 5

// explainUnlinked says how to go on in a folder that isn't linked when the
// organization has no apps, or several (listing a few, sorted).
func explainUnlinked(e *kxerr.Unlinked) Problem {
	if len(e.Apps) == 0 {
		return Problem{"not_linked", "There are no apps in " + orgOr(e.Org, "this organization") + " yet.", "Deploy this folder: kx deploy", ExitUsage}
	}
	names := slices.Sorted(slices.Values(e.Apps))
	list := strings.Join(names[:min(len(names), shownApps)], ", ")
	if more := len(names) - shownApps; more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return Problem{"not_linked", "This folder isn't linked to an app, so kx doesn't know which one you mean.",
		"Run: kx " + e.Command + " --app <name>. Your apps: " + list, ExitUsage}
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

// rollbackRefused explains a rollback an API token may not make: the
// version differs from what runs in settings the token can't change. The
// platform's message says which, then what to do instead.
func rollbackRefused(msg string) Problem {
	what, fix := splitSentence(clean(msg, maxMessageLen))
	if what == "" {
		what = "This token can't roll back to that version: it differs in settings the token can't change."
	}
	if fix == "" {
		fix = "Roll back in the dashboard, or deploy a new version."
	}
	return Problem{"token_not_allowed", what, fix, ExitAuth}
}

// hintOr is the platform's hint for a refused field, or fallback.
func hintOr(e *Error, fallback string) string {
	if h := clean(e.Hint, maxMessageLen); h != "" {
		return sentence(strings.ReplaceAll(h, "e.g. ", "for example "))
	}
	return fallback
}

// platformSentence is the platform's message as one sentence (a second
// one joins with a semicolon), or fallback when there is none.
func platformSentence(e *Error, fallback string) string {
	msg := clean(e.Message, maxMessageLen)
	if msg == "" {
		return fallback
	}
	first, rest := splitSentence(sentence(msg))
	if rest == "" {
		return first
	}
	return strings.TrimSuffix(first, ".") + "; " + lowerFirst(rest)
}

func loginEnded(org string) Problem {
	if org == "" {
		return Problem{"login_ended", "Your login to Kubernetix has ended.", "Run: kx login", ExitAuth}
	}
	return Problem{"login_ended", "Your login to " + org + " has ended.", "Run: kx login --org " + org, ExitAuth}
}

// missingPermissionRe finds the permission in the platform's refusal of a
// token that lacks one ("This API token doesn't have the read permission.").
var missingPermissionRe = regexp.MustCompile(`doesn't have the (deploy|read|registry:push|configure) permission`)

// permissionName is how the dashboard names a token permission.
func permissionName(p string) string {
	switch p {
	case PermDeploy:
		return "Deploy"
	case PermRead:
		return "Read"
	case PermPush:
		return "Push images"
	case PermConfigure:
		return "Configure"
	}
	return p
}

// tokenLacks explains a token that /whoami says can't do what kx deploy
// needs, naming the missing permissions.
func tokenLacks(e *kxerr.TokenLacks) Problem {
	names := make([]string, 0, len(e.Permissions))
	for _, p := range e.Permissions {
		names = append(names, permissionName(p))
	}
	noun := "permission"
	if len(names) > 1 {
		noun = "permissions"
	}
	who, fix := "This token", "Log in with a token that can deploy: kx login"
	switch e.Source {
	case kxerr.SourceEnv:
		who, fix = "The token in the KX_TOKEN secret", "Create a deploy token on the app's Deploy tab (it has Deploy, Read and Push images) and replace the KX_TOKEN secret."
	case kxerr.SourceLogin:
		who = "The token this computer uses"
		if e.Org != "" {
			who += " for " + e.Org
		}
		fix = "Log in with a token that can deploy: " + strings.TrimPrefix(loginFix(e.Org), "Run: ")
	}
	for _, p := range e.Permissions {
		if p == PermConfigure {
			fix = configureFix(e.Source, e.Org)
		}
	}
	if len(e.Permissions) == 1 && e.Permissions[0] == PermDeploy && e.Command != "" && e.Command != "kx deploy" {
		// kx env and kx scale can save the change without deploying.
		fix = "Add --no-deploy to save the change without deploying, or " + deployFix(e.Source, e.Org)
	}
	command := e.Command
	if command == "" {
		command = "kx deploy"
	}
	return Problem{"token_not_allowed", fmt.Sprintf("%s doesn't have the %s %s, which %s needs.", who, joinAnd(names), noun, command), fix, ExitAuth}
}

// billingBlocked explains a deploy (or new app) that Kubernetix refused
// because the organization's payment is overdue or its subscription ended.
// Line one is the platform's own message for the customer, which says what
// is paused and that running apps keep running; the fix names what lifts
// the block and the dashboard's Billing page. Exit 5, like a plan limit:
// running kx again won't help until someone acts in the dashboard.
func billingBlocked(e *Error) Problem {
	org := e.Ctx.Org
	page := "the dashboard's Billing page"
	if org != "" {
		page = possessive(org) + " Billing page in the dashboard"
	}
	var what, fix string
	switch {
	case e.Code == "subscription_ended":
		what = possessive(orgOr(org, "Your organization")) + " subscription has ended, so new deploys are paused."
		fix = "Subscribe again"
	case e.Reason == "outstanding_invoice":
		what = orgOr(org, "Your organization") + " has an unpaid invoice, so new deploys are paused."
		fix = "Pay the open invoice"
	case e.Reason == "payment_approval_required":
		what = "The latest payment for " + orgOr(org, "your organization") + " needs your approval, so new deploys are paused."
		fix = "Approve the payment"
	default:
		what = "Payment for " + orgOr(org, "your organization") + " is overdue, so new deploys are paused."
		fix = "Pay the overdue balance"
	}
	if msg := clean(e.Message, maxMessageLen); msg != "" {
		what = sentence(msg)
	}
	return Problem{e.Code, what, fix + " on " + page + ": " + billingURL, ExitLimit}
}

// noDockerfile explains a folder without a Dockerfile that kx didn't
// write one for. kx deploy prints the same first line before it offers to
// write one.
func noDockerfile(e *kxerr.NoDockerfile) Problem {
	if e.What != "" {
		return Problem{"no_dockerfile", e.What, e.Fix, ExitUsage}
	}
	what := "kx needs a Dockerfile to build your app, and there isn't one in " + e.Dir + "."
	switch e.Why {
	case kxerr.NoDockerfileCI:
		return Problem{"no_dockerfile", what, "Add a Dockerfile to this folder and commit it; kx deploy on your computer can write one.", ExitUsage}
	case kxerr.NoDockerfileUndetected:
		return Problem{"no_dockerfile", what, "kx can write one for Node, Python, Go and static sites, but couldn't tell what this folder is. Add a Dockerfile, or cd to your app's folder.", ExitUsage}
	case kxerr.NoDockerfileEnvExposed:
		return Problem{"no_dockerfile", envExposedWhat(e.Exposed), "Add the lines **/.env and **/.env.* to .dockerignore, then run kx deploy again.", ExitUsage}
	}
	return Problem{"no_dockerfile", what, "Add a Dockerfile to this folder, or cd to the folder that has one. To have kx write one, run kx deploy in a terminal.", ExitUsage}
}

// envExposedWhat says that .env files would go into the image, naming
// them when kx was given their names (because .dockerignore does list
// .env, just not them).
func envExposedWhat(exposed []string) string {
	if len(exposed) == 0 {
		return ".dockerignore doesn't list .env, so a .env file in this folder would be copied into the image."
	}
	if len(exposed) == 1 {
		return exposed[0] + " would be copied into the image, because .dockerignore doesn't exclude it."
	}
	names := joinAnd(exposed)
	if len(exposed) > 4 {
		names = strings.Join(exposed[:3], ", ") + " and " + strconv.Itoa(len(exposed)-3) + " more"
	}
	return names + " would be copied into the image, because .dockerignore doesn't exclude them."
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

// tooManyTries explains a deploy refused because the organization has
// started as many deploys today, failed ones included, as it may. It
// doesn't say "used all its deploys", which kx status, counting only
// deploys that didn't fail, would contradict. It names a number only when
// Kubernetix gave one (tries), and never the plan: an organization's own
// quota can differ from its plan's.
func tooManyTries(org string, tries int) Problem {
	who := orgOr(org, "Your organization")
	what := who + " has started as many deploys today as its plan allows in a day, failed ones included."
	if tries > 0 {
		what = fmt.Sprintf("%s has tried %d deploys today, the most it can try in a day.", who, tries)
	}
	return Problem{"deploy_limit_reached", what, "More are available at 00:00 UTC; fix what makes the deploys fail before trying again.", ExitLimit}
}

func deployFailed(e *kxerr.DeployFailed) Problem {
	if e.Code == "quota_blocked" {
		return noRoom(e)
	}
	if c, ok := failureClasses[e.Code]; ok {
		fix := strings.ReplaceAll(c.fix, "{v}", fmt.Sprintf("v%d", e.Version))
		return Problem{c.code, fmt.Sprintf("%s v%d didn't go live: %s.", e.App, e.Version, c.what) + servingSentence(e), fix, c.exit}
	}
	reason := e.Reason
	if reason == "" {
		reason = "your container is crashing on start"
	}
	fix := fmt.Sprintf("See why: kx logs --deployment v%d", e.Version)
	switch ns := e.NotStarting; {
	case platformNamedCause(reason):
		// The platform's message names the cause, then what to do.
		first, rest := splitSentence(clean(reason, maxMessageLen))
		reason = lowerFirst(first)
		if rest != "" {
			fix = rest
		}
	case ns != nil && (e.Code == "" || e.Code == "not_ready" || e.Reason == ""):
		// What kx saw is more specific than the platform's message for a
		// version that never got ready, unless the platform named a
		// specific cause: then its message says what went wrong. not_ready
		// only says the version never got ready.
		reason, fix = cause(ns, past), failedFix(ns)
	}
	what := fmt.Sprintf("%s v%d didn't go live: %s", e.App, e.Version, sentence(clean(reason, maxMessageLen))) + servingSentence(e)
	return Problem{"deploy_failed", what, fix, ExitDeploy}
}

// servingSentence says which version serves after a failed deploy: still,
// or again when Kubernetix put it back as a new version.
func servingSentence(e *kxerr.DeployFailed) string {
	switch {
	case e.ServingVersion > 0 && e.RestoredAs > 0:
		return fmt.Sprintf(" Version %d is serving again, recorded as v%d.", e.ServingVersion, e.RestoredAs)
	case e.ServingVersion > 0:
		return fmt.Sprintf(" Version %d is still serving.", e.ServingVersion)
	}
	return ""
}

// failureClass is how kx words a version that failed for a known class:
// the cause, the fix ({v} stands for the version) and the exit code.
type failureClass struct {
	code, what, fix string
	exit            int
}

// failureClasses word the classes Kubernetix gives a version that can't
// run. They beat what kx guessed from the replicas.
var failureClasses = map[string]failureClass{
	"crash_loop": {"crash_loop", "its container kept crashing on start",
		"Find the error in the log lines above (or kx logs --deployment {v}), fix it or the start command, then run kx deploy again.", ExitDeploy},
	"oom_killed": {"oom_killed", "it ran out of memory",
		"Give the app more memory (a larger size, in the dashboard) or make it use less, then run kx deploy again.", ExitDeploy},
	"image_pull_failed": {"image_pull_failed", "Kubernetix couldn't pull its image",
		"Check the image name and that Kubernetix may pull it (Registries, in the dashboard), then run kx deploy again.", ExitDeploy},
	"container_config_error": {"container_config_error", "its container couldn't be configured",
		"Check its environment variables (kx env), secrets and volumes, then run kx deploy again.", ExitDeploy},
	"container_create_error": {"container_create_error", "its container couldn't be created",
		"Check its start command, arguments and mounted paths, then run kx deploy again.", ExitDeploy},
	"image_not_found": {"image_not_found", "its image isn't in the registry",
		"Check the image name and tag, then run kx deploy again.", ExitNotFound},
	"image_not_allowed": {"image_refused", "its image is in another organization's private repository",
		"Use your own registry, a public image, or a registry you added a credential for in the dashboard.", ExitUsage},
	"image_access_denied": {"image_refused", "its image is private, and Kubernetix has no credential for that registry",
		"Add a credential for the registry (Registries, in the dashboard), or fix the image name.", ExitUsage},
	"image_credential_rejected": {"image_refused", "the registry rejected the credential for its image",
		"Rotate the credential (Registries, in the dashboard), then run kx deploy again.", ExitUsage},
	"image_platform_unsupported": {"image_refused", "its image has no linux/amd64 build",
		"Build it for linux/amd64 (kx deploy does) or publish a multi-arch image, then deploy again.", ExitUsage},
	"invalid_image_name": {"image_refused", "its image name isn't valid",
		"Use registry/repository:tag, then run kx deploy again.", ExitUsage},
}

// platformCauses open the messages Kubernetix gives when it fails a
// version at once because its pods can't run (out of memory, an image it
// can't pull, a container it can't configure or create, a crash loop).
// Kubernetix doesn't send a code for these yet, so kx knows them by their
// first words; an unknown message falls back to what kx saw.
var platformCauses = []string{
	"Your pod ran out of memory",
	"Couldn't pull the container image",
	"The image reference is not a valid name",
	"The container could not be configured",
	"The container could not be created",
	"Your container is crashing on start",
}

// splitSentence splits s after its first sentence.
func splitSentence(s string) (first, rest string) {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1], strings.TrimSpace(s[i+2:])
	}
	return s, ""
}

// lowerFirst lowercases s's first letter, for a sentence that follows a
// colon.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// platformNamedCause reports whether reason is one of platformCauses.
func platformNamedCause(reason string) bool {
	for _, c := range platformCauses {
		if strings.HasPrefix(strings.TrimSpace(reason), c) {
			return true
		}
	}
	return false
}

// noRoom explains a version the plan had no room for (error_code
// quota_blocked): exit 5, a plan limit, since deploying it again fails the
// same way until room is made. The platform's message already says what is
// full, what still runs and what to do, so kx adds no "still serving"
// sentence of its own; and it beats any cause kx guessed from the replicas.
func noRoom(e *kxerr.DeployFailed) Problem {
	what := fmt.Sprintf("%s v%d didn't go live: ", e.App, e.Version)
	if msg := clean(e.Reason, maxMessageLen); msg != "" {
		what += sentence(msg)
	} else {
		what += "your plan has no room left for it." + servingSentence(e)
	}
	return Problem{"quota_blocked", what, "Free up room in your plan or upgrade at " + plansURL + ", then try again.", ExitLimit}
}

// The tenses cause words a version's containers in: while kx still waits,
// when it stops waiting, and once the platform failed the version.
const (
	ongoing = iota
	present
	past
)

// cause words what kx saw of containers that didn't get ready: crashing
// on start, or (with an HTTP health check) not answering it, which is also
// what listening on another port looks like.
func cause(ns *kxerr.NotStarting, tense int) string {
	if !ns.HealthCheck {
		return [...]string{
			"for " + howLong(ns.After) + " its container has kept stopping right after it started",
			"its container keeps stopping right after it starts",
			"its container kept stopping right after it started",
		}[tense]
	}
	where := healthWhere(ns)
	return [...]string{
		"for " + howLong(ns.After) + " its container has kept crashing or hasn't answered health checks" + where,
		"its container keeps crashing or isn't answering health checks" + where,
		"its container kept crashing or didn't answer health checks" + where,
	}[tense]
}

// healthWhere is " on / at port 8080", with whatever parts are known.
func healthWhere(ns *kxerr.NotStarting) string {
	var where string
	if path := clean(ns.Path, 100); path != "" && !strings.Contains(path, " ") {
		where = " on " + path
	}
	if ns.Port > 0 {
		where += fmt.Sprintf(" at port %d", ns.Port)
	}
	return where
}

// passHint is what the app must do to pass: "answer on / at port 8080", or
// "answer health checks at port 8080" when the path isn't known.
func passHint(ns *kxerr.NotStarting) string {
	where := healthWhere(ns)
	if ns.Path == "" {
		return "answer health checks" + where
	}
	return "answer" + where
}

// NotLiveYet words what kx saw while it keeps waiting for App vVersion:
// the two lines it prints at once, before the log lines.
func NotLiveYet(app string, version int, ns *kxerr.NotStarting) (what, fix string) {
	what = fmt.Sprintf("%s v%d isn't live yet: %s.", app, version, cause(ns, ongoing))
	switch {
	case ns.Rollback:
		fix = "If it doesn't go live, pick another version to roll back to: kx status"
	case ns.HealthCheck:
		fix = "Make your app start and " + passHint(ns) + "; the log lines below may say why it hasn't."
	default:
		fix = "Fix what stops your app from starting; the log lines below may say what it is."
	}
	return what, fix
}

func failedFix(ns *kxerr.NotStarting) string {
	switch {
	case ns.Rollback:
		return "Pick another version to roll back to: kx status"
	case ns.HealthCheck:
		return "Make your app start and " + passHint(ns) + " (the log lines above may say why it didn't), then run kx deploy again."
	}
	return "Fix what stops your app from starting (the log lines above may say), then run kx deploy again."
}

func stillWaitingFix(ns *kxerr.NotStarting) string {
	switch {
	case ns.Rollback:
		return "Check on it with kx status; if it doesn't go live, pick another version to roll back to."
	case ns.HealthCheck:
		return "Check on it with kx status; to fix it, make your app start and " + passHint(ns) + ", then run kx deploy again."
	}
	return "Check on it with kx status; the log lines above may say what stops your app from starting."
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

// howLong says how long something lasted without overstating it: seconds
// under two minutes, otherwise whole minutes, rounded down.
func howLong(d time.Duration) string {
	if d < 2*time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
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

// configureFix says how to get a token that can configure: a deploy
// token's form never offers Configure, so CI needs an API token; kx login
// makes one with Configure ticked.
func configureFix(src kxerr.TokenSource, org string) string {
	if src == kxerr.SourceEnv {
		return "Create an API token with Configure (Settings → API tokens) and replace the KX_TOKEN secret."
	}
	return "Log in again and keep Configure ticked: " + strings.TrimPrefix(loginFix(org), "Run: ")
}

// deployFix, after "or ", says how to get a token that can deploy and
// configure.
func deployFix(src kxerr.TokenSource, org string) string {
	switch src {
	case kxerr.SourceEnv:
		return "create an API token with Deploy and Configure (Settings → API tokens) and replace the KX_TOKEN secret."
	case kxerr.SourceLogin:
		return "log in again with Deploy ticked: " + strings.TrimPrefix(loginFix(org), "Run: ")
	}
	return "log in with a token that can deploy: kx login"
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
