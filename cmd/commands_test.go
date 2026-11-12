package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/version"
)

func TestBareKx_Quickstart(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run(), 0)
	golden(t, "quickstart", h.out.String())

	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run(), 0)
	golden(t, "quickstart_logged_in", h.out.String())
}

func TestHelp_OpensWithExamples(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("--help"), 0)
	out := h.out.String()
	ex, usage := strings.Index(out, "Examples:"), strings.Index(out, "Usage:")
	if ex < 0 || usage < 0 || ex > usage {
		t.Fatalf("--help does not open with examples:\n%s", out)
	}
	for _, name := range []string{"login", "link", "status", "logs", "open", "apps", "token", "whoami", "logout", "version"} {
		if !strings.Contains(out, "\n  "+name+" ") {
			t.Errorf("--help does not list %s", name)
		}
	}
	for _, sub := range []string{"login", "logs", "token", "apps", "link", "status", "open", "whoami", "logout", "version"} {
		h.expect(h.run(sub, "--help"), 0)
		if !strings.Contains(h.out.String(), "Examples:") {
			t.Errorf("kx %s --help has no examples:\n%s", sub, h.out.String())
		}
	}
}

// kx help <command> works like kx <command> --help, but --help is what kx
// advertises, so the help command itself is hidden.
func TestHelp_CommandWorksButIsHidden(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("help", "logs"), 0)
	if !strings.Contains(h.out.String(), "kx logs --follow") {
		t.Fatalf("kx help logs:\n%s", h.out.String())
	}
	var help *cobra.Command
	for _, c := range newRoot(&App{}).Commands() {
		if c.Name() == "help" {
			help = c
		}
	}
	if help == nil || !help.Hidden {
		t.Fatalf("help command = %+v, want a hidden one", help)
	}
}

func TestUnknownCommand(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("deplyo"), 2)
	if h.err.String() != "kx has no command called \"deplyo\".\nSee every command: kx --help\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
	h.expect(h.run("lgos"), 2)
	if !strings.Contains(h.err.String(), "Did you mean: kx logs") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

func TestUnknownFlag(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("status", "--nope"), 2)
	if h.err.String() != "kx status doesn't have a --nope option.\nSee examples: kx status --help\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
	// pflag's own wording (with Go parse errors) never reaches the user.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"logs", "--follow=maybe"}, "kx logs --follow doesn't take \"maybe\".\nSee examples: kx logs --help\n"},
		{[]string{"logs", "-f=maybe"}, "kx logs --follow doesn't take \"maybe\".\nSee examples: kx logs --help\n"},
		{[]string{"logs", "--since"}, "kx logs --since needs a value.\nSee examples: kx logs --help\n"},
		{[]string{"token", "revoke", "x", "-y=maybe"}, "kx token revoke --yes doesn't take \"maybe\".\nSee examples: kx token revoke --help\n"},
	} {
		h.expect(h.run(tc.args...), 2)
		if h.err.String() != tc.want {
			t.Errorf("kx %v: stderr = %q, want %q", tc.args, h.err.String(), tc.want)
		}
	}

	h.expect(h.run("status", "--nope", "--json"), 2)
	if h.err.String() != "" || !strings.HasPrefix(h.out.String(), `{"error":{"code":"usage"`) {
		t.Fatalf("--json usage error: stdout=%q stderr=%q", h.out.String(), h.err.String())
	}
}

func TestVersion(t *testing.T) {
	old := version.Version
	version.Version = "1.0.0"
	defer func() { version.Version = old }()
	h := newHarness(t)
	h.expect(h.run("version"), 0)
	if !strings.HasPrefix(h.out.String(), "kx 1.0.0 (") || !strings.HasSuffix(h.out.String(), runtime.GOOS+"/"+runtime.GOARCH+")\n") {
		t.Fatalf("stdout = %q", h.out.String())
	}
	h.expect(h.run("version", "--json"), 0)
	var v map[string]string
	if err := json.Unmarshal(h.out.Bytes(), &v); err != nil || v["version"] != "1.0.0" || v["os"] != runtime.GOOS {
		t.Fatalf("json = %q", h.out.String())
	}
}

func TestLink_PickerDefaultsToFolderAndOffersNewApp(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.dir = filepath.Join(filepath.Dir(h.dir), "billing-worker")
	if err := os.MkdirAll(filepath.Join(h.dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("link"), 0)
	if !strings.Contains(h.err.String(), "Which app should this folder deploy to?\n  1) billing-worker (new app)\n  2) shop-api\n  3) web\n") {
		t.Fatalf("picker:\n%s", h.err.String())
	}
	_, p, err := config.FindProject(h.dir)
	if err != nil || p == nil || p.App != "billing-worker" || p.AppID != "" || p.Org != "acme-corp" {
		t.Fatalf(".kx.json = %+v, %v", p, err)
	}
	if !strings.Contains(h.err.String(), "billing-worker doesn't exist yet; kx deploy will create it.") {
		t.Fatalf("stderr = %s", h.err.String())
	}
}

func TestLink_PickerExistingFolderApp(t *testing.T) {
	h := newHarness(t) // folder is named shop-api
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("link"), 0)
	if strings.Contains(h.err.String(), "(new app)") {
		t.Fatalf("offered a new app when the folder's app exists:\n%s", h.err.String())
	}
	_, p, _ := config.FindProject(h.dir)
	if p == nil || p.App != "shop-api" || p.AppID != appShopID || p.Org != "acme-corp" {
		t.Fatalf(".kx.json = %+v", p)
	}
	if !strings.Contains(h.err.String(), "Linked this folder to shop-api in acme-corp (saved in .kx.json).\nNext: kx deploy\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

func TestLink_Arg(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("link", "web"), 0)
	_, p, _ := config.FindProject(h.dir)
	if p == nil || p.App != "web" || p.AppID != appWebID {
		t.Fatalf(".kx.json = %+v", p)
	}
	h.expect(h.run("init", "web", "--json"), 0)
	if !strings.Contains(h.out.String(), `"app_id": "`+appWebID+`"`) {
		t.Fatalf("json = %s", h.out.String())
	}
	h.expect(h.run("link", "Not An App"), 2)
}

func TestLink_AppToken(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokApp
	h.expect(h.run("link"), 0) // no prompt needed: the token names the app
	_, p, _ := config.FindProject(h.dir)
	if p == nil || p.App != "shop-api" || p.AppID != appShopID || p.Org != "acme-corp" {
		t.Fatalf(".kx.json = %+v", p)
	}
	h.expect(h.run("link", "web"), 3)
	if h.err.String() != "This token can only deploy shop-api, and you asked to deploy web.\nLog in with a token that can deploy web: kx login\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

func TestLink_NoTerminalNeedsName(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("link"), 2)
	if h.err.String() != "kx link needs an app name when it can't ask.\nRun: kx link <app>\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

func TestApps(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("apps", "list"), 0)
	golden(t, "apps_list", h.out.String())
	h.expect(h.run("apps"), 0)
	golden(t, "apps_list", h.out.String())

	h.expect(h.run("apps", "get", "shop-api"), 0)
	golden(t, "apps_get", h.out.String())

	h.expect(h.run("apps", "get", "nope"), 4)
	if h.err.String() != "There's no app called nope in acme-corp.\nSee your apps: kx apps list\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}

	h.expect(h.run("apps", "list", "--json"), 0)
	var v struct {
		Apps []map[string]any `json:"apps"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &v); err != nil || len(v.Apps) != 2 || v.Apps[0]["name"] != "shop-api" {
		t.Fatalf("json = %s", h.out.String())
	}
}

func TestApps_AppTokenSeesItsApp(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokApp
	h.expect(h.run("apps", "list"), 0)
	if !strings.Contains(h.out.String(), "shop-api") || strings.Contains(h.out.String(), "web") {
		t.Fatalf("stdout = %s", h.out.String())
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("status"), 0)
	golden(t, "status", h.out.String())

	h.expect(h.run("status", "--json"), 0)
	golden(t, "status_json", h.out.String())
}

func TestStatus_DegradesWhenReplicasFail(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.replicas = nil
	h.api.usage.Usage = nil
	h.expect(h.run("status"), 0)
	if !strings.Contains(h.out.String(), "Replicas  none running") || strings.Contains(h.out.String(), "Deploys") {
		t.Fatalf("stdout = %s", h.out.String())
	}
}

func TestOpen(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("open"), 0)
	want := "https://" + appShopID + "--acme-corp.apps.example.test"
	if h.out.String() != want+"\n" || len(h.opened) != 1 || h.opened[0] != want {
		t.Fatalf("stdout = %q opened = %v", h.out.String(), h.opened)
	}
	h.opened = nil
	h.expect(h.run("open", "--dashboard"), 0)
	if h.norm(h.out.String()) != "https://platform.kubernetix.ai/apps/"+appShopID+"\n" {
		t.Fatalf("stdout = %q", h.out.String())
	}
	h.expect(h.run("open", "--app", "web"), 4)
	if h.err.String() != "web isn't live yet, so it has no URL.\nDeploy it: kx deploy\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
	h.canOpen, h.opened = false, nil
	h.expect(h.run("open"), 0)
	if len(h.opened) != 0 || h.out.String() != want+"\n" {
		t.Fatalf("without a browser: stdout=%q opened=%v", h.out.String(), h.opened)
	}
}

func TestToken_Create(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("token", "create", "ci", "--app", "shop-api", "--expires", "7d"), 0)
	if h.out.String() != tokLabs+"\n" {
		t.Fatalf("stdout = %q, want the new token alone", h.out.String())
	}
	if !strings.HasPrefix(h.err.String(), "Copy this token now. You won't be able to see it again.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	body := h.api.created[0]
	perms, _ := json.Marshal(body["permissions"])
	if body["name"] != "ci" || body["app_id"] != appShopID || string(perms) != `["deploy","read"]` || body["expires_in_days"] != float64(7) {
		t.Fatalf("request = %v", body)
	}
}

func TestToken_CreateDefaults(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("token", "create"), 0)
	body := h.api.created[0]
	if !strings.HasPrefix(body["name"].(string), "kx-") {
		t.Fatalf("default name = %v", body["name"])
	}
	perms, _ := json.Marshal(body["permissions"])
	if string(perms) != `["deploy","read","registry:push"]` {
		t.Fatalf("org token default permissions = %s", perms)
	}
	if _, ok := body["expires_in_days"]; ok {
		t.Fatalf("default expiry should leave expires_in_days out, got %v", body)
	}
	// The default expiry gives way to the login's: it ends with the login.
	if !strings.HasSuffix(h.err.String(), "on every app until 11 Dec 2026.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	h.expect(h.run("token", "create", "x", "--json"), 0)
	if !strings.Contains(h.out.String(), `"token": "`+tokLabs+`"`) {
		t.Fatalf("--json lost the token: %s", h.out.String())
	}
}

// A token made with kx can't do more or last longer than the token that
// makes it (the platform refuses with token_not_allowed). kx says what the
// limit is and where to get a longer-lived token, in two lines.
func TestToken_CreateBeyondTheLoginExplains(t *testing.T) {
	const dashboardFix = "Create longer-lived tokens in the dashboard: Settings → API tokens\n"
	tests := []struct {
		name  string
		setup func(h *harness)
		args  []string
		want  string
	}{
		{"1y from a 30-day login", func(h *harness) { h.loginAs("acme-corp", tokCorp, &dec11) },
			[]string{"token", "create", "ci", "--expires", "1y"},
			"This login can only create tokens that end by 11 Dec 2026.\n" + dashboardFix},
		{"never from a 30-day login", func(h *harness) { h.loginAs("acme-corp", tokCorp, &dec11) },
			[]string{"token", "create", "ci", "--expires", "never"},
			"This login can only create tokens that end by 11 Dec 2026.\n" + dashboardFix},
		{"1y from KX_TOKEN", func(h *harness) { h.env["KX_TOKEN"] = tokCorp },
			[]string{"token", "create", "ci", "--expires", "365d"},
			"The token in KX_TOKEN can only create tokens that end by 11 Dec 2026.\n" + dashboardFix},
		{"never from a login that never ends", func(h *harness) { h.loginAs("acme-labs", tokLabs, nil) },
			[]string{"token", "create", "ci", "--expires", "never"},
			"This login can only create tokens with an end date.\nChoose one with --expires, up to 365d, or create a token that never expires in the dashboard: Settings → API tokens\n"},
		{"more permissions than the token", func(h *harness) { h.env["KX_TOKEN"] = tokApp },
			[]string{"token", "create", "ci", "--permissions", "deploy,push"},
			"The token in KX_TOKEN can only create tokens that can deploy and read.\nCreate tokens with more permissions in the dashboard: Settings → API tokens\n"},
		{"made by another token", func(h *harness) {
			h.env["KX_TOKEN"] = tokApp
			who := h.api.who[tokApp]
			child := *who.Token
			child.ParentTokenID, child.CreatedVia = "tok-corp", "token"
			who.Token = &child
			h.api.who[tokApp] = who
		}, []string{"token", "create", "ci"},
			"The token in KX_TOKEN was made by another token, so it can't create tokens.\nCreate tokens in the dashboard: Settings → API tokens\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			h.expect(h.run(tt.args...), 3)
			if h.err.String() != tt.want {
				t.Fatalf("stderr =\n%q\nwant\n%q", h.err.String(), tt.want)
			}
			if len(h.api.created) != 1 {
				t.Fatalf("requests sent: %d", len(h.api.created))
			}
		})
	}

	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("token", "create", "ci", "--expires", "never", "--json"), 3)
	if !strings.HasPrefix(h.out.String(), `{"error":{"code":"token_create_not_allowed","message":"This login can only create tokens that end by 11 Dec 2026."`) {
		t.Fatalf("--json = %q", h.out.String())
	}
	if body := h.api.created[0]; body["expires_in_days"] != nil {
		t.Fatalf("never should send expires_in_days: null, got %v", body)
	}
}

// Without flags, a token made from an app token is for that app and has at
// most that token's permissions, so the defaults never get refused.
func TestToken_CreateDefaultsStayWithinTheToken(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokApp
	h.expect(h.run("token", "create", "ci"), 0)
	body := h.api.created[0]
	perms, _ := json.Marshal(body["permissions"])
	if body["app_id"] != appShopID || string(perms) != `["deploy","read"]` {
		t.Fatalf("request = %v", body)
	}
}

func TestToken_CreateRejects(t *testing.T) {
	for _, args := range [][]string{
		{"token", "create", "x", "--expires", "0d"},
		{"token", "create", "x", "--expires", "366d"},
		{"token", "create", "x", "--expires", "2y"},
		{"token", "create", "x", "--permissions", "admin"},
		{"token", "create", "x", "--permissions", ""},
		{"token", "create", strings.Repeat("n", 65)},
	} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.expect(h.run(args...), 2)
		if len(h.api.created) != 0 {
			t.Fatalf("%v sent a request", args)
		}
	}
}

func TestToken_ListAndRevoke(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	used := nov12.Add(-2 * 60 * 1e9)
	h.api.tokens = []apiToken{
		{ID: "tok-corp", Name: "laptop", Prefix: "kx_01234567", Scope: "org", Permissions: []string{"deploy", "read", "registry:push"}, ExpiresAt: &dec11, LastUsedAt: &used, LastUsedUserAgent: "kx/1.0.0 (darwin/arm64)", Status: "active"},
		{ID: "tok-app", Name: "github", Prefix: "kx_00000000", Scope: "app", AppID: appShopID, AppName: "shop-api", Permissions: []string{"deploy", "read"}, Status: "active"},
	}
	h.expect(h.run("token", "list"), 0)
	golden(t, "token_list", h.out.String())

	h.expect(h.run("token", "revoke", "kx_00000000"), 2) // no terminal, no --yes
	h.expect(h.run("token", "revoke", "kx_00000000", "--yes"), 0)
	if !strings.Contains(strings.Join(h.api.got(), "\n"), "DELETE /api/v1/orgs/acme-corp/tokens/tok-app") {
		t.Fatal("no revoke request")
	}
	if h.err.String() != "Revoked kx_00000000 (github).\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}

	h.tty, h.stdin = true, "y\n"
	h.expect(h.run("token", "revoke", "github"), 0)
	h.tty, h.stdin = true, "n\n"
	h.expect(h.run("token", "revoke", "github"), 0)
	if !strings.Contains(h.err.String(), "Nothing revoked.") {
		t.Fatalf("stderr = %q", h.err.String())
	}

	h.expect(h.run("token", "revoke", "kx_nothere", "--yes"), 4)
	h.tty = false
	h.expect(h.run("token", "revoke", "kx_01234567", "--yes"), 0)
	if !strings.Contains(h.err.String(), "This computer was using that token for acme-corp; run kx login to log in again.") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// The dashboard shows "kx 1.0.0 on GitHub Actions" by parsing the
// User-Agent, so its shape is fixed: kx/<ver> (<os>/<arch>) on a laptop and
// kx/<ver> (<os>/<arch>; <CI name>) in CI.
func TestUserAgent_LaptopAndCI(t *testing.T) {
	old := version.Version
	version.Version = "1.0.0"
	defer func() { version.Version = old }()
	plat := runtime.GOOS + "/" + runtime.GOARCH
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"laptop", nil, "kx/1.0.0 (" + plat + ")"},
		{"GitHub Actions", map[string]string{"CI": "true", "GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push"}, "kx/1.0.0 (" + plat + "; GitHub Actions)"},
		{"GitLab CI", map[string]string{"CI": "true", "GITLAB_CI": "true"}, "kx/1.0.0 (" + plat + "; GitLab CI)"},
		{"Bitbucket Pipelines", map[string]string{"CI": "true", "BITBUCKET_BUILD_NUMBER": "7"}, "kx/1.0.0 (" + plat + "; Bitbucket Pipelines)"},
		{"other CI", map[string]string{"CI": "true"}, "kx/1.0.0 (" + plat + "; CI)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.env == nil {
				h.loginAs("acme-corp", tokCorp, &dec11)
			} else {
				h.env["KX_TOKEN"] = tokCorp
				for k, v := range tt.env {
					h.env[k] = v
				}
			}
			h.expect(h.run("apps", "list"), 0)
			uas := h.api.userAgents
			if len(uas) == 0 {
				t.Fatal("no request reached the API")
			}
			for _, ua := range uas {
				if ua != tt.want {
					t.Fatalf("User-Agent = %q, want %q", ua, tt.want)
				}
			}
		})
	}
}

// KX_TOKEN wins over this computer's saved logins, and the org of the last
// login (default_org) is not a request for that org, so it must not be
// mistaken for one. An org named by --org, KX_ORG or .kx.json still is.
func TestKXToken_IgnoresDefaultOrgButNotAskedOrg(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-labs", tokLabs, nil) // default_org is now acme-labs
	h.env["KX_TOKEN"] = tokCorp          // a token for acme-corp
	h.api.tokens = []apiToken{*orgToken("tok-corp", "kx_01234567", "acme-corp", &dec11)}
	h.expect(h.run("token", "list"), 0)
	if !strings.Contains(h.out.String(), "kx_01234567") {
		t.Fatalf("stdout = %q", h.out.String())
	}

	h.expect(h.run("token", "list", "--org", "acme-labs"), 3)
	if h.err.String() != "KX_TOKEN belongs to acme-corp, but you asked for acme-labs.\nPut a token for acme-labs in KX_TOKEN.\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
	h.link("shop-api", appShopID, "acme-labs")
	h.expect(h.run("token", "list"), 3)
	if h.err.String() != "KX_TOKEN belongs to acme-corp, but this folder deploys to acme-labs.\nPut a token for acme-labs in KX_TOKEN.\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// After rotating a token called ci, kx token revoke ci means the live one:
// names match only active tokens, while a prefix or ID matches any.
func TestToken_RevokeByNameSkipsDeadTokens(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.tokens = []apiToken{
		{ID: "tok-old", Name: "ci", Prefix: "kx_oldoldol", Scope: "org", Permissions: []string{"deploy"}, Status: "revoked"},
		{ID: "tok-exp", Name: "ci", Prefix: "kx_expexpex", Scope: "org", Permissions: []string{"deploy"}, Status: "expired"},
		{ID: "tok-new", Name: "ci", Prefix: "kx_newnewne", Scope: "org", Permissions: []string{"deploy"}, Status: "active"},
	}
	h.expect(h.run("token", "revoke", "ci", "--yes"), 0)
	if reqs := strings.Join(h.api.got(), "\n"); !strings.Contains(reqs, "DELETE /api/v1/orgs/acme-corp/tokens/tok-new") {
		t.Fatalf("did not revoke the active ci token:\n%s", reqs)
	}
	h.expect(h.run("token", "revoke", "kx_oldoldol", "--yes"), 0) // revoking again is harmless
}
