package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/config"
)

// gitOrigin writes a git config with an origin remote into the
// repository at dir.
func gitOrigin(t *testing.T, dir, url string) {
	t.Helper()
	cfg := "[core]\n\trepositoryformatversion = 0\n[remote \"upstream\"]\n\turl = https://github.com/someone/else.git\n[remote \"origin\"]\n\turl = " + url + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// inSubfolder moves the harness into a new folder name inside the current
// one (not a repository of its own).
func (h *harness) inSubfolder(name string) {
	h.t.Helper()
	h.dir = filepath.Join(h.dir, name)
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func TestDeploy_NoApps_CreatesFolderNamedAppWithoutAsking(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.inFolder("my-shop")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, ""
	h.expect(h.run("deploy"), 0)
	if strings.Contains(h.err.String(), "Which app") {
		t.Fatalf("asked although there is nothing to choose:\n%s", h.err.String())
	}
	if !strings.HasPrefix(h.err.String(), "Creating a new app called my-shop. Saved in .kx.json.\n\n✓ Checked ") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != "my-shop" {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}
	_, p, _ := config.FindProject(h.dir)
	if p == nil || p.App != "my-shop" || p.Org != "acme-corp" || p.AppID == "" {
		t.Fatalf(".kx.json = %+v", p)
	}
}

// A folder called src (or app, Desktop…) says nothing about the app, so
// kx asks, suggesting the git repository's name.
func TestDeploy_NoApps_GenericFolderName_AsksForName(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	gitOrigin(t, h.dir, "https://github.com/acme/shop-api.git")
	h.inSubfolder("src")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 0)
	if !strings.HasPrefix(h.err.String(), "What should the app be called? [shop-api] Saved in .kx.json. Next time, just run kx deploy.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != "shop-api" {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}

	// A name that isn't an app name asks again.
	h = newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	gitOrigin(t, h.dir, "git@github.com:acme/shop-api.git")
	h.inSubfolder("src")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "Shop API\nshop-web\n"
	h.expect(h.run("deploy"), 0)
	want := "What should the app be called? [shop-api] App names use lowercase letters, numbers and hyphens, like shop-api. Try again.\n" +
		"What should the app be called? [shop-api] "
	if !strings.HasPrefix(h.err.String(), want) {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != "shop-web" {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}
}

func TestDeploy_NoApps_GenericFolderNames(t *testing.T) {
	for _, folder := range []string{"app", "Code", "Desktop", "New folder", "untitled", "WWW", "tmp",
		"html", "public", "Users", "source", "git", "GitHub", "go"} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.api.apps = nil
		h.inFolder(folder)
		h.dockerfile("FROM scratch\nEXPOSE 8080\n")
		h.tty, h.stdin = true, "shop-api\n"
		h.expect(h.run("deploy"), 0)
		def := config.AppNameFrom(folder) // no git remote: the folder's name
		if !strings.HasPrefix(h.err.String(), "What should the app be called? ["+def+"] ") {
			t.Fatalf("%s: stderr:\n%s", folder, h.err.String())
		}
		if h.api.newApps[0]["name"] != "shop-api" {
			t.Fatalf("%s: POST /apps = %v", folder, h.api.newApps)
		}
	}
}

func TestDeploy_NoApps_HomeFolder_AsksForName(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.env["HOME"], h.env["USERPROFILE"] = h.dir, h.dir // the folder is shop-api
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 0)
	if !strings.HasPrefix(h.err.String(), "What should the app be called? [shop-api] ") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_NoApps_NoTerminal_CreatesWithoutAsking(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.inFolder("my-shop")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.expect(h.run("deploy"), 0)
	if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != "my-shop" {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}
	if !strings.Contains(h.err.String(), "Creating a new app called my-shop. Saved in .kx.json.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	// A generic name needs a question, which kx can't ask here.
	h = newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.inFolder("src")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.expect(h.run("deploy"), 2)
	if !strings.HasSuffix(h.err.String(), "kx needs an app name for this folder.\nRun: kx deploy --app <name>\n") || len(h.api.newApps) != 0 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if _, p, _ := config.FindProject(h.dir); p != nil {
		t.Fatalf("saved .kx.json %+v", p)
	}
}

// In CI kx never names an app after a folder: the run would create a new
// app on every fresh checkout that lacks .kx.json.
func TestDeploy_NoApps_InCI_StillNeedsApp(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"], h.env["CI"] = tokCorp, "true"
	h.api.apps = nil
	h.inFolder("my-shop")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.expect(h.run("deploy"), 2)
	if !strings.Contains(h.err.String(), "This folder isn't linked to an app yet, and kx can't ask which one here.") || len(h.api.newApps) != 0 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_OtherAppsExist_StillAsks(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = h.api.apps[1:] // web only
	h.inFolder("my-shop")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 0)
	if !strings.HasPrefix(h.err.String(), "Which app should this folder deploy to?\n  1) (new app: my-shop)\n  2) web\nChoose [1]: Saved in .kx.json. Next time, just run kx deploy.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	// Without a terminal, the choice can't be made.
	h.inFolder("my-other-shop")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = false, ""
	h.expect(h.run("deploy"), 2)
	if !strings.Contains(h.err.String(), "This folder isn't linked to an app yet, and kx can't ask which one here.\nRun: kx deploy --app <app>\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestLink_NoApps_LinksWithoutAsking(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.inFolder("my-shop")
	h.expect(h.run("link"), 0)
	if strings.Contains(h.err.String(), "Which app") || !strings.Contains(h.err.String(), "my-shop doesn't exist yet; kx deploy will create it.\nLinked this folder to my-shop in acme-corp (saved in .kx.json).\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	h.inFolder("src")
	h.expect(h.run("link"), 2)
	if !strings.HasSuffix(h.err.String(), "kx needs an app name for this folder.\nRun: kx link <name>\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestGitOriginName(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/acme/shop-api.git":   "shop-api",
		"https://github.com/acme/shop-api":       "shop-api",
		"https://github.com/acme/shop-api/":      "shop-api",
		"git@github.com:acme/Shop_API.git":       "shop-api",
		"ssh://git@gitlab.com:2222/a/b/web.git":  "web",
		"\"https://github.com/acme/quoted.git\"": "quoted",
		"/srv/git/billing.git":                   "billing",
		"___":                                    "",
	} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitOrigin(t, dir, url)
		sub := filepath.Join(dir, "a", "b")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := gitOriginName(sub); got != want {
			t.Errorf("origin %q: %q, want %q", url, got, want)
		}
	}

	// A worktree's .git is a file pointing at its folder in the main
	// repository, whose config holds the remotes.
	main := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, ".git", "worktrees", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOrigin(t, main, "https://github.com/acme/from-worktree.git")
	if err := os.WriteFile(filepath.Join(main, ".git", "worktrees", "wt", "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitOriginName(wt); got != "from-worktree" {
		t.Errorf("worktree: %q", got)
	}

	// No repository, or no origin.
	if got := gitOriginName(t.TempDir()); got != "" {
		t.Errorf("no repository: %q", got)
	}
}

func TestStatus_NotLinked_OneApp_UsesIt(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = h.api.apps[:1] // shop-api only
	h.expect(h.run("status"), 0)
	if h.err.String() != "Using shop-api, the only app in acme-corp.\n" || !strings.HasPrefix(h.out.String(), "App       shop-api\n") {
		t.Fatalf("stderr:\n%s\nstdout:\n%s", h.err.String(), h.out.String())
	}
}

func TestOpen_NotLinked_NoApps(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.expect(h.run("open"), 2)
	if h.err.String() != "There are no apps in acme-corp yet.\nDeploy this folder: kx deploy\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

func TestLogs_NotLinked_SeveralApps_ListsThem(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	for i, name := range []string{"web", "shop-api", "zeta", "docs", "api", "yak", "admin"} {
		h.api.apps = append(h.api.apps, api.App{ID: fmt.Sprintf("app-%d", i), Name: name})
	}
	h.expect(h.run("logs"), 2)
	want := "This folder isn't linked to an app, so kx doesn't know which one you mean.\nRun: kx logs --app <name>. Your apps: admin, api, docs, shop-api, web and 2 more\n"
	if h.err.String() != want {
		t.Fatalf("stderr = %q\nwant     %q", h.err.String(), want)
	}

	h.api.apps = h.api.apps[:2]
	h.expect(h.run("status"), 2)
	if !strings.HasSuffix(h.err.String(), "Run: kx status --app <name>. Your apps: shop-api, web\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// kx rollback changes what's live, so it never guesses the app.
func TestRollback_NotLinked_StillNeedsApp(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = h.api.apps[:1]
	h.expect(h.run("rollback", "--yes"), 2)
	if h.err.String() != "This folder isn't linked to an app yet.\nRun: kx link\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
}
