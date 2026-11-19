// Package scaffold writes a Dockerfile for a folder that has none: it
// recognises Node, Python and Go apps and static sites, and renders a
// Dockerfile that listens on port 8080 and runs as a numeric non-root
// user, plus a .dockerignore that keeps .env files, keys and .git out of
// the image.
//
// Nothing here asks questions or prints: kx deploy decides what to offer
// and shows what was written.
package scaffold

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Stack is a kind of app kx can write a Dockerfile for.
type Stack string

// The stacks, in the order Detect reports them.
const (
	StackNode   Stack = "node"
	StackPython Stack = "python"
	StackGo     Stack = "go"
	StackStatic Stack = "static"
)

// Detection is one kind of app kx recognised in a folder, and how to
// build and run it.
type Detection struct {
	Stack Stack
	Label string // how kx names it to people: "a Node app"

	Image     string   // the base image (for Go, the build stage's)
	CopyFirst []string // files copied before Install, so its layer is reused
	Install   []string // shell commands, one RUN each
	Build     []string // shell commands, run once the whole folder is copied
	Start     []string // what the container runs, in exec form

	// Problem is set when kx recognised the app but can't write a
	// Dockerfile for it; Problem and Fix are the two lines kx prints.
	Problem, Fix string
}

// maxFileBytes bounds what kx reads of any one file it looks at.
const maxFileBytes = 1 << 20

// Detect reports every kind of app in dir, in the order node, python, go,
// static. A static site is an index.html with nothing else kx recognises.
func Detect(dir string) ([]Detection, error) {
	var found []Detection
	for _, detect := range []func(string) (*Detection, error){detectNode, detectPython, detectGo} {
		d, err := detect(dir)
		if err != nil {
			return nil, err
		}
		if d != nil {
			found = append(found, *d)
		}
	}
	if len(found) == 0 && isFile(dir, "index.html") {
		found = append(found, Detection{Stack: StackStatic, Label: "a static site", Image: "nginxinc/nginx-unprivileged:1.27-alpine"})
	}
	return found, nil
}

// ---- Node ----

var nodeVersionRe = regexp.MustCompile(`^(?:[\^~=]|v)?\s*([0-9]{1,2})(?:\.(?:[0-9]+|[xX*])){0,2}$`)

const defaultNodeMajor = 22

func detectNode(dir string) (*Detection, error) {
	data, err := readFile(dir, "package.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := &Detection{Stack: StackNode, Label: "a Node app"}
	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\ufeff")), &pkg); err != nil {
		d.Problem = "This looks like a Node app, but package.json isn't valid JSON, so kx can't tell how to run it."
		d.Fix = "Fix package.json, or add a Dockerfile."
		return d, nil
	}
	// Each field is read on its own; one of an unexpected type is ignored.
	var main string
	var scripts, engines map[string]any
	_ = json.Unmarshal(pkg["main"], &main)
	_ = json.Unmarshal(pkg["scripts"], &scripts)
	_ = json.Unmarshal(pkg["engines"], &engines)
	_, workspaces := pkg["workspaces"]

	major := nodeMajor(firstLine(dir, ".nvmrc"))
	if major == 0 {
		v, _ := engines["node"].(string)
		major = nodeMajor(v)
	}
	if major == 0 {
		major = defaultNodeMajor
	}
	d.Image = fmt.Sprintf("node:%d-alpine", major)

	// Node 25 and later no longer include corepack.
	corepack := "corepack enable && "
	if major >= 25 {
		corepack = "npm install -g corepack && " + corepack
	}
	lock, install := "", "npm install"
	switch {
	case isFile(dir, "package-lock.json"):
		lock, install = "package-lock.json", "npm ci"
	case isFile(dir, "yarn.lock"):
		lock, install = "yarn.lock", corepack+"yarn install --frozen-lockfile"
	case isFile(dir, "pnpm-lock.yaml"):
		lock, install = "pnpm-lock.yaml", corepack+"pnpm install --frozen-lockfile"
	}
	d.Install = []string{install}
	// A workspace, or Yarn's own settings, need the whole folder first.
	if !workspaces && !isFile(dir, "pnpm-workspace.yaml") && !isFile(dir, ".yarnrc.yml") {
		d.CopyFirst = []string{"package.json"}
		if lock != "" {
			d.CopyFirst = append(d.CopyFirst, lock)
		}
		if isFile(dir, ".npmrc") {
			d.CopyFirst = append(d.CopyFirst, ".npmrc")
		}
	}
	if b, _ := scripts["build"].(string); strings.TrimSpace(b) != "" {
		d.Build = []string{"npm run build"}
	}

	// used is the value from package.json that decides how the app runs.
	start, _ := scripts["start"].(string)
	used := ""
	switch {
	case strings.TrimSpace(start) != "":
		used, d.Start = start, []string{"npm", "start"}
	case strings.TrimSpace(main) != "":
		used, d.Start = main, []string{"node", main}
	default:
		for _, f := range []string{"server.js", "index.js", "app.js"} {
			if isFile(dir, f) {
				d.Start = []string{"node", f}
				break
			}
		}
	}
	switch {
	case strings.ContainsAny(used, "\r\n"):
		d.Problem = "This looks like a Node app, but its start command in package.json is more than one line, so kx can't tell how to run it."
		d.Fix = "Put it on one line, or add a Dockerfile."
	case d.Start == nil:
		d.Problem = "This looks like a Node app, but package.json has no start script, so kx can't tell how to run it."
		d.Fix = `Add a "start" script to package.json, or add a Dockerfile.`
	}
	return d, nil
}

// nodeMajor reads one major version ("20", "v20.11.0", "^20", "20.x");
// anything else (a range, "lts/*") is 0.
func nodeMajor(v string) int {
	m := nodeVersionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ---- Python ----

var (
	pythonVersionRe = regexp.MustCompile(`^(3\.[0-9]{1,2})(?:\.[0-9]+)?$`)
	procfileWebRe   = regexp.MustCompile(`^web:\s*(.*\S)\s*$`)
	portVarRe       = regexp.MustCompile(`\$\{PORT(?::?-[^}]*)?\}|\$PORT\b`)
	djangoSettingRe = regexp.MustCompile(`DJANGO_SETTINGS_MODULE["']\s*,\s*["']([A-Za-z_][A-Za-z0-9_]*)\.settings["']`)
	pythonNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// A requirements line that needs other files from the folder.
	requirementFileRe = regexp.MustCompile(`(?m)^\s*(?:-r|-c|-e|--requirement|--constraint|--editable|\.|/|file:)`)
)

// shellChars are what make a Procfile command need a shell.
const shellChars = "$`\\\"'&|;<>()*?~#{}[]!"

const defaultPython = "3.12"

func detectPython(dir string) (*Detection, error) {
	req, reqErr := readFile(dir, "requirements.txt")
	proj, projErr := readFile(dir, "pyproject.toml")
	for _, err := range []error{reqErr, projErr} {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if reqErr != nil && projErr != nil {
		return nil, nil
	}
	d := &Detection{Stack: StackPython, Label: "a Python app", Image: "python:" + defaultPython + "-slim"}
	if m := pythonVersionRe.FindStringSubmatch(firstLine(dir, ".python-version")); m != nil {
		d.Image = "python:" + m[1] + "-slim"
	}
	if reqErr == nil {
		d.Install = []string{"pip install --no-cache-dir -r requirements.txt"}
		if !requirementFileRe.Match(req) {
			d.CopyFirst = []string{"requirements.txt"}
		}
	} else {
		d.Install = []string{"pip install --no-cache-dir ."}
	}
	deps := strings.ToLower(string(req) + "\n" + string(proj))

	start, err := pythonStart(dir, deps)
	if err != nil {
		return nil, err
	}
	if start == nil {
		d.Problem = "This looks like a Python app, but it has no Procfile with a web: line, so kx can't tell how to run it."
		d.Fix = "Add a Procfile with a web: line, or add a Dockerfile."
		return d, nil
	}
	d.Start = start
	server := start[0]
	if server == "sh" {
		server, _, _ = strings.Cut(start[2], " ")
	}
	if (server == "gunicorn" || server == "uvicorn") && !hasDependency(deps, server) {
		d.Install = append(d.Install, "pip install --no-cache-dir "+server)
	}
	return d, nil
}

// pythonStart works out the command that runs the app, or nil.
func pythonStart(dir, deps string) ([]string, error) {
	if procfile, err := readFile(dir, "Procfile"); err == nil {
		for _, line := range strings.Split(string(procfile), "\n") {
			if m := procfileWebRe.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
				cmd := portVarRe.ReplaceAllString(m[1], "8080")
				if strings.ContainsAny(cmd, shellChars) {
					return []string{"sh", "-c", cmd}, nil
				}
				return strings.Fields(cmd), nil
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if isFile(dir, "manage.py") {
		if pkg := djangoPackage(dir); pkg != "" {
			return []string{"gunicorn", pkg + ".wsgi", "--bind", "0.0.0.0:8080"}, nil
		}
	}
	for _, fw := range []struct{ dep, class string }{{"fastapi", "FastAPI"}, {"flask", "Flask"}} {
		if !hasDependency(deps, fw.dep) {
			continue
		}
		for _, file := range []string{"main.py", "app.py"} {
			src, err := readFile(dir, file)
			if err != nil || !strings.Contains(string(src), fw.class+"(") {
				continue
			}
			target := strings.TrimSuffix(file, ".py") + ":" + appVariable(string(src), fw.dep, fw.class)
			if fw.dep == "fastapi" {
				return []string{"uvicorn", target, "--host", "0.0.0.0", "--port", "8080"}, nil
			}
			return []string{"gunicorn", target, "--bind", "0.0.0.0:8080"}, nil
		}
	}
	for _, file := range []string{"app.py", "main.py"} {
		if isFile(dir, file) {
			return []string{"python", file}, nil
		}
	}
	return nil, nil
}

// djangoPackage is the package holding the project's wsgi.py: the only
// one at the top of the folder, or the one manage.py's settings name.
func djangoPackage(dir string) string {
	var pkgs []string
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && pythonNameRe.MatchString(e.Name()) && isFile(dir, filepath.Join(e.Name(), "wsgi.py")) {
				pkgs = append(pkgs, e.Name())
			}
		}
	}
	if len(pkgs) == 1 {
		return pkgs[0]
	}
	if src, err := readFile(dir, "manage.py"); err == nil {
		if m := djangoSettingRe.FindSubmatch(src); m != nil && isFile(dir, filepath.Join(string(m[1]), "wsgi.py")) {
			return string(m[1])
		}
	}
	return ""
}

// appVariable finds the name the app object is assigned to (app =
// FastAPI(), api: FastAPI = fastapi.FastAPI()); "app" when it can't.
func appVariable(src, module, class string) string {
	re := regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*(?::[^=\n]+)?=\s*(?:` + module + `\.)?` + class + `\(`)
	if m := re.FindStringSubmatch(src); m != nil {
		return m[1]
	}
	return "app"
}

// hasDependency reports whether deps (requirements.txt and pyproject.toml,
// lower-cased) names the package name itself, not one named like it.
func hasDependency(deps, name string) bool {
	re := regexp.MustCompile(`(?m)(?:^|[\s"'(,])` + regexp.QuoteMeta(name) + `(?:[\s"'\[\]<>=~!;,@)]|$)`)
	return re.MatchString(deps)
}

// ---- Go ----

var (
	goLineRe  = regexp.MustCompile(`(?m)^go\s+1\.([0-9]+)(?:\.[0-9]+)?\s*(?://.*)?$`)
	cmdNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// oldestGoLine is the oldest go line kx names a toolchain image for;
// golang images from 1.21 on refuse a go.mod that asks for a newer Go.
const oldestGoLine = 21

func detectGo(dir string) (*Detection, error) {
	data, err := readFile(dir, "go.mod")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := &Detection{Stack: StackGo, Label: "a Go app", Image: "golang:1-alpine", Start: []string{"/app"}}
	if m := goLineRe.FindSubmatch(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))); m != nil {
		if minor, _ := strconv.Atoi(string(m[1])); minor >= oldestGoLine {
			d.Image = fmt.Sprintf("golang:1.%d-alpine", minor)
		}
	}
	if isFile(dir, "go.sum") && !isFile(dir, "go.work") {
		d.CopyFirst = []string{"go.mod", "go.sum"}
		d.Install = []string{"go mod download"}
	}

	pkg, mains := ".", []string(nil)
	if !hasMainPackage(dir) {
		pkg = ""
		entries, _ := os.ReadDir(filepath.Join(dir, "cmd"))
		for _, e := range entries {
			if e.IsDir() && cmdNameRe.MatchString(e.Name()) && hasMainFile(filepath.Join(dir, "cmd", e.Name(), "main.go")) {
				mains = append(mains, "cmd/"+e.Name())
			}
		}
		if len(mains) == 1 {
			pkg = "./" + mains[0]
		}
	}
	switch {
	case pkg != "":
		d.Build = []string{"CGO_ENABLED=0 go build -o /out/app " + pkg}
	case len(mains) == 0:
		d.Problem = "This looks like a Go app, but it has no main package, so kx can't tell how to run it."
		d.Fix = "Add a main package, or add a Dockerfile."
	default:
		d.Problem = "This looks like a Go app, but it has more than one main package (" + joinAnd(mains) + "), so kx can't tell how to run it."
		d.Fix = "Keep one main package, or add a Dockerfile."
	}
	return d, nil
}

// hasMainPackage reports whether the folder's own .go files are package
// main (files built only on request, like a //go:build ignore generator,
// don't count).
func hasMainPackage(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && hasMainFile(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

func hasMainFile(path string) bool {
	src, err := readFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return false
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil || f.Name.Name != "main" {
		return false
	}
	for _, group := range f.Comments {
		for _, c := range group.List {
			constraint := strings.HasPrefix(c.Text, "//go:build") || strings.HasPrefix(c.Text, "// +build")
			if constraint && strings.Contains(c.Text, "ignore") {
				return false
			}
		}
	}
	return true
}

// ---- files ----

// readFile reads name in dir, at most maxFileBytes of it.
func readFile(dir, name string) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // G304: a file in the folder kx was asked to deploy
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "read", Path: filepath.Join(dir, name), Err: fs.ErrNotExist}
	}
	return io.ReadAll(io.LimitReader(f, maxFileBytes))
}

func isFile(dir, name string) bool {
	fi, err := os.Stat(filepath.Join(dir, name))
	return err == nil && fi.Mode().IsRegular()
}

// firstLine is the first line of name in dir that isn't blank or a
// comment, trimmed; "" if there is none.
func firstLine(dir, name string) string {
	data, err := readFile(dir, name)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

func joinAnd(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
