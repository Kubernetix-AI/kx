package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetix-ai/kx/internal/scaffold"
)

// put writes files (path → content) into the harness folder.
func (h *harness) put(files map[string]string) {
	h.t.Helper()
	for name, body := range files {
		p := filepath.Join(h.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

// nodeApp puts a small Node app, with no Dockerfile, in the harness folder.
func (h *harness) nodeApp() {
	h.put(map[string]string{
		"package.json":      `{"name":"shop-api","scripts":{"start":"node server.js"}}`,
		"package-lock.json": `{"lockfileVersion":3}`,
		"server.js":         "require('http').createServer((q, s) => s.end('hi')).listen(process.env.PORT)\n",
	})
}

func (h *harness) read(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(h.dir, name))
	return string(b), err == nil
}

// noDockerfileWhat is A1.1's first line for the harness folder.
func (h *harness) noDockerfileWhat() string {
	return "kx needs a Dockerfile to build your app, and there isn't one in " + h.dir + "."
}

// The offer, on a terminal: Enter writes the Dockerfile and a
// .dockerignore, shows the Dockerfile, and the deploy carries on with it.
func TestDeploy_NoDockerfile_OffersAndContinues(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.nodeApp()
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 0)

	// The whole transcript, with the temporary folder named as in the README.
	out := strings.ReplaceAll(h.err.String()+h.out.String(), filepath.Join(h.dir, "Dockerfile"), "/Users/sam/code/shop-api/Dockerfile")
	out = strings.ReplaceAll(out, h.dir, "/Users/sam/code/shop-api")
	golden(t, "deploy_writes_dockerfile", h.norm(out))

	if want := "https://" + appShopID + "--acme-corp.apps.example.test\n"; h.out.String() != want {
		t.Fatalf("stdout = %q, want the URL alone", h.out.String())
	}
	if len(h.docker.builds) != 1 || h.docker.builds[0].Dockerfile != filepath.Join(h.dir, "Dockerfile") || h.docker.builds[0].Dir != h.dir {
		t.Fatalf("builds = %+v, want one of %s", h.docker.builds, filepath.Join(h.dir, "Dockerfile"))
	}
	found, err := scaffold.Detect(h.dir)
	if err != nil || len(found) != 1 {
		t.Fatalf("Detect() = %v, %v", found, err)
	}
	want, ignore, _ := scaffold.Render(found[0])
	if got, _ := h.read("Dockerfile"); got != string(want) {
		t.Fatalf("Dockerfile:\n%s\nwant:\n%s", got, want)
	}
	if got, _ := h.read(".dockerignore"); got != string(ignore) {
		t.Fatalf(".dockerignore:\n%s", got)
	}
	// The printed Dockerfile is the one written, indented 4 spaces.
	var shown strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(string(want), "\n"), "\n") {
		shown.WriteString(strings.TrimRight("    "+line, " ") + "\n")
	}
	if !strings.Contains(h.err.String(), "Wrote "+filepath.Join(h.dir, "Dockerfile")+":\n"+shown.String()) {
		t.Fatalf("stderr doesn't show the Dockerfile:\n%s", h.err.String())
	}
}

func TestDeploy_NoDockerfile_Declined_ExitsUsage(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.nodeApp()
	h.tty, h.stdin = true, "n\n"
	h.expect(h.run("deploy"), 2)
	stderr := h.err.String()
	if !strings.HasPrefix(stderr, h.noDockerfileWhat()+"\nThis looks like a Node app. Create one now? [Y/n] ") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.HasSuffix(stderr, h.noDockerfileWhat()+"\nAdd a Dockerfile to this folder, or cd to the folder that has one. To have kx write one, run kx deploy in a terminal.\n") {
		t.Fatalf("stderr = %q", stderr)
	}
	for _, name := range []string{"Dockerfile", ".dockerignore"} {
		if _, ok := h.read(name); ok {
			t.Errorf("wrote %s after a no", name)
		}
	}
	if len(h.api.got()) != 0 || len(h.docker.got()) != 0 {
		t.Fatalf("API calls %v, docker calls %v", h.api.got(), h.docker.got())
	}
}

// In CI nobody sees what kx would write, so it writes nothing, even when
// the job has a terminal.
func TestDeploy_NoDockerfile_InCI_SaysHow(t *testing.T) {
	h := newHarness(t)
	h.env["GITHUB_ACTIONS"], h.env["GITHUB_EVENT_NAME"], h.env["KX_TOKEN"] = "true", "push", tokDeploy
	h.link("shop-api", appShopID, "acme-corp")
	h.nodeApp()
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 2)
	if want := h.noDockerfileWhat() + "\nAdd a Dockerfile to this folder and commit it; kx deploy on your computer can write one.\n"; h.err.String() != want {
		t.Fatalf("stderr = %q, want %q", h.err.String(), want)
	}
	if _, ok := h.read("Dockerfile"); ok || len(h.api.got()) != 0 || len(h.docker.got()) != 0 {
		t.Fatal("did something without a Dockerfile in CI")
	}
}

func TestDeploy_NoDockerfile_Undetected(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.put(map[string]string{"notes.txt": "hello"})
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 2)
	if want := h.noDockerfileWhat() + "\nkx can write one for Node, Python, Go and static sites, but couldn't tell what this folder is. Add a Dockerfile, or cd to your app's folder.\n"; h.err.String() != want {
		t.Fatalf("stderr = %q, want %q", h.err.String(), want)
	}
}

// An app kx recognises but can't run gets the reason, and no question.
func TestDeploy_NoDockerfile_NodeWithoutStart(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.put(map[string]string{"package.json": `{"name":"shop-api"}`})
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 2)
	if want := "This looks like a Node app, but package.json has no start script, so kx can't tell how to run it.\nAdd a \"start\" script to package.json, or add a Dockerfile.\n"; h.err.String() != want {
		t.Fatalf("stderr = %q, want %q", h.err.String(), want)
	}
	if _, ok := h.read("Dockerfile"); ok {
		t.Fatal("wrote a Dockerfile")
	}
}

// A .env file the existing .dockerignore doesn't exclude would go
// into the image; kx asks to add it, then builds.
func TestScaffold_EnvPresentAndNotIgnored_AsksThenAppends(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.nodeApp()
	h.put(map[string]string{".dockerignore": "node_modules\n", ".env": "STRIPE_KEY=test-only\n"})
	h.tty, h.stdin = true, "\n\n"
	h.expect(h.run("deploy"), 0)
	if !strings.Contains(h.err.String(), "Create one now? [Y/n] ! .dockerignore doesn't list .env, so a .env file in this folder would be copied into the image.\nAdd .env to .dockerignore? [Y/n] ") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if got, _ := h.read(".dockerignore"); got != "node_modules\n.env\n.env.*\n" {
		t.Fatalf(".dockerignore = %q", got)
	}
	if strings.Contains(h.err.String(), "Wrote .dockerignore") {
		t.Fatal("said it wrote the .dockerignore that was already there")
	}
	if _, ok := h.read("Dockerfile"); !ok || len(h.docker.builds) != 1 {
		t.Fatalf("Dockerfile written %v, builds %d", ok, len(h.docker.builds))
	}
}

// Answering no builds nothing and writes nothing.
func TestScaffold_EnvPresentDeclined_BuildsNothing(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.nodeApp()
	h.put(map[string]string{".dockerignore": "node_modules\n", ".env.production": "STRIPE_KEY=test-only\n"})
	h.tty, h.stdin = true, "\nn\n"
	h.expect(h.run("deploy"), 2)
	if !strings.HasSuffix(h.err.String(), "Add .env to .dockerignore? [Y/n] .dockerignore doesn't list .env, so a .env file in this folder would be copied into the image.\nAdd a line .env to .dockerignore, then run kx deploy again.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if _, ok := h.read("Dockerfile"); ok {
		t.Fatal("wrote a Dockerfile")
	}
	if got, _ := h.read(".dockerignore"); got != "node_modules\n" {
		t.Fatalf(".dockerignore = %q", got)
	}
	if len(h.docker.builds) != 0 || len(h.api.got()) != 0 {
		t.Fatalf("builds %d, API calls %v", len(h.docker.builds), h.api.got())
	}
}

// Two kinds of app in one folder; kx asks which, never guesses.
func TestDetect_TwoStacks_Asks(t *testing.T) {
	for _, tt := range []struct {
		answer string
		code   int
		from   string // the Dockerfile's FROM line, "" for none
	}{
		{"2\n", 0, "FROM python:3.12-slim\n"},
		{"1\n", 0, "FROM node:22-alpine\n"},
		{"3\n", 2, ""},
		{"", 2, ""},
	} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.link("shop-api", appShopID, "acme-corp")
		h.nodeApp()
		h.put(map[string]string{"requirements.txt": "flask\n", "app.py": "from flask import Flask\napp = Flask(__name__)\n"})
		if found, err := scaffold.Detect(h.dir); err != nil || len(found) != 2 {
			t.Fatalf("Detect() found %d: %+v, %v", len(found), found, err)
		}
		h.tty, h.stdin = true, tt.answer
		h.expect(h.run("deploy"), tt.code)
		if want := h.noDockerfileWhat() + "\nThis folder looks like more than one kind of app. Which one is it?\n  1) a Node app\n  2) a Python app\n  3) None of these\nChoose a number: "; !strings.HasPrefix(h.err.String(), want) {
			t.Fatalf("%q: stderr = %q", tt.answer, h.err.String())
		}
		if strings.Contains(h.err.String(), "Create one now?") {
			t.Fatalf("%q: asked again after the choice", tt.answer)
		}
		df, ok := h.read("Dockerfile")
		if tt.from == "" {
			if ok || len(h.docker.builds) != 0 {
				t.Fatalf("%q: wrote or built after None of these", tt.answer)
			}
			if !strings.HasSuffix(h.err.String(), "To have kx write one, run kx deploy in a terminal.\n") {
				t.Fatalf("%q: stderr = %q", tt.answer, h.err.String())
			}
			continue
		}
		if !strings.Contains(df, "\n"+tt.from) || len(h.docker.builds) != 1 {
			t.Fatalf("%q: Dockerfile:\n%s\nbuilds %d", tt.answer, df, len(h.docker.builds))
		}
	}
}

// On a first run the offer comes first, then the app picker, both
// answered from the same terminal.
func TestDeploy_NoDockerfile_FirstRunOffersThenPicks(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.inFolder("billing-worker")
	h.nodeApp()
	h.tty, h.stdin = true, "\n\n"
	h.expect(h.run("deploy"), 0)
	if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != "billing-worker" || h.api.newApps[0]["port"] != float64(8080) {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}
	if _, ok := h.read("Dockerfile"); !ok {
		t.Fatal("no Dockerfile")
	}
}
