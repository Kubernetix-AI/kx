package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A secret typed without NAME= must never become a variable's name, nor
// appear in an error.
func TestEnvSet_SecretsTypedWithoutAName(t *testing.T) {
	h := configured(t)
	const (
		fernet  = "gAAAAABk3xFernetlikekey0123456789abcdefgh" // base64, no - or _
		dbURL   = "postgres://u:pa55w0rd@db.example/x?sslmode=require"
		padded1 = fernet + "="
		padded2 = fernet + "=="
	)
	tests := []struct {
		arg  string
		what string
	}{
		{padded1, "Argument 1 sets an empty value: if it's a value, write NAME=value; to set an empty value, add --allow-empty."},
		{padded2, "Argument 1 looks like a value: write NAME=value, or NAME to take the value from kx's environment."},
		{dbURL, "Argument 1 isn't NAME=VALUE: names use letters, digits and _, and don't start with a digit."},
	}
	for _, tt := range tests {
		h.expect(h.run("env", "set", tt.arg), 2)
		if !strings.Contains(h.err.String(), tt.what+"\n") {
			t.Errorf("kx env set %s…: stderr =\n%s\nwant %q", tt.arg[:6], h.err.String(), tt.what)
		}
		h.noSecrets(fernet, "pa55w0rd", "sslmode")
	}
	// With a name, the same values are fine: padding and all.
	h.expect(h.run("env", "set", "FERNET_KEY="+padded2, "DATABASE_URL="+dbURL, "--no-deploy"), 0)
	got := h.api.envPatches[0]["set"].(map[string]any)
	if got["FERNET_KEY"] != padded2 || got["DATABASE_URL"] != dbURL || len(h.api.envPatches) != 1 {
		t.Fatalf("set = %v", got)
	}
	h.noSecrets(fernet, "pa55w0rd")
}

func TestEnvSet_EmptyValueNeedsAllowEmpty(t *testing.T) {
	h := configured(t)
	h.expect(h.run("env", "set", "A=1", "FEATURE_X="), 2)
	if !strings.HasSuffix(h.err.String(), "Argument 2 sets an empty value: if it's a value, write NAME=value; to set an empty value, add --allow-empty.\nTo remove a variable instead: kx env unset NAME\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	file := filepath.Join(h.dir, ".env")
	if err := os.WriteFile(file, []byte("A=1\nB=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.expect(h.run("env", "set", "--from-file", ".env"), 2)
	if !strings.Contains(h.err.String(), "Line 2 of .env sets an empty value; to set it empty, add --allow-empty.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	h.expect(h.run("env", "set", "FEATURE_X=", "--allow-empty", "--no-deploy"), 0)
	if fmt.Sprint(h.api.envPatches) != "[map[set:map[FEATURE_X:]]]" {
		t.Fatalf("patches = %v", h.api.envPatches)
	}
}

func TestEnvSet_Stdin(t *testing.T) {
	h := configured(t)
	h.stdin = secretValue + "\n"
	h.expect(h.run("env", "set", "API_KEY", "--stdin", "--no-deploy"), 0)
	if fmt.Sprint(h.api.envPatches) != "[map[set:map[API_KEY:"+secretValue+"]]]" {
		t.Fatalf("patches = %v; the value is stdin without its last line break", h.api.envPatches)
	}
	h.noSecrets(secretValue)

	// On a terminal kx asks, without echoing.
	h.tty, h.password = true, secretValue2
	h.stdin = ""
	h.expect(h.run("env", "set", "STRIPE_KEY", "--stdin", "--no-deploy"), 0)
	if h.api.envPatches[1]["set"].(map[string]any)["STRIPE_KEY"] != secretValue2 || !strings.Contains(h.err.String(), "Value for STRIPE_KEY: ") {
		t.Fatalf("patches %v, stderr:\n%s", h.api.envPatches, h.err.String())
	}
	h.noSecrets(secretValue2)
	h.tty = false

	for _, args := range [][]string{
		{"env", "set", "--stdin"},
		{"env", "set", "A", "B", "--stdin"},
		{"env", "set", "A=1", "--stdin"},
		{"env", "set", "A", "--stdin", "--from-file", ".env"},
	} {
		h.expect(h.run(args...), 2)
		if !strings.Contains(h.err.String(), "--stdin reads one value: give just its name, like kx env set API_KEY --stdin.\n") {
			t.Fatalf("kx %v: stderr:\n%s", args, h.err.String())
		}
	}
	h.stdin = "\n"
	h.expect(h.run("env", "set", "API_KEY", "--stdin"), 2)
	if !strings.Contains(h.err.String(), "Standard input holds an empty value; to set an empty value, add --allow-empty.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestEnvFile_DotenvRules(t *testing.T) {
	h := configured(t)
	cases := []struct {
		body, what string
	}{
		{"A=\"x\\\"\n", "Line 1 of .env isn't NAME=VALUE."},        // the closing quote is escaped
		{"A='open\n", "Line 1 of .env isn't NAME=VALUE."},          // no closing quote
		{"spring.profile=x\n", "Line 1 of .env isn't NAME=VALUE."}, // not a name
	}
	for _, c := range cases {
		if err := os.WriteFile(filepath.Join(h.dir, ".env"), []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		h.expect(h.run("env", "set", "--from-file", ".env"), 2)
		if !strings.Contains(h.err.String(), c.what+"\n") {
			t.Fatalf("%q: stderr:\n%s", c.body, h.err.String())
		}
	}
}

func TestEnvSet_MasksValuesInGitHubActions(t *testing.T) {
	h := configured(t)
	h.env["GITHUB_ACTIONS"], h.env["CI"], h.env["GITHUB_EVENT_NAME"], h.env["KX_TOKEN"] = "true", "true", "push", tokCorp
	h.expect(h.run("env", "set", "API_KEY="+secretValue, "PEM=line-one\nline-two", "DEBUG=1", "--no-deploy"), 0)
	stderr := h.err.String()
	for _, want := range []string{"::add-mask::" + secretValue + "\n", "::add-mask::line-one\n", "::add-mask::line-two\n"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "::add-mask::1\n") {
		t.Fatalf("masked a 1-character value, which would hide every 1 in the log:\n%s", stderr)
	}
	if i := strings.Index(stderr, "::add-mask::"); i != 0 {
		t.Fatalf("the masks must come before any other output:\n%s", stderr)
	}
	// Outside GitHub Actions kx prints no workflow commands.
	h2 := configured(t)
	h2.expect(h2.run("env", "set", "API_KEY="+secretValue, "--no-deploy"), 0)
	if strings.Contains(h2.err.String(), "::") {
		t.Fatalf("stderr:\n%s", h2.err.String())
	}
}
