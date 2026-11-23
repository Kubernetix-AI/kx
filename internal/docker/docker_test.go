package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

const testPassword = "test-only-registry-password-0123456789"

func testCred() api.RegistryCredential {
	return api.RegistryCredential{
		RegistryURL: "registry.example.test/acme-corp",
		Username:    "kx-push",
		Password:    testPassword,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

// noSecret fails the test if s holds the test password, without printing it.
func noSecret(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, testPassword) {
		t.Errorf("%s contains the registry password", where)
	}
	if strings.Contains(s, base64.StdEncoding.EncodeToString([]byte("kx-push:"+testPassword))) {
		t.Errorf("%s contains the encoded registry credential", where)
	}
}

func TestCheck_Missing(t *testing.T) {
	useFakeDocker(t)
	t.Setenv("PATH", t.TempDir()) // no docker anywhere
	if err := Check(context.Background()); !errors.Is(err, kxerr.ErrDockerMissing) {
		t.Fatalf("Check() = %v, want ErrDockerMissing", err)
	}
}

func TestCheck_EngineStates(t *testing.T) {
	tests := []struct {
		server string
		want   error
	}{
		{"", nil},
		{"windows", kxerr.ErrDockerWindowsContainers},
		{"down", kxerr.ErrDockerStopped},
		{"denied", kxerr.ErrDockerPermission},
	}
	for _, tt := range tests {
		t.Run("server="+tt.server, func(t *testing.T) {
			calls := useFakeDocker(t)
			t.Setenv("FAKE_DOCKER_SERVER", tt.server)
			err := Check(context.Background())
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("Check() = %v, want %v", err, tt.want)
			}
			got := calls()
			if len(got) != 1 || strings.Join(got[0].Args, " ") != "version --format {{.Server.Os}}" {
				t.Fatalf("docker calls = %+v", got)
			}
		})
	}
}

func TestDockerNeverSeesKxOrGitHubTokens(t *testing.T) {
	calls := useFakeDocker(t)
	t.Setenv("KX_TOKEN", "kx_fake")
	t.Setenv("GITHUB_TOKEN", "ghs_fake")
	t.Setenv("GH_TOKEN", "gho_fake")
	_ = Check(context.Background())
	_ = Build(context.Background(), BuildOptions{Dir: t.TempDir(), Tag: "shop-api:abc"}, &bytes.Buffer{})
	_ = (Engine{}).Tag(context.Background(), "shop-api:abc", "registry.example.test/acme-corp/shop-api:abc")
	_, _ = Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{})
	got := calls()
	if len(got) < 5 {
		t.Fatalf("only %d docker calls", len(got))
	}
	for _, c := range got {
		for _, k := range []string{"KX_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
			if _, ok := c.Env[k]; ok {
				t.Errorf("docker %s inherited %s", c.Args[0], k)
			}
		}
	}
}

func TestBuild_LinuxAmd64(t *testing.T) {
	calls := useFakeDocker(t)
	dir := t.TempDir()
	var out bytes.Buffer
	err := Build(context.Background(), BuildOptions{Dir: dir, Dockerfile: filepath.Join(dir, "Dockerfile.prod"), Tag: "shop-api:3f9c2a1b7d4e"}, &out)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	got := callsTo(calls(), "build")
	if len(got) != 1 {
		t.Fatalf("build calls = %+v", calls())
	}
	want := []string{"build", "--platform", "linux/amd64", "--tag", "shop-api:3f9c2a1b7d4e", "--file", filepath.Join(dir, "Dockerfile.prod"), dir}
	if strings.Join(got[0].Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q\nwant  %q", got[0].Args, want)
	}
	if got[0].Env["BUILDKIT_PROGRESS"] != "plain" {
		t.Errorf("BUILDKIT_PROGRESS = %q, want plain", got[0].Env["BUILDKIT_PROGRESS"])
	}
	if _, ok := got[0].Env["DOCKER_CONFIG"]; ok {
		t.Errorf("build must use the person's own Docker settings, got DOCKER_CONFIG=%q", got[0].Env["DOCKER_CONFIG"])
	}
	if !strings.Contains(out.String(), "naming to docker.io/library/shop-api") {
		t.Errorf("build output not passed to w: %q", out.String())
	}
}

func TestBuild_Labels(t *testing.T) {
	calls := useFakeDocker(t)
	dir := t.TempDir()
	labels := map[string]string{
		"org.opencontainers.image.revision": "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345",
		"ai.kubernetix.kx.context":          "services/api",
	}
	if err := Build(context.Background(), BuildOptions{Dir: dir, Tag: "shop-api:3f9c2a1b7d4e", Labels: labels}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	want := []string{"build", "--platform", "linux/amd64", "--tag", "shop-api:3f9c2a1b7d4e",
		"--label", "ai.kubernetix.kx.context=services/api",
		"--label", "org.opencontainers.image.revision=3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345", dir}
	if got := callsTo(calls(), "build")[0].Args; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q\nwant  %q", got, want)
	}
	for _, bad := range []map[string]string{{"Bad Key": "x"}, {"a.b": "line\nbreak"}, {"": "x"}, {"a.b": strings.Repeat("x", 600)}} {
		if err := Build(context.Background(), BuildOptions{Dir: dir, Tag: "shop-api:abc", Labels: bad}, &bytes.Buffer{}); err == nil {
			t.Errorf("Build() accepted the label %q", bad)
		}
	}
}

func TestBuild_LoadsFromAContainerBuilder(t *testing.T) {
	tests := []struct {
		buildx, buildkit string
		load             bool
	}{
		{"docker-container", "", true},
		{"docker", "", false},
		{"missing", "", false},
		{"docker-container", "0", false}, // the classic builder has no --load
	}
	for _, tt := range tests {
		calls := useFakeDocker(t)
		t.Setenv("FAKE_DOCKER_BUILDX", tt.buildx)
		if tt.buildkit != "" {
			t.Setenv("DOCKER_BUILDKIT", tt.buildkit)
		}
		if err := Build(context.Background(), BuildOptions{Dir: t.TempDir(), Tag: "shop-api:abc"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		args := callsTo(calls(), "build")[0].Args
		if got := slices.Contains(args, "--load"); got != tt.load {
			t.Errorf("buildx %s, DOCKER_BUILDKIT=%q: --load = %v, want %v (%q)", tt.buildx, tt.buildkit, got, tt.load, args)
		}
		unsetEnv(t, "DOCKER_BUILDKIT")
	}
}

func TestParseBuildxDriver(t *testing.T) {
	tests := map[string]string{
		"Name:          b\nDriver:        docker-container\n": "docker-container",
		"Name: default\nDriver: docker\n":                     "docker",
		"Driver:   kubernetes\r\n":                            "kubernetes",
		"no driver here":                                      "",
		"":                                                    "",
	}
	for in, want := range tests {
		if got := parseBuildxDriver(in); got != want {
			t.Errorf("parseBuildxDriver(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzParseBuildxDriver(f *testing.F) {
	f.Add("Name: b\nDriver: docker-container\n")
	f.Fuzz(func(t *testing.T, s string) {
		d := parseBuildxDriver(s)
		if strings.ContainsAny(d, " \t\r\n") {
			t.Fatalf("driver %q has spaces", d)
		}
	})
}

func TestBuild_RelativeDirBecomesAbsolute(t *testing.T) {
	calls := useFakeDocker(t)
	t.Chdir(t.TempDir())
	if err := os.Mkdir("api", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Build(context.Background(), BuildOptions{Dir: "api", Tag: "shop-api:abc"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	args := callsTo(calls(), "build")[0].Args
	last := args[len(args)-1]
	if !filepath.IsAbs(last) || filepath.Base(last) != "api" {
		t.Fatalf("context = %q, want an absolute path (a folder named -x must never read as a flag)", last)
	}
	for _, a := range args {
		if a == "--file" {
			t.Fatalf("--file passed without a Dockerfile: %q", args)
		}
	}
}

func TestBuild_KeepsBuildkitProgressSetting(t *testing.T) {
	calls := useFakeDocker(t)
	t.Setenv("BUILDKIT_PROGRESS", "rawjson")
	if err := Build(context.Background(), BuildOptions{Dir: t.TempDir(), Tag: "shop-api:abc"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := callsTo(calls(), "build")[0].Env["BUILDKIT_PROGRESS"]; got != "rawjson" {
		t.Fatalf("BUILDKIT_PROGRESS = %q, want the person's own value", got)
	}
}

func TestBuild_Fails(t *testing.T) {
	useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_BUILD", "fail")
	var out bytes.Buffer
	err := Build(context.Background(), BuildOptions{Dir: t.TempDir(), Tag: "shop-api:abc"}, &out)
	var bf *kxerr.BuildFailed
	if !errors.As(err, &bf) {
		t.Fatalf("Build() = %v, want *kxerr.BuildFailed", err)
	}
	if !strings.Contains(out.String(), "npm ci") {
		t.Fatalf("the failing step's output must reach w: %q", out.String())
	}
}

func TestBuild_RejectsBadTag(t *testing.T) {
	calls := useFakeDocker(t)
	for _, tag := range []string{"", "Shop:abc", "shop-api", "shop-api:-x", "shop api:abc", "--push:abc"} {
		if err := Build(context.Background(), BuildOptions{Dir: t.TempDir(), Tag: tag}, &bytes.Buffer{}); err == nil {
			t.Errorf("Build(tag %q) = nil, want an error", tag)
		}
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("docker ran %d times for invalid tags", n)
	}
}

func TestPush_ThrowawayConfig(t *testing.T) {
	calls := useFakeDocker(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	userConfig := filepath.Join(t.TempDir(), "my-docker")
	t.Setenv("DOCKER_CONFIG", userConfig)
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")

	var out bytes.Buffer
	ref := "registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e"
	digest, err := Push(context.Background(), ref, testCred(), &out)
	if err != nil {
		t.Fatalf("Push() = %v", err)
	}
	if digest != fakeDigest {
		t.Fatalf("digest = %q, want %q", digest, fakeDigest)
	}
	pushes := callsTo(calls(), "push")
	if len(pushes) != 1 {
		t.Fatalf("push calls: %d", len(pushes))
	}
	p := pushes[0]
	if strings.Join(p.Args, " ") != "push "+ref {
		t.Fatalf("args = %q", redactSecret(strings.Join(p.Args, " ")))
	}
	if p.ConfigDir == "" || p.ConfigDir == userConfig {
		t.Fatalf("DOCKER_CONFIG = %q, want a throwaway folder", p.ConfigDir)
	}
	if rel, err := filepath.Rel(tmp, p.ConfigDir); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("throwaway folder %q is not under the temp dir", p.ConfigDir)
	}
	if runtime.GOOS != "windows" {
		if p.DirMode != "700" || p.FileMode != "600" {
			t.Errorf("modes: dir %s, file %s; want 700 and 600", p.DirMode, p.FileMode)
		}
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
		CredsStore string `json:"credsStore"`
	}
	if err := json.Unmarshal([]byte(p.Config), &cfg); err != nil {
		t.Fatalf("config.json is not JSON: %v", err)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("kx-push:" + testPassword))
	if len(cfg.Auths) != 1 || cfg.Auths["registry.example.test"].Auth != wantAuth || cfg.CredsStore != "" {
		t.Errorf("config.json does not hold exactly the one registry login")
	}
	if _, err := os.Stat(p.ConfigDir); !os.IsNotExist(err) {
		t.Errorf("throwaway folder still exists after Push (stat err %v)", err)
	}
	for _, c := range calls() {
		noSecret(t, "docker argv", strings.Join(c.Args, " "))
	}
	noSecret(t, "push output", out.String())
	if _, err := os.Stat(userConfig); !os.IsNotExist(err) {
		t.Errorf("Push wrote to the person's own Docker config folder")
	}
}

func TestRemoveOnCancel(t *testing.T) {
	dir := t.TempDir()
	login := filepath.Join(dir, "kx-docker-1")
	if err := os.Mkdir(login, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := removeOnCancel(ctx, login)
	defer stop()
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(login); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the login folder outlived the first signal")
		}
		time.Sleep(5 * time.Millisecond)
	}

	kept := filepath.Join(dir, "kx-docker-2")
	if err := os.Mkdir(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	removeOnCancel(ctx2, kept)() // a push that finished: the watcher goes away
	cancel2()
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Stat(kept); err != nil {
		t.Fatal("stop() didn't stop the watcher")
	}
}

func TestPush_InterruptedRemovesLogin(t *testing.T) {
	useFakeDocker(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("FAKE_DOCKER_PUSH", "hang")
	t.Setenv("FAKE_DOCKER_STARTED", started)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Push(ctx, "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{})
		done <- err
	}()
	var login string
	for i := 0; i < 500 && login == ""; i++ {
		b, _ := os.ReadFile(started)
		login = string(b)
		time.Sleep(10 * time.Millisecond)
	}
	if login == "" {
		t.Fatal("the fake docker never started")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Push() = %v", err)
	}
	if _, err := os.Stat(login); !os.IsNotExist(err) {
		t.Fatal("the login folder survived an interrupted push")
	}
}

func TestSweepStaleLogins(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, mod time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "config.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk("kx-docker-123456", old)
	fresh := mk("kx-docker-654321", time.Now().Add(-5*time.Minute))
	other := mk("not-kx-123", old)
	sweepStaleLogins(dir, time.Now())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a login folder older than a credential's lifetime was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("removed a login folder another kx may still be using")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("removed a folder kx didn't make")
	}
}

func TestPush_KeepsDockerHostAndTLS(t *testing.T) {
	calls := useFakeDocker(t)
	t.Setenv("DOCKER_HOST", "tcp://docker:2376")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/certs/client")
	if _, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if n := len(callsTo(got, "context")); n != 0 {
		t.Errorf("asked docker for its context %d times although DOCKER_HOST is set", n)
	}
	env := callsTo(got, "push")[0].Env
	if env["DOCKER_HOST"] != "tcp://docker:2376" || env["DOCKER_TLS_VERIFY"] != "1" || env["DOCKER_CERT_PATH"] != "/certs/client" {
		t.Fatalf("push env = %v, want DOCKER_HOST and TLS settings untouched", env)
	}
}

func TestPush_DerivesDockerHostFromActiveContext(t *testing.T) {
	calls := useFakeDocker(t)
	userConfig := filepath.Join(t.TempDir(), "my-docker")
	t.Setenv("DOCKER_CONFIG", userConfig)
	t.Setenv("DOCKER_CONTEXT", "colima")
	t.Setenv("FAKE_DOCKER_CONTEXT_HOST", "unix:///home/sam/.colima/default/docker.sock")
	if _, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got := calls()
	ctxCalls := callsTo(got, "context")
	if len(ctxCalls) != 1 || strings.Join(ctxCalls[0].Args, " ") != "context inspect --format {{json .}}" {
		t.Fatalf("context calls = %+v", ctxCalls)
	}
	if ctxCalls[0].Env["DOCKER_CONFIG"] != userConfig || ctxCalls[0].Env["DOCKER_CONTEXT"] != "colima" {
		t.Errorf("the context must be read from the person's own settings, env = %v", ctxCalls[0].Env)
	}
	env := callsTo(got, "push")[0].Env
	if env["DOCKER_HOST"] != "unix:///home/sam/.colima/default/docker.sock" {
		t.Errorf("push DOCKER_HOST = %q", env["DOCKER_HOST"])
	}
	if _, ok := env["DOCKER_CONTEXT"]; ok {
		t.Errorf("DOCKER_CONTEXT must not reach a push with a throwaway config")
	}
}

func TestPush_ContextLookupFailsOnDefaultContext(t *testing.T) {
	calls := useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_CONTEXT_HOST", "fail")
	t.Setenv("FAKE_DOCKER_CONTEXT_NAME", "default")
	if _, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := callsTo(calls(), "push")[0].Env["DOCKER_HOST"]; ok {
		t.Fatal("DOCKER_HOST set although the default context is active")
	}

	calls = useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_CONTEXT_NAME", "default")
	t.Setenv("FAKE_DOCKER_CONTEXT_HOST", "http://evil.example.test")
	if _, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if h, ok := callsTo(calls(), "push")[0].Env["DOCKER_HOST"]; ok {
		t.Fatalf("DOCKER_HOST = %q from an unrecognised address", h)
	}
}

func TestPush_ContextLookupFailsOnAnotherContext(t *testing.T) {
	for _, mode := range []string{"lookup fails", "odd address"} {
		calls := useFakeDocker(t)
		t.Setenv("FAKE_DOCKER_CONTEXT_NAME", "colima")
		if mode == "lookup fails" {
			t.Setenv("FAKE_DOCKER_CONTEXT_HOST", "fail")
		} else {
			t.Setenv("FAKE_DOCKER_CONTEXT_HOST", "http://evil.example.test")
		}
		_, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{})
		var unknown *kxerr.DockerContextUnknown
		if !errors.As(err, &unknown) || unknown.Name != "colima" {
			t.Fatalf("%s: Push() = %v, want DockerContextUnknown{colima}", mode, err)
		}
		if n := len(callsTo(calls(), "push")); n != 0 {
			t.Fatalf("%s: pushed to the default engine instead", mode)
		}
	}
}

func TestPush_TLSContext(t *testing.T) {
	for _, skip := range []bool{false, true} {
		calls := useFakeDocker(t)
		tlsPath := t.TempDir()
		if err := os.MkdirAll(filepath.Join(tlsPath, "docker"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"ca.pem", "cert.pem", "key.pem"} {
			if err := os.WriteFile(filepath.Join(tlsPath, "docker", f), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := json.Marshal(map[string]any{"Name": "remote", "Endpoints": map[string]any{"docker": map[string]any{"Host": "tcp://build.example.test:2376", "SkipTLSVerify": skip}},
			"TLSMaterial": map[string]any{"docker": []string{"ca.pem", "cert.pem", "key.pem"}}, "Storage": map[string]any{"TLSPath": tlsPath}})
		t.Setenv("FAKE_DOCKER_CONTEXT_JSON", string(b))
		if _, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		env := callsTo(calls(), "push")[0].Env
		if env["DOCKER_HOST"] != "tcp://build.example.test:2376" || env["DOCKER_CERT_PATH"] != filepath.Join(tlsPath, "docker") {
			t.Fatalf("skip=%v: push env = %v", skip, env)
		}
		if skip && (env["DOCKER_TLS"] != "1" || env["DOCKER_TLS_VERIFY"] != "") {
			t.Fatalf("skip=true: want TLS without verification, env = %v", env)
		}
		if !skip && env["DOCKER_TLS_VERIFY"] != "1" {
			t.Fatalf("skip=false: want DOCKER_TLS_VERIFY=1, env = %v", env)
		}
	}
}

func TestParseDockerContext(t *testing.T) {
	c, err := parseDockerContext(`{"Name":"remote","Endpoints":{"docker":{"Host":"tcp://h:2376","SkipTLSVerify":true}},"TLSMaterial":{"docker":["ca.pem"]},"Storage":{"TLSPath":"/t"}}`)
	if err != nil || c.Name != "remote" || c.Host() != "tcp://h:2376" || !c.SkipTLSVerify() || !c.HasTLS() || c.Storage.TLSPath != "/t" {
		t.Fatalf("parseDockerContext = %+v, %v", c, err)
	}
	for _, bad := range []string{"", "not json", "[]"} {
		if _, err := parseDockerContext(bad); err == nil {
			t.Errorf("parseDockerContext(%q) accepted it", bad)
		}
	}
}

func FuzzParseDockerContext(f *testing.F) {
	f.Add(`{"Name":"colima","Endpoints":{"docker":{"Host":"unix:///x.sock"}}}`)
	f.Fuzz(func(t *testing.T, s string) {
		c, err := parseDockerContext(s)
		if err == nil && c == nil {
			t.Fatal("nil context without an error")
		}
	})
}

func TestPush_Unauthorized(t *testing.T) {
	useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_PUSH", "unauthorized")
	var out bytes.Buffer
	_, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &out)
	var pf *kxerr.PushFailed
	if !errors.As(err, &pf) || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Push() = %v, want *kxerr.PushFailed wrapping ErrUnauthorized", err)
	}
	noSecret(t, "error", err.Error())
}

func TestPush_TagAlreadyExists(t *testing.T) {
	useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_PUSH", "immutable")
	_, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e", testCred(), &bytes.Buffer{})
	var pf *kxerr.PushFailed
	if !errors.As(err, &pf) || !errors.Is(err, kxerr.ErrTagExists) {
		t.Fatalf("Push() = %v, want *kxerr.PushFailed wrapping ErrTagExists", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatal("an existing tag was taken for a refused credential")
	}
}

func TestPush_LayerAlreadyExistsIsNotATagProblem(t *testing.T) {
	useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_PUSH", "fail-after-layers")
	_, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{})
	var pf *kxerr.PushFailed
	if !errors.As(err, &pf) || errors.Is(err, kxerr.ErrTagExists) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Push() = %v, want a plain *kxerr.PushFailed", err)
	}
}

func TestPush_OtherFailure(t *testing.T) {
	useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_PUSH", "fail")
	_, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{})
	var pf *kxerr.PushFailed
	if !errors.As(err, &pf) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Push() = %v, want a plain *kxerr.PushFailed", err)
	}
}

func TestPush_DigestFromInspectWhenOutputHasNone(t *testing.T) {
	calls := useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_PUSH", "nodigest")
	t.Setenv("FAKE_DOCKER_REPO_DIGESTS", `["registry.example.test/acme-corp/other@sha256:1111111111111111111111111111111111111111111111111111111111111111","registry.example.test/acme-corp/shop-api@`+fakeDigest+`"]`)
	digest, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{})
	if err != nil || digest != fakeDigest {
		t.Fatalf("Push() = %q, %v; want %q", digest, err, fakeDigest)
	}
	inspect := callsTo(calls(), "image")
	if len(inspect) != 1 || strings.Join(inspect[0].Args, " ") != "image inspect --format {{json .RepoDigests}} registry.example.test/acme-corp/shop-api:abc" {
		t.Fatalf("inspect calls = %+v", inspect)
	}
}

func TestPush_NoDigestAnywhere(t *testing.T) {
	useFakeDocker(t)
	t.Setenv("FAKE_DOCKER_PUSH", "nodigest")
	digest, err := Push(context.Background(), "registry.example.test/acme-corp/shop-api:abc", testCred(), &bytes.Buffer{})
	if err != nil || digest != "" {
		t.Fatalf("Push() = %q, %v; want no digest and no error", digest, err)
	}
}

func TestPush_RejectsBadInput(t *testing.T) {
	calls := useFakeDocker(t)
	other := testCred()
	other.RegistryURL = "registry.other.test/acme-corp"
	empty := testCred()
	empty.Password = ""
	tests := []struct {
		name string
		ref  string
		cred api.RegistryCredential
	}{
		{"no registry host", "shop-api:abc", testCred()},
		{"flag-like", "--all-tags", testCred()},
		{"credential for another registry", "registry.example.test/acme-corp/shop-api:abc", other},
		{"empty password", "registry.example.test/acme-corp/shop-api:abc", empty},
	}
	for _, tt := range tests {
		if _, err := Push(context.Background(), tt.ref, tt.cred, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: Push() = nil error", tt.name)
		}
	}
	if n := len(callsTo(calls(), "push")); n != 0 {
		t.Fatalf("docker push ran %d times for bad input", n)
	}
}

func TestTag(t *testing.T) {
	calls := useFakeDocker(t)
	if err := (Engine{}).Tag(context.Background(), "shop-api:abc", "registry.example.test/acme-corp/shop-api:abc"); err != nil {
		t.Fatal(err)
	}
	got := callsTo(calls(), "tag")
	if len(got) != 1 || strings.Join(got[0].Args, " ") != "tag shop-api:abc registry.example.test/acme-corp/shop-api:abc" {
		t.Fatalf("tag calls = %+v", got)
	}
}

func TestParseDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		out  string
		want string
	}{
		{"3f9c: digest: " + d + " size: 1573\n", d},
		{"a\nb\nlatest: digest: " + d + " size: 1\n", d},
		{"first: digest: sha256:" + strings.Repeat("11", 32) + " size: 1\nsecond: digest: " + d + " size: 2", d},
		{"digest: sha256:" + strings.Repeat("ab", 31) + " size: 1", ""},
		{"digest: sha256:" + strings.Repeat("AB", 32) + " size: 1", ""},
		{"digest: sha256:" + strings.Repeat("ab", 33) + " size: 1", ""},
		{"no digest here", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ParseDigest(tt.out); got != tt.want {
			t.Errorf("ParseDigest(%q) = %q, want %q", tt.out, got, tt.want)
		}
	}
}

func FuzzParseDigest(f *testing.F) {
	f.Add("3f9c: digest: sha256:" + strings.Repeat("ab", 32) + " size: 1573\n")
	f.Add("digest: sha256:\x00")
	f.Fuzz(func(t *testing.T, s string) {
		d := ParseDigest(s)
		if d != "" && !digestRe.MatchString(d) {
			t.Fatalf("ParseDigest returned %q", d)
		}
		if d != "" && !strings.Contains(s, d) {
			t.Fatalf("ParseDigest invented %q", d)
		}
	})
}

func TestValidRef(t *testing.T) {
	good := []string{"shop-api:3f9c2a1b7d4e", "registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e-dirty-20261112090000", "localhost:5000/a/b:v1"}
	bad := []string{"", "shop-api", "Shop-API:abc", "-x:abc", "a b:c", "registry.example.test/acme-corp/shop-api:", "a:" + strings.Repeat("x", 129), "a\n:b"}
	for _, r := range good {
		if !validRef(r) {
			t.Errorf("validRef(%q) = false", r)
		}
	}
	for _, r := range bad {
		if validRef(r) {
			t.Errorf("validRef(%q) = true", r)
		}
	}
}
