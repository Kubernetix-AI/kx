package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/ui"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

// Test tokens: the public checksum test vectors shared with the platform.
// They are well formed but were never issued, so they unlock nothing.
const (
	tokCorp  = "kx_0123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU"
	tokLabs  = "kx_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz2x81PZ"
	tokApp   = "kx_00000000000000000000000000000000000000002kaqcA"
	tokCorp2 = "kx_33333333333333333333333333333333333333330oWRgv"

	appShopID = "6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c"
	appWebID  = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
)

var (
	dec11 = time.Date(2026, 12, 11, 9, 0, 0, 0, time.UTC)
	nov12 = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
)

// fakeAPI is an in-memory Kubernetix API.
type fakeAPI struct {
	t  *testing.T
	mu sync.Mutex

	who         map[string]api.WhoAmI // by bearer token
	apps        []api.App
	deployments map[string][]api.Deployment // by app ID
	depLogs     map[string]api.DeploymentLogs
	tokens      []api.Token
	usage       api.Usage
	replicas    []api.Replica
	runtimeLogs func(q map[string]string) string // the API's log-stream JSON
	created     []map[string]any

	requests   []string
	userAgents []string

	now time.Time // the server's clock, for token expiry
	// override, when it returns ok, answers a request (by bearer token)
	// before the normal routes, e.g. with an error the auth layer sends.
	override func(r *http.Request, tok string) (status int, body string, ok bool)
}

func orgToken(id, prefix, org string, exp *time.Time) *api.Token {
	return &api.Token{ID: id, OrgID: org, Name: "laptop", Prefix: prefix, Scope: "org",
		Permissions: []string{"deploy", "read", "registry:push"}, ExpiresAt: exp, Status: "active"}
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t: t,
		who: map[string]api.WhoAmI{
			tokCorp: {Kind: "token", Org: api.Org{ID: "acme-corp"}, User: api.Principal{Email: "sam@acme.dev"}, Token: orgToken("tok-corp", "kx_01234567", "acme-corp", &dec11)},
			tokLabs: {Kind: "token", Org: api.Org{ID: "acme-labs"}, User: api.Principal{Email: "sam@acme.dev"}, Token: orgToken("tok-labs", "kx_zzzzzzzz", "acme-labs", nil)},
			tokApp: {Kind: "token", Org: api.Org{ID: "acme-corp"}, User: api.Principal{Email: "sam@acme.dev"}, Token: &api.Token{
				ID: "tok-app", OrgID: "acme-corp", Name: "github", Prefix: "kx_00000000", Scope: "app", AppID: appShopID, AppName: "shop-api",
				Permissions: []string{"deploy", "read"}, ExpiresAt: &dec11, Status: "active"}},
			tokCorp2: {Kind: "token", Org: api.Org{ID: "acme-corp"}, User: api.Principal{Email: "sam@acme.dev"}, Token: orgToken("tok-corp-2", "kx_33333333", "acme-corp", &dec11)},
		},
		now: nov12,
		apps: []api.App{
			{ID: appShopID, Name: "shop-api", Status: "ready", URL: "https://" + appShopID + "--acme-corp.apps.example.test", LatestReleaseVersion: 15, Port: 8080, Replicas: 2},
			{ID: appWebID, Name: "web", Status: "failed", LatestReleaseVersion: 1, Port: 3000, Replicas: 1},
		},
		deployments: map[string][]api.Deployment{
			appShopID: {
				{ID: "dep-15", AppID: appShopID, Version: 15, Status: "failed", CustomerError: "your container is crashing on start"},
				{ID: "dep-14", AppID: appShopID, Version: 14, Status: "ready"},
				{ID: "dep-13", AppID: appShopID, Version: 13, Status: "ready"},
			},
		},
		depLogs: map[string]api.DeploymentLogs{
			"dep-15": {DeploymentID: "dep-15", AppID: appShopID, Status: "failed", BuildLogs: "", DeploymentLogs: "Error: listen EADDRINUSE :8080\nexit 1\n"},
		},
		usage:    api.Usage{OrgID: "acme-corp", Plan: "free", Usage: map[string]api.UsageItem{"deployments_today": {Current: 1, Limit: 5}}},
		replicas: []api.Replica{{Name: "a", Status: "Running", Ready: true}, {Name: "b", Status: "Running", Ready: true}},
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.userAgents = append(f.userAgents, r.Header.Get("User-Agent"))

	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if f.override != nil {
		if status, body, ok := f.override(r, tok); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body+"\n")
			return
		}
	}
	who, ok := f.who[tok]
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "invalid_token", "message": "Invalid token"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("User-Agent"), "kx/") {
		f.t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	appToken := who.Token != nil && who.Token.Scope == "app"

	switch {
	case r.Method == "GET" && path == "/whoami":
		writeJSON(w, 200, who)
	case r.Method == "GET" && path == "/apps":
		if appToken {
			writeJSON(w, 403, map[string]string{"error": "token_not_allowed", "message": "no"})
			return
		}
		writeJSON(w, 200, map[string]any{"apps": f.apps, "total": len(f.apps)})
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "apps":
		for _, a := range f.apps {
			if a.ID == parts[1] {
				writeJSON(w, 200, a)
				return
			}
		}
		writeJSON(w, 404, map[string]string{"error": "app_not_found", "message": "App not found"})
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "apps" && parts[2] == "deployments":
		writeJSON(w, 200, map[string]any{"deployments": f.deployments[parts[1]], "count": len(f.deployments[parts[1]])})
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "apps" && parts[2] == "replicas":
		writeJSON(w, 200, f.replicas)
	case r.Method == "GET" && len(parts) == 4 && parts[0] == "apps" && parts[2] == "logs" && parts[3] == "runtime":
		q := map[string]string{}
		for k := range r.URL.Query() {
			q[k] = r.URL.Query().Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		body := `{"status":"success","data":{"resultType":"streams","result":[]}}`
		if f.runtimeLogs != nil {
			body = f.runtimeLogs(q)
		}
		_, _ = io.WriteString(w, body)
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "deployments":
		for _, ds := range f.deployments {
			for _, d := range ds {
				if d.ID == parts[1] {
					writeJSON(w, 200, d)
					return
				}
			}
		}
		writeJSON(w, 404, map[string]string{"error": "deployment_not_found", "message": "Deployment not found"})
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "deployments" && parts[2] == "logs":
		if l, ok := f.depLogs[parts[1]]; ok {
			writeJSON(w, 200, l)
			return
		}
		writeJSON(w, 404, map[string]string{"error": "deployment_not_found", "message": "Deployment not found"})
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "orgs" && parts[2] == "usage":
		writeJSON(w, 200, f.usage)
	case len(parts) >= 3 && parts[0] == "orgs" && parts[2] == "tokens":
		if parts[1] != who.Org.ID {
			writeJSON(w, 403, map[string]string{"error": "insufficient_permissions", "message": "no"})
			return
		}
		f.serveTokens(w, r, parts, who.Token)
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found", "message": "no route"})
	}
}

func (f *fakeAPI) serveTokens(w http.ResponseWriter, r *http.Request, parts []string, caller *api.Token) {
	switch {
	case r.Method == "GET" && len(parts) == 3:
		var list []api.Token
		for _, t := range f.tokens {
			if app := r.URL.Query().Get("app_id"); app == "" || t.AppID == app {
				list = append(list, t)
			}
		}
		writeJSON(w, 200, map[string]any{"tokens": list})
	case r.Method == "POST" && len(parts) == 3:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		exp, refusal := f.decision5(caller, body)
		if refusal != "" {
			// The platform's body for a refusal, naming the calling token.
			writeJSON(w, 403, map[string]string{"error": "token_not_allowed", "message": refusal, "token_prefix": caller.Prefix, "token_name": caller.Name})
			return
		}
		view := map[string]any{"id": "tok-new", "org_id": parts[1], "name": body["name"], "token_prefix": "kx_zzzzzzzz",
			"scope": "org", "permissions": body["permissions"], "status": "active", "token": tokLabs,
			"expires_at": exp.Format(time.RFC3339), "created_at": f.now.Format(time.RFC3339)}
		if id, ok := body["app_id"].(string); ok && id != "" {
			view["scope"], view["app_id"], view["app_name"] = "app", id, "shop-api"
		}
		writeJSON(w, 201, view)
	case r.Method == "DELETE" && len(parts) == 4:
		w.WriteHeader(204)
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found", "message": "no route"})
	}
}

// decision5 applies the platform's rules for a token that makes a token:
// narrower only, within the caller's permissions, app and expiry, never
// "never", and not from a token that was itself made by a token. It
// returns the new token's expiry, or the platform's refusal message.
func (f *fakeAPI) decision5(caller *api.Token, body map[string]any) (time.Time, string) {
	if caller.ParentTokenID != "" {
		return time.Time{}, "A token made by another token can't make tokens."
	}
	perms, _ := body["permissions"].([]any)
	for _, p := range perms {
		if !caller.Can(p.(string)) {
			return time.Time{}, "A token can only make tokens with permissions it has itself."
		}
	}
	if app, _ := body["app_id"].(string); caller.AppScoped() && app != caller.AppID {
		return time.Time{}, "This token can only make tokens for its own app; set app_id to " + caller.AppID + "."
	}
	days, set := body["expires_in_days"]
	if set && days == nil {
		return time.Time{}, "A token made by another token must expire, within a year and no later than the token that makes it."
	}
	exp := f.now.Add(90 * 24 * time.Hour)
	if n, ok := days.(float64); ok {
		exp = f.now.Add(time.Duration(n) * 24 * time.Hour)
	}
	if caller.ExpiresAt != nil && exp.After(*caller.ExpiresAt) {
		if set {
			return time.Time{}, "A token can't outlive the token that makes it, which expires on " + caller.ExpiresAt.UTC().Format("2 Jan 2006 15:04 MST") + "."
		}
		exp = caller.ExpiresAt.UTC()
	}
	return exp, ""
}

func (f *fakeAPI) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type harness struct {
	t         *testing.T
	api       *fakeAPI
	srv       *httptest.Server
	env       map[string]string
	dir       string
	configDir string
	now       time.Time

	stdin        string
	tty          bool
	password     string
	readPassword func() (string, error) // overrides password when set
	canOpen      bool
	opened       []string

	sleeps    int
	sleepHook func(n int) error

	out, err bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := newFakeAPI(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	h := &harness{
		t: t, api: f, srv: srv,
		dir:       filepath.Join(root, "code", "shop-api"),
		configDir: filepath.Join(root, "config"),
		now:       nov12,
		canOpen:   true,
	}
	if err := os.MkdirAll(filepath.Join(h.dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env = map[string]string{
		"KX_API_URL":    srv.URL + "/api/v1",
		"KX_CONFIG_DIR": h.configDir,
	}
	return h
}

func (h *harness) run(args ...string) int {
	return h.runCtx(context.Background(), args...)
}

func (h *harness) runCtx(ctx context.Context, args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	app := &App{
		Stdin:          strings.NewReader(h.stdin),
		Stdout:         &h.out,
		Stderr:         &h.err,
		Getenv:         func(k string) string { return h.env[k] },
		Dir:            h.dir,
		Now:            func() time.Time { return h.now },
		OpenBrowser:    func(u string) error { h.opened = append(h.opened, u); return nil },
		CanOpenBrowser: func() bool { return h.canOpen },
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.sleeps++
			if h.sleepHook != nil {
				return h.sleepHook(h.sleeps)
			}
			return ctx.Err()
		},
		PreparePrinter: func(p *ui.Printer) {
			p.InTTY, p.TTY = h.tty, h.tty
			p.ReadPassword = func() (string, error) { return h.password, nil }
			if h.readPassword != nil {
				p.ReadPassword = h.readPassword
			}
		},
	}
	code := app.Run(ctx, args)
	all := h.out.String() + h.err.String()
	for _, tok := range []string{tokCorp, tokLabs, tokApp} {
		creating := len(args) >= 2 && args[0] == "token" && args[1] == "create"
		if strings.Contains(all, tok) && !creating {
			h.t.Errorf("kx %v printed a whole token", args)
		}
	}
	return code
}

// norm replaces the test server's address with the public one.
func (h *harness) norm(s string) string {
	s = strings.ReplaceAll(s, h.srv.URL, "https://platform.kubernetix.ai")
	return strings.ReplaceAll(s, strings.TrimPrefix(h.srv.URL, "http://"), "platform.kubernetix.ai")
}

func (h *harness) user() *config.User {
	h.t.Helper()
	u, err := config.LoadUser(filepath.Join(h.configDir, "config.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) saveUser(u *config.User) {
	h.t.Helper()
	if err := config.SaveUser(filepath.Join(h.configDir, "config.json"), u); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) loginAs(org, tok string, exp *time.Time) {
	u := h.user()
	u.AddLogin(org, config.OrgLogin{Token: tok, ExpiresAt: exp, User: "sam@acme.dev"})
	h.saveUser(u)
}

func (h *harness) link(app, appID, org string) {
	h.t.Helper()
	if _, err := config.SaveProject(h.dir, config.Project{App: app, AppID: appID, Org: org}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) expect(code, want int) {
	h.t.Helper()
	if code != want {
		h.t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, want, h.out.String(), h.err.String())
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s (run go test ./cmd -update): %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func logsJSON(lines ...[2]string) string {
	var vals []string
	for _, l := range lines {
		b, _ := json.Marshal(l[1])
		vals = append(vals, fmt.Sprintf(`["%s",%s]`, l[0], b))
	}
	return `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[` + strings.Join(vals, ",") + `]}]}}`
}

type apiToken = api.Token
