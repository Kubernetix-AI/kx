package docker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The tests never touch a real Docker engine. TestMain copies the test
// binary into a temporary folder as "docker"; with KX_FAKE_DOCKER=1 in its
// environment that copy behaves like a scripted docker command (fakeDocker)
// and logs every call to FAKE_DOCKER_LOG. Copying the test binary, rather
// than writing a shell script, keeps the fake working on Windows too.

var fakeBinDir string

func TestMain(m *testing.M) {
	if os.Getenv("KX_FAKE_DOCKER") == "1" {
		os.Exit(fakeDocker(os.Args[1:]))
	}
	dir, err := os.MkdirTemp("", "kx-fake-docker-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := copyExecutable(filepath.Join(dir, "docker"+exeSuffix())); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeBinDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func copyExecutable(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// fakeCall is one logged docker invocation.
type fakeCall struct {
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Config    string            `json:"config,omitempty"`     // $DOCKER_CONFIG/config.json at call time
	ConfigDir string            `json:"config_dir,omitempty"` // $DOCKER_CONFIG
	FileMode  string            `json:"file_mode,omitempty"`
	DirMode   string            `json:"dir_mode,omitempty"`
}

// String keeps the logged config file (which holds the encoded credential)
// and any credential in argv out of test failure messages.
func (c fakeCall) String() string {
	return fmt.Sprintf("{args:%q env:%v}", redactSecret(strings.Join(c.Args, " ")), c.Env)
}

// redactSecret hides the test password and its encoded form.
func redactSecret(s string) string {
	s = strings.ReplaceAll(s, testPassword, "<password>")
	return strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte("kx-push:"+testPassword)), "<auth>")
}

var loggedEnv = []string{"DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_TLS", "BUILDKIT_PROGRESS", "KX_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"}

// stampedTagRe matches a tag kx re-made with a time stamp.
var stampedTagRe = regexp.MustCompile(`-[0-9]{14}$`)

const fakeDigest = "sha256:4d3c2b1a00112233445566778899aabbccddeeff00112233445566778899aabb"

func fakeDocker(args []string) int {
	call := fakeCall{Args: args, Env: map[string]string{}}
	for _, k := range loggedEnv {
		if v, ok := os.LookupEnv(k); ok {
			call.Env[k] = v
		}
	}
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		call.ConfigDir = dir
		if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
			call.Config = string(b)
		}
		if fi, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			call.FileMode = fmt.Sprintf("%o", fi.Mode().Perm())
		}
		if fi, err := os.Stat(dir); err == nil {
			call.DirMode = fmt.Sprintf("%o", fi.Mode().Perm())
		}
	}
	if path := os.Getenv("FAKE_DOCKER_LOG"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			b, _ := json.Marshal(call)
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "version":
		switch os.Getenv("FAKE_DOCKER_SERVER") {
		case "down":
			fmt.Println("Client:\n Version: 27.3.1")
			fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
			return 1
		case "denied":
			fmt.Fprintln(os.Stderr, "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock")
			return 1
		case "windows":
			fmt.Println("windows")
		default:
			fmt.Fprintln(os.Stderr, "WARNING: a harmless warning")
			fmt.Println("linux")
		}
		return 0
	case "context":
		name := os.Getenv("FAKE_DOCKER_CONTEXT_NAME")
		if name == "" {
			name = "colima"
		}
		if len(args) > 1 && args[1] == "show" {
			fmt.Println(name)
			return 0
		}
		if j := os.Getenv("FAKE_DOCKER_CONTEXT_JSON"); j != "" {
			fmt.Println(j)
			return 0
		}
		h := os.Getenv("FAKE_DOCKER_CONTEXT_HOST")
		if h == "fail" {
			fmt.Fprintln(os.Stderr, "context not found")
			return 1
		}
		if h == "" {
			h = "unix:///home/sam/.colima/default/docker.sock"
		}
		b, _ := json.Marshal(map[string]any{"Name": name, "Endpoints": map[string]any{"docker": map[string]any{"Host": h, "SkipTLSVerify": false}},
			"TLSMaterial": map[string]any{}, "Storage": map[string]any{"MetadataPath": "/x", "TLSPath": "/nonexistent"}})
		fmt.Println(string(b))
		return 0
	case "buildx":
		switch os.Getenv("FAKE_DOCKER_BUILDX") {
		case "missing":
			fmt.Fprintln(os.Stderr, "docker: 'buildx' is not a docker command.")
			return 1
		case "docker-container":
			fmt.Println("Name:          ci-builder\nDriver:        docker-container\nLast Activity: 2026-11-12 09:00:00 +0000 UTC")
		default:
			fmt.Println("Name:   default\nDriver: docker")
		}
		return 0
	case "build":
		fmt.Println("#1 [internal] load build definition from Dockerfile")
		if os.Getenv("FAKE_DOCKER_BUILD") == "fail" {
			fmt.Fprintln(os.Stderr, `ERROR: failed to solve: process "/bin/sh -c npm ci" did not complete successfully: exit code: 1`)
			return 1
		}
		fmt.Println("#9 naming to docker.io/library/shop-api:3f9c2a1b7d4e done")
		return 0
	case "tag":
		return 0
	case "push":
		fmt.Println("The push refers to repository [" + strings.SplitN(args[len(args)-1], ":", 2)[0] + "]")
		switch os.Getenv("FAKE_DOCKER_PUSH") {
		case "hang":
			_ = os.WriteFile(os.Getenv("FAKE_DOCKER_STARTED"), []byte(os.Getenv("DOCKER_CONFIG")), 0o600)
			time.Sleep(20 * time.Second)
			return 1
		case "immutable":
			fmt.Println("5f70bf18a086: Layer already exists")
			if !stampedTagRe.MatchString(args[len(args)-1]) {
				fmt.Fprintln(os.Stderr, `denied: Failed to write tag: tag "`+args[len(args)-1]+`" already exists and is immutable`)
				return 1
			}
		case "fail-after-layers":
			fmt.Println("5f70bf18a086: Layer already exists")
			fmt.Fprintln(os.Stderr, "write tcp: connection reset by peer")
			return 1
		case "unauthorized":
			fmt.Fprintln(os.Stderr, "unauthorized: authentication required")
			return 1
		case "fail":
			fmt.Fprintln(os.Stderr, "dial tcp: lookup registry.example.test: no such host")
			return 1
		case "nodigest":
			fmt.Println("5f70bf18a086: Pushed")
			return 0
		}
		fmt.Println("5f70bf18a086: Pushed")
		tag := args[len(args)-1]
		tag = tag[strings.LastIndex(tag, ":")+1:]
		fmt.Printf("%s: digest: %s size: 1573\n", tag, fakeDigest)
		return 0
	case "image":
		if out := os.Getenv("FAKE_DOCKER_REPO_DIGESTS"); out != "" {
			fmt.Println(out)
			return 0
		}
		fmt.Println("[]")
		return 0
	}
	fmt.Fprintf(os.Stderr, "fake docker: unexpected %q\n", args)
	return 2
}

// useFakeDocker puts the fake docker first on PATH and returns a function
// that reads the calls it logged.
func useFakeDocker(t *testing.T) func() []fakeCall {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("PATH", fakeBinDir)
	t.Setenv("KX_FAKE_DOCKER", "1")
	t.Setenv("FAKE_DOCKER_LOG", logPath)
	for _, k := range append([]string{"FAKE_DOCKER_SERVER", "FAKE_DOCKER_CONTEXT_HOST", "FAKE_DOCKER_BUILD", "FAKE_DOCKER_PUSH", "FAKE_DOCKER_REPO_DIGESTS", "FAKE_DOCKER_STARTED", "FAKE_DOCKER_BUILDX", "DOCKER_BUILDKIT", "BUILDX_BUILDER", "FAKE_DOCKER_CONTEXT_NAME", "FAKE_DOCKER_CONTEXT_JSON"}, loggedEnv...) {
		unsetEnv(t, k)
	}
	return func() []fakeCall {
		t.Helper()
		b, err := os.ReadFile(logPath)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var calls []fakeCall
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var c fakeCall
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				t.Fatalf("bad log line %q: %v", line, err)
			}
			calls = append(calls, c)
		}
		return calls
	}
}

// unsetEnv removes k for the test and restores it afterwards.
func unsetEnv(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "") // registers the restore
	_ = os.Unsetenv(k)
}

func callsTo(calls []fakeCall, sub string) []fakeCall {
	var out []fakeCall
	for _, c := range calls {
		if len(c.Args) > 0 && c.Args[0] == sub {
			out = append(out, c)
		}
	}
	return out
}
