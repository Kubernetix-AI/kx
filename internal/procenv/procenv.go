// Package procenv builds the environment of the programs kx starts (docker,
// git, the browser): kx's own environment without the secrets they never
// need, so a Docker plugin, credential helper, git hook or browser can't
// read them.
package procenv

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// secrets are the variables child processes never get.
var secrets = []string{"KX_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"}

// Environ is os.Environ() without the secrets.
func Environ() []string { return Clean(os.Environ()) }

// Clean returns env without the secrets.
func Clean(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !isSecret(k) {
			out = append(out, kv)
		}
	}
	return out
}

func isSecret(k string) bool {
	for _, s := range secrets {
		if k == s || (runtime.GOOS == "windows" && strings.EqualFold(k, s)) {
			return true
		}
	}
	return false
}

// Command is exec.CommandContext with the cleaned environment.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = Environ()
	return cmd
}
