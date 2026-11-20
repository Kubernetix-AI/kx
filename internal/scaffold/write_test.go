package scaffold

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func nodeApp(t *testing.T) Detection {
	t.Helper()
	found, err := Detect(filepath.Join("testdata", "node-npm-start"))
	if err != nil || len(found) != 1 {
		t.Fatalf("Detect() = %+v, %v", found, err)
	}
	return found[0]
}

// The .dockerignore kx writes keeps .env files out of the image.
func TestScaffold_DockerignoreKeepsEnvOut(t *testing.T) {
	dir := files(t, map[string]string{".env": "SECRET=1\n", ".env.local": "SECRET=2\n"})
	wrote, err := Write(dir, nodeApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wrote, []string{"Dockerfile", ".dockerignore"}) {
		t.Fatalf("wrote %q", wrote)
	}
	ignore, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{".env", ".env.*", ".git", "node_modules", "__pycache__", ".venv", "*.pem", "*.key", "id_rsa*", ".ssh", ".aws", ".netrc", ".pypirc", "*.tfstate", ".kx.json",
		"Dockerfile", ".dockerignore", "**/.env", "**/.env.*", "**/*.pem", "**/*.key", "**/id_rsa*"} {
		if !hasLine(string(ignore), line) {
			t.Errorf(".dockerignore has no line %q:\n%s", line, ignore)
		}
	}
	if exposed, err := EnvExposed(dir); err != nil || len(exposed) != 0 {
		t.Fatalf("EnvExposed() after Write = %q, %v", exposed, err)
	}
	// The same files in subfolders stay out too.
	rules := parseDockerignore(ignore)
	for _, p := range []string{".env", ".env.local", "api/.env", "api/config/.env.production", "certs/server.key", "deploy/id_rsa", "a/b/c.pem"} {
		if !rules.excludes(p) {
			t.Errorf("the .dockerignore kx writes lets %s into the image", p)
		}
	}
	df, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := Render(nodeApp(t))
	if string(df) != string(want) {
		t.Fatalf("Dockerfile on disk differs from Render:\n%s", df)
	}
}

func TestScaffold_NeverOverwritesDockerfile(t *testing.T) {
	for _, name := range []string{"Dockerfile", "dockerfile"} {
		dir := files(t, map[string]string{name: "FROM scratch\n"})
		wrote, err := Write(dir, nodeApp(t))
		if !errors.Is(err, fs.ErrExist) || len(wrote) != 0 {
			t.Fatalf("%s present: Write() = %q, %v; want fs.ErrExist and nothing written", name, wrote, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != "FROM scratch\n" {
			t.Fatalf("%s was changed to %q", name, b)
		}
		if _, err := os.Lstat(filepath.Join(dir, ".dockerignore")); err == nil {
			t.Fatalf("%s present: wrote a .dockerignore anyway", name)
		}
	}
}

// A Dockerfile that is a link to a missing file is a file kx would
// replace by following the link: Write refuses, and creates nothing there.
func TestScaffold_DanglingSymlinkDockerfile_NotWritten(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere", "target")
	if err := os.Symlink(target, filepath.Join(dir, "Dockerfile")); err != nil {
		t.Skipf("can't make a symbolic link here: %v", err)
	}
	if _, err := Write(dir, nodeApp(t)); err == nil {
		t.Fatal("Write() followed a dangling Dockerfile link")
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link's target exists: %v", err)
	}
}

func TestScaffold_KeepsAnExistingDockerignore(t *testing.T) {
	dir := files(t, map[string]string{".dockerignore": "dist\n"})
	wrote, err := Write(dir, nodeApp(t))
	if err != nil || !slices.Equal(wrote, []string{"Dockerfile"}) {
		t.Fatalf("Write() = %q, %v", wrote, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".dockerignore")); string(b) != "dist\n" {
		t.Fatalf(".dockerignore changed to %q", b)
	}
}

func TestScaffold_RefusesAProblem(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, Detection{Stack: StackNode, Label: "a Node app", Problem: "x", Fix: "y"}); err == nil {
		t.Fatal("wrote a Dockerfile for an app with a problem")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %v", entries)
	}
}

func TestEnvExposed(t *testing.T) {
	tests := []struct {
		name   string
		ignore string // "" means no .dockerignore
		env    []string
		want   []string
	}{
		{"no .dockerignore: kx writes one", "", []string{".env", "api/.env"}, nil},
		{"no .env", "dist\n", nil, nil},
		{"not listed", "node_modules\n", []string{".env", ".env.local"}, []string{".env", ".env.local"}},
		{"listed", "node_modules\n.env\n.env.*\n", []string{".env", ".env.production"}, nil},
		{"only .env listed", ".env\n", []string{".env", ".env.local"}, []string{".env.local"}},
		{"a wildcard", ".env*\n", []string{".env", ".env.local"}, nil},
		{"anywhere", "**/.env\n", []string{".env", "api/.env"}, nil},
		{"everything, then some back", "*\n!src\n", []string{".env"}, nil},
		{"listed, then let back in", ".env\n!.env\n", []string{".env"}, []string{".env"}},
		{"a leading slash", "/.env\r\n", []string{".env"}, nil},
		{"a comment is not a pattern", "# .env\n", []string{".env"}, []string{".env"}},
		{"*.env is not .env", "*.env\n", []string{".env.local"}, []string{".env.local"}},
		// Patterns without **/ match only at the top of the folder.
		{"in a subfolder", "node_modules\n.env\n.env.*\n", []string{"config/.env", "api/.env.production"}, []string{"api/.env.production", "config/.env"}},
		{"its folder is listed", "config\n", []string{"config/.env", "config/deep/.env"}, nil},
		{"its folder is listed, the file let back in", "config\n!config/.env\n", []string{"config/.env"}, []string{"config/.env"}},
		{"its folder is listed, something else let back in", "config\n!README.md\n", []string{"config/.env", "config/deep/.env.local"}, nil},
		{"skipped folders", "dist\n", []string{".git/.env", "node_modules/pkg/.env", ".venv/.env"}, nil},
	}
	for _, tt := range tests {
		m := map[string]string{}
		if tt.ignore != "" {
			m[".dockerignore"] = tt.ignore
		}
		for _, e := range tt.env {
			m[e] = "SECRET=1\n"
		}
		got, err := EnvExposed(files(t, m))
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("%s: EnvExposed() = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}

// EnvListed reports whether the folder's .dockerignore keeps the top .env
// out, so a message saying it doesn't list .env would be wrong.
func TestEnvListed(t *testing.T) {
	for ignore, want := range map[string]bool{"": false, "node_modules\n": false, ".env\n": true, ".env*\n": true, "**/.env\n": true, ".env\n!.env\n": false} {
		m := map[string]string{}
		if ignore != "" {
			m[".dockerignore"] = ignore
		}
		if got := EnvListed(files(t, m)); got != want {
			t.Errorf(".dockerignore %q: EnvListed() = %v, want %v", ignore, got, want)
		}
	}
}

// A .dockerignore Docker can't read (a link to nothing, a folder) keeps
// nothing out: kx treats the folder as having none it can rely on.
func TestEnvExposed_BrokenDockerignore(t *testing.T) {
	dir := files(t, map[string]string{".env": "SECRET=1\n", "api/.env": "SECRET=2\n"})
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, ".dockerignore")); err != nil {
		t.Skipf("can't make a symbolic link here: %v", err)
	}
	if !DockerignoreBroken(dir) {
		t.Fatal("DockerignoreBroken() = false for a link to nothing")
	}
	got, err := EnvExposed(dir)
	if err != nil || !slices.Equal(got, []string{".env", "api/.env"}) {
		t.Fatalf("EnvExposed() = %q, %v", got, err)
	}
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".dockerignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !DockerignoreBroken(dir) {
		t.Fatal("DockerignoreBroken() = false for a folder")
	}
	for _, m := range []map[string]string{{}, {".dockerignore": ".git\n"}} {
		if DockerignoreBroken(files(t, m)) {
			t.Fatalf("DockerignoreBroken(%v) = true", m)
		}
	}
}

func TestIgnoreEnv(t *testing.T) {
	for before, after := range map[string]string{
		"dist\n":        "dist\n**/.env\n**/.env.*\n",
		"dist":          "dist\n**/.env\n**/.env.*\n",
		"dist\r\n":      "dist\r\n**/.env\r\n**/.env.*\r\n",
		".env\n!.env\n": ".env\n!.env\n**/.env\n**/.env.*\n",
		"*\n!config/\n": "*\n!config/\n**/.env\n**/.env.*\n",
	} {
		dir := files(t, map[string]string{".dockerignore": before, ".env": "", ".env.local": "", "config/.env": "", "config/.env.prod": ""})
		if err := IgnoreEnv(dir); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, ".dockerignore"))
		if string(b) != after {
			t.Errorf("%q became %q, want %q", before, b, after)
		}
		if exposed, err := EnvExposed(dir); err != nil || len(exposed) != 0 {
			t.Errorf("%q: still exposed after IgnoreEnv: %q, %v", before, exposed, err)
		}
	}
	if err := IgnoreEnv(t.TempDir()); err == nil || !strings.Contains(err.Error(), ".dockerignore") {
		t.Fatalf("IgnoreEnv without a .dockerignore = %v", err)
	}
}

// UnreadableDockerfile names a Dockerfile that is there but can't be used
// (a link to nothing, a folder), which kx neither builds nor replaces.
func TestUnreadableDockerfile(t *testing.T) {
	t.Run("empty folder", func(t *testing.T) {
		if got := UnreadableDockerfile(t.TempDir()); got != "" {
			t.Fatalf("UnreadableDockerfile() = %q", got)
		}
	})
	t.Run("a folder named dockerfile", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "dockerfile"), 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, "dockerfile")
		if caseInsensitive(t, dir) {
			// Here Dockerfile names the same folder, and kx looks for
			// that name first, so the path it reports is that one.
			want = filepath.Join(dir, "Dockerfile")
		}
		if got := UnreadableDockerfile(dir); got != want {
			t.Fatalf("UnreadableDockerfile() = %q, want %q", got, want)
		}
	})
	t.Run("a link to nothing", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "Dockerfile")); err != nil {
			t.Skipf("can't make a symbolic link here: %v", err)
		}
		if got := UnreadableDockerfile(dir); got != filepath.Join(dir, "Dockerfile") {
			t.Fatalf("UnreadableDockerfile() = %q", got)
		}
	})
}

// caseInsensitive reports whether names in dir that differ only in case
// are the same file, as on the usual macOS and Windows file systems.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// A Dockerfile kx isn't allowed to read is unreadable too, so kx says so
// instead of offering to write one it then can't.
func TestUnreadableDockerfile_NoPermission(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a user that isn't root")
	}
	dir := files(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	p := filepath.Join(dir, "Dockerfile")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if got := UnreadableDockerfile(dir); got != p {
		t.Fatalf("mode 000: %q", got)
	}
	if got := UnreadableDockerfile(files(t, map[string]string{"Dockerfile": "FROM scratch\n"})); got != "" {
		t.Fatalf("a readable Dockerfile: %q", got)
	}
}
