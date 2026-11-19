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
			wantPath := filepath.Join(dir, "want.Dockerfile")
			if *update {
				if err := os.WriteFile(wantPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatal(err)
			}
			// A checkout with CRLF line endings must not fail on Windows.
			if string(got) != strings.ReplaceAll(string(want), "\r\n", "\n") {
				t.Fatalf("Dockerfile differs from %s\n--- got ---\n%s\n--- want ---\n%s", wantPath, got, want)
			}
			if !hasLine(string(ignore), ".env") || !hasLine(string(ignore), ".env.*") {
				t.Fatalf(".dockerignore doesn't keep .env out:\n%s", ignore)
			}
		})
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
		if err != nil || !strings.Contains(string(df), "WORKDIR /app\nCOPY . .\nRUN ") {
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
	d := only(t, files(t, map[string]string{"go.mod": "module example.com/lib\n", "lib.go": "package lib\n"}))
	wantProblem(t, d,
		"This looks like a Go app, but it has no main package, so kx can't tell how to run it.",
		"Add a main package, or add a Dockerfile.")
}

func TestDetect_GoMainPackage(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		build string
	}{
		{"root wins over cmd", map[string]string{"main.go": "package main\n", "cmd/x/main.go": "package main\n"}, "CGO_ENABLED=0 go build -o /out/app ."},
		{"a generator at the root is not the app", map[string]string{"gen.go": "//go:build ignore\n\npackage main\n", "lib.go": "// Package shop.\npackage shop\n", "cmd/server/main.go": "package main\n"}, "CGO_ENABLED=0 go build -o /out/app ./cmd/server"},
		{"a cmd folder kx can't name safely is skipped", map[string]string{"cmd/$(x)/main.go": "package main\n", "cmd/web/main.go": "package main\n"}, "CGO_ENABLED=0 go build -o /out/app ./cmd/web"},
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
	if d.Stack != StackStatic || d.Label != "a static site" {
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
