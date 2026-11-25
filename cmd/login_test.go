package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func TestLogin_PasteFlow_Golden(t *testing.T) {
	h := newHarness(t)
	h.tty, h.password = true, "  "+tokCorp+"\n"
	h.expect(h.run("login"), 0)

	if h.out.String() != "" {
		t.Fatalf("stdout = %q, want nothing", h.out.String())
	}
	golden(t, "login_paste", h.norm(h.err.String()))

	if len(h.opened) != 1 || h.norm(h.opened[0]) != "https://platform.kubernetix.ai/settings?tab=tokens&new=cli&expires=1d" {
		t.Fatalf("opened %v", h.opened)
	}
	u := h.user()
	l, ok := u.Orgs["acme-corp"]
	if !ok || l.Token != tokCorp || l.ExpiresAt == nil || !l.ExpiresAt.Equal(dec11) || l.User != "sam@acme.dev" || u.DefaultOrg != "acme-corp" {
		t.Fatalf("saved login: default=%q org saved=%v expires=%v user=%q", u.DefaultOrg, ok, l.ExpiresAt, l.User)
	}
}

func TestLogin_NoBrowser_Golden(t *testing.T) {
	h := newHarness(t)
	h.tty, h.password, h.canOpen = true, tokCorp, false
	h.expect(h.run("login"), 0)
	if len(h.opened) != 0 {
		t.Fatalf("opened a browser: %v", h.opened)
	}
	golden(t, "login_no_browser", h.norm(h.err.String()))
}

func TestLogin_SecondOrgAdds(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.tty, h.password = true, tokLabs
	h.expect(h.run("login", "--org", "acme-labs"), 0)

	u := h.user()
	if u.Orgs["acme-corp"].Token != tokCorp || u.Orgs["acme-labs"].Token != tokLabs {
		t.Fatalf("logins: %v", u.OrgNames())
	}
	if u.DefaultOrg != "acme-labs" {
		t.Fatalf("default_org = %q, want the most recent login", u.DefaultOrg)
	}
	if u.Orgs["acme-labs"].ExpiresAt != nil {
		t.Fatalf("acme-labs token never expires, saved %v", u.Orgs["acme-labs"].ExpiresAt)
	}
	if !strings.Contains(h.err.String(), "Logged in to acme-labs as sam@acme.dev. This token can deploy every app in acme-labs and never expires.") {
		t.Fatalf("stderr = %s", h.err.String())
	}
}

func TestLogin_Expires(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		// kx asks for 1 day itself, so the default holds whatever the
		// token page's own default is.
		{[]string{"login"}, "https://platform.kubernetix.ai/settings?tab=tokens&new=cli&expires=1d"},
		{[]string{"login", "--expires", "1d"}, "https://platform.kubernetix.ai/settings?tab=tokens&new=cli&expires=1d"},
		{[]string{"login", "--org", "acme-corp", "--expires", "1d"}, "https://platform.kubernetix.ai/settings?tab=tokens&new=cli&org=acme-corp&expires=1d"},
		{[]string{"login", "--expires", "30d"}, "https://platform.kubernetix.ai/settings?tab=tokens&new=cli&expires=30d"},
		{[]string{"login", "--org", "acme-corp", "--expires=30d"}, "https://platform.kubernetix.ai/settings?tab=tokens&new=cli&org=acme-corp&expires=30d"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			h := newHarness(t)
			h.tty, h.password = true, tokCorp
			h.expect(h.run(tt.args...), 0)
			if len(h.opened) != 1 || h.norm(h.opened[0]) != tt.want {
				t.Fatalf("opened %v, want %s", h.opened, tt.want)
			}
			if !strings.Contains(h.norm(h.err.String()), tt.want) {
				t.Fatalf("the URL is not printed:\n%s", h.err.String())
			}
		})
	}
}

func TestLogin_ExpiresRejectsOtherValues(t *testing.T) {
	h := newHarness(t)
	h.tty, h.password = true, tokCorp
	h.expect(h.run("login", "--expires", "90d"), 2)
	if len(h.opened) != 0 {
		t.Fatal("opened the browser for a bad flag")
	}
	if want := "kx login --expires takes 1d or 30d (the default is 1 day).\nRun: kx login --expires 30d\n"; !strings.HasSuffix(h.err.String(), want) {
		t.Fatalf("stderr:\n%s\nwant it to end with\n%s", h.err.String(), want)
	}
	h.expect(h.run("login", "--help"), 0)
	if !strings.Contains(h.out.String(), "make the new token last this time: 1d or 30d (default 1 day)") ||
		!strings.Contains(h.out.String(), "kx login --expires 30d") {
		t.Fatalf("help:\n%s", h.out.String())
	}
}

func TestTokenSentence(t *testing.T) {
	tests := []struct {
		perms []string
		app   string
		want  string
	}{
		{[]string{"deploy", "read", "registry:push", "configure"}, "", "This token can deploy and configure every app in acme-corp until 11 Dec 2026."},
		{[]string{"configure", "read", "deploy"}, "shop-api", "This token can deploy and configure shop-api until 11 Dec 2026."},
		{[]string{"deploy", "read", "registry:push"}, "", "This token can deploy every app in acme-corp until 11 Dec 2026."},
		{[]string{"read", "configure"}, "", "This token can configure every app in acme-corp until 11 Dec 2026."},
		{[]string{"read"}, "", "This token can read every app in acme-corp until 11 Dec 2026."},
	}
	for _, tt := range tests {
		tok := &api.Token{Scope: "org", Permissions: tt.perms, ExpiresAt: &dec11}
		if tt.app != "" {
			tok.Scope, tok.AppName = "app", tt.app
		}
		if got := tokenSentence(tok, "acme-corp", nov12); got != tt.want {
			t.Errorf("tokenSentence(%v) = %q, want %q", tt.perms, got, tt.want)
		}
	}
}

func TestLogin_TokenThatCanConfigure(t *testing.T) {
	h := newHarness(t)
	h.tty, h.password = true, tokCorp
	h.api.who[tokCorp].Token.Permissions = []string{"deploy", "read", "registry:push", "configure"}
	h.api.who[tokCorp].Token.ExpiresAt = &nov13
	defer setLocal(time.FixedZone("UTC+2", 2*60*60))()
	h.expect(h.run("login"), 0)
	// It ends within two days, so kx shows the local time too.
	if !strings.Contains(h.err.String(), "Logged in to acme-corp as sam@acme.dev. This token can deploy and configure every app in acme-corp until 13 Nov 11:00.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestLogin_TokenStdin(t *testing.T) {
	h := newHarness(t)
	h.stdin = tokCorp + "\n"
	h.expect(h.run("login", "--token-stdin"), 0)
	if len(h.opened) != 0 {
		t.Fatal("--token-stdin opened a browser")
	}
	if h.user().Orgs["acme-corp"].Token != tokCorp {
		t.Fatal("token not saved")
	}
	if strings.Contains(h.err.String(), "Token:") {
		t.Fatalf("--token-stdin prompted: %s", h.err.String())
	}
}

func TestLogin_JSON(t *testing.T) {
	h := newHarness(t)
	h.stdin = tokCorp
	h.expect(h.run("login", "--token-stdin", "--json"), 0)
	want := "{\n  \"org\": \"acme-corp\",\n  \"user\": \"sam@acme.dev\",\n  \"token_prefix\": \"kx_01234567\",\n  \"scope\": \"org\",\n  \"permissions\": [\n    \"deploy\",\n    \"read\",\n    \"registry:push\"\n  ],\n  \"expires_at\": \"2026-12-11T09:00:00Z\"\n}\n"
	if h.out.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", h.out.String(), want)
	}
}

func TestLogin_Refusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		args  []string
		code  int
		line1 string
	}{
		{"legacy key", func(h *harness) { h.tty, h.password = true, "ct_12345678" }, []string{"login"}, 3, "kx can't use the old ct_ API keys."},
		{"typo", func(h *harness) { h.tty, h.password = true, tokCorp[:len(tokCorp)-1]+"X" }, []string{"login"}, 3, "That doesn't look like a whole Kubernetix token."},
		{"empty", func(h *harness) { h.tty, h.password = true, "" }, []string{"login"}, 3, "That doesn't look like a whole Kubernetix token."},
		{"unknown to the server", func(h *harness) { h.tty = true; delete(h.api.who, tokCorp); h.password = tokCorp }, []string{"login"}, 3, "Kubernetix doesn't recognise that token."},
		{"no terminal", func(h *harness) {}, []string{"login"}, 2, "kx login needs a terminal to paste the token into."},
		{"in CI", func(h *harness) { h.env["GITHUB_ACTIONS"] = "true"; h.stdin = tokCorp }, []string{"login", "--token-stdin"}, 2, "kx login saves a token on this computer, so it doesn't run in CI."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			h.expect(h.run(tt.args...), tt.code)
			if first := strings.SplitN(h.err.String(), "\n", 2)[0]; !strings.HasSuffix(h.err.String(), "\n") || !strings.Contains(h.err.String(), tt.line1) {
				t.Fatalf("stderr = %q (first line %q), want %q", h.err.String(), first, tt.line1)
			}
			if len(h.user().Orgs) != 0 {
				t.Fatal("saved a login after a refusal")
			}
		})
	}
}

func TestLogin_WarnsWhenKXTokenIsSet(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokLabs
	h.stdin = tokCorp
	h.expect(h.run("login", "--token-stdin"), 0)
	if !strings.Contains(h.err.String(), "KX_TOKEN is set, so kx keeps using it instead of this login.") {
		t.Fatalf("stderr = %s", h.err.String())
	}
}

func TestLogout_RevokesOnServer(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-labs", tokLabs, nil)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("logout"), 0)

	reqs := strings.Join(h.api.got(), "\n")
	if !strings.Contains(reqs, "DELETE /api/v1/orgs/acme-corp/tokens/tok-corp") {
		t.Fatalf("no revoke request; got:\n%s", reqs)
	}
	u := h.user()
	if _, ok := u.Orgs["acme-corp"]; ok {
		t.Fatal("acme-corp login still saved")
	}
	if u.Orgs["acme-labs"].Token != tokLabs || u.DefaultOrg != "acme-labs" {
		t.Fatalf("acme-labs login lost or not default: %q", u.DefaultOrg)
	}
	want := "Logged out of acme-corp and revoked its token (kx_01234567).\nStill logged in to acme-labs.\n"
	if h.err.String() != want {
		t.Fatalf("stderr = %q, want %q", h.err.String(), want)
	}
}

func TestLogout_Org(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-labs", tokLabs, nil)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("logout", "--org", "acme-labs"), 0)
	if !strings.Contains(strings.Join(h.api.got(), "\n"), "DELETE /api/v1/orgs/acme-labs/tokens/tok-labs") {
		t.Fatal("did not revoke acme-labs")
	}
	if _, ok := h.user().Orgs["acme-labs"]; ok {
		t.Fatal("acme-labs still saved")
	}
}

func TestLogout_DeadTokenStillLogsOut(t *testing.T) {
	h := newHarness(t)
	past := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	h.loginAs("acme-corp", tokCorp, &past)
	h.expect(h.run("logout"), 0)
	if len(h.api.got()) != 0 {
		t.Fatalf("contacted the server for an ended login: %v", h.api.got())
	}
	if len(h.user().Orgs) != 0 {
		t.Fatal("login still saved")
	}

	h2 := newHarness(t)
	h2.loginAs("acme-corp", tokCorp, &dec11)
	delete(h2.api.who, tokCorp) // the server no longer knows it
	h2.expect(h2.run("logout"), 0)
	if len(h2.user().Orgs) != 0 {
		t.Fatal("login still saved")
	}
}

func TestLogout_KeepsLoginWhenServerUnreachable(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.srv.Close()
	h.expect(h.run("logout"), 1)
	if h.user().Orgs["acme-corp"].Token != tokCorp {
		t.Fatal("deleted the login without revoking it")
	}
	if !strings.HasPrefix(h.err.String(), "kx couldn't reach Kubernetix.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

func TestLogout_NotLoggedIn(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("logout"), 3)
}

func TestWhoami_ListsOrgs(t *testing.T) {
	h := newHarness(t)
	past := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	u := &config.User{}
	u.AddLogin("acme-old", config.OrgLogin{Token: tokApp, ExpiresAt: &past, User: "sam@acme.dev"})
	u.AddLogin("acme-labs", config.OrgLogin{Token: tokLabs, User: "sam@acme.dev"})
	u.AddLogin("acme-corp", config.OrgLogin{Token: tokCorp, ExpiresAt: &dec11, User: "sam@acme.dev"})
	h.saveUser(u)
	h.expect(h.run("whoami"), 0)
	golden(t, "whoami", h.out.String())
	// It checks each login that hasn't ended with Kubernetix, once.
	if got := len(h.api.got()); got != 2 {
		t.Fatalf("whoami made %d requests, want one per login that hasn't ended", got)
	}
}

func TestWhoami_EnvToken(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokApp
	h.expect(h.run("whoami"), 0)
	want := "ORG        USER          TOKEN                 ACCESS                    ENDS\nacme-corp  sam@acme.dev  kx_00000000 (github)  deploy, read on shop-api  11 Dec 2026\n"
	if h.out.String() != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", h.out.String(), want)
	}
}

func TestWhoami_NotLoggedIn(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("whoami"), 3)
	if h.err.String() != "You're not logged in to Kubernetix.\nRun: kx login\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// Ctrl-C at the token prompt stops kx at once and saves nothing.
func TestLogin_CtrlCAtTokenPrompt(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	h.readPassword = func() (string, error) { <-never; return "", nil }
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	h.expect(h.runCtx(ctx, "login"), 1)
	if !strings.HasSuffix(h.err.String(), "Token: \nkx stopped because it was interrupted.\nRun the command again when you're ready.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if len(h.user().Orgs) != 0 {
		t.Fatal("a login was saved")
	}
}

// kx whoami --json has one shape whether the token comes from kx login or
// KX_TOKEN, so scripts can rely on its keys.
func TestWhoami_JSONSameShape(t *testing.T) {
	keys := func(t *testing.T, out string) ([]map[string]any, string) {
		t.Helper()
		var got struct {
			Orgs []map[string]any `json:"orgs"`
		}
		dec := json.NewDecoder(strings.NewReader(out))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&got); err != nil || len(got.Orgs) == 0 {
			t.Fatalf("whoami --json = %q (%v)", out, err)
		}
		var names []string
		for k := range got.Orgs[0] {
			names = append(names, k)
		}
		sort.Strings(names)
		return got.Orgs, strings.Join(names, ",")
	}
	const want = "default,ended,expires_at,org,source,status,token_prefix,user"

	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("whoami", "--json"), 0)
	orgs, got := keys(t, h.out.String())
	if got != want || orgs[0]["source"] != "login" || orgs[0]["org"] != "acme-corp" || orgs[0]["token_prefix"] != "kx_01234567" {
		t.Fatalf("login: keys %s, orgs %v", got, orgs)
	}

	h2 := newHarness(t)
	h2.env["KX_TOKEN"] = tokApp
	h2.expect(h2.run("whoami", "--json"), 0)
	orgs, got = keys(t, h2.out.String())
	if got != want || orgs[0]["source"] != "env" || orgs[0]["org"] != "acme-corp" || orgs[0]["token_prefix"] != "kx_00000000" ||
		orgs[0]["user"] != "sam@acme.dev" || orgs[0]["expires_at"] != "2026-12-11T09:00:00Z" || orgs[0]["ended"] != false {
		t.Fatalf("KX_TOKEN: keys %s, orgs %v", got, orgs)
	}
}

// A token the platform refuses because of its creator (awaiting approval,
// not approved) can't be revoked with itself. kx logout still forgets it
// and says where to revoke it.
func TestLogout_RefusedTokenStillLogsOut(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.override = func(r *http.Request, tok string) (int, string, bool) {
		return 403, `{"error":"token_owner_pending","message":"The account that created this API token is awaiting approval.","token_name":"laptop","token_prefix":"kx_01234567"}`, tok == tokCorp
	}
	h.expect(h.run("logout"), 0)
	if len(h.user().Orgs) != 0 {
		t.Fatal("login still saved")
	}
	want := "Logged out of acme-corp, but kx couldn't revoke its token (kx_01234567) because Kubernetix refused it; revoke it in Settings → API tokens.\n"
	if h.err.String() != want {
		t.Fatalf("stderr = %q, want %q", h.err.String(), want)
	}
}

// Logging in to the same org again revokes the token this computer used
// before (once the new one works), so it isn't left active and forgotten.
func TestLogin_AgainRevokesPreviousToken(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.tokens = []apiToken{*orgToken("tok-corp", "kx_01234567", "acme-corp", &dec11), *orgToken("tok-corp-2", "kx_33333333", "acme-corp", &dec11)}
	h.tty, h.password = true, tokCorp2
	h.expect(h.run("login"), 0)
	if h.user().Orgs["acme-corp"].Token != tokCorp2 {
		t.Fatal("new login not saved")
	}
	if reqs := strings.Join(h.api.got(), "\n"); !strings.Contains(reqs, "DELETE /api/v1/orgs/acme-corp/tokens/tok-corp\n") && !strings.HasSuffix(reqs, "DELETE /api/v1/orgs/acme-corp/tokens/tok-corp") {
		t.Fatalf("previous token not revoked:\n%s", reqs)
	}
	if !strings.Contains(h.err.String(), "\nRevoked the token this computer used before for acme-corp (kx_01234567).\nLogged in to acme-corp") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// Revoking a token revokes any tokens that were made with it, so kx keeps
// the previous token if anything depends on it, and says so.
func TestLogin_AgainKeepsPreviousTokenWithChildren(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	child := *orgToken("tok-ci", "kx_cicicici", "acme-corp", &dec11)
	child.ParentTokenID = "tok-corp"
	dead := child
	dead.ID, dead.Status = "tok-old-ci", "revoked"
	h.api.tokens = []apiToken{*orgToken("tok-corp", "kx_01234567", "acme-corp", &dec11), child, dead}
	h.tty, h.password = true, tokCorp2
	h.expect(h.run("login"), 0)
	if strings.Contains(strings.Join(h.api.got(), "\n"), "DELETE") {
		t.Fatalf("revoked a token with children: %v", h.api.got())
	}
	if !strings.Contains(h.err.String(), "\nThe token this computer used before for acme-corp (kx_01234567) still works, because 1 token made with it depends on it; to revoke them all: kx token revoke kx_01234567\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// Revoking the previous token is best effort: if it fails, the new login
// stands and kx says how to revoke the old one.
func TestLogin_AgainWhenPreviousTokenCantBeRevoked(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.override = func(r *http.Request, tok string) (int, string, bool) {
		return 500, `{"error":"database_error","message":"API tokens are unavailable right now. Please retry."}`, tok == tokCorp
	}
	h.tty, h.password = true, tokCorp2
	h.expect(h.run("login"), 0)
	if h.user().Orgs["acme-corp"].Token != tokCorp2 {
		t.Fatal("new login not saved")
	}
	if !strings.Contains(h.err.String(), "\n! kx couldn't revoke the token this computer used before for acme-corp (kx_01234567); revoke it with: kx token revoke kx_01234567\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// logoutWithChildren is a login whose token made a CI token (still active)
// and an old one (revoked), next to an unrelated token.
func logoutWithChildren(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	ci := *orgToken("tok-ci", "kx_cicicici", "acme-corp", &dec11)
	ci.Name, ci.ParentTokenID, ci.Scope, ci.AppID, ci.AppName = "ci", "tok-corp", "app", appShopID, "shop-api"
	old := ci
	old.ID, old.Prefix, old.Status = "tok-old-ci", "kx_oldoldol", "revoked"
	other := *orgToken("tok-other", "kx_otherotr", "acme-corp", &dec11)
	h.api.tokens = []apiToken{*orgToken("tok-corp", "kx_01234567", "acme-corp", &dec11), ci, old, other}
	return h
}

func revokedOnServer(h *harness) bool {
	return strings.Contains(strings.Join(h.api.got(), "\n"), "DELETE /api/v1/orgs/acme-corp/tokens/tok-corp")
}

// kx logout never silently revokes the tokens made with that login:
// revoking a token revokes those too. On a terminal it lists them and asks.
func TestLogout_ChildrenAskOnTerminal(t *testing.T) {
	const listed = "This token was made with this login and stops working if it's revoked:\n  kx_cicicici  ci  shop-api\nAlso revoke this token? [y/N] "

	t.Run("yes", func(t *testing.T) {
		h := logoutWithChildren(t)
		h.tty, h.stdin = true, "y\n"
		h.expect(h.run("logout"), 0)
		if !revokedOnServer(h) || len(h.user().Orgs) != 0 {
			t.Fatalf("revoked=%v, orgs=%v", revokedOnServer(h), h.user().Orgs)
		}
		if want := listed + "Logged out of acme-corp and revoked its token (kx_01234567) and the 1 token made with it.\n"; h.err.String() != want {
			t.Fatalf("stderr =\n%q\nwant\n%q", h.err.String(), want)
		}
	})
	t.Run("no keeps the token on the server", func(t *testing.T) {
		h := logoutWithChildren(t)
		h.tty, h.stdin = true, "\n"
		h.expect(h.run("logout"), 0)
		if revokedOnServer(h) || len(h.user().Orgs) != 0 {
			t.Fatalf("revoked=%v, orgs=%v", revokedOnServer(h), h.user().Orgs)
		}
		if want := listed + "Logged out of acme-corp on this computer only; its token (kx_01234567) still works on Kubernetix, so the 1 token made with it keeps working.\n"; h.err.String() != want {
			t.Fatalf("stderr =\n%q\nwant\n%q", h.err.String(), want)
		}
	})
}

// Without a terminal, kx logout needs --yes to revoke the tokens made with
// the login; otherwise it logs out on this computer only and explains.
func TestLogout_ChildrenWithoutTerminal(t *testing.T) {
	t.Run("without --yes", func(t *testing.T) {
		h := logoutWithChildren(t)
		h.expect(h.run("logout"), 0)
		if revokedOnServer(h) || len(h.user().Orgs) != 0 {
			t.Fatalf("revoked=%v, orgs=%v", revokedOnServer(h), h.user().Orgs)
		}
		want := "Logged out of acme-corp on this computer only; its token (kx_01234567) still works on Kubernetix, because revoking it would also revoke the 1 token made with it.\n" +
			"To revoke them too, revoke kx_01234567 in the dashboard (Settings → API tokens), or use kx logout --yes next time.\n"
		if h.err.String() != want {
			t.Fatalf("stderr =\n%q\nwant\n%q", h.err.String(), want)
		}
	})
	t.Run("with --yes", func(t *testing.T) {
		h := logoutWithChildren(t)
		h.expect(h.run("logout", "--yes"), 0)
		if !revokedOnServer(h) || len(h.user().Orgs) != 0 {
			t.Fatalf("revoked=%v, orgs=%v", revokedOnServer(h), h.user().Orgs)
		}
		if want := "Logged out of acme-corp and revoked its token (kx_01234567) and the 1 token made with it.\n"; h.err.String() != want {
			t.Fatalf("stderr = %q", h.err.String())
		}
	})
	t.Run("--json reports what happened", func(t *testing.T) {
		h := logoutWithChildren(t)
		h.expect(h.run("logout", "--json"), 0)
		want := `{
  "logged_out": "acme-corp",
  "revoked": false,
  "kept_on_server": true,
  "needs_revoking": false,
  "tokens_made_with_it": [
    {
      "token_prefix": "kx_cicicici",
      "name": "ci",
      "scope": "app",
      "app": "shop-api"
    }
  ],
  "tokens_made_with_it_unknown": false
}
`
		if h.out.String() != want {
			t.Fatalf("stdout =\n%s\nwant\n%s", h.out.String(), want)
		}
		h2 := logoutWithChildren(t)
		h2.expect(h2.run("logout", "--json", "--yes"), 0)
		if !strings.Contains(h2.out.String(), `"revoked": true,`) || !strings.Contains(h2.out.String(), `"kept_on_server": false,`) {
			t.Fatalf("stdout = %s", h2.out.String())
		}
	})
	t.Run("token that can't list tokens", func(t *testing.T) {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.api.override = func(r *http.Request, tok string) (int, string, bool) {
			return 403, `{"error":"token_not_allowed","message":"Listing tokens needs a token with the read permission.","token_name":"laptop","token_prefix":"kx_01234567"}`,
				r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens")
		}
		h.expect(h.run("logout"), 0)
		if revokedOnServer(h) || len(h.user().Orgs) != 0 {
			t.Fatalf("revoked=%v, orgs=%v", revokedOnServer(h), h.user().Orgs)
		}
		want := "Logged out of acme-corp on this computer only; its token (kx_01234567) still works on Kubernetix, because kx couldn't check for tokens made with it, which revoking it would also revoke.\n" +
			"To revoke it, use the dashboard (Settings → API tokens), or kx logout --yes next time.\n"
		if h.err.String() != want {
			t.Fatalf("stderr =\n%q\nwant\n%q", h.err.String(), want)
		}
		h2 := newHarness(t)
		h2.loginAs("acme-corp", tokCorp, &dec11)
		h2.api.override = h.api.override
		h2.expect(h2.run("logout", "--yes"), 0)
		if !revokedOnServer(h2) {
			t.Fatal("--yes did not revoke")
		}
	})
}

// setLocal makes ui show local times in loc and returns the undo.
func setLocal(loc *time.Location) func() {
	old := ui.Local
	ui.Local = loc
	return func() { ui.Local = old }
}

func TestWhoami_LoginEndingSoonShowsTheTime(t *testing.T) {
	h := newHarness(t)
	soon := nov12.Add(30 * time.Hour)
	h.loginAs("acme-corp", tokCorp, &soon)
	defer setLocal(time.UTC)()
	h.expect(h.run("whoami"), 0)
	if !strings.Contains(h.out.String()+h.err.String(), "13 Nov 15:00") {
		t.Fatalf("whoami:\n%s%s", h.out.String(), h.err.String())
	}
}
