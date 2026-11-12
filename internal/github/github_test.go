package github

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
)

const (
	testSHA   = "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345"
	testToken = "ghs_test-only-github-token"
)

type statusCall struct {
	Path    string
	Auth    string
	Accept  string
	Version string
	Body    map[string]string
}

type fakeGitHub struct {
	mu     sync.Mutex
	calls  []statusCall
	status int
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.calls = append(f.calls, statusCall{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("X-GitHub-Api-Version"), body})
	code := f.status
	if code == 0 {
		code = http.StatusCreated
	}
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `{}`)
}

func (f *fakeGitHub) got() []statusCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]statusCall(nil), f.calls...)
}

type ghEnv map[string]string

func (e ghEnv) get(k string) string { return e[k] }

func newTestReporter(t *testing.T) (*Reporter, *fakeGitHub, ghEnv, *bytes.Buffer) {
	t.Helper()
	gh := &fakeGitHub{}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	env := ghEnv{
		"GITHUB_ACTIONS":      "true",
		"GITHUB_TOKEN":        testToken,
		"GITHUB_REPOSITORY":   "acme/shop-api",
		"GITHUB_SHA":          testSHA,
		"GITHUB_API_URL":      srv.URL,
		"GITHUB_OUTPUT":       filepath.Join(dir, "output"),
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
	}
	var out bytes.Buffer
	r := New(env.get, &out, "production", "kx/test (linux/amd64; GitHub Actions)")
	return r, gh, env, &out
}

func TestMasksRegistryCredentialCommand(t *testing.T) {
	r, _, _, out := newTestReporter(t)
	r.MaskCredential(api.RegistryCredential{RegistryURL: "registry.example.test/acme-corp", Username: "kx-push", Password: "secret%value"})
	want := "::add-mask::secret%25value\n::add-mask::a3gtcHVzaDpzZWNyZXQldmFsdWU=\n"
	if out.String() != want {
		t.Fatalf("mask output = %q\nwant %q", out.String(), want)
	}
}

func TestMask_SplitsLines(t *testing.T) {
	r, _, _, out := newTestReporter(t)
	r.Mask("first\r\nsecond\n")
	r.Mask("")
	if out.String() != "::add-mask::first\n::add-mask::second\n" {
		t.Fatalf("mask output = %q", out.String())
	}
}

func TestEscape(t *testing.T) {
	if got := EscapeData("50% done\r\nnext::x"); got != "50%25 done%0D%0Anext::x" {
		t.Errorf("EscapeData = %q", got)
	}
	if got := EscapeProperty("a:b,c%\n"); got != "a%3Ab%2Cc%25%0A" {
		t.Errorf("EscapeProperty = %q", got)
	}
}

func TestError_BothLinesOneCommand(t *testing.T) {
	r, _, _, out := newTestReporter(t)
	r.Error("shop-api v15 didn't go live: crash\n::add-mask::x", "See why: kx logs --deployment v15")
	want := "::error title=Kubernetix deploy::shop-api v15 didn't go live: crash%0A::add-mask::x%0ASee why: kx logs --deployment v15\n"
	if out.String() != want {
		t.Fatalf("error output = %q\nwant %q", out.String(), want)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatal("a newline in the message started a second workflow command")
	}
}

func TestWarning_OncePerMessage(t *testing.T) {
	r, _, _, out := newTestReporter(t)
	r.Warning("50% of\nsomething")
	r.Warning("50% of\nsomething")
	if out.String() != "::warning title=Kubernetix deploy::50%25 of%0Asomething\n" {
		t.Fatalf("warning output = %q", out.String())
	}
}

func TestSetStatus_Posts(t *testing.T) {
	r, gh, _, out := newTestReporter(t)
	if err := r.SetStatus(context.Background(), "pending", "Building", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.SetStatus(context.Background(), "success", "Live in 3m 14s", "https://app.example.test"); err != nil {
		t.Fatal(err)
	}
	calls := gh.got()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if c.Path != "/repos/acme/shop-api/statuses/"+testSHA || c.Auth != "Bearer "+testToken || c.Accept != "application/vnd.github+json" || c.Version != "2022-11-28" {
		t.Fatalf("request = %s auth-ok=%v accept=%s version=%s", c.Path, c.Auth == "Bearer "+testToken, c.Accept, c.Version)
	}
	if c.Body["state"] != "pending" || c.Body["description"] != "Building" || c.Body["context"] != "Kubernetix / production" {
		t.Fatalf("body = %v", c.Body)
	}
	if _, ok := c.Body["target_url"]; ok {
		t.Errorf("empty target_url was sent")
	}
	if calls[1].Body["state"] != "success" || calls[1].Body["description"] != "Live in 3m 14s" || calls[1].Body["target_url"] != "https://app.example.test" {
		t.Fatalf("body = %v", calls[1].Body)
	}
	if out.Len() != 0 {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestSetStatus_EnvironmentNamesContext(t *testing.T) {
	r, gh, _, _ := newTestReporter(t)
	r.Environment = "staging"
	_ = r.SetStatus(context.Background(), "pending", "Building", "")
	if got := gh.got()[0].Body["context"]; got != "Kubernetix / staging" {
		t.Fatalf("context = %q", got)
	}
}

func TestSetStatus_DescriptionCleanedAndCut(t *testing.T) {
	r, gh, _, _ := newTestReporter(t)
	long := "line one\nline two " + strings.Repeat("é", 200)
	_ = r.SetStatus(context.Background(), "failure", long, "javascript:alert(1)")
	b := gh.got()[0].Body
	if n := len([]rune(b["description"])); n > 140 {
		t.Errorf("description has %d characters, want at most 140", n)
	}
	if strings.ContainsAny(b["description"], "\r\n") {
		t.Errorf("description has a line break: %q", b["description"])
	}
	if _, ok := b["target_url"]; ok {
		t.Errorf("non-https target_url sent: %q", b["target_url"])
	}
}

func TestSetStatus_NoTokenWarnsOnceAndContinues(t *testing.T) {
	r, gh, env, out := newTestReporter(t)
	env["GITHUB_TOKEN"] = ""
	for i := 0; i < 3; i++ {
		if err := r.SetStatus(context.Background(), "pending", "Building", ""); err != nil {
			t.Fatalf("SetStatus() = %v; a missing token must not stop the deploy", err)
		}
	}
	if n := len(gh.got()); n != 0 {
		t.Fatalf("%d requests without a token", n)
	}
	if strings.Count(out.String(), "::warning") != 1 || !strings.Contains(out.String(), "GITHUB_TOKEN") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSetStatus_MissingPermissionBecomesWarning(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		r, gh, _, out := newTestReporter(t)
		gh.status = code
		for i := 0; i < 3; i++ {
			if err := r.SetStatus(context.Background(), "pending", "Building", ""); err != nil {
				t.Fatalf("SetStatus() = %v", err)
			}
		}
		if n := len(gh.got()); n != 1 {
			t.Errorf("status %d: %d requests, want 1 (then stop asking)", code, n)
		}
		if strings.Count(out.String(), "::warning") != 1 || !strings.Contains(out.String(), "statuses: write") {
			t.Errorf("status %d: output = %q", code, out.String())
		}
	}
}

func TestSetStatus_ServerErrorWarnsAndKeepsTrying(t *testing.T) {
	r, gh, _, out := newTestReporter(t)
	gh.status = http.StatusBadGateway
	_ = r.SetStatus(context.Background(), "pending", "Building", "")
	_ = r.SetStatus(context.Background(), "pending", "Deploying v15", "")
	if n := len(gh.got()); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	if strings.Count(out.String(), "::warning") != 1 {
		t.Errorf("output = %q", out.String())
	}
}

func TestSetStatus_RefusesUnsafeTargets(t *testing.T) {
	tests := map[string]string{
		"GITHUB_API_URL":    "http://api.example.test",
		"GITHUB_REPOSITORY": "acme/../../orgs/acme",
		"GITHUB_SHA":        "main",
	}
	for key, bad := range tests {
		r, gh, env, out := newTestReporter(t)
		env[key] = bad
		if err := r.SetStatus(context.Background(), "pending", "Building", ""); err != nil {
			t.Fatalf("%s: SetStatus() = %v", key, err)
		}
		if n := len(gh.got()); n != 0 {
			t.Errorf("%s=%q: sent %d requests", key, bad, n)
		}
		if !strings.Contains(out.String(), "::warning") {
			t.Errorf("%s=%q: no warning", key, bad)
		}
		if strings.Contains(out.String(), testToken) {
			t.Errorf("warning shows the GitHub token")
		}
	}
}

func TestSetStatus_DoesNotFollowRedirects(t *testing.T) {
	var hits int
	var mu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	r, _, env, _ := newTestReporter(t)
	env["GITHUB_API_URL"] = redirect.URL
	_ = r.SetStatus(context.Background(), "pending", "Building", "")
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatal("followed a redirect, sending the GitHub token to another host")
	}
}

func TestSetStatus_UnknownState(t *testing.T) {
	r, _, _, _ := newTestReporter(t)
	if err := r.SetStatus(context.Background(), "done", "x", ""); err == nil {
		t.Fatal("SetStatus accepted an unknown state")
	}
}

// parseOutputs reads a GITHUB_OUTPUT file the way the Actions runner does:
// "name=value" lines, or "name<<DELIM" followed by lines up to DELIM.
func parseOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if name, delim, ok := strings.Cut(line, "<<"); ok {
			var val []string
			closed := false
			for sc.Scan() {
				if sc.Text() == delim {
					closed = true
					break
				}
				val = append(val, sc.Text())
			}
			if !closed {
				t.Fatalf("output %q never closed", name)
			}
			out[name] = strings.Join(val, "\n")
			continue
		}
		if name, val, ok := strings.Cut(line, "="); ok {
			out[name] = val
			continue
		}
		if line != "" {
			t.Fatalf("invalid output line %q", line)
		}
	}
	return out
}

func TestSetOutputs(t *testing.T) {
	r, _, env, _ := newTestReporter(t)
	if err := os.WriteFile(env["GITHUB_OUTPUT"], []byte("earlier=kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := Outputs{URL: "https://app.example.test", DeploymentID: "dep-15", Version: "v15", Image: "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat("ab", 32)}
	if err := r.SetOutputs(o); err != nil {
		t.Fatal(err)
	}
	got := parseOutputs(t, env["GITHUB_OUTPUT"])
	want := map[string]string{"earlier": "kept", "url": o.URL, "deployment-id": o.DeploymentID, "version": o.Version, "image": o.Image}
	if len(got) != len(want) {
		t.Fatalf("outputs = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	raw, _ := os.ReadFile(env["GITHUB_OUTPUT"])
	if !strings.Contains(string(raw), "url<<") {
		t.Errorf("outputs not written in the heredoc form:\n%s", raw)
	}
}

func TestSetOutputs_RandomDelimiter(t *testing.T) {
	r, _, env, _ := newTestReporter(t)
	_ = r.SetOutputs(Outputs{URL: "a"})
	_ = r.SetOutputs(Outputs{URL: "b"})
	raw, _ := os.ReadFile(env["GITHUB_OUTPUT"])
	var delims []string
	for _, line := range strings.Split(string(raw), "\n") {
		if _, d, ok := strings.Cut(line, "<<"); ok {
			delims = append(delims, d)
		}
	}
	if len(delims) != 2 || delims[0] == delims[1] || len(delims[0]) < 20 {
		t.Fatalf("delimiters = %q, want two long random ones", delims)
	}
}

func TestSetOutputs_InjectionCannotAddOutputsOrCommands(t *testing.T) {
	r, _, env, out := newTestReporter(t)
	evil := "https://x.example.test\nversion=v999\nimage<<EOF\nevil\nEOF\n::add-mask::x\n::error::boom\nghadelimiter_1\n"
	if err := r.SetOutputs(Outputs{URL: evil, Version: "v15"}); err != nil {
		t.Fatal(err)
	}
	got := parseOutputs(t, env["GITHUB_OUTPUT"])
	if len(got) != 2 || got["url"] != strings.TrimSuffix(evil, "\n") && got["url"] != evil || got["version"] != "v15" {
		t.Fatalf("outputs = %q", got)
	}
	if _, ok := got["image"]; ok {
		t.Fatal("a value added an output")
	}
	if out.Len() != 0 {
		t.Fatalf("SetOutputs printed %q; outputs never go through workflow commands", out.String())
	}
}

func TestSetOutputs_NoFileIsFine(t *testing.T) {
	r, _, env, _ := newTestReporter(t)
	env["GITHUB_OUTPUT"] = ""
	if err := r.SetOutputs(Outputs{URL: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteSummary_Live(t *testing.T) {
	r, _, env, _ := newTestReporter(t)
	err := r.WriteSummary(Summary{
		App: "shop-api", Version: 15, URL: "https://app.example.test",
		Image: "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat("ab", 32), Commit: testSHA,
		Took: 3*time.Minute + 14*time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"])
	s := string(b)
	for _, want := range []string{"### shop-api v15 is live", "| URL | https://app.example.test |", "| Version | v15 |", "`registry.example.test/acme-corp/shop-api@sha256:", "| Commit | `3f9c2a1b7d4e` |", "| Took | 3m 14s |", "kx rollback --app shop-api"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
}

func TestWriteSummary_Failed(t *testing.T) {
	r, _, env, _ := newTestReporter(t)
	_ = r.WriteSummary(Summary{
		Failed: true, App: "shop-api", Version: 15,
		What: "shop-api v15 didn't go live: <script>|x. Version 14 is still serving.", Fix: "See why: kx logs --deployment v15",
		Note: "",
	})
	b, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"])
	s := string(b)
	if !strings.Contains(s, "### shop-api v15 didn't go live") || !strings.Contains(s, "See why: kx logs --deployment v15") {
		t.Fatalf("summary:\n%s", s)
	}
	if strings.Contains(s, "<script>") || strings.Contains(s, "|x") {
		t.Fatalf("untrusted text not escaped:\n%s", s)
	}
}

func TestWriteSummary_FailedBeforeDeploy(t *testing.T) {
	r, _, env, _ := newTestReporter(t)
	_ = r.WriteSummary(Summary{Failed: true, App: "shop-api", What: "Docker couldn't build your app.", Fix: "Fix the error shown above, then run kx deploy again.", Note: "Nothing was deployed, so what was live before is still live."})
	b, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"])
	s := string(b)
	if !strings.Contains(s, "### shop-api wasn't deployed") || !strings.Contains(s, "Nothing was deployed, so what was live before is still live.") {
		t.Fatalf("summary:\n%s", s)
	}
}
