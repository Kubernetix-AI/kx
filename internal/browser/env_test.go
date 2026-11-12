//go:build unix

package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestOpen_BrowserGetsNoTokens runs Open against a fake opener on PATH
// that records its environment: the browser (and anything it starts) must
// not inherit KX_TOKEN or a GitHub token.
func TestOpen_BrowserGetsNoTokens(t *testing.T) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	bin := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env")
	script := "#!/bin/sh\nenv > \"$KX_TEST_ENV_FILE.tmp\" && mv \"$KX_TEST_ENV_FILE.tmp\" \"$KX_TEST_ENV_FILE\"\n"
	if err := os.WriteFile(filepath.Join(bin, opener), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KX_TEST_ENV_FILE", envFile)
	t.Setenv("KX_TOKEN", "kx-token-for-the-test")
	t.Setenv("GITHUB_TOKEN", "github-token-for-the-test")
	t.Setenv("GH_TOKEN", "gh-token-for-the-test")

	getenv := func(k string) string {
		if k == "DISPLAY" {
			return ":0"
		}
		return ""
	}
	if err := Open(getenv, "https://platform.kubernetix.ai/settings"); err != nil {
		t.Fatal(err)
	}
	var env []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(envFile); err == nil {
			env = b
			break
		}
	}
	if env == nil {
		t.Fatal("the fake opener never ran")
	}
	if !strings.Contains(string(env), "KX_TEST_ENV_FILE=") {
		t.Fatalf("the opener's environment wasn't recorded:\n%s", env)
	}
	for _, k := range []string{"KX_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		for _, line := range strings.Split(string(env), "\n") {
			if strings.HasPrefix(line, k+"=") {
				t.Errorf("the browser got %s", k)
			}
		}
	}
}
