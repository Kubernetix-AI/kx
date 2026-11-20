// Package docker runs the docker command for kx deploy: it checks that
// Docker can build Linux images, builds for linux/amd64, and pushes with a
// short-lived registry credential that never touches the person's own
// Docker settings.
//
// The push uses a throwaway DOCKER_CONFIG folder (mode 0700, config.json
// 0600) that holds only that one registry login and is removed afterwards;
// the credential is never put on a command line or printed. Docker
// contexts live in the person's own settings, so when DOCKER_HOST is not
// set kx reads the active context's address first and hands it to the push
// (Colima, OrbStack and Docker Desktop all use contexts). An existing
// DOCKER_HOST, DOCKER_TLS_VERIFY and DOCKER_CERT_PATH are left exactly as
// they are, which is what CI services such as GitLab's docker:dind rely on.
package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/procenv"
)

// Platform is the platform kx builds for: Kubernetix runs x86-64 Linux.
const Platform = "linux/amd64"

const (
	checkTimeout   = 30 * time.Second
	contextTimeout = 10 * time.Second
	captureBytes   = 256 << 10
	waitDelay      = 10 * time.Second
)

// ErrUnauthorized means the registry refused the push credential.
var ErrUnauthorized = errors.New("the registry refused the credential")

// BuildOptions says what to build.
type BuildOptions struct {
	Dir        string // the build context folder
	Dockerfile string // the Dockerfile; "" means the Dockerfile in Dir
	Tag        string // the local image name, e.g. shop-api:3f9c2a1b7d4e
	// Labels go on the image (docker build --label), such as the commit
	// it was built from.
	Labels map[string]string
}

var (
	labelKeyRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,126}[a-z0-9])?$`)
	labelValueRe = regexp.MustCompile(`^[ -~]{0,512}$`)
)

// Engine runs the docker command found on PATH. The zero value is ready to
// use; the package-level functions use it.
type Engine struct{}

// Check reports whether Docker can build Linux images here:
// kxerr.ErrDockerMissing (no docker command), ErrDockerStopped (no engine
// answering), ErrDockerPermission (not allowed to use it) or
// ErrDockerWindowsContainers (Docker Desktop in Windows-containers mode).
func Check(ctx context.Context) error { return Engine{}.Check(ctx) }

// Build runs docker build for linux/amd64. Docker's output goes to w.
func Build(ctx context.Context, o BuildOptions, w io.Writer) error { return Engine{}.Build(ctx, o, w) }

// Push pushes ref with cred and returns the pushed image's digest
// ("sha256:…"), or "" if Docker did not report one. Docker's output goes to
// w; it never contains the credential. A registry that already has the tag
// and won't replace it gives a *kxerr.PushFailed wrapping
// kxerr.ErrTagExists.
func Push(ctx context.Context, ref string, cred api.RegistryCredential, w io.Writer) (string, error) {
	return Engine{}.Push(ctx, ref, cred, w)
}

// Check implements the package-level Check.
func (Engine) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var stdout, stderr capture
	err := run(cctx, procenv.Environ(), &stdout, &stderr, "version", "--format", "{{.Server.Os}}")
	switch {
	case errors.Is(err, kxerr.ErrDockerMissing):
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		if strings.Contains(strings.ToLower(stderr.String()+stdout.String()), "permission denied") {
			return kxerr.ErrDockerPermission
		}
		return kxerr.ErrDockerStopped
	}
	if strings.EqualFold(lastLine(stdout.String()), "windows") {
		return kxerr.ErrDockerWindowsContainers
	}
	return nil
}

// Build implements the package-level Build.
func (Engine) Build(ctx context.Context, o BuildOptions, w io.Writer) error {
	if !validRef(o.Tag) {
		return fmt.Errorf("docker: %q is not an image name kx builds", o.Tag)
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return err
	}
	env := procenv.Environ()
	args := []string{"build", "--platform", Platform, "--tag", o.Tag}
	keys := make([]string, 0, len(o.Labels))
	for k, v := range o.Labels {
		if !labelKeyRe.MatchString(k) || !labelValueRe.MatchString(v) {
			return fmt.Errorf("docker: %q is not a label kx sets", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	if needsLoad(ctx, env) {
		// A docker-container (or other non-docker) buildx builder keeps
		// the image in its cache unless asked to load it into the engine.
		args = append(args, "--load")
	}
	if o.Dockerfile != "" {
		file, err := filepath.Abs(o.Dockerfile)
		if err != nil {
			return err
		}
		args = append(args, "--file", file)
	}
	// Absolute paths never start with "-", so docker can't read them as flags.
	args = append(args, dir)

	if lookup(env, "BUILDKIT_PROGRESS") == "" {
		env = append(env, "BUILDKIT_PROGRESS=plain") // line-by-line output, for folding and CI logs
	}
	out := &lockedWriter{w: w}
	if err := run(ctx, env, out, out, args...); err != nil {
		if errors.Is(err, kxerr.ErrDockerMissing) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &kxerr.BuildFailed{Err: err}
	}
	return nil
}

// needsLoad reports whether docker build must be told --load: buildx is
// the builder and its current driver isn't "docker". Without buildx, or
// with the classic builder (DOCKER_BUILDKIT=0), the flag doesn't exist.
func needsLoad(ctx context.Context, env []string) bool {
	if lookup(env, "DOCKER_BUILDKIT") == "0" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	var stdout, stderr capture
	if err := run(cctx, env, &stdout, &stderr, "buildx", "inspect"); err != nil {
		return false
	}
	d := parseBuildxDriver(stdout.String())
	return d != "" && d != "docker"
}

// parseBuildxDriver reads the "Driver:" line of docker buildx inspect.
func parseBuildxDriver(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Driver:"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				return f[0]
			}
			return ""
		}
	}
	return ""
}

// Tag gives the image src the extra name dst.
func (Engine) Tag(ctx context.Context, src, dst string) error {
	if !validRef(src) || !validRef(dst) {
		return errors.New("docker: invalid image name")
	}
	var out capture
	if err := run(ctx, procenv.Environ(), &out, &out, "tag", src, dst); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, kxerr.ErrDockerMissing) {
			return err
		}
		return fmt.Errorf("docker tag: %s", firstLine(out.String()))
	}
	return nil
}

// Push implements the package-level Push.
func (Engine) Push(ctx context.Context, ref string, cred api.RegistryCredential, w io.Writer) (string, error) {
	host, repo, ok := splitPushRef(ref)
	if !ok {
		return "", fmt.Errorf("docker: %q is not a registry image name", ref)
	}
	if cred.Username == "" || cred.Password == "" || strings.ContainsAny(cred.Username, ":\r\n") {
		return "", errors.New("docker: the registry credential is incomplete")
	}
	if !strings.EqualFold(cred.Host(), host) {
		return "", errors.New("docker: the registry credential is for another registry")
	}

	sweepStaleLogins(os.TempDir(), time.Now())
	dir, err := writeThrowawayConfig(host, cred)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// A second Ctrl-C ends kx at once, before any defer runs, so the login
	// goes as soon as the first one arrives; Docker has read it by then.
	defer removeOnCancel(ctx, dir)()

	env, err := pushEnv(ctx, procenv.Environ(), dir)
	if err != nil {
		return "", err
	}
	var tail capture
	out := &lockedWriter{w: io.MultiWriter(w, &tail)}
	if err := run(ctx, env, out, out, "push", ref); err != nil {
		switch {
		case errors.Is(err, kxerr.ErrDockerMissing):
			return "", err
		case ctx.Err() != nil:
			return "", ctx.Err()
		case tagExistsRe.MatchString(tail.String()):
			// Before the credential check: registries word this "denied: …".
			return "", &kxerr.PushFailed{Err: kxerr.ErrTagExists}
		case unauthorizedRe.MatchString(tail.String()):
			return "", &kxerr.PushFailed{Err: ErrUnauthorized}
		}
		return "", &kxerr.PushFailed{Err: err}
	}
	if d := ParseDigest(tail.String()); d != "" {
		return d, nil
	}
	return inspectDigest(ctx, env, ref, repo), nil
}

// writeThrowawayConfig makes a private folder holding a config.json with
// the one registry login and returns its path. The caller removes it.
func writeThrowawayConfig(host string, cred api.RegistryCredential) (string, error) {
	dir, err := os.MkdirTemp("", loginPrefix)
	if err != nil {
		return "", fmt.Errorf("docker: cannot make a temporary folder: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && runtime.GOOS != "windows" { //nolint:gosec // G302: a folder needs x to be usable; 0700 is owner-only
		_ = os.RemoveAll(dir)
		return "", err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Password))
	cfg, err := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": auth}}})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("docker: cannot write the temporary login: %w", err)
	}
	return dir, nil
}

// removeOnCancel deletes dir as soon as ctx ends (kx was interrupted),
// without waiting for Docker to stop. The returned function ends the watch.
func removeOnCancel(ctx context.Context, dir string) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = os.RemoveAll(dir)
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// loginPrefix starts the name of every throwaway login folder.
const loginPrefix = "kx-docker-"

// staleLogin is older than any credential in it can be valid.
const staleLogin = time.Hour

// sweepStaleLogins removes throwaway login folders left in tmp by a kx
// that was killed outright. Only folders older than a credential's
// lifetime go, so another kx's push in progress is never touched.
func sweepStaleLogins(tmp string, now time.Time) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), loginPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < staleLogin {
			continue
		}
		_ = os.RemoveAll(filepath.Join(tmp, e.Name())) // fails harmlessly on other users' folders
	}
}

// pushEnv is the environment for a push with the throwaway config folder.
// DOCKER_HOST (and the TLS variables) stay as they are; only when
// DOCKER_HOST is unset are they filled from the active context, because
// the throwaway folder has no contexts. DOCKER_CONTEXT is dropped for the
// same reason. A non-default context kx can't read is an error: pushing
// to the default engine instead would push from the wrong engine.
func pushEnv(ctx context.Context, base []string, configDir string) ([]string, error) {
	env := without(base, "DOCKER_CONFIG", "DOCKER_CONTEXT")
	if lookup(base, "DOCKER_HOST") == "" {
		extra, err := contextEnv(ctx, base)
		if err != nil {
			return nil, err
		}
		for _, kv := range extra {
			k, _, _ := strings.Cut(kv, "=")
			if lookup(base, k) == "" {
				env = append(env, kv)
			}
		}
	}
	return append(env, "DOCKER_CONFIG="+configDir), nil
}

var contextHostRe = regexp.MustCompile(`^(unix|npipe|tcp|ssh|fd)://[^\s]{1,1000}$`)

// contextEnv reads the active Docker context with the person's own
// settings and returns DOCKER_HOST, plus DOCKER_CERT_PATH and
// DOCKER_TLS_VERIFY (or DOCKER_TLS when the context skips verification)
// for a TLS context.
func contextEnv(ctx context.Context, env []string) ([]string, error) {
	c, err := inspectContext(ctx, env)
	if err != nil {
		if name := currentContext(ctx, env); name != "" && name != "default" {
			return nil, &kxerr.DockerContextUnknown{Name: name}
		}
		return nil, nil
	}
	host := c.Host()
	if !contextHostRe.MatchString(host) {
		if c.Name != "" && c.Name != "default" {
			return nil, &kxerr.DockerContextUnknown{Name: c.Name}
		}
		return nil, nil
	}
	out := []string{"DOCKER_HOST=" + host}
	if strings.HasPrefix(host, "tcp://") && c.HasTLS() && c.Storage.TLSPath != "" {
		certDir := filepath.Join(c.Storage.TLSPath, "docker")
		if fi, err := os.Stat(certDir); err == nil && fi.IsDir() {
			out = append(out, "DOCKER_CERT_PATH="+certDir)
			if c.SkipTLSVerify() {
				out = append(out, "DOCKER_TLS=1")
			} else {
				out = append(out, "DOCKER_TLS_VERIFY=1")
			}
		}
	}
	return out, nil
}

// dockerContext is what docker context inspect --format '{{json .}}'
// says about a context.
type dockerContext struct {
	Name      string `json:"Name"`
	Endpoints struct {
		Docker struct {
			Host          string `json:"Host"`
			SkipTLSVerify bool   `json:"SkipTLSVerify"`
		} `json:"docker"`
	} `json:"Endpoints"`
	TLSMaterial map[string][]string `json:"TLSMaterial"`
	Storage     struct {
		TLSPath string `json:"TLSPath"`
	} `json:"Storage"`
}

func (c *dockerContext) Host() string        { return c.Endpoints.Docker.Host }
func (c *dockerContext) SkipTLSVerify() bool { return c.Endpoints.Docker.SkipTLSVerify }
func (c *dockerContext) HasTLS() bool        { return len(c.TLSMaterial["docker"]) > 0 }

func parseDockerContext(out string) (*dockerContext, error) {
	var c dockerContext
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func inspectContext(ctx context.Context, env []string) (*dockerContext, error) {
	cctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	var stdout, stderr capture
	if err := run(cctx, env, &stdout, &stderr, "context", "inspect", "--format", "{{json .}}"); err != nil {
		return nil, err
	}
	return parseDockerContext(stdout.String())
}

// currentContext is the active context's name, or "" if docker can't say.
func currentContext(ctx context.Context, env []string) string {
	cctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	var stdout, stderr capture
	if err := run(cctx, env, &stdout, &stderr, "context", "show"); err != nil {
		return ""
	}
	return lastLine(stdout.String())
}

func inspectDigest(ctx context.Context, env []string, ref, repo string) string {
	var stdout, stderr capture
	if err := run(ctx, env, &stdout, &stderr, "image", "inspect", "--format", "{{json .RepoDigests}}", ref); err != nil {
		return ""
	}
	var digests []string
	if json.Unmarshal([]byte(lastLine(stdout.String())), &digests) != nil {
		return ""
	}
	for _, rd := range digests {
		if d, ok := strings.CutPrefix(rd, repo+"@"); ok && digestRe.MatchString(d) {
			return d
		}
	}
	return ""
}

var (
	digestRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	pushDigestRe = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})(?:[^0-9A-Za-z]|$)`)
	// tagExistsRe finds a registry refusing to replace a tag. It needs the
	// word "tag" (or "immutable"): every push prints "Layer already exists".
	tagExistsRe    = regexp.MustCompile(`(?i)(immutable|tag[^\n]{0,120}already exists|already exists[^\n]{0,120}cannot be overwritten)`)
	unauthorizedRe = regexp.MustCompile(`(?i)(unauthorized|authentication required|access to the resource is denied|denied: )`)
	// refRe accepts lower-case image names with a tag, optionally starting
	// with a registry host (and port).
	refRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]+([._-]+[a-z0-9]+)*)*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// ParseDigest finds the digest in docker push output (the last
// "<tag>: digest: sha256:… size: …" line), or returns "".
func ParseDigest(out string) string {
	m := pushDigestRe.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

func validRef(ref string) bool { return len(ref) <= 512 && refRe.MatchString(ref) }

// splitPushRef splits host/path:tag into the host and host/path.
func splitPushRef(ref string) (host, repo string, ok bool) {
	if !validRef(ref) || !strings.Contains(ref, "/") {
		return "", "", false
	}
	repo = ref[:strings.LastIndex(ref, ":")]
	host, _, _ = strings.Cut(repo, "/")
	return host, repo, true
}

// run starts docker with args and env, waits for it, and maps a missing
// docker command to kxerr.ErrDockerMissing.
func run(ctx context.Context, env []string, stdout, stderr io.Writer, args ...string) error {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return kxerr.ErrDockerMissing
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = waitDelay
	return cmd.Run()
}

func lookup(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && sameKey(k, key) {
			return v
		}
	}
	return ""
}

func without(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
outer:
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		for _, drop := range keys {
			if sameKey(k, drop) {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

// sameKey compares environment variable names the way the OS does.
func sameKey(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// capture keeps the last captureBytes written to it.
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(p)
	if over := c.buf.Len() - captureBytes; over > 0 {
		c.buf.Next(over)
	}
	return len(p), nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// lockedWriter serialises writes from docker's stdout and stderr.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
