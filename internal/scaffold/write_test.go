package scaffold

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	for _, line := range []string{".env", ".env.*", ".git", "node_modules", "__pycache__", ".venv", "*.pem", "*.key", "id_rsa*", ".ssh", ".aws", ".netrc", ".pypirc", "*.tfstate", ".kx.json"} {
		if !hasLine(string(ignore), line) {
			t.Errorf(".dockerignore has no line %q:\n%s", line, ignore)
		}
	}
	if exposed, err := EnvExposed(dir); err != nil || len(exposed) != 0 {
		t.Fatalf("EnvExposed() after Write = %q, %v", exposed, err)
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
		{"no .dockerignore: kx writes one", "", []string{".env"}, nil},
		{"no .env", "dist\n", nil, nil},
		{"not listed", "node_modules\n", []string{".env", ".env.local"}, []string{".env", ".env.local"}},
		{"listed", "node_modules\n.env\n.env.*\n", []string{".env", ".env.production"}, nil},
		{"only .env listed", ".env\n", []string{".env", ".env.local"}, []string{".env.local"}},
		{"a wildcard", ".env*\n", []string{".env", ".env.local"}, nil},
		{"anywhere", "**/.env\n", []string{".env"}, nil},
		{"everything, then some back", "*\n!src\n", []string{".env"}, nil},
		{"listed, then let back in", ".env\n!.env\n", []string{".env"}, []string{".env"}},
		{"a leading slash", "/.env\r\n", []string{".env"}, nil},
		{"a comment is not a pattern", "# .env\n", []string{".env"}, []string{".env"}},
		{"*.env is not .env", "*.env\n", []string{".env.local"}, []string{".env.local"}},
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

func TestIgnoreEnv(t *testing.T) {
	for before, after := range map[string]string{
		"dist\n":        "dist\n.env\n.env.*\n",
		"dist":          "dist\n.env\n.env.*\n",
		"dist\r\n":      "dist\r\n.env\r\n.env.*\r\n",
		".env\n!.env\n": ".env\n!.env\n.env\n.env.*\n",
	} {
		dir := files(t, map[string]string{".dockerignore": before, ".env": "", ".env.local": ""})
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
