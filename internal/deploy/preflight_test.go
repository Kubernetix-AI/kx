package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

func TestPreflight_FindsDockerfileAndPort(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM node:22 AS build\nEXPOSE 9999\nFROM node:22-slim\nEXPOSE 8080/tcp\n")
	b := &fakeBuilder{}
	df, err := Preflight(context.Background(), b, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if df.Dir != dir || df.Path != filepath.Join(dir, "Dockerfile") || df.Port != 8080 {
		t.Fatalf("Preflight() = %+v", df)
	}
	if got := b.got(); len(got) != 1 || got[0] != "check" {
		t.Fatalf("docker calls = %v, want one check", got)
	}
}

func TestPreflight_NoDockerfile(t *testing.T) {
	dir := t.TempDir()
	b := &fakeBuilder{}
	_, err := Preflight(context.Background(), b, dir, "")
	var nd *kxerr.NoDockerfile
	if !errors.As(err, &nd) || nd.Dir != dir {
		t.Fatalf("Preflight() = %v, want NoDockerfile{%s}", err, dir)
	}
	if len(b.got()) != 0 {
		t.Fatal("checked Docker before the Dockerfile")
	}
}

func TestPreflight_ExplicitDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile.prod"), "FROM scratch\nEXPOSE 3000\n")
	df, err := Preflight(context.Background(), &fakeBuilder{}, dir, filepath.Join(dir, "Dockerfile.prod"))
	if err != nil || df.Path != filepath.Join(dir, "Dockerfile.prod") || df.Port != 3000 {
		t.Fatalf("Preflight() = %+v, %v", df, err)
	}
	_, err = Preflight(context.Background(), &fakeBuilder{}, dir, filepath.Join(dir, "nope"))
	var u *kxerr.Usage
	if !errors.As(err, &u) || !strings.Contains(u.What, "nope") {
		t.Fatalf("missing --dockerfile: %v", err)
	}
}

func TestPreflight_LowercaseDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dockerfile"), "FROM scratch\n")
	df, err := Preflight(context.Background(), &fakeBuilder{}, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Base(df.Path), "dockerfile") {
		t.Fatalf("path = %s", df.Path)
	}
}

func TestPreflight_MissingFolder(t *testing.T) {
	_, err := Preflight(context.Background(), &fakeBuilder{}, filepath.Join(t.TempDir(), "api"), "")
	var u *kxerr.Usage
	if !errors.As(err, &u) || u.Fix != "Check the --build path, then run kx deploy again." {
		t.Fatalf("Preflight() = %v", err)
	}
}

func TestPreflight_DockerProblemsPassThrough(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	for _, want := range []error{kxerr.ErrDockerMissing, kxerr.ErrDockerStopped, kxerr.ErrDockerWindowsContainers} {
		_, err := Preflight(context.Background(), &fakeBuilder{checkErr: want}, dir, "")
		if !errors.Is(err, want) {
			t.Errorf("Preflight() = %v, want %v", err, want)
		}
	}
}

func TestExposedPort(t *testing.T) {
	tests := []struct {
		name, file string
		want       int
	}{
		{"simple", "FROM x\nEXPOSE 8080\n", 8080},
		{"protocol", "FROM x\nexpose 3000/tcp\n", 3000},
		{"udp skipped", "FROM x\nEXPOSE 53/udp 8053\n", 8053},
		{"several", "FROM x\nEXPOSE 80 443\n", 80},
		{"final stage wins", "FROM a AS build\nEXPOSE 1\nFROM b\nEXPOSE 2\n", 2},
		{"final stage without EXPOSE", "FROM a AS build\nEXPOSE 1\nFROM b\n", 0},
		{"first EXPOSE in stage", "FROM x\nEXPOSE 5000\nEXPOSE 6000\n", 5000},
		{"variable skipped", "FROM x\nARG PORT=1\nEXPOSE $PORT 9000\n", 9000},
		{"continuation", "FROM x\nEXPOSE \\\n  7000\n", 7000},
		{"comment", "FROM x\n# EXPOSE 1\nEXPOSE 2\n", 2},
		{"range", "FROM x\nEXPOSE 7000-7010\n", 7000},
		{"out of range", "FROM x\nEXPOSE 70000\n", 0},
		{"zero", "FROM x\nEXPOSE 0\n", 0},
		{"heredoc ignored", "FROM x\nRUN <<EOF\nEXPOSE 1\nEOF\nEXPOSE 2\n", 2},
		{"heredoc dash", "FROM x\nRUN <<-'END'\n\tEXPOSE 1\n\tEND\nEXPOSE 3\n", 3},
		{"here-string is not a heredoc", "FROM x\nRUN cat <<<word\nEXPOSE 5\nword\n", 5},
		{"crlf", "FROM x\r\nEXPOSE 4000\r\n", 4000},
		{"escape directive", "# escape=`\nFROM x\nEXPOSE `\n 4100\n", 4100},
		{"none", "FROM x\n", 0},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		if got := ExposedPort([]byte(tt.file)); got != tt.want {
			t.Errorf("%s: ExposedPort = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func FuzzExposedPort(f *testing.F) {
	f.Add([]byte("FROM x\nEXPOSE 8080\n"))
	f.Add([]byte("# escape=`\nFROM x\nRUN <<EOF\nEXPOSE 1\nEOF\nEXPOSE `\n 2/tcp\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if p := ExposedPort(data); p < 0 || p > 65535 {
			t.Fatalf("ExposedPort = %d", p)
		}
	})
}

func TestImageTag(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 30, 5, 0, time.UTC)
	t.Run("CI outside a repository", func(t *testing.T) {
		// kx tags what is checked out; the CI system's commit doesn't name it.
		env := ci.Env{IsCI: true, CommitSHA: "3F9C2A1B7D4E5F60718293A4B5C6D7E8F9012345"}
		if got := ImageTag(context.Background(), env, t.TempDir(), now); got != "local-20261112093005" {
			t.Fatalf("tag = %q", got)
		}
	})
	t.Run("not a repository", func(t *testing.T) {
		if got := ImageTag(context.Background(), ci.Env{}, t.TempDir(), now); got != "local-20261112093005" {
			t.Fatalf("tag = %q", got)
		}
	})
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "x")
	sha := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	t.Run("clean", func(t *testing.T) {
		if got := ImageTag(context.Background(), ci.Env{}, dir, now); got != sha[:12] {
			t.Fatalf("tag = %q, want %q", got, sha[:12])
		}
	})
	t.Run(".kx.json alone is not a change", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, ".kx.json"), `{"app":"shop-api"}`)
		if got := ImageTag(context.Background(), ci.Env{}, dir, now); got != sha[:12] {
			t.Fatalf("tag = %q, want the clean %q", got, sha[:12])
		}
		if err := os.MkdirAll(filepath.Join(dir, "api"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "api", ".kx.json"), `{"app":"api"}`)
		if got := ImageTag(context.Background(), ci.Env{}, filepath.Join(dir, "api"), now); got != sha[:12] {
			t.Fatalf("tag in a subfolder = %q", got)
		}
	})
	t.Run("CI tags what is checked out", func(t *testing.T) {
		env := ci.Env{IsCI: true, CommitSHA: strings.Repeat("a", 40)}
		if got := ImageTag(context.Background(), env, dir, now); got != sha[:12] {
			t.Fatalf("tag = %q, want HEAD's %q, not the CI system's commit", got, sha[:12])
		}
	})
	t.Run("dirty", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "new.txt"), "x")
		if got := ImageTag(context.Background(), ci.Env{}, dir, now); got != sha[:12]+"-dirty-20261112093005" {
			t.Fatalf("tag = %q", got)
		}
	})
	t.Run("CI builds the folder as it is", func(t *testing.T) {
		env := ci.Env{IsCI: true, CommitSHA: sha}
		if got := ImageTag(context.Background(), env, dir, now); got != sha[:12] {
			t.Fatalf("tag = %q, want %q", got, sha[:12])
		}
	})
}

func TestDirtyEntries(t *testing.T) {
	tests := []struct {
		porcelain string
		dirty     bool
	}{
		{"", false},
		{"?? .kx.json\x00", false},
		{" M .kx.json\x00?? api/.kx.json\x00", false},
		{"?? .kx.json\x00 M main.go\x00", true},
		{"R  new.go\x00old.go\x00", true},
		{"R  .kx.json\x00old.json\x00", false},
		{"?? src/\x00", true},
	}
	for _, tt := range tests {
		if got := changedBesidesKxJSON(tt.porcelain); got != tt.dirty {
			t.Errorf("changedBesidesKxJSON(%q) = %v, want %v", tt.porcelain, got, tt.dirty)
		}
	}
}

func FuzzChangedBesidesKxJSON(f *testing.F) {
	f.Add("?? .kx.json\x00R  a\x00b\x00")
	f.Fuzz(func(t *testing.T, s string) { _ = changedBesidesKxJSON(s) })
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
