package cmd

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	autoLoginIntro = "First, let's connect this computer to Kubernetix. Opening your browser…\n"
	wantHint       = "If the page says \"Awaiting approval\", your account isn't approved yet; we'll email you, then run kx deploy again. Press Ctrl-C to stop.\n"
	shopURL        = "https://" + appShopID + "--acme-corp.apps.example.test"
)

// whoamiFails makes GET /whoami with tok answer status and body, n times
// (every time when n is negative).
func whoamiFails(h *harness, tok string, n, status int, body string) {
	h.api.override = func(r *http.Request, bearer string) (int, string, bool) {
		if r.URL.Path != "/api/v1/whoami" || bearer != tok || n == 0 {
			return 0, "", false
		}
		n--
		return status, body, true
	}
}

func TestDeploy_AutoLogin_WhenLoggedOut(t *testing.T) {
	h := newHarness(t)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password = true, tokCorp
	h.expect(h.run("deploy", "--app", "shop-api"), 0)

	stderr := h.norm(h.err.String())
	wantLogin := autoLoginIntro +
		"Opening platform.kubernetix.ai so you can create a token for this computer.\n" +
		"If your browser didn't open, go to https://platform.kubernetix.ai/settings?tab=tokens&new=cli\n" +
		"Click \"Create token\", then \"Copy\", and paste it here.\n" +
		wantHint +
		"Token: ••••••••••••••••\n" +
		"Logged in to acme-corp as sam@acme.dev. This token can deploy every app in acme-corp until 11 Dec 2026.\n"
	if !strings.HasPrefix(stderr, wantLogin) {
		t.Fatalf("stderr:\n%s\nwant it to start with:\n%s", stderr, wantLogin)
	}
	if strings.Contains(stderr, "Next: open your project folder") {
		t.Fatalf("the deploy carries on, so there is no next step to print:\n%s", stderr)
	}
	if len(h.opened) != 1 || h.norm(h.opened[0]) != "https://platform.kubernetix.ai/settings?tab=tokens&new=cli" {
		t.Fatalf("opened %v", h.opened)
	}
	if h.out.String() != shopURL+"\n" {
		t.Fatalf("stdout = %q, want the URL alone", h.out.String())
	}
	if l, ok := h.user().Orgs["acme-corp"]; !ok || l.Token != tokCorp {
		t.Fatalf("saved logins: %v", h.user().OrgNames())
	}
	if len(h.api.deployPosts) != 1 {
		t.Fatalf("deploys: %v", h.api.deployPosts)
	}
}

func TestDeploy_AutoLogin_LoginEnded(t *testing.T) {
	h := newHarness(t)
	ended := nov12.Add(-time.Hour)
	h.loginAs("acme-corp", tokCorp, &ended)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password = true, tokCorp2
	h.expect(h.run("deploy"), 0)
	if len(h.opened) != 1 || !strings.HasSuffix(h.opened[0], "/settings?tab=tokens&new=cli&org=acme-corp") {
		t.Fatalf("opened %v", h.opened)
	}
	if !strings.Contains(h.err.String(), autoLoginIntro) {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if h.user().Orgs["acme-corp"].Token != tokCorp2 || h.out.String() != shopURL+"\n" {
		t.Fatalf("saved %q, stdout %q", h.user().Orgs["acme-corp"].Token, h.out.String())
	}
}

// A folder linked to an organization this computer isn't logged in to
// asks for a token for that organization.
func TestDeploy_AutoLogin_OrgNotLoggedIn(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", "", "acme-labs")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password = true, tokLabs
	h.expect(h.run("deploy"), 0)
	if len(h.opened) != 1 || !strings.HasSuffix(h.opened[0], "&org=acme-labs") {
		t.Fatalf("opened %v", h.opened)
	}
	if u := h.user(); u.Orgs["acme-labs"].Token != tokLabs || u.Orgs["acme-corp"].Token != tokCorp {
		t.Fatalf("saved logins: %v", u.OrgNames())
	}
}

func TestDeploy_AutoLogin_ServerSaysExpired(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	whoamiFails(h, tokCorp, 1, 401, `{"error":"token_expired","message":"This API token has expired."}`)
	h.tty, h.password = true, tokCorp2
	h.expect(h.run("deploy"), 0)
	if len(h.opened) != 1 || !strings.HasSuffix(h.opened[0], "&org=acme-corp") {
		t.Fatalf("opened %v", h.opened)
	}
	if h.user().Orgs["acme-corp"].Token != tokCorp2 || len(h.api.deployPosts) != 1 || h.out.String() != shopURL+"\n" {
		t.Fatalf("saved %q, deploys %d, stdout %q", h.user().Orgs["acme-corp"].Token, len(h.api.deployPosts), h.out.String())
	}
	if !strings.Contains(h.err.String(), autoLoginIntro) {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

// Kubernetix refuses the new token too: kx opens the browser only once.
func TestDeploy_AutoLogin_ServerSaysExpiredTwice(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.api.override = func(r *http.Request, _ string) (int, string, bool) {
		if r.URL.Path == "/api/v1/whoami" {
			return 401, `{"error":"token_expired","message":"This API token has expired."}`, true
		}
		return 0, "", false
	}
	h.tty, h.password = true, tokCorp2
	h.expect(h.run("deploy"), 3)
	if len(h.opened) != 1 || len(h.api.deployPosts) != 0 {
		t.Fatalf("opened %v, deploys %d", h.opened, len(h.api.deployPosts))
	}
}

// Revoked and unknown tokens log in again too; the token stays saved
// until the new login replaces it.
func TestDeploy_AutoLogin_ServerSaysRevokedOrUnknown(t *testing.T) {
	for _, body := range []string{
		`{"error":"token_revoked","message":"This API token has been revoked."}`,
		`{"error":"invalid_token","message":"This API token is not valid."}`,
	} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.link("shop-api", appShopID, "acme-corp")
		h.dockerfile("FROM scratch\nEXPOSE 8080\n")
		whoamiFails(h, tokCorp, -1, 401, body)
		h.tty, h.password = true, tokCorp2
		h.expect(h.run("deploy"), 0)
		if len(h.opened) != 1 || h.user().Orgs["acme-corp"].Token != tokCorp2 {
			t.Fatalf("%s: opened %v, saved %q", body, h.opened, h.user().Orgs["acme-corp"].Token)
		}
	}
}

func TestDeploy_AutoLogin_NeverInCI(t *testing.T) {
	h := newHarness(t)
	h.env["GITHUB_ACTIONS"], h.env["GITHUB_EVENT_NAME"] = "true", "push"
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password = true, tokCorp
	h.expect(h.run("deploy", "--app", "shop-api"), 3)
	if len(h.opened) != 0 || !strings.Contains(h.err.String(), "You're not logged in to Kubernetix.\nRun: kx login\n") {
		t.Fatalf("opened %v, stderr:\n%s", h.opened, h.err.String())
	}
	for _, v := range []string{"CI", "GITLAB_CI", "BITBUCKET_BUILD_NUMBER"} {
		h := newHarness(t)
		h.env[v] = "true"
		h.dockerfile("FROM scratch\nEXPOSE 8080\n")
		h.tty, h.password = true, tokCorp
		h.expect(h.run("deploy", "--app", "shop-api"), 3)
		if len(h.opened) != 0 {
			t.Fatalf("%s: opened %v", v, h.opened)
		}
	}
}

func TestDeploy_AutoLogin_NoTerminal(t *testing.T) {
	h := newHarness(t)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.password = tokCorp
	h.expect(h.run("deploy", "--app", "shop-api"), 3)
	if len(h.opened) != 0 || !strings.Contains(h.err.String(), "You're not logged in to Kubernetix.\nRun: kx login\n") {
		t.Fatalf("opened %v, stderr:\n%s", h.opened, h.err.String())
	}

	// A terminal to type in, but stderr goes to a file: nobody sees the
	// prompt.
	h.tty, h.stderrFile = true, true
	h.expect(h.run("deploy", "--app", "shop-api"), 3)
	if len(h.opened) != 0 {
		t.Fatalf("opened %v", h.opened)
	}

	// The prompt shows, but stdin comes from a file: nobody can paste.
	h.stderrFile, h.stdinFile = false, true
	h.expect(h.run("deploy", "--app", "shop-api"), 3)
	if len(h.opened) != 0 || !strings.Contains(h.err.String(), "You're not logged in to Kubernetix.\nRun: kx login\n") {
		t.Fatalf("opened %v, stderr:\n%s", h.opened, h.err.String())
	}
}

// A run logs in by itself at most once. Here the new token passes the
// login's own check, then Kubernetix refuses it on the next request (as a
// token store that hasn't caught up with a brand-new token can): kx stops
// instead of opening the browser again.
func TestDeploy_AutoLogin_AtMostOncePerRun(t *testing.T) {
	h := newHarness(t)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	n := 0
	h.api.override = func(r *http.Request, bearer string) (int, string, bool) {
		if r.URL.Path != "/api/v1/whoami" || bearer != tokCorp {
			return 0, "", false
		}
		if n++; n == 1 {
			return 0, "", false // the login's own check passes
		}
		return 401, `{"error":"invalid_token","message":"This API token is not valid."}`, true
	}
	h.tty, h.password = true, tokCorp
	h.expect(h.run("deploy", "--app", "shop-api"), 3)
	if len(h.opened) != 1 || strings.Count(h.err.String(), "Token: ") != 1 {
		t.Fatalf("opened %d times: %v\nstderr:\n%s", len(h.opened), h.opened, h.err.String())
	}
}

// A KX_TOKEN that stopped working fails with its own message, even on a
// terminal: switching to another token unasked would hide the problem.
func TestDeploy_AutoLogin_NeverWithKXToken(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokCorp2
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	whoamiFails(h, tokCorp2, -1, 401, `{"error":"token_revoked","message":"This API token has been revoked.","token_prefix":"kx_33333333","token_name":"ci"}`)
	h.tty, h.password = true, tokCorp
	h.expect(h.run("deploy"), 3)
	want := "The KX_TOKEN secret holds a token (kx_33333333, \"ci\") that was revoked.\nCreate a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret.\n"
	if len(h.opened) != 0 || !strings.HasSuffix(h.err.String(), want) {
		t.Fatalf("opened %v, stderr:\n%s", h.opened, h.err.String())
	}
}

func TestDeploy_AutoLogin_NeverWithJSON(t *testing.T) {
	h := newHarness(t)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password = true, tokCorp
	h.expect(h.run("deploy", "--app", "shop-api", "--json"), 3)
	want := `{"error":{"code":"not_logged_in","message":"You're not logged in to Kubernetix.","fix":"Run: kx login"}}` + "\n"
	if len(h.opened) != 0 || h.out.String() != want {
		t.Fatalf("opened %v, stdout %q", h.opened, h.out.String())
	}
}

// When kx can't open a browser (SSH, no display), it prints the address,
// asks for the token and carries on.
func TestDeploy_AutoLogin_NoBrowser_PrintsURLAndAsks(t *testing.T) {
	h := newHarness(t)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password, h.canOpen = true, tokCorp, false
	h.expect(h.run("deploy", "--app", "shop-api"), 0)
	want := "First, let's connect this computer to Kubernetix.\n" +
		"Open this page so you can create a token for this computer:\n" +
		"https://platform.kubernetix.ai/settings?tab=tokens&new=cli\n" +
		"Click \"Create token\", then \"Copy\", and paste it here.\n" +
		wantHint +
		"Token: ••••••••••••••••\n"
	if !strings.HasPrefix(h.norm(h.err.String()), want) {
		t.Fatalf("stderr:\n%s\nwant it to start with:\n%s", h.norm(h.err.String()), want)
	}
	if len(h.opened) != 0 || h.out.String() != shopURL+"\n" {
		t.Fatalf("opened %v, stdout %q", h.opened, h.out.String())
	}
}

// A 401 that gives no reason may come from Kubernetix being updated: kx
// doesn't replace a token that may still be good.
func TestDeploy_AutoLogin_NotOnBare401(t *testing.T) {
	for _, body := range []string{"", `{"error":"invalid_token","message":"Invalid or expired token"}`} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.link("shop-api", appShopID, "acme-corp")
		h.dockerfile("FROM scratch\nEXPOSE 8080\n")
		whoamiFails(h, tokCorp, -1, 401, body)
		h.tty, h.password = true, tokCorp2
		h.expect(h.run("deploy"), 3)
		want := "Kubernetix didn't accept the token this computer uses for acme-corp.\nIf it worked before, Kubernetix may be being updated: try again in a few minutes.\n"
		if len(h.opened) != 0 || !strings.HasSuffix(h.err.String(), want) {
			t.Fatalf("body %q: opened %v, stderr:\n%s", body, h.opened, h.err.String())
		}
		if h.user().Orgs["acme-corp"].Token != tokCorp {
			t.Fatalf("body %q: the saved token was replaced", body)
		}
	}
}

// Other commands that need a login log in the same way.
func TestStatus_AutoLogin_WhenLoggedOut(t *testing.T) {
	h := newHarness(t)
	h.link("shop-api", appShopID, "acme-corp")
	h.tty, h.password = true, tokCorp
	h.expect(h.run("status"), 0)
	if len(h.opened) != 1 || !strings.Contains(h.out.String(), "shop-api") {
		t.Fatalf("opened %v, stdout:\n%s", h.opened, h.out.String())
	}
}

// The commands that manage logins and tokens never log in by themselves.
func TestAutoLogin_NotForAccountCommands(t *testing.T) {
	for _, args := range [][]string{{"whoami"}, {"logout"}, {"token", "list"}, {"version"}, {"login", "--token-stdin"}} {
		h := newHarness(t)
		h.tty, h.password = true, tokCorp
		h.run(args...)
		if len(h.opened) != 0 || strings.Contains(h.err.String(), "First, let's connect") {
			t.Fatalf("kx %v: opened %v, stderr:\n%s", args, h.opened, h.err.String())
		}
	}

	// An ended login: kx token revokes nothing and opens nothing.
	h := newHarness(t)
	ended := nov12.Add(-time.Hour)
	h.loginAs("acme-corp", tokCorp, &ended)
	h.tty, h.password = true, tokCorp2
	h.expect(h.run("token", "list"), 3)
	if len(h.opened) != 0 || !strings.Contains(h.err.String(), "Your login to acme-corp has ended.") {
		t.Fatalf("opened %v, stderr:\n%s", h.opened, h.err.String())
	}
}

func TestLogin_PendingApprovalHint(t *testing.T) {
	h := newHarness(t)
	h.tty, h.password = true, tokCorp
	h.expect(h.run("login"), 0)
	if !strings.Contains(h.err.String(), "paste it here.\n"+wantHint+"Token: ") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

// A command logs in for the organization it was for; a token for another
// one is saved (as kx login does) but the command doesn't carry on there,
// even when its organization was only the last one logged in to.
func TestDeploy_AutoLogin_TokenForAnotherOrg_Stops(t *testing.T) {
	h := newHarness(t)
	ended := nov12.Add(-time.Hour)
	h.loginAs("acme-corp", tokCorp, &ended) // the default org, no folder link
	h.inFolder("my-shop")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password, h.stdin = true, tokLabs, "\n"
	h.expect(h.run("deploy"), 3)
	want := "! This token is for acme-labs, not acme-corp, so kx saved it for acme-labs.\n" +
		"Logged in to acme-labs as sam@acme.dev. This token can deploy every app in acme-labs and never expires.\n\n" +
		"The token you pasted is for acme-labs, but kx was going to use acme-corp, where this computer was last logged in.\n" +
		"Run kx deploy again to use acme-labs, or log in to acme-corp: kx login --org acme-corp\n"
	if !strings.HasSuffix(h.err.String(), want) {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if strings.Contains(h.err.String(), "Which app") || len(h.api.newApps) != 0 || len(h.api.deployPosts) != 0 {
		t.Fatalf("carried on in acme-labs:\n%s", h.err.String())
	}
	if h.user().Orgs["acme-labs"].Token != tokLabs {
		t.Fatal("the pasted token wasn't saved")
	}

	// The same from another command names that command.
	h = newHarness(t)
	h.loginAs("acme-corp", tokCorp, &ended)
	h.tty, h.password = true, tokLabs
	h.expect(h.run("apps", "list"), 3)
	if !strings.HasSuffix(h.err.String(), "Run kx apps list again to use acme-labs, or log in to acme-corp: kx login --org acme-corp\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	// A folder linked to acme-labs, and a token for acme-corp.
	h = newHarness(t)
	h.link("shop-api", "", "acme-labs")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.password = true, tokCorp
	h.expect(h.run("deploy"), 3)
	if !strings.HasSuffix(h.err.String(), "The token you pasted is for acme-corp, but this command is for acme-labs.\nRun: kx login --org acme-labs\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}
