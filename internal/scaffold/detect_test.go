package scaffold

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the want.Dockerfile files under testdata/")

// renderCases are the fixtures under testdata/: each is a tiny app that
// works, and want.Dockerfile is what kx writes for it. CI builds every
// want.Dockerfile and checks that the container listens on port 8080.
var renderCases = []string{
	"node-npm-start", "node-yarn-main", "node-pnpm-build", "node-nvmrc-20",
	"python-django", "python-fastapi", "python-flask-adds-gunicorn", "python-procfile",
	"go-root", "go-one-cmd", "go-newer-toolchain",
	"static",
}

func TestRender(t *testing.T) {
	for _, name := range renderCases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", name)
			found, err := Detect(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 1 || found[0].Problem != "" {
				t.Fatalf("Detect() = %+v, want one app kx can write a Dockerfile for", found)
			}
			got, ignore, err := Render(found[0])
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, filepath.Join(dir, "want.Dockerfile"), got)
			// CI builds each test app with this as its .dockerignore.
			checkGolden(t, filepath.Join(dir, "want.dockerignore"), ignore)
		})
	}
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A checkout with CRLF line endings must not fail on Windows.
	if string(got) != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Fatalf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// Every fixture has a golden test, so the CI job that builds them all
// never builds a Dockerfile kx no longer writes.
func TestRender_EveryFixtureIsTested(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && !slices.Contains(renderCases, e.Name()) {
			t.Errorf("testdata/%s has no entry in renderCases", e.Name())
		}
	}
}

// Every template says why it sets a numeric user, and listens on 8080.
func TestRender_NumericUserAndPort(t *testing.T) {
	for _, name := range renderCases {
		found, err := Detect(filepath.Join("testdata", name))
		if err != nil || len(found) != 1 {
			t.Fatalf("%s: %v %v", name, found, err)
		}
		df, _, err := Render(found[0])
		if err != nil {
			t.Fatal(err)
		}
		text := string(df)
		if !strings.Contains(text, "\nEXPOSE 8080\n") {
			t.Errorf("%s: no EXPOSE 8080", name)
		}
		if !strings.Contains(text, "# Some hosts refuse containers that run as root, and can only check a numeric user.\n") {
			t.Errorf("%s: no comment on the user", name)
		}
		lines := strings.Split(strings.TrimSpace(text), "\n")
		user := ""
		for _, l := range lines {
			if strings.HasPrefix(l, "USER ") {
				user = strings.TrimPrefix(l, "USER ")
			}
		}
		if user == "" || strings.Trim(user, "0123456789:") != "" {
			t.Errorf("%s: USER %q is not numeric", name, user)
		}
	}
}

// Render itself refuses a value that spans lines, whatever made it.
func TestRender_RefusesAMultiLineCommand(t *testing.T) {
	d := Detection{Stack: StackNode, Label: "a Node app", Image: "node:22-alpine", Install: []string{"npm ci"}, Start: []string{"node", "a.js\nRUN rm -rf /"}}
	if df, _, err := Render(d); err == nil {
		t.Fatalf("rendered:\n%s", df)
	}
}

func TestRender_RefusesAProblem(t *testing.T) {
	if _, _, err := Render(Detection{Stack: StackNode, Label: "a Node app", Problem: "x", Fix: "y"}); err == nil {
		t.Fatal("rendered a detection with a problem")
	}
}

func TestDetect_Nothing(t *testing.T) {
	found, err := Detect(t.TempDir())
	if err != nil || len(found) != 0 {
		t.Fatalf("Detect(empty) = %+v, %v", found, err)
	}
}

func TestDetect_NodeWithoutStart_Problem(t *testing.T) {
	dir := files(t, map[string]string{"package.json": `{"name":"shop","scripts":{"test":"node --test"}}`})
	d := only(t, dir)
	if d.Stack != StackNode || d.Label != "a Node app" {
		t.Fatalf("Detect() = %+v", d)
	}
	wantProblem(t, d,
		"This looks like a Node app, but package.json has no start script, so kx can't tell how to run it.",
		`Add a "start" script to package.json, or add a Dockerfile.`)
}

func TestDetect_StartScriptWithNewline_Problem(t *testing.T) {
	for _, pkg := range []string{
		`{"scripts":{"start":"node server.js\nrm -rf /tmp/x"}}`,
		`{"scripts":{"start":"node server.js\r"}}`,
		`{"main":"server.js\nRUN echo hi"}`,
	} {
		d := only(t, files(t, map[string]string{"package.json": pkg, "server.js": ""}))
		wantProblem(t, d,
			"This looks like a Node app, but its start command in package.json is more than one line, so kx can't tell how to run it.",
			"Put it on one line, or add a Dockerfile.")
	}
}

func TestDetect_NodeInvalidPackageJSON_Problem(t *testing.T) {
	d := only(t, files(t, map[string]string{"package.json": `{"name": "shop",`, "server.js": ""}))
	wantProblem(t, d,
		"This looks like a Node app, but package.json isn't valid JSON, so kx can't tell how to run it.",
		"Fix package.json, or add a Dockerfile.")
}

// A field of an unexpected type is ignored, not called invalid JSON.
func TestDetect_NodeOddFieldTypes(t *testing.T) {
	d := only(t, files(t, map[string]string{"package.json": `{"main":["a.js"],"scripts":"none","engines":{"node":20}}`, "index.js": ""}))
	if d.Problem != "" || !slices.Equal(d.Start, []string{"node", "index.js"}) || d.Image != "node:22-alpine" {
		t.Fatalf("Detect() = %+v", d)
	}
}

func TestDetect_NodeStart(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		start []string
	}{
		{"start script wins", map[string]string{"package.json": `{"main":"lib.js","scripts":{"start":"node server.js"}}`, "server.js": ""}, []string{"npm", "start"}},
		{"main", map[string]string{"package.json": `{"main":"dist/main.js"}`}, []string{"node", "dist/main.js"}},
		{"server.js first", map[string]string{"package.json": `{}`, "index.js": "", "server.js": ""}, []string{"node", "server.js"}},
		{"index.js", map[string]string{"package.json": `{}`, "index.js": "", "app.js": ""}, []string{"node", "index.js"}},
		{"app.js", map[string]string{"package.json": `{}`, "app.js": ""}, []string{"node", "app.js"}},
		{"a byte order mark", map[string]string{"package.json": "\ufeff{\"scripts\":{\"start\":\"node a.js\"}}"}, []string{"npm", "start"}},
	}
	for _, tt := range tests {
		d := only(t, files(t, tt.files))
		if d.Problem != "" || !slices.Equal(d.Start, tt.start) {
			t.Errorf("%s: Start = %q (problem %q), want %q", tt.name, d.Start, d.Problem, tt.start)
		}
	}
}

func TestDetect_NodeVersion(t *testing.T) {
	tests := []struct {
		nvmrc, engines string
		image          string
	}{
		{"", "", "node:22-alpine"},
		{"20", "", "node:20-alpine"},
		{"v18.19.0\n", "", "node:18-alpine"},
		{"lts/iron", "", "node:22-alpine"},
		{"", "^20.11.0", "node:20-alpine"},
		{"", "20.x", "node:20-alpine"},
		{"", "~24.1", "node:24-alpine"},
		{"", ">=18", "node:22-alpine"},
		{"", "18 || 20", "node:22-alpine"},
		{"24", "20.x", "node:24-alpine"},
	}
	for _, tt := range tests {
		m := map[string]string{"package.json": `{"scripts":{"start":"node a.js"},"engines":{"node":"` + tt.engines + `"}}`}
		if tt.nvmrc != "" {
			m[".nvmrc"] = tt.nvmrc
		}
		d := only(t, files(t, m))
		if d.Image != tt.image {
			t.Errorf(".nvmrc %q, engines.node %q: image %s, want %s", tt.nvmrc, tt.engines, d.Image, tt.image)
		}
	}
}

// Node 25 and later don't include corepack, so kx installs it first.
func TestDetect_NodeWithoutCorepack_InstallsIt(t *testing.T) {
	d := only(t, files(t, map[string]string{".nvmrc": "26", "package.json": `{"scripts":{"start":"node a.js"}}`, "pnpm-lock.yaml": ""}))
	want := []string{"npm install -g corepack && corepack enable && pnpm install --frozen-lockfile"}
	if !slices.Equal(d.Install, want) {
		t.Fatalf("Install = %q, want %q", d.Install, want)
	}
}

// The yarn or pnpm corepack downloads goes where any user can run it,
// not into root's home folder; npm alone needs no corepack.
func TestDetect_NodeCorepackHome(t *testing.T) {
	want := []string{"COREPACK_HOME=/usr/local/share/corepack", "COREPACK_ENABLE_DOWNLOAD_PROMPT=0"}
	for _, lock := range []string{"yarn.lock", "pnpm-lock.yaml"} {
		d := only(t, files(t, map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`, lock: ""}))
		if !slices.Equal(d.Env, want) {
			t.Errorf("%s: Env = %q, want %q", lock, d.Env, want)
		}
	}
	if d := only(t, files(t, map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`, "package-lock.json": "{}"})); d.Env != nil {
		t.Errorf("npm: Env = %q", d.Env)
	}
}

func TestDetect_NodeYarnrc_CopiesEverythingFirst(t *testing.T) {
	d := only(t, files(t, map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`, "yarn.lock": "", ".yarnrc.yml": "nodeLinker: node-modules\n"}))
	if d.CopyFirst != nil {
		t.Fatalf("CopyFirst = %q", d.CopyFirst)
	}
}

// A package.json that isn't a file isn't a Node app.
func TestDetect_PackageJSONNotAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "package.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if found, err := Detect(dir); err != nil || len(found) != 0 {
		t.Fatalf("Detect() = %+v, %v", found, err)
	}
}

const (
	npmTokenWhat = "Your .npmrc holds a registry token, and the Dockerfile would copy it into your image."
	npmTokenFix  = "Remove the token if your packages are public; if they're private, build and push the image yourself, then run: kx deploy --image <image>"
)

// A token file below the top of the folder (a workspace package's) is
// checked too, whenever the build would copy it.
func TestDetect_TokenInASubfolder(t *testing.T) {
	const what = "Your packages/api/.npmrc holds a registry token, and the Dockerfile would copy it into your image."
	node := map[string]string{"package.json": `{"workspaces":["packages/*"],"scripts":{"start":"node a.js"}}`, "package-lock.json": "{}",
		"packages/api/.npmrc": "//registry.npmjs.org/:_authToken=test-only-not-a-token\n"}
	// With no .dockerignore, the one kx writes keeps it out.
	d := only(t, files(t, node))
	if d.Problem != "" {
		t.Fatalf("no .dockerignore: Problem %q", d.Problem)
	}
	_, ignore, _ := Render(d)
	rules := parseDockerignore(ignore)
	if !rules.excludes("packages/api/.npmrc") || !rules.excludes("packages/api/.yarnrc.yml") || rules.excludes(".npmrc") || rules.excludes(".yarnrc.yml") {
		t.Fatalf("the Node .dockerignore should keep nested .npmrc and .yarnrc.yml out, and the top ones in:\n%s", ignore)
	}
	// An existing .dockerignore that lets it through.
	node[".dockerignore"] = "node_modules\n"
	wantProblem(t, only(t, files(t, node)), what, npmTokenFix)
	node[".dockerignore"] = "node_modules\n**/.npmrc\n"
	if d := only(t, files(t, node)); d.Problem != "" {
		t.Fatalf("kept out: Problem %q", d.Problem)
	}
}

// For apps other than Node, a .dockerignore Docker can't read lets a
// token file through.
func TestDetect_TokenWithBrokenDockerignore(t *testing.T) {
	dir := files(t, map[string]string{".npmrc": "//registry.npmjs.org/:_authToken=test-only-not-a-token\n", "requirements.txt": "", "main.py": ""})
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, ".dockerignore")); err != nil {
		t.Skipf("can't make a symbolic link here: %v", err)
	}
	wantProblem(t, only(t, dir), "Your .npmrc holds a registry token, and the Dockerfile would copy it into your image.", npmTokenFix)
}

// The Go template works with Docker's classic builder too, which doesn't
// set BUILDPLATFORM, TARGETOS or TARGETARCH: each has a default.
func TestRender_GoDefaultsForTheClassicBuilder(t *testing.T) {
	d := only(t, files(t, map[string]string{"go.mod": "module x\n\ngo 1.24\n", "main.go": "package main\n"}))
	df, _, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(df), "FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.24-alpine AS build\n") {
		t.Fatalf("no defaulted platform:\n%s", df)
	}
	for _, v := range []string{"BUILDPLATFORM", "TARGETOS", "TARGETARCH", "TARGETPLATFORM"} {
		text := strings.ReplaceAll(string(df), "${"+v+":-", "")
		text = strings.ReplaceAll(text, "ARG "+v+"\n", "")
		if strings.Contains(text, "$"+v) || strings.Contains(text, "${"+v+"}") {
			t.Errorf("%s is used without a default:\n%s", v, df)
		}
	}
}

// A registry token written into .yarnrc.yml is stopped the same way.
func TestDetect_YarnrcToken_Problem(t *testing.T) {
	const what = "Your .yarnrc.yml holds a registry token, and the Dockerfile would copy it into your image."
	node := map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`, "yarn.lock": ""}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range node {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, yarnrc := range []string{
		"nodeLinker: node-modules\nnpmAuthToken: test-only-not-a-token\n",
		"npmScopes:\n  acme:\n    npmRegistryServer: \"https://npm.example.test\"\n    npmAuthToken: \"test-only\"  # private packages\r\n",
		"npmRegistries:\n  \"//npm.example.test\":\n    npmAuthIdent: 'dGVzdDp0ZXN0'\n",
		"npmScopes:\n  acme:\n    \"npmAuthToken\": test-only\n",
		"npmScopes: {acme: {npmAuthToken: test-only, npmRegistryServer: \"https://npm.example.test\"}}\n",
		"\ufeffnpmAuthToken: test-only\n",
		// The second key on the line holds the token.
		"npmScopes: {acme: {npmAuthIdent: \"${NPM_USER}:${NPM_PASS}\", npmAuthToken: test-only}}\n",
	} {
		wantProblem(t, only(t, files(t, with(map[string]string{".yarnrc.yml": yarnrc}))), what, npmTokenFix)
	}
	for _, yarnrc := range []string{
		"npmAuthToken: \"${NPM_TOKEN}\"\n",
		"npmAuthToken: ${NPM_TOKEN:-}\n",
		"npmAuthToken: ${NPM_TOKEN} # set in the environment\n",
		"npmAuthIdent: \"${NPM_USER}:${NPM_PASS}\"\n",
		"npmAuthToken: # fill in\n",
		// In a flow mapping the value ends at a comma or a closing brace.
		"npmScopes: {acme: {npmAuthToken: ${NPM_TOKEN}, npmRegistryServer: \"https://npm.example.test\"}}\n",
		"npmScopes: {acme: {npmAuthToken: ${NPM_TOKEN}}}\n",
		"# npmAuthToken: test-only-not-a-token\nnodeLinker: node-modules\n",
		"npmAuthToken: ''\n",
	} {
		if d := only(t, files(t, with(map[string]string{".yarnrc.yml": yarnrc}))); d.Problem != "" {
			t.Errorf(".yarnrc.yml %q: Problem %q", yarnrc, d.Problem)
		}
	}
	if d := only(t, files(t, with(map[string]string{".yarnrc.yml": "npmAuthToken: test-only\n", ".dockerignore": ".yarnrc.yml\n"}))); d.Problem != "" {
		t.Fatalf("kept out by .dockerignore: Problem %q", d.Problem)
	}
	// Other apps: the .dockerignore kx writes keeps it out.
	py := map[string]string{".yarnrc.yml": "npmAuthToken: test-only\n", "requirements.txt": "", "main.py": ""}
	d := only(t, files(t, py))
	if d.Problem != "" {
		t.Fatalf("Python app, no .dockerignore: Problem %q", d.Problem)
	}
	if _, ignore, _ := Render(d); !hasLine(string(ignore), ".yarnrc.yml") || !hasLine(string(ignore), "**/.yarnrc.yml") {
		t.Fatalf(".dockerignore for a Python app doesn't keep .yarnrc.yml out:\n%s", ignore)
	}
	py[".dockerignore"] = "dist\n"
	wantProblem(t, only(t, files(t, py)), what, npmTokenFix)
}

// A registry token written into .npmrc must never be copied into an image.
func TestDetect_NpmrcToken_Problem(t *testing.T) {
	node := map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`, "package-lock.json": "{}"}
	with := func(m map[string]string, extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, npmrc := range []string{
		"//registry.npmjs.org/:_authToken=test-only-not-a-token\n",
		"registry=https://npm.example.test/\n//npm.example.test/:_auth = dGVzdDp0ZXN0\n",
		"//npm.example.test/:_password=\"test-only\"\r\n",
		"\ufeff_auth=dGVzdDp0ZXN0\n",
		// In quotes, # and ; are part of the value, not a comment.
		"//npm.example.test/:_password=\"#test-only\"\n",
		"//npm.example.test/:_authToken=';test-only'\n",
	} {
		wantProblem(t, only(t, files(t, with(node, map[string]string{".npmrc": npmrc}))), npmTokenWhat, npmTokenFix)
	}
	for _, npmrc := range []string{
		"//registry.npmjs.org/:_authToken=${NPM_TOKEN}\n",
		"//npm.example.test/:_auth=\"${NPM_AUTH}\"\n",
		"# //registry.npmjs.org/:_authToken=test-only-not-a-token\n; _password=test-only\nlegacy-peer-deps=true\n",
		"//registry.npmjs.org/:_authToken=\n",
		"//registry.npmjs.org/:_authToken=${NPM_TOKEN} # set in CI\n",
		"//registry.npmjs.org/:_authToken=${NPM_TOKEN} ; set in CI\n",
		"//registry.npmjs.org/:_authToken= # fill in\n",
		"//npm.example.test/:_auth=\"${NPM_USER}:${NPM_PASS}\"\n",
	} {
		if d := only(t, files(t, with(node, map[string]string{".npmrc": npmrc}))); d.Problem != "" {
			t.Errorf(".npmrc %q: Problem %q", npmrc, d.Problem)
		}
	}
	token := map[string]string{".npmrc": "//registry.npmjs.org/:_authToken=test-only-not-a-token\n"}
	// A Node app whose .dockerignore keeps .npmrc out: not copied, so no
	// problem, and the Dockerfile doesn't name it.
	d := only(t, files(t, with(with(node, token), map[string]string{".dockerignore": ".npmrc\n"})))
	if d.Problem != "" || slices.Contains(d.CopyFirst, ".npmrc") {
		t.Fatalf("Problem %q, CopyFirst %q", d.Problem, d.CopyFirst)
	}
	// Other apps don't need .npmrc: the .dockerignore kx writes keeps it
	// out, but an existing one that doesn't is a problem.
	py := with(token, map[string]string{"requirements.txt": "", "main.py": ""})
	d = only(t, files(t, py))
	if d.Problem != "" {
		t.Fatalf("Python app, no .dockerignore: Problem %q", d.Problem)
	}
	if _, ignore, _ := Render(d); !hasLine(string(ignore), ".npmrc") || !hasLine(string(ignore), "**/.npmrc") {
		t.Fatalf(".dockerignore for a Python app doesn't keep .npmrc out:\n%s", ignore)
	}
	wantProblem(t, only(t, files(t, with(py, map[string]string{".dockerignore": "dist\n"}))), npmTokenWhat, npmTokenFix)
	if d := only(t, files(t, with(py, map[string]string{".dockerignore": "**/.npmrc\n"}))); d.Problem != "" {
		t.Fatalf("Python app, .npmrc ignored: Problem %q", d.Problem)
	}
}

// A workspace needs the whole folder before installing.
func TestDetect_NodeWorkspace_CopiesEverythingFirst(t *testing.T) {
	for _, m := range []map[string]string{
		{"package.json": `{"workspaces":["packages/*"],"scripts":{"start":"node a.js"}}`, "package-lock.json": "{}"},
		{"package.json": `{"scripts":{"start":"node a.js"}}`, "pnpm-lock.yaml": "", "pnpm-workspace.yaml": "packages: ['a']"},
	} {
		d := only(t, files(t, m))
		if d.CopyFirst != nil {
			t.Errorf("CopyFirst = %q, want nothing copied before the install", d.CopyFirst)
		}
		df, _, err := Render(d)
		if err != nil || !strings.Contains(string(df), "\nCOPY . .\nRUN ") || strings.Count(string(df), "COPY ") != 1 {
			t.Errorf("Dockerfile doesn't copy the folder before installing:\n%s", df)
		}
	}
}

func TestDetect_NodeNpmrcCopiedWithTheLockfile(t *testing.T) {
	d := only(t, files(t, map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`, "package-lock.json": "{}", ".npmrc": "legacy-peer-deps=true\n"}))
	if want := []string{"package.json", "package-lock.json", ".npmrc"}; !slices.Equal(d.CopyFirst, want) {
		t.Fatalf("CopyFirst = %q, want %q", d.CopyFirst, want)
	}
}

func TestDetect_PythonStart(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		start   []string
		install []string
	}{
		{"Procfile with ${PORT:-5000}", map[string]string{"requirements.txt": "uvicorn\n", "Procfile": "release: python migrate.py\r\nweb: uvicorn api:app --host 0.0.0.0 --port ${PORT:-5000}\r\n"},
			[]string{"uvicorn", "api:app", "--host", "0.0.0.0", "--port", "8080"}, []string{"pip install --no-cache-dir -r requirements.txt"}},
		{"Procfile that needs a shell", map[string]string{"requirements.txt": "gunicorn\n", "Procfile": "web: gunicorn app:app --workers $WEB_CONCURRENCY --bind 0.0.0.0:$PORT"},
			[]string{"sh", "-c", "gunicorn app:app --workers $WEB_CONCURRENCY --bind 0.0.0.0:8080"}, []string{"pip install --no-cache-dir -r requirements.txt"}},
		{"Procfile gunicorn missing", map[string]string{"requirements.txt": "flask\n", "Procfile": "web: gunicorn app:app"},
			[]string{"gunicorn", "app:app"}, []string{"pip install --no-cache-dir -r requirements.txt", "pip install --no-cache-dir gunicorn"}},
		{"Django settings named in manage.py", map[string]string{"requirements.txt": "django\ngunicorn==23.0.0\n", "manage.py": "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'site_a.settings')\n", "site_a/wsgi.py": "", "site_b/wsgi.py": ""},
			[]string{"gunicorn", "site_a.wsgi", "--bind", "0.0.0.0:8080"}, []string{"pip install --no-cache-dir -r requirements.txt"}},
		{"FastAPI under another name", map[string]string{"requirements.txt": "FastAPI[standard]>=0.115\n", "app.py": "import fastapi\napi: fastapi.FastAPI = fastapi.FastAPI()\n"},
			[]string{"uvicorn", "app:api", "--host", "0.0.0.0", "--port", "8080"}, []string{"pip install --no-cache-dir -r requirements.txt", "pip install --no-cache-dir uvicorn"}},
		{"Flask in pyproject", map[string]string{"pyproject.toml": "[project]\nname = \"shop\"\ndependencies = [\"flask>=3\", \"gunicorn\"]\n", "main.py": "from flask import Flask\nserver = Flask(__name__)\n"},
			[]string{"gunicorn", "main:server", "--bind", "0.0.0.0:8080"}, []string{"pip install --no-cache-dir ."}},
		{"flask-cors is not flask", map[string]string{"requirements.txt": "flask-cors\n", "app.py": "x = Flask(__name__)\n"},
			[]string{"python", "app.py"}, []string{"pip install --no-cache-dir -r requirements.txt"}},
		{"main.py", map[string]string{"requirements.txt": "", "main.py": "print('hi')\n"},
			[]string{"python", "main.py"}, []string{"pip install --no-cache-dir -r requirements.txt"}},
	}
	for _, tt := range tests {
		d := only(t, files(t, tt.files))
		if d.Problem != "" || !slices.Equal(d.Start, tt.start) || !slices.Equal(d.Install, tt.install) {
			t.Errorf("%s:\n  Start   %q\n  Install %q\n  Problem %q\nwant\n  Start   %q\n  Install %q", tt.name, d.Start, d.Install, d.Problem, tt.start, tt.install)
		}
	}
}

func TestDetect_PythonVersion(t *testing.T) {
	for pv, image := range map[string]string{"": "python:3.12-slim", "3.11.4\n": "python:3.11-slim", "3.13": "python:3.13-slim", "pypy3.10": "python:3.12-slim"} {
		m := map[string]string{"requirements.txt": "", "main.py": ""}
		if pv != "" {
			m[".python-version"] = pv
		}
		if d := only(t, files(t, m)); d.Image != image {
			t.Errorf(".python-version %q: image %s, want %s", pv, d.Image, image)
		}
	}
}

// A requirements file that names other files, or the project itself,
// needs the whole folder before installing.
func TestDetect_PythonRequirementsNamingFiles_CopiesEverythingFirst(t *testing.T) {
	d := only(t, files(t, map[string]string{"requirements.txt": "-r base.txt\n", "main.py": ""}))
	if d.CopyFirst != nil {
		t.Fatalf("CopyFirst = %q", d.CopyFirst)
	}
}

func TestDetect_PythonWithoutStart_Problem(t *testing.T) {
	d := only(t, files(t, map[string]string{"requirements.txt": "requests\n", "worker.py": ""}))
	if d.Stack != StackPython || d.Label != "a Python app" {
		t.Fatalf("Detect() = %+v", d)
	}
	wantProblem(t, d,
		"This looks like a Python app, but it has no Procfile with a web: line, so kx can't tell how to run it.",
		"Add a Procfile with a web: line, or add a Dockerfile.")
}

func TestDetect_GoTwoCmds_Problem(t *testing.T) {
	d := only(t, files(t, map[string]string{
		"go.mod":              "module example.com/shop\n\ngo 1.24\n",
		"cmd/api/main.go":     "package main\n\nfunc main() {}\n",
		"cmd/worker/main.go":  "package main\n\nfunc main() {}\n",
		"cmd/tools/README.md": "",
	}))
	if d.Stack != StackGo || d.Label != "a Go app" {
		t.Fatalf("Detect() = %+v", d)
	}
	wantProblem(t, d,
		"This looks like a Go app, but it has more than one main package (cmd/api and cmd/worker), so kx can't tell how to run it.",
		"Keep one main package, or add a Dockerfile.")
}

func TestDetect_GoWithoutMain_Problem(t *testing.T) {
	d := only(t, files(t, map[string]string{
		"go.mod":                  "module example.com/lib\n",
		"lib.go":                  "package lib\n",
		"internal/x/x.go":         "package x\n",
		"vendor/a/main.go":        "package main\n",
		"testdata/app/main.go":    "package main\n",
		".hidden/tool/main.go":    "package main\n",
		"node_modules/b/main.go":  "package main\n",
		"examples/gen/gen.go":     "//go:build ignore\n\npackage main\n",
		"internal/x/main_test.go": "package main\n",
	}))
	wantProblem(t, d,
		"This looks like a Go app, but it has no main package, so kx can't tell how to run it.",
		"Add a main package, or add a Dockerfile.")
}

// A main package outside cmd/ is found too.
func TestDetect_GoMainOutsideCmd(t *testing.T) {
	d := only(t, files(t, map[string]string{"go.mod": "module x\n\ngo 1.24\n", "server/main.go": "package main\n", "store/store.go": "package store\n"}))
	if d.Problem != "" || !slices.Equal(d.Build, []string{goBuild("./server")}) {
		t.Fatalf("Build = %q (problem %q)", d.Build, d.Problem)
	}
	// cmd/ wins over the rest of the folder.
	d = only(t, files(t, map[string]string{"go.mod": "module x\n", "cmd/api/main.go": "package main\n", "tools/gen/main.go": "package main\n"}))
	if d.Problem != "" || !slices.Equal(d.Build, []string{goBuild("./cmd/api")}) {
		t.Fatalf("Build = %q (problem %q)", d.Build, d.Problem)
	}
	d = only(t, files(t, map[string]string{"go.mod": "module x\n", "server/main.go": "package main\n", "tools/gen/main.go": "package main\n"}))
	wantProblem(t, d,
		"This looks like a Go app, but it has more than one main package (server and tools/gen), so kx can't tell how to run it.",
		"Keep one main package, or add a Dockerfile.")
	m := map[string]string{"go.mod": "module x\n"}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		m["tools/"+n+"/main.go"] = "package main\n"
	}
	wantProblem(t, only(t, files(t, m)),
		"This looks like a Go app, but it has more than one main package (tools/a, tools/b, tools/c and 3 more), so kx can't tell how to run it.",
		"Keep one main package, or add a Dockerfile.")
}

// The Go build runs on the build machine's own platform and cross-compiles
// for the image's, and doesn't need git.
func goBuild(pkg string) string {
	return "CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -buildvcs=false -trimpath -o /out/app " + pkg
}

// With go.work, the whole folder is needed before downloading anything.
func TestDetect_GoWork_CopiesEverythingFirst(t *testing.T) {
	d := only(t, files(t, map[string]string{"go.mod": "module x\n", "go.sum": "", "go.work": "go 1.24\n\nuse .\n", "main.go": "package main\n"}))
	if d.CopyFirst != nil || d.Install != nil {
		t.Fatalf("CopyFirst %q, Install %q", d.CopyFirst, d.Install)
	}
}

func TestDetect_GoMainPackage(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		build string
	}{
		{"root wins over cmd", map[string]string{"main.go": "package main\n", "cmd/x/main.go": "package main\n"}, goBuild(".")},
		{"a generator at the root is not the app", map[string]string{"gen.go": "//go:build ignore\n\npackage main\n", "lib.go": "// Package shop.\npackage shop\n", "cmd/server/main.go": "package main\n"}, goBuild("./cmd/server")},
		{"a cmd folder kx can't name safely is skipped", map[string]string{"cmd/$(x)/main.go": "package main\n", "cmd/web/main.go": "package main\n"}, goBuild("./cmd/web")},
	}
	for _, tt := range tests {
		m := map[string]string{"go.mod": "module example.com/shop\n\ngo 1.24\n"}
		for k, v := range tt.files {
			m[k] = v
		}
		d := only(t, files(t, m))
		if d.Problem != "" || !slices.Equal(d.Build, []string{tt.build}) {
			t.Errorf("%s: Build = %q (problem %q), want %q", tt.name, d.Build, d.Problem, tt.build)
		}
	}
}

func TestDetect_GoVersion(t *testing.T) {
	for gomod, image := range map[string]string{
		"module x\n":                                    "golang:1-alpine",
		"module x\n\ngo 1.20\n":                         "golang:1-alpine",
		"module x\n\ngo 1.21\n":                         "golang:1.21-alpine",
		"module x\r\n\r\ngo 1.24.2\r\n":                 "golang:1.24-alpine",
		"module x\n\ngo 1.25.0\n\ntoolchain go1.27.4\n": "golang:1.25-alpine",
	} {
		d := only(t, files(t, map[string]string{"go.mod": gomod, "main.go": "package main\n"}))
		if d.Image != image {
			t.Errorf("go.mod %q: image %s, want %s", gomod, d.Image, image)
		}
	}
}

// With go.sum, the modules are downloaded in their own cached layer.
func TestDetect_GoSum_DownloadsFirst(t *testing.T) {
	d := only(t, files(t, map[string]string{"go.mod": "module x\n\ngo 1.24\n", "go.sum": "", "main.go": "package main\n"}))
	if !slices.Equal(d.CopyFirst, []string{"go.mod", "go.sum"}) || !slices.Equal(d.Install, []string{"go mod download"}) {
		t.Fatalf("CopyFirst %q, Install %q", d.CopyFirst, d.Install)
	}
	df, _, err := Render(d)
	if err != nil || !strings.Contains(string(df), "WORKDIR /src\nCOPY go.mod go.sum ./\nRUN go mod download\nCOPY . .\n") {
		t.Fatalf("Dockerfile:\n%s", df)
	}
}

// A static site is an index.html with nothing else kx recognises.
func TestDetect_Static(t *testing.T) {
	d := only(t, files(t, map[string]string{"index.html": "<p>hi</p>"}))
	if d.Stack != StackStatic || d.Label != "a static site" || d.Image != "nginx/nginx-unprivileged:1.30-alpine" {
		t.Fatalf("Detect() = %+v", d)
	}
	d = only(t, files(t, map[string]string{"index.html": "", "package.json": `{"scripts":{"start":"vite preview"}}`}))
	if d.Stack != StackNode {
		t.Fatalf("index.html beside package.json detected as %s", d.Stack)
	}
}

func TestDetect_Order(t *testing.T) {
	dir := files(t, map[string]string{
		"package.json":     `{"scripts":{"start":"node a.js"}}`,
		"requirements.txt": "",
		"main.py":          "",
		"go.mod":           "module x\n",
		"main.go":          "package main\n",
	})
	found, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []Stack
	for _, d := range found {
		got = append(got, d.Stack)
	}
	if want := []Stack{StackNode, StackPython, StackGo}; !slices.Equal(got, want) {
		t.Fatalf("stacks %v, want %v", got, want)
	}
}

// files makes a folder holding m (path → content) and returns it.
func files(t *testing.T, m map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range m {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Skipf("can't make %s here: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Skipf("can't make %s here: %v", name, err)
		}
	}
	return dir
}

func only(t *testing.T, dir string) Detection {
	t.Helper()
	found, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("Detect() found %d apps: %+v", len(found), found)
	}
	return found[0]
}

func wantProblem(t *testing.T, d Detection, what, fix string) {
	t.Helper()
	if d.Problem != what || d.Fix != fix {
		t.Fatalf("Problem:\n  %q\n  %q\nwant\n  %q\n  %q", d.Problem, d.Fix, what, fix)
	}
}

func hasLine(text, line string) bool {
	return slices.Contains(strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), line)
}
