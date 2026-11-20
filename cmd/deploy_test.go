package cmd

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/safetext"
)

const ghSHA = "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345"

func (h *harness) dockerfile(body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, "Dockerfile"), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// inFolder moves the harness to a new project folder called name.
func (h *harness) inFolder(name string) {
	h.t.Helper()
	h.dir = filepath.Join(filepath.Dir(h.dir), name)
	if err := os.MkdirAll(filepath.Join(h.dir, ".git"), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func TestDeploy_FirstRunPickerSavesKxJSON_Golden(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.inFolder("billing-worker")
	h.dockerfile("FROM node:22\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 0)
	golden(t, "deploy_first_run", h.norm(h.err.String()+h.out.String()))

	_, p, err := config.FindProject(h.dir)
	if err != nil || p == nil || p.App != "billing-worker" || p.Org != "acme-corp" || p.AppID == "" {
		t.Fatalf(".kx.json = %+v, %v (want the new app's ID saved after creating it)", p, err)
	}
	if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != "billing-worker" || h.api.newApps[0]["port"] != float64(8080) {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}

	// The next run asks nothing.
	h.tty, h.stdin = false, ""
	h.expect(h.run("deploy"), 0)
	if strings.Contains(h.err.String(), "Which app") || len(h.api.newApps) != 1 {
		t.Fatalf("second run asked again or created another app:\n%s", h.err.String())
	}
}

// The first run offers a new app named after the folder even when the
// folder's name isn't an app name, and preselects it.
func TestDeploy_FirstRunOffersNewAppFromAnyFolderName(t *testing.T) {
	for folder, app := range map[string]string{"My_App": "my-app", "ShopAPI": "shopapi", "todo.app": "todo-app", "My Project": "my-project"} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.inFolder(folder)
		h.dockerfile("FROM scratch\nEXPOSE 8080\n")
		h.tty, h.stdin = true, "\n"
		h.expect(h.run("deploy"), 0)
		if !strings.Contains(h.err.String(), "Which app should this folder deploy to?\n  1) (new app: "+app+")\n  2) shop-api\n  3) web\nChoose [1]: ") {
			t.Fatalf("%s: picker:\n%s", folder, h.err.String())
		}
		if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != app {
			t.Fatalf("%s: POST /apps = %v", folder, h.api.newApps)
		}
	}
}

// An existing app is never preselected: Enter alone deploys nothing, even
// when the app has the folder's name.
func TestDeploy_FirstRunNeverPreselectsAnExistingApp(t *testing.T) {
	for _, folder := range []string{"shop-api", "web"} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.inFolder(folder)
		h.dockerfile("FROM scratch\n")
		h.tty, h.stdin = true, "\n"
		h.expect(h.run("deploy"), 2)
		if strings.Contains(h.err.String(), "(new app") || !strings.Contains(h.err.String(), "\n  1) "+folder+"\n") || !strings.Contains(h.err.String(), "Choose a number: ") {
			t.Fatalf("%s: picker:\n%s", folder, h.err.String())
		}
		if !strings.Contains(h.err.String(), "No app was chosen, so this folder isn't linked yet.\nRun kx deploy again and choose an app by its number, or name it: kx deploy --app <app>\n") {
			t.Fatalf("%s: stderr:\n%s", folder, h.err.String())
		}
		if len(h.api.deployPosts) != 0 || len(h.docker.got()) > 1 {
			t.Fatalf("%s: deployed without an explicit pick", folder)
		}
		if _, p, _ := config.FindProject(h.dir); p != nil {
			t.Fatalf("%s: saved .kx.json %+v", folder, p)
		}

		// Choosing it by its number deploys to it.
		h.stdin = "1\n"
		h.expect(h.run("deploy"), 0)
		if len(h.api.deployPosts) != 1 || len(h.api.newApps) != 0 {
			t.Fatalf("%s: deploys %v, new apps %v", folder, h.api.deployPosts, h.api.newApps)
		}
	}
}

// With no apps and a folder name kx can't turn into an app name, the fix
// says how to name one.
func TestDeploy_FirstRunWithNoAppsAndNoUsableFolderName(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.inFolder("___")
	h.dockerfile("FROM scratch\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 2)
	if !strings.Contains(h.err.String(), "There are no apps yet, and kx can't make an app name from this folder's name.\nName the new app: kx link <name>, then run kx deploy\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	h.env["KX_TOKEN"] = tokRead // can't create apps (or deploy: kx link asks)
	h.inFolder("billing-worker")
	h.expect(h.run("link"), 2)
	if !strings.Contains(h.err.String(), "There are no apps to link to yet.\nCreate an app in the dashboard, or log in with a token that can deploy: kx login\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_StdoutIsOnlyTheURL(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.expect(h.run("deploy"), 0)
	if h.out.String() != "https://"+appShopID+"--acme-corp.apps.example.test\n" {
		t.Fatalf("stdout = %q", h.out.String())
	}
	errOut := h.err.String()
	for _, want := range []string{"✓ Checked   Dockerfile, port 8080\n", "✓ Built     shop-api:local-20261112090000\n", "✓ Pushed    registry.kubernetix.ai/acme-corp/shop-api\n", "✓ Live      v16  queued → starting → live\n", "  4 of 5 deploys left today on the Free plan.\n"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	post := h.api.deployPosts[0]
	if post.AppID != appShopID || post.Body["image_url"] != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest {
		t.Fatalf("deploy request = %+v", post)
	}
	if got := strings.Join(h.docker.got(), "\n"); got != "check\nbuild shop-api:local-20261112090000\ntag shop-api:local-20261112090000 registry.kubernetix.ai/acme-corp/shop-api:local-20261112090000\npush registry.kubernetix.ai/acme-corp/shop-api:local-20261112090000" {
		t.Fatalf("docker calls:\n%s", got)
	}
}

func TestDeploy_JSON(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--json"), 0)
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, h.out.String())
	}
	if out["url"] != "https://"+appShopID+"--acme-corp.apps.example.test" || out["version"] != float64(16) || out["status"] != "live" ||
		out["app"] != "shop-api" || out["image"] != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest || out["deployment_id"] == "" {
		t.Fatalf("json = %v", out)
	}
}

func TestDeploy_FailureOutput_Golden(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.api.statuses = []string{"pending", "deploying", "failed"}
	h.api.failReason = "your container is crashing on start"
	var logLines []string
	for i := 1; i <= 25; i++ {
		logLines = append(logLines, "boot step "+string(rune('a'+i-1)))
	}
	h.api.depLogsFor = func(id string) (api.DeploymentLogs, bool) {
		if strings.HasPrefix(id, "new-") {
			return api.DeploymentLogs{DeploymentID: id, DeploymentLogs: strings.Join(logLines, "\n") + "\n"}, true
		}
		return api.DeploymentLogs{}, false
	}
	h.expect(h.run("deploy"), 8)
	if h.out.String() != "" {
		t.Fatalf("stdout = %q, want nothing when the deploy fails", h.out.String())
	}
	golden(t, "deploy_failed", h.norm(h.err.String()))
}

func TestDeploy_TimeoutOutput(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.api.statuses = []string{"pending", "deploying"}
	h.sleepHook = func(int) error { h.now = h.now.Add(time.Minute); return nil }
	h.expect(h.run("deploy"), 9)
	lines := strings.Split(strings.TrimRight(h.err.String(), "\n"), "\n")
	got := strings.Join(lines[len(lines)-2:], "\n")
	want := "shop-api v16 is still starting after 18 minutes, so kx stopped waiting; the deploy continues.\nCheck on it: kx status"
	if got != want || h.out.String() != "" {
		t.Fatalf("last lines =\n%s\nwant\n%s\nstdout %q", got, want, h.out.String())
	}
	h.expect(h.run("deploy", "--timeout", "3m"), 9)
	if !strings.Contains(h.err.String(), "after 3 minutes") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	// The default outlasts the platform's own retries, so its verdict
	// comes first; --help says what it is.
	h.expect(h.run("deploy", "--help"), 0)
	if !strings.Contains(h.out.String(), `how long to wait for the deploy to go live, like 18m or 1h (default "18m")`) {
		t.Fatalf("help:\n%s", h.out.String())
	}
}

func TestDeploy_FlagErrors(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	tests := []struct {
		args []string
		what string
	}{
		{[]string{"deploy", "--timeout", "soon"}, "kx deploy --timeout takes a length of time"},
		{[]string{"deploy", "--timeout", "0s"}, "kx deploy --timeout takes a length of time"},
		{[]string{"deploy", "--image", "registry.kubernetix.ai/acme-corp/shop-api:v1", "--build", "./api"}, "--image deploys an image that's already built"},
		{[]string{"deploy", "--image", "registry.kubernetix.ai/acme-corp/shop-api:v1", "--dockerfile", "Dockerfile.prod"}, "--image deploys an image that's already built"},
		{[]string{"deploy", "--image", "-v"}, "isn't an image name"},
		{[]string{"deploy", "--image", "a b"}, "isn't an image name"},
		{[]string{"deploy", "--github"}, "kx deploy --github only works inside a GitHub Actions job."},
		{[]string{"deploy", "extra"}, "kx deploy takes no arguments."},
	}
	for _, tt := range tests {
		h.expect(h.run(tt.args...), 2)
		if !strings.Contains(h.err.String(), tt.what) {
			t.Errorf("kx %v: stderr = %q, want %q", tt.args, h.err.String(), tt.what)
		}
	}
	if n := len(h.api.got()); n != 0 {
		t.Fatalf("made %d API calls for flag errors", n)
	}
}

func TestDeploy_NoDockerfile(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("deploy"), 2)
	if !strings.Contains(h.err.String(), "kx needs a Dockerfile to build your app, and there isn't one in "+h.dir+".\nAdd a Dockerfile to this folder, or cd to the folder that has one. To have kx write one, run kx deploy in a terminal.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if len(h.api.got()) != 0 || len(h.docker.got()) != 0 {
		t.Fatal("did something without a Dockerfile")
	}
}

func TestDeploy_DockerProblemsStopBeforeAnyAPICall(t *testing.T) {
	for _, tt := range []struct {
		err  error
		what string
	}{
		{kxerr.ErrDockerMissing, "kx builds your app with Docker, and Docker isn't installed on this computer."},
		{kxerr.ErrDockerStopped, "Docker is installed but not running."},
		{kxerr.ErrDockerWindowsContainers, "Docker Desktop is set to Windows containers"},
	} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.link("shop-api", appShopID, "acme-corp")
		h.dockerfile("FROM scratch\n")
		h.docker.checkErr = tt.err
		h.expect(h.run("deploy"), 10)
		if !strings.Contains(h.err.String(), tt.what) {
			t.Errorf("stderr = %q", h.err.String())
		}
		if n := len(h.api.got()); n != 0 {
			t.Errorf("%v: made %d API calls before giving up", tt.err, n)
		}
	}
}

func TestDeploy_ImageWorksWithoutDocker(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.docker.checkErr = kxerr.ErrDockerMissing
	img := "registry.kubernetix.ai/acme-corp/shop-api@" + testDigest
	h.expect(h.run("deploy", "--image", img), 0)
	if len(h.docker.got()) != 0 || h.api.mints != 0 {
		t.Fatalf("docker calls %v, mints %d", h.docker.got(), h.api.mints)
	}
	if h.api.deployPosts[0].Body["image_url"] != img {
		t.Fatalf("image_url = %v", h.api.deployPosts[0].Body["image_url"])
	}
}

func TestDeploy_BuildFolderAndDockerfileFlags(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	if err := os.MkdirAll(filepath.Join(h.dir, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "api", "Dockerfile.prod"), []byte("FROM scratch\nEXPOSE 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.expect(h.run("deploy", "--build", "api", "--dockerfile", filepath.Join("api", "Dockerfile.prod")), 0)
	if !strings.Contains(h.err.String(), "✓ Checked   Dockerfile.prod, port 8080") || !strings.Contains(h.err.String(), "The Dockerfile exposes port 3000, but shop-api listens on port 8080.") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_DockerfileRelativeToBuildFolder(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	if err := os.MkdirAll(filepath.Join(h.dir, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "api", "Dockerfile.prod"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// As the Action passes them: path: api, dockerfile: Dockerfile.prod.
	h.expect(h.run("deploy", "--build=api", "--dockerfile=Dockerfile.prod"), 0)
	if !strings.Contains(h.err.String(), "✓ Checked   Dockerfile.prod, port 8080") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	// A path that exists from the working folder wins, as with docker build -f.
	if err := os.WriteFile(filepath.Join(h.dir, "Dockerfile.prod"), []byte("FROM scratch\nEXPOSE 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.expect(h.run("deploy", "--build=api", "--dockerfile=Dockerfile.prod"), 0)
	if !strings.Contains(h.err.String(), "exposes port 3000") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_NotLinkedAndNoTerminal(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 2)
	if !strings.Contains(h.err.String(), "This folder isn't linked to an app yet, and kx can't ask which one here.\nRun: kx deploy --app <app>") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

func TestDeploy_LinkedNewAppIsCreated(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.inFolder("billing-worker")
	h.link("billing-worker", "", "acme-corp") // kx link chose "(new app)"
	h.dockerfile("FROM scratch\nEXPOSE 9000\n")
	h.expect(h.run("deploy"), 0)
	if len(h.api.newApps) != 1 || h.api.newApps[0]["port"] != float64(9000) {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}
	if !strings.Contains(h.err.String(), "✓ Created   app billing-worker\n✓ Live      v1  ") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_AppTokenCannotCreateApps(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokDeploy
	h.inFolder("billing-worker")
	h.link("billing-worker", "", "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 3)
	if !strings.Contains(h.err.String(), "This token can only deploy shop-api, and you asked to deploy billing-worker.") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if len(h.api.newApps) != 0 || len(h.api.deployPosts) != 0 {
		t.Fatal("an app token created or deployed something")
	}
}

func TestDeploy_TokenWithoutDeployDoesNotCreateApps(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokRead
	h.inFolder("billing-worker")
	h.link("billing-worker", "", "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 3)
	if !strings.Contains(h.err.String(), "doesn't have the Deploy and Push images permissions") || len(h.api.newApps) != 0 {
		t.Fatalf("stderr = %q, POST /apps %d", h.err.String(), len(h.api.newApps))
	}
	// With --image, Push images isn't needed and Deploy is still missing.
	h.expect(h.run("deploy", "--image", "registry.kubernetix.ai/acme-corp/billing-worker@"+testDigest), 3)
	if !strings.Contains(h.err.String(), "doesn't have the Deploy permission, which kx deploy needs.") || len(h.api.newApps) != 0 {
		t.Fatalf("stderr = %q, POST /apps %d", h.err.String(), len(h.api.newApps))
	}
}

func TestDeploy_PickerOffersNewAppOnlyToTokensThatCanDeploy(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokRead
	h.inFolder("billing-worker")
	h.dockerfile("FROM scratch\n")
	h.tty, h.stdin = true, "\n"
	h.run("link")
	if strings.Contains(h.err.String(), "(new app") || !strings.Contains(h.err.String(), "Which app should this folder deploy to?") {
		t.Fatalf("picker:\n%s", h.err.String())
	}
}

func TestDeploy_AppTokenDeploysItsAppWithoutAsking(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokDeploy
	h.dockerfile("FROM scratch\n")
	h.tty, h.stdin = true, ""
	h.expect(h.run("deploy"), 0)
	if strings.Contains(h.err.String(), "Which app") || h.api.deployPosts[0].AppID != appShopID {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_TokenWithoutPushImages(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokApp // Deploy and Read, no Push images
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 3)
	if !strings.Contains(h.err.String(), "This token can't upload images.\nCreate a token with Push images on the app's Deploy tab, or deploy an image that's already pushed: kx deploy --image <image>\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if len(h.api.deployPosts) != 0 {
		t.Fatal("deployed without an image")
	}
	// /whoami already says the token can't push, so kx stops before building.
	if calls := h.docker.got(); len(calls) != 1 || calls[0] != "check" {
		t.Fatalf("docker calls %v, want only the check", calls)
	}
}

// kx checks the token's permissions (from /whoami) before building and
// names the one that is missing.
func TestDeploy_MissingPermissionNamedBeforeBuilding(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokNoRead
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 3)
	if !strings.Contains(h.err.String(), "The token in the KX_TOKEN secret doesn't have the Read permission, which kx deploy needs.\nCreate a deploy token on the app's Deploy tab (it has Deploy, Read and Push images) and replace the KX_TOKEN secret.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if reqs := h.api.got(); len(reqs) != 1 || reqs[0] != "GET /api/v1/whoami" {
		t.Fatalf("requests %v, want only /whoami", reqs)
	}
	if calls := h.docker.got(); len(calls) != 1 {
		t.Fatalf("docker calls %v", calls)
	}

	// A login without Deploy.
	h = newHarness(t)
	h.loginAs("acme-corp", tokRead, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 3)
	if !strings.Contains(h.err.String(), "The token this computer uses for acme-corp doesn't have the Deploy and Push images permissions, which kx deploy needs.\nLog in with a token that can deploy: kx login --org acme-corp\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// Deploying an image that's already pushed needs no Push images.
func TestDeploy_ImageNeedsNoPushPermission(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokApp // Deploy and Read
	img := "registry.kubernetix.ai/acme-corp/shop-api@" + testDigest
	h.expect(h.run("deploy", "--image", img), 0)
}

func TestDeploy_CommitAlreadyPushedUnderImmutableTags(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	h.dockerfile("FROM scratch\n")
	h.docker.pushErrs = []error{&kxerr.PushFailed{Err: kxerr.ErrTagExists}}
	h.expect(h.run("deploy", "--github"), 0)
	calls := strings.Join(h.docker.got(), "\n")
	if !strings.Contains(calls, "push registry.kubernetix.ai/acme-corp/shop-api:3f9c2a1b7d4e\n") ||
		!strings.Contains(calls, "push registry.kubernetix.ai/acme-corp/shop-api:3f9c2a1b7d4e-20261112090000") {
		t.Fatalf("docker calls:\n%s", calls)
	}
	if !strings.Contains(h.err.String(), "This commit was already pushed; uploaded it as 3f9c2a1b7d4e-20261112090000.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if got := readOutputs(t, h.env["GITHUB_OUTPUT"])["image"]; got != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest {
		t.Fatalf("image output = %q, want the digest kx pushed", got)
	}
}

// In CI nothing retries a job that hits the upload limit, so kx waits it
// out (up to about six minutes); on a laptop it says how long to wait.
func TestDeploy_UploadLimitWaitedOutOnlyInCI(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	h.api.mintLimits = []string{"300"}
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 0)
	if !strings.Contains(h.err.String(), "This token has uploaded images too often in the last hour, so kx waits 5m 0s before uploading.\n") || h.api.mints != 1 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	h = newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.mintLimits = []string{"300"}
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 5)
	if !strings.Contains(h.err.String(), "This token has uploaded images too often in the last hour.\nWait 5 minutes, then run kx deploy again.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_LaptopCachesRegistryCredential(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	h.expect(h.run("deploy"), 0)
	if h.api.mints != 1 {
		t.Fatalf("minted %d credentials for two deploys, want 1 (cached)", h.api.mints)
	}
	entries, _ := filepath.Glob(filepath.Join(h.configDir, "cache", "registry-*.json"))
	if len(entries) != 1 {
		t.Fatalf("cache files = %v", entries)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(entries[0]); fi.Mode().Perm() != 0o600 {
			t.Fatalf("cache file mode %o", fi.Mode().Perm())
		}
	}
}

func TestDeploy_NoCredentialCacheForKXToken(t *testing.T) {
	h := newHarness(t) // no CI variables at all, like a Jenkins or Azure agent
	h.env["KX_TOKEN"] = tokDeploy
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	h.expect(h.run("deploy"), 0)
	if h.api.mints != 2 {
		t.Fatalf("minted %d credentials for two KX_TOKEN deploys, want 2 (never cached)", h.api.mints)
	}
	if entries, _ := filepath.Glob(filepath.Join(h.configDir, "cache", "*")); len(entries) != 0 {
		t.Fatalf("cached a credential for KX_TOKEN: %v", entries)
	}
}

// ghHarness sets up a GitHub Actions push job with a fake GitHub API.
type ghServer struct {
	mu       sync.Mutex
	statuses []map[string]string
	code     int
}

func (g *ghServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	g.statuses = append(g.statuses, body)
	if g.code != 0 {
		w.WriteHeader(g.code)
		return
	}
	w.WriteHeader(201)
}

func (g *ghServer) got() []map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]string(nil), g.statuses...)
}

func githubJob(h *harness) *ghServer {
	h.t.Helper()
	gh := &ghServer{}
	srv := httptest.NewServer(gh)
	h.t.Cleanup(srv.Close)
	dir := h.t.TempDir()
	for k, v := range map[string]string{
		"GITHUB_ACTIONS": "true", "CI": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_SHA": ghSHA,
		"GITHUB_REPOSITORY": "acme/shop-api", "GITHUB_RUN_ID": "9001", "GITHUB_RUN_ATTEMPT": "1",
		"GITHUB_TOKEN": "ghs_test", "GITHUB_API_URL": srv.URL,
		"GITHUB_OUTPUT": filepath.Join(dir, "output"), "GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
		"KX_TOKEN": tokDeploy,
	} {
		h.env[k] = v
	}
	return gh
}

func readOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, delim, ok := strings.Cut(sc.Text(), "<<")
		if !ok {
			t.Fatalf("output line %q is not in the heredoc form", sc.Text())
		}
		var val []string
		for sc.Scan() && sc.Text() != delim {
			val = append(val, sc.Text())
		}
		out[name] = strings.Join(val, "\n")
	}
	return out
}

func TestMasksRegistryCredential(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	h.dockerfile("FROM scratch\n")
	var stderrAtPush string
	h.docker.onPush = func(api.RegistryCredential) { stderrAtPush = h.err.String() }
	h.expect(h.run("deploy", "--github", "--environment", "production"), 0)
	all := h.err.String()
	first := -1
	for i, line := range strings.Split(all, "\n") {
		if strings.Contains(line, regPassword) {
			first = i
			if line != "::add-mask::"+regPassword {
				t.Fatalf("first line with the password is not the mask command")
			}
			break
		}
	}
	if first < 0 {
		t.Fatal("the registry credential was never masked")
	}
	if !strings.Contains(stderrAtPush, "::add-mask::"+regPassword+"\n") {
		t.Fatal("the credential was not masked before docker push ran")
	}
	if !strings.Contains(all, "::add-mask::"+base64.StdEncoding.EncodeToString([]byte("kx-push:"+regPassword))+"\n") {
		t.Fatal("the encoded Docker login was not masked")
	}
}

func TestDeploy_GitHubMode(t *testing.T) {
	h := newHarness(t)
	gh := githubJob(h)
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github", "--environment", "staging", "--build", ".", "--timeout", "18m"), 0)

	if h.out.String() != "https://"+appShopID+"--acme-corp.apps.example.test\n" {
		t.Fatalf("stdout = %q", h.out.String())
	}
	if !strings.Contains(h.err.String(), "✓ Built     shop-api:3f9c2a1b7d4e") {
		t.Fatalf("tag is not the commit SHA:\n%s", h.err.String())
	}
	statuses := gh.got()
	var descs []string
	for _, s := range statuses {
		if s["context"] != "Kubernetix / staging" {
			t.Fatalf("status context = %q", s["context"])
		}
		descs = append(descs, s["state"]+": "+s["description"])
	}
	if strings.Join(descs, " | ") != "pending: Building | pending: Deploying v16 | success: Live in 0s" &&
		!strings.HasPrefix(strings.Join(descs, " | "), "pending: Building | pending: Deploying v16 | success: Live in") {
		t.Fatalf("statuses = %v", descs)
	}
	if statuses[len(statuses)-1]["target_url"] != "https://"+appShopID+"--acme-corp.apps.example.test" {
		t.Fatalf("final status links to %q", statuses[len(statuses)-1]["target_url"])
	}
	// (The earlier statuses link to the dashboard, which is http in this
	// test, so kx leaves the link off: GitHub only gets https links.)

	outs := readOutputs(t, h.env["GITHUB_OUTPUT"])
	if outs["url"] != "https://"+appShopID+"--acme-corp.apps.example.test" || outs["version"] != "v16" ||
		outs["image"] != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest || outs["deployment-id"] == "" || len(outs) != 4 {
		t.Fatalf("outputs = %v", outs)
	}
	sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(sum), "### shop-api v16 is live") || !strings.Contains(string(sum), "| Commit | `3f9c2a1b7d4e` |") {
		t.Fatalf("summary:\n%s", sum)
	}
	if h.api.mints != 1 {
		t.Fatalf("mints = %d", h.api.mints)
	}
	if entries, _ := filepath.Glob(filepath.Join(h.configDir, "cache", "*")); len(entries) != 0 {
		t.Fatalf("cached a credential in CI: %v", entries)
	}
	want := ci.Detect(func(k string) string { return h.env[k] }).IdempotencyKey("shop-api")
	if want == "" || h.api.deployPosts[0].Key != want {
		t.Fatalf("Idempotency-Key = %q, want the run's UUIDv5 %q", h.api.deployPosts[0].Key, want)
	}
}

func TestDeploy_GitHubReRunOfSameAttemptDeploysOnce(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 0)
	h.expect(h.run("deploy", "--github"), 0) // e.g. the step retried inside one attempt
	if len(h.api.deployPosts) != 2 || h.api.deployPosts[0].Key != h.api.deployPosts[1].Key {
		t.Fatalf("posts = %+v", h.api.deployPosts)
	}
	if !strings.Contains(h.err.String(), "This run already deployed v16, so nothing new was started; kx reports that deploy.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"]); !strings.Contains(string(sum), "This run already deployed v16, so nothing new was started.") {
		t.Fatalf("summary:\n%s", sum)
	}
	n := 0
	for _, d := range h.api.deployments[appShopID] {
		if strings.HasPrefix(d.ID, "new-") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d deployments created, want 1", n)
	}
	h.env["GITHUB_RUN_ATTEMPT"] = "2" // a re-run deploys again
	h.expect(h.run("deploy", "--github"), 0)
	if h.api.deployPosts[2].Key == h.api.deployPosts[0].Key {
		t.Fatal("a re-run reused the first attempt's key")
	}
}

func TestDeploy_GitHubFailure(t *testing.T) {
	h := newHarness(t)
	gh := githubJob(h)
	h.dockerfile("FROM scratch\n")
	h.api.statuses = []string{"pending", "failed"}
	h.api.failReason = "your container is crashing on start"
	h.expect(h.run("deploy", "--github"), 8)
	want := "::error title=Kubernetix deploy::shop-api v16 didn't go live: your container is crashing on start. Version 14 is still serving.%0ASee why: kx logs --deployment v16\n"
	if !strings.Contains(h.err.String(), want) {
		t.Fatalf("stderr lacks the annotation:\n%s", h.err.String())
	}
	st := gh.got()
	last := st[len(st)-1]
	if last["state"] != "failure" || !strings.HasPrefix(last["description"], "shop-api v16 didn't go live") {
		t.Fatalf("final status = %v", last)
	}
	outs := readOutputs(t, h.env["GITHUB_OUTPUT"])
	if outs["version"] != "v16" || outs["deployment-id"] == "" {
		t.Fatalf("outputs = %v", outs)
	}
	sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(sum), "### shop-api v16 didn't go live") || !strings.Contains(string(sum), "Version 14 is still serving.") {
		t.Fatalf("summary:\n%s", sum)
	}
}

// statusesFor is what successive GETs of a new deployment return: pending,
// then n times deploying, then last.
func statusesFor(n int, last string) []string {
	out := []string{"pending"}
	for range n {
		out = append(out, "deploying")
	}
	return append(out, last)
}

// neverReady sets the fake up with a new version whose replica runs but
// isn't ready, and the clock moving 5 s per poll; ready, when set, is how
// long after the start the replica gets ready.
func neverReady(h *harness, ready time.Duration) {
	h.api.replicas = []api.Replica{
		{Name: "app-old", Status: "Running", Ready: true, StartedAt: "2026-11-12T08:00:00Z"},
		{Name: "app-new", Status: "Running", Ready: false, StartedAt: "2026-11-12T09:00:30Z"},
	}
	h.api.runtimeLogs = func(map[string]string) string {
		return `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"pod":"app-new"},"values":[["1762938030000000000","listening on :3000"]]}]}}`
	}
	start := h.now
	h.sleepHook = func(int) error {
		h.now = h.now.Add(5 * time.Second)
		if ready > 0 && h.now.Sub(start) >= ready {
			h.api.mu.Lock()
			h.api.replicas[1].Ready = true
			h.api.mu.Unlock()
		}
		return nil
	}
}

// The early signal never fails a deploy on its own: a container that gets
// ready on a later start (here at 160 s, after the signal at about two
// minutes) is a success with a green status. The signal is a warning
// annotation, not an error.
func TestDeploy_GitHubReadyAfterTheSignalIsGreen(t *testing.T) {
	h := newHarness(t)
	gh := githubJob(h)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.api.statuses = statusesFor(34, "ready") // live at about 175 s
	neverReady(h, 160*time.Second)
	h.expect(h.run("deploy", "--github"), 0)
	out := h.err.String()
	if !strings.Contains(out, "::warning title=Kubernetix deploy::shop-api v16 isn't live yet: for 2 minutes its container has kept crashing or hasn't answered health checks on / at port 8080.%0AMake your app start and answer on / at port 8080; the log lines below may say why it hasn't.\n") {
		t.Fatalf("stderr lacks the warning annotation:\n%s", out)
	}
	if strings.Contains(out, "::error") || !strings.Contains(out, "  listening on :3000\n") || !strings.Contains(out, "✓ Live      v16") {
		t.Fatalf("stderr:\n%s", out)
	}
	st := gh.got()
	if last := st[len(st)-1]; last["state"] != "success" {
		t.Fatalf("final status = %v", last)
	}
	if h.out.String() == "" {
		t.Fatal("no URL on standard output")
	}
}

// When the platform does fail the version, the job fails (exit 8) with
// what kx saw.
func TestDeploy_GitHubNeverReadyFailsWithTheCause(t *testing.T) {
	h := newHarness(t)
	gh := githubJob(h)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.api.statuses = statusesFor(60, "failed")
	h.api.failReason = "Deployment failed. The platform team has been alerted; please retry, or contact support if the issue persists."
	neverReady(h, 0)
	h.expect(h.run("deploy", "--github"), 8)
	st := gh.got()
	last := st[len(st)-1]
	if last["state"] != "failure" || !strings.HasPrefix(last["description"], "shop-api v16 didn't go live: its container kept crashing or didn't answer health checks on / at port 8080.") {
		t.Fatalf("final status = %v", last)
	}
}

func TestDeploy_OwnErrorStillWorksAfterAnUntrustedBlock(t *testing.T) {
	for _, github := range []bool{true, false} {
		h := newHarness(t)
		githubJob(h)
		h.dockerfile("FROM scratch\n")
		h.api.statuses = []string{"pending", "failed"}
		h.api.failReason = "your container is crashing on start"
		h.api.depLogsFor = func(id string) (api.DeploymentLogs, bool) {
			return api.DeploymentLogs{DeploymentLogs: "\u3000::stop-commands::guess\n::error::forged\nstep ##[warning]x\n"}, true
		}
		args := []string{"deploy"}
		if github {
			args = append(args, "--github")
		}
		h.expect(h.run(args...), 8)
		got := safetext.Commands(h.err.String())
		var kinds []string
		for _, c := range got {
			name, _, _ := strings.Cut(strings.TrimPrefix(c, "::"), "::")
			name, _, _ = strings.Cut(name, " ")
			kinds = append(kinds, name)
		}
		want := "stop-commands,resume-commands" // wrapped in any GitHub Actions run
		if github {
			want = "add-mask,add-mask," + want + ",error"
		}
		if strings.Join(kinds, ",") != want {
			t.Fatalf("github=%v: runner would act on %q", github, got)
		}
		if strings.Contains(h.err.String(), "forged") && strings.Contains(strings.Join(got, "\n"), "forged") {
			t.Fatal("the forged annotation got through")
		}
	}
}

func TestDeploy_GitHubBuildFailureSummary(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	h.dockerfile("FROM scratch\n")
	h.docker.buildErr = &kxerr.BuildFailed{Err: errors.New("exit status 1")}
	h.expect(h.run("deploy", "--github"), 6)
	if !strings.Contains(h.err.String(), "#5 ERROR: npm ci failed") {
		t.Fatalf("Docker's output not shown:\n%s", h.err.String())
	}
	sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(sum), "### shop-api wasn't deployed") || !strings.Contains(string(sum), "Nothing was deployed, so what was live before is still live.") {
		t.Fatalf("summary:\n%s", sum)
	}
	if outs := readOutputs(t, h.env["GITHUB_OUTPUT"]); len(outs) != 0 {
		t.Fatalf("outputs after a failed build = %v", outs)
	}
}

func TestDeploy_GitHubReportsEarlyFailures(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		setup      func(h *harness)
		exit       int
		what       string
		wantStatus bool
	}{
		{"bad timeout", []string{"deploy", "--github", "--timeout", "soon"}, nil, 2, "kx deploy --timeout takes a length of time", true},
		{"damaged .kx.json", []string{"deploy", "--github"}, func(h *harness) {
			if err := os.WriteFile(filepath.Join(h.dir, ".kx.json"), []byte("{not json"), 0o644); err != nil {
				h.t.Fatal(err)
			}
		}, 2, "The .kx.json in", true},
		{"bad environment", []string{"deploy", "--github", "--environment", ""}, nil, 2, "--environment needs a name", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			gh := githubJob(h)
			if tt.setup != nil {
				tt.setup(h)
			}
			h.expect(h.run(tt.args...), tt.exit)
			if !strings.Contains(h.err.String(), "::error title=Kubernetix deploy::"+tt.what) {
				t.Fatalf("no annotation:\n%s", h.err.String())
			}
			st := gh.got()
			if tt.wantStatus && (len(st) != 1 || st[0]["state"] != "failure" || !strings.HasPrefix(st[0]["description"], tt.what)) {
				t.Fatalf("statuses = %v", st)
			}
			if !tt.wantStatus && len(st) != 0 {
				t.Fatalf("set a status under an environment name nobody asked for: %v", st)
			}
			sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"])
			if !strings.Contains(string(sum), "wasn't deployed") || !strings.Contains(string(sum), "Nothing was deployed") {
				t.Fatalf("summary:\n%s", sum)
			}
		})
	}
}

func TestDeploy_GitHubWithoutTokenWarnsAndDeploys(t *testing.T) {
	h := newHarness(t)
	gh := githubJob(h)
	h.env["GITHUB_TOKEN"] = ""
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 0)
	if strings.Count(h.err.String(), "::warning") != 1 || len(gh.got()) != 0 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_GitHubMissingPermissionWarnsAndDeploys(t *testing.T) {
	h := newHarness(t)
	gh := githubJob(h)
	gh.code = 403
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 0)
	if !strings.Contains(h.err.String(), "::warning title=Kubernetix deploy::The job's GITHUB_TOKEN can't set commit statuses") || len(gh.got()) != 1 {
		t.Fatalf("stderr:\n%s\nstatus calls %d", h.err.String(), len(gh.got()))
	}
}

func TestPullRequest_Skips(t *testing.T) {
	for _, event := range []string{"pull_request", "pull_request_target", "merge_group", "pull_request_review"} {
		h := newHarness(t)
		githubJob(h)
		h.env["GITHUB_EVENT_NAME"] = event
		h.env["KX_TOKEN"] = "" // forks get no secrets
		h.expect(h.run("deploy", "--github"), 0)
		if h.err.String() != ci.SkipMessage+"\n" || h.out.String() != "" {
			t.Fatalf("%s: stderr %q stdout %q", event, h.err.String(), h.out.String())
		}
		if len(h.api.got()) != 0 || len(h.docker.got()) != 0 {
			t.Fatalf("%s: did work in a pull request", event)
		}
		if _, err := os.Stat(h.env["GITHUB_OUTPUT"]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: wrote outputs", event)
		}
	}
}

// Only pull request runs are skipped: a release, a schedule or another
// workflow deploys the commit the workflow checked out, rather than
// succeeding without deploying.
func TestDeploy_OtherGitHubEventsDeploy(t *testing.T) {
	for _, event := range []string{"release", "schedule", "workflow_run", "repository_dispatch"} {
		h := newHarness(t)
		githubJob(h)
		h.env["GITHUB_EVENT_NAME"] = event
		h.dockerfile("FROM scratch\n")
		h.expect(h.run("deploy", "--github"), 0)
		if len(h.api.deployPosts) != 1 || strings.Contains(h.err.String(), ci.SkipMessage) {
			t.Fatalf("%s: deploys %d\n%s", event, len(h.api.deployPosts), h.err.String())
		}
	}
}

// A GitHub run that names no event is refused (exit 2), neither skipped as
// a success nor deployed.
func TestDeploy_GitHubWithoutAnEventIsRefused(t *testing.T) {
	h := newHarness(t)
	gh := githubJob(h)
	h.env["GITHUB_EVENT_NAME"] = ""
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 2)
	if !strings.Contains(h.err.String(), "GITHUB_EVENT_NAME is empty, so kx can't tell whether this run is for a pull request, and it doesn't deploy.") || len(h.api.deployPosts) != 0 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if st := gh.got(); len(st) != 1 || st[0]["state"] != "failure" {
		t.Fatalf("commit statuses %v", gh.got())
	}
}

func TestDeploy_WorkflowDispatchDeploys(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	h.env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 0)
}

func TestDeploy_GitLabMergeRequestSkips(t *testing.T) {
	for _, env := range []map[string]string{
		{"CI_PIPELINE_SOURCE": "merge_request_event"},
		{"CI_PIPELINE_SOURCE": "external_pull_request_event"},
		{"CI_PIPELINE_SOURCE": "push", "CI_MERGE_REQUEST_IID": "12"},
	} {
		h := newHarness(t)
		h.env["GITLAB_CI"], h.env["CI"] = "true", "true"
		for k, v := range env {
			h.env[k] = v
		}
		h.expect(h.run("deploy"), 0)
		if h.err.String() != ci.SkipMessage+"\n" || len(h.api.got()) != 0 {
			t.Fatalf("%v: stderr %q, requests %v", env, h.err.String(), h.api.got())
		}
	}
}

func TestDeploy_BitbucketPullRequestSkips(t *testing.T) {
	h := newHarness(t)
	h.env["BITBUCKET_BUILD_NUMBER"], h.env["BITBUCKET_PR_ID"], h.env["CI"] = "41", "7", "true"
	h.expect(h.run("deploy", "--json"), 0)
	if h.err.String() != "" || !strings.Contains(h.out.String(), `"skipped": true`) || len(h.api.got()) != 0 {
		t.Fatalf("stderr %q stdout %q", h.err.String(), h.out.String())
	}
}

func TestDeploy_IdempotencyKeyPerPlatform(t *testing.T) {
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	tests := []struct {
		name          string
		env           map[string]string
		retry         map[string]string // what changes on a re-run or retried job
		deterministic bool
	}{
		{"GitHub", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_SHA": ghSHA, "GITHUB_REPOSITORY": "acme/shop-api", "GITHUB_RUN_ID": "9001", "GITHUB_RUN_ATTEMPT": "1"},
			map[string]string{"GITHUB_RUN_ATTEMPT": "2"}, true},
		{"GitLab", map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_COMMIT_SHA": ghSHA, "CI_PROJECT_PATH": "acme/shop-api", "CI_PIPELINE_ID": "55", "CI_JOB_ID": "77"},
			map[string]string{"CI_JOB_ID": "78"}, true},
		{"Bitbucket", map[string]string{"BITBUCKET_BUILD_NUMBER": "41", "BITBUCKET_COMMIT": ghSHA, "BITBUCKET_REPO_FULL_NAME": "acme/shop-api", "BITBUCKET_STEP_UUID": "{9f1c}"}, nil, false},
		{"generic CI", map[string]string{"CI": "true"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			for k, v := range tt.env {
				h.env[k] = v
			}
			h.env["KX_TOKEN"] = tokDeploy
			h.dockerfile("FROM scratch\n")
			h.api.failPosts = 1 // kx's own retry must resend the same key
			h.expect(h.run("deploy"), 0)
			h.expect(h.run("deploy"), 0) // the same attempt again (or, off GitHub/GitLab, any new run)
			for k, v := range tt.retry {
				h.env[k] = v
			}
			h.expect(h.run("deploy"), 0)
			p := h.api.deployPosts
			if len(p) != 4 {
				t.Fatalf("%d deploy requests, want 4", len(p))
			}
			if p[0].Key == "" || p[0].Key != p[1].Key {
				t.Fatalf("an HTTP retry changed the key: %q then %q", p[0].Key, p[1].Key)
			}
			if tt.deterministic {
				want := ci.Detect(func(k string) string { return h.env[k] }).IdempotencyKey("shop-api")
				if p[1].Key != p[2].Key {
					t.Fatal("the same attempt got a different key, so a retried step would deploy twice")
				}
				if p[3].Key == p[2].Key || p[3].Key != want {
					t.Fatalf("re-run key %q, want a new deterministic key %q", p[3].Key, want)
				}
				return
			}
			for i, q := range p {
				if !uuidV4.MatchString(q.Key) {
					t.Fatalf("request %d key %q is not a random (v4) UUID", i, q.Key)
				}
			}
			if p[2].Key == p[1].Key || p[3].Key == p[2].Key {
				t.Fatal("two kx runs shared a key, so a re-run would get the earlier deploy back")
			}
		})
	}
}

func TestDeploy_GitLabPushDeploys(t *testing.T) {
	h := newHarness(t)
	for k, v := range map[string]string{"GITLAB_CI": "true", "CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_COMMIT_SHA": ghSHA,
		"CI_PROJECT_PATH": "acme/shop-api", "CI_PIPELINE_ID": "55", "CI_JOB_ID": "77", "KX_TOKEN": tokDeploy} {
		h.env[k] = v
	}
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	want := ci.Detect(func(k string) string { return h.env[k] }).IdempotencyKey("shop-api")
	if h.api.deployPosts[0].Key != want || !strings.Contains(h.err.String(), "shop-api:3f9c2a1b7d4e") {
		t.Fatalf("key %q want %q\n%s", h.api.deployPosts[0].Key, want, h.err.String())
	}
	if strings.Contains(h.err.String(), "::") {
		t.Fatal("printed GitHub workflow commands outside GitHub mode")
	}
}

// billingBlocked answers the requests the platform's billing gate refuses
// (a new deployment, a new app) with the 402 it sends for an unpaid org.
func billingBlocked(r *http.Request, _ string) (int, string, bool) {
	gated := r.Method == http.MethodPost && (r.URL.Path == "/api/v1/apps" || strings.HasSuffix(r.URL.Path, "/deployments"))
	return 402, `{"error":"billing_unpaid","message":"Payment for this organization is overdue. New deployments and new resources are paused until the balance is paid. Your running apps are not affected."}`, gated
}

// A deploy refused for billing exits 5 with the platform's message and the
// dashboard's Billing page; in GitHub mode the status and annotation say
// the same, and the summary says nothing was deployed.
func TestDeploy_BillingBlockExplained(t *testing.T) {
	const (
		what = "Payment for this organization is overdue. New deployments and new resources are paused until the balance is paid. Your running apps are not affected."
		fix  = "Pay the overdue balance on acme-corp's Billing page in the dashboard: platform.kubernetix.ai/settings?tab=billing"
	)
	t.Run("laptop", func(t *testing.T) {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.link("shop-api", appShopID, "acme-corp")
		h.dockerfile("FROM scratch\n")
		h.api.override = billingBlocked
		h.expect(h.run("deploy"), 5)
		if !strings.HasSuffix(h.err.String(), what+"\n"+fix+"\n") {
			t.Fatalf("stderr:\n%s", h.err.String())
		}
	})
	t.Run("json", func(t *testing.T) {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.link("shop-api", appShopID, "acme-corp")
		h.dockerfile("FROM scratch\n")
		h.api.override = billingBlocked
		h.expect(h.run("deploy", "--json"), 5)
		var out struct {
			Error struct{ Code, Message, Fix string } `json:"error"`
		}
		if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
			t.Fatalf("stdout is not one JSON value: %v\n%s", err, h.out.String())
		}
		if out.Error.Code != "billing_unpaid" || out.Error.Message != what || out.Error.Fix != fix {
			t.Fatalf("json = %+v", out)
		}
	})
	t.Run("github", func(t *testing.T) {
		h := newHarness(t)
		gh := githubJob(h)
		h.dockerfile("FROM scratch\n")
		h.api.override = billingBlocked
		h.expect(h.run("deploy", "--github"), 5)
		if !strings.Contains(h.err.String(), "::error title=Kubernetix deploy::"+what+"%0A"+fix+"\n") {
			t.Fatalf("stderr lacks the annotation:\n%s", h.err.String())
		}
		st := gh.got()
		if last := st[len(st)-1]; last["state"] != "failure" || !strings.HasPrefix(last["description"], "Payment for this organization is overdue.") {
			t.Fatalf("final status = %v", last)
		}
		sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"])
		if !strings.Contains(string(sum), "Nothing was deployed") {
			t.Fatalf("summary:\n%s", sum)
		}
	})
	t.Run("new app", func(t *testing.T) {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.link("brand-new", "", "acme-corp") // kx link brand-new: an app that doesn't exist yet
		h.dockerfile("FROM scratch\n")
		h.api.override = billingBlocked
		h.expect(h.run("deploy"), 5)
		if len(h.api.newApps) != 0 {
			t.Fatalf("created %v", h.api.newApps)
		}
		if !slices.Contains(h.api.requests, "POST /api/v1/apps") {
			t.Fatalf("requests: %v", h.api.requests)
		}
		if !strings.HasSuffix(h.err.String(), what+"\n"+fix+"\n") {
			t.Fatalf("stderr:\n%s", h.err.String())
		}
	})
}

// A version the plan had no room for exits 5 (a plan limit) with the
// platform's message; in GitHub mode the status says the same.
func TestDeploy_NoRoomInThePlanExits5(t *testing.T) {
	const msg = "Your plan's memory is full. The new version was not started; the previous one still runs. Pick a smaller size, remove an app or database, or upgrade at platform.kubernetix.ai/plans."
	h := newHarness(t)
	gh := githubJob(h)
	h.dockerfile("FROM scratch\n")
	h.api.statuses = []string{"pending", "deploying", "failed"}
	h.api.failReason, h.api.failCode = msg, "quota_blocked"
	h.expect(h.run("deploy", "--github"), 5)
	want := "::error title=Kubernetix deploy::shop-api v16 didn't go live: " + msg + "%0AFree up room in your plan or upgrade at platform.kubernetix.ai/plans, then try again.\n"
	if !strings.Contains(h.err.String(), want) {
		t.Fatalf("stderr lacks the annotation:\n%s", h.err.String())
	}
	st := gh.got()
	if last := st[len(st)-1]; last["state"] != "failure" || !strings.HasPrefix(last["description"], "shop-api v16 didn't go live: Your plan's memory is full.") {
		t.Fatalf("final status = %v", last)
	}
}

// kxJSON reads the folder's .kx.json, or "" if there is none.
func (h *harness) kxJSON() string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, ".kx.json"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(b)), "")
}

func TestDeploy_AppFlagNewName_AsksThenCreates(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = h.api.apps[:1] // shop-api only
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy", "--app", "web"), 0)
	if !strings.HasPrefix(h.err.String(), "There's no app called web in acme-corp. Create it? [Y/n] Saved in .kx.json. Next time, just run kx deploy.\n\n✓ Checked ") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.newApps) != 1 || h.api.newApps[0]["name"] != "web" {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}
	if got := h.kxJSON(); got != `{"app":"web","app_id":"7a7a7a7a-0000-4000-8000-000000000001","org":"acme-corp"}` {
		t.Fatalf(".kx.json = %s", got)
	}
}

func TestDeploy_AppFlagNewName_Declined(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "n\n"
	h.expect(h.run("deploy", "--app", "admin"), 2)
	if !strings.HasSuffix(h.err.String(), "Nothing was deployed: there's no app called admin in acme-corp.\nSee your apps: kx apps list\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.newApps) != 0 || len(h.docker.builds) != 0 || h.kxJSON() != "" {
		t.Fatalf("created %v, built %d, .kx.json %q", h.api.newApps, len(h.docker.builds), h.kxJSON())
	}
}

func TestDeploy_AppFlagNewName_NoTerminal_SaysHow(t *testing.T) {
	for _, flag := range []bool{true, false} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		h.dockerfile("FROM scratch\nEXPOSE 8080\n")
		args := []string{"deploy", "--app", "admin"}
		if !flag {
			args = args[:1]
			h.env["KX_APP"] = "admin"
		}
		h.expect(h.run(args...), 2)
		if !strings.HasSuffix(h.err.String(), "There's no app called admin in acme-corp.\nCreate it: kx link admin, then kx deploy\n") || len(h.api.newApps) != 0 {
			t.Fatalf("stderr:\n%s", h.err.String())
		}
	}
}

func TestDeploy_AppFlagNewName_InCI_Fails(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"], h.env["GITHUB_ACTIONS"], h.env["GITHUB_EVENT_NAME"] = tokCorp, "true", "push"
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy", "--app", "admin"), 4)
	if !strings.HasSuffix(h.err.String(), "There's no app called admin in acme-corp.\nSee your apps: kx apps list\n") || len(h.api.newApps) != 0 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

// Only tokens that can create apps are offered to create one.
func TestDeploy_AppFlagNewName_TokenThatCantCreate(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"] = tokRead
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy", "--app", "admin", "--image", "registry.kubernetix.ai/acme-corp/admin@"+testDigest), 3)
	if strings.Contains(h.err.String(), "Create it?") || len(h.api.newApps) != 0 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_AppFlagExistingApp_SavesKxJSON(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.inFolder("checkout")
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.expect(h.run("deploy", "--app", "shop-api"), 0)
	if got := h.kxJSON(); got != `{"app":"shop-api","app_id":"`+appShopID+`","org":"acme-corp"}` {
		t.Fatalf(".kx.json = %s", got)
	}
	if !strings.HasPrefix(h.err.String(), "Saved in .kx.json. Next time, just run kx deploy.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	// A folder that already has one keeps it.
	h.expect(h.run("deploy", "--app", "web"), 0)
	if got := h.kxJSON(); !strings.Contains(got, `"app":"shop-api"`) || strings.Contains(h.err.String(), "Saved in .kx.json") {
		t.Fatalf(".kx.json = %s, stderr:\n%s", got, h.err.String())
	}
}

// An image deploy names no project folder, so kx doesn't link this one.
func TestDeploy_AppFlagWithImage_WritesNoKxJSON(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("deploy", "--app", "shop-api", "--image", "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest), 0)
	if h.kxJSON() != "" || strings.Contains(h.err.String(), "Saved in .kx.json") {
		t.Fatalf(".kx.json = %q, stderr:\n%s", h.kxJSON(), h.err.String())
	}
}

// CI checkouts are thrown away: no .kx.json there either.
func TestDeploy_AppFlagInCI_WritesNoKxJSON(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"], h.env["CI"] = tokCorp, "true"
	h.dockerfile("FROM scratch\nEXPOSE 8080\n")
	h.expect(h.run("deploy", "--app", "shop-api"), 0)
	if h.kxJSON() != "" {
		t.Fatalf(".kx.json = %q", h.kxJSON())
	}
}

func TestDeploy_NoExpose_AsksPortOnce(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.inFolder("my-shop")
	h.dockerfile("FROM scratch\n")
	h.tty, h.stdin = true, "3000\n"
	h.expect(h.run("deploy"), 0)
	if !strings.Contains(h.err.String(), "Which port does your app listen on? [8080] ✓ Checked   Dockerfile, port 3000\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.newApps) != 1 || h.api.newApps[0]["port"] != float64(3000) {
		t.Fatalf("POST /apps = %v", h.api.newApps)
	}
	h.stdin = ""
	h.expect(h.run("deploy"), 0)
	if strings.Contains(h.err.String(), "Which port") {
		t.Fatalf("asked again:\n%s", h.err.String())
	}
}

func TestDeploy_NoExpose_NoTerminal_KeepsWarning(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.apps = nil
	h.inFolder("my-shop")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	want := "! Your Dockerfile has no EXPOSE line, so kx assumes your app listens on port 8080.\n" +
		"  Kubernetix checks that it answers on / there; if it listens on another port, add EXPOSE <port> to your Dockerfile.\n"
	if !strings.Contains(h.err.String(), want) || h.api.newApps[0]["port"] != float64(8080) {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

const expose80Lines = "! Your Dockerfile exposes port 80. Apps on Kubernetix don't run as root, so they may not be able to listen on ports below 1024.\n" +
	"  Make your app listen on 8080 and change the line to EXPOSE 8080.\n"

func TestDeploy_Expose80_AsksAndStopsByDefault(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM nginx\nEXPOSE 80\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 2)
	if !strings.Contains(h.err.String(), expose80Lines+"Deploy anyway? [y/N] Nothing was deployed, because your app may not be able to listen on port 80.\nChange the line to EXPOSE 8080, then run kx deploy again.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.docker.builds) != 0 || len(h.api.deployPosts) != 0 {
		t.Fatalf("built %d, deployed %d", len(h.docker.builds), len(h.api.deployPosts))
	}
}

func TestDeploy_Expose80_Yes_Deploys(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM nginx\nEXPOSE 80\n")
	h.tty, h.stdin = true, "y\n"
	h.expect(h.run("deploy"), 0)
	if len(h.api.deployPosts) != 1 {
		t.Fatalf("deployed %d", len(h.api.deployPosts))
	}
}

func TestDeploy_Expose80_InCI_WarnsAndDeploys(t *testing.T) {
	h := newHarness(t)
	h.env["KX_TOKEN"], h.env["CI"] = tokCorp, "true"
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM nginx\nEXPOSE 80\n")
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("deploy"), 0)
	if !strings.Contains(h.err.String(), expose80Lines) || strings.Contains(h.err.String(), "Deploy anyway?") || len(h.api.deployPosts) != 1 {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}
