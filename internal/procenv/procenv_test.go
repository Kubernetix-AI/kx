package procenv

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestClean_DropsSecrets(t *testing.T) {
	env := []string{"PATH=/bin", "KX_TOKEN=kx_secret", "GITHUB_TOKEN=ghs_secret", "GH_TOKEN=gho_secret", "HOME=/home/sam", "KX_TOKEN_NOT=keep"}
	got := Clean(env)
	want := []string{"PATH=/bin", "HOME=/home/sam", "KX_TOKEN_NOT=keep"}
	if !slices.Equal(got, want) {
		t.Fatalf("Clean = %q, want %q", got, want)
	}
	if runtime.GOOS == "windows" {
		if got := Clean([]string{"kx_token=x", "Github_Token=y"}); len(got) != 0 {
			t.Fatalf("Windows names are case-insensitive, got %q", got)
		}
	}
}

func TestCommand_HasNoSecrets(t *testing.T) {
	t.Setenv("KX_TOKEN", "kx_secret")
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	cmd := Command(context.Background(), "git", "status")
	if cmd.Env == nil {
		t.Fatal("Command left Env nil, so git would inherit everything")
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "KX_TOKEN=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Fatalf("child environment has %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}
