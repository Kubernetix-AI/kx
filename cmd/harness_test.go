package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/kubernetix-ai/kx/internal/docker"
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
	// tokRead and tokDeploy are well formed (valid checksum) but never
	// issued, so they unlock nothing.
	tokRead = "kx_readonlyreadonlyreadonlyreadonlyreadonly0CJuiU" // org-wide, read only
	// tokDeploy is the dashboard's deploy token: one app, Deploy, Push images, Read.
	tokDeploy = "kx_000000000000000000000000000000000000035600U3y3"

	regPassword = "test-only-registry-password"
	testDigest  = "sha256:4d3c2b1a00112233445566778899aabbccddeeff00112233445566778899aabb"

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
	depLogsFor  func(id string) (api.DeploymentLogs, bool) // consulted first
	tokens      []api.Token
	usage       api.Usage
	replicas    []api.Replica
	runtimeLogs func(q map[string]string) string // the API's log-stream JSON

	// Deploy side.
	regCred     api.RegistryCredential
	mints       int
	mintLimits  []string // successive mints answer 429 with these Retry-After values first
	newApps     []map[string]any
	deployPosts []deployPost
	rollbacks   []string
	statuses    []string // what successive GETs of a new deployment return; the last repeats
	failPosts   int      // POST /apps/{id}/deployments answers this many 503s first
	rollbackErr int      // POST /deployments/{id}/rollback answers with this status
	gets        map[string]int
	failReason  string

	requests   []string
	userAgents []string

	now time.Time // the server's clock, for token expiry
	// override, when it returns ok, answers a request (by bearer token)
	// before the normal routes, e.g. with an error the auth layer sends.
	override func(r *http.Request, tok string) (status int, body string, ok bool)
}

type deployPost struct {
	AppID, Key string
	Body       map[string]any
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
			tokDeploy: {Kind: "token", Org: api.Org{ID: "acme-corp"}, User: api.Principal{Email: "sam@acme.dev"}, Token: &api.Token{
				ID: "tok-deploy", OrgID: "acme-corp", Name: "shop-api deploy", Prefix: "kx_00000000", Scope: "app", AppID: appShopID, AppName: "shop-api",
				Permissions: []string{"deploy", "registry:push", "read"}, ExpiresAt: &dec11, Status: "active"}},
			tokRead: {Kind: "token", Org: api.Org{ID: "acme-corp"}, User: api.Principal{Email: "sam@acme.dev"}, Token: &api.Token{
				ID: "tok-read", OrgID: "acme-corp", Name: "viewer", Prefix: "kx_readonly", Scope: "org",
				Permissions: []string{"read"}, ExpiresAt: &dec11, Status: "active"}},
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
		regCred:  api.RegistryCredential{RegistryURL: "registry.kubernetix.ai/acme-corp", Username: "kx-push", Password: regPassword, ExpiresAt: nov12.Add(time.Hour)},
		statuses: []string{"pending", "building", "ready"},
		gets:     map[string]int{},
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
		if d := f.findDeployment(parts[1]); d != nil {
			writeJSON(w, 200, d)
			return
		}
		writeJSON(w, 404, map[string]string{"error": "deployment_not_found", "message": "Deployment not found"})
	case r.Method == "POST" && path == "/apps":
		f.createApp(w, r, appToken, who)
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "apps" && parts[2] == "deployments":
		f.createDeployment(w, r, parts[1])
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "deployments" && parts[2] == "rollback":
		f.rollback(w, r, parts[1])
	case r.Method == "POST" && path == "/registry/credentials":
		if who.Token != nil && !who.Token.Can(api.PermPush) {
			writeJSON(w, 403, map[string]string{"error": "token_not_allowed", "message": "no"})
			return
		}
		if len(f.mintLimits) > 0 {
			w.Header().Set("Retry-After", f.mintLimits[0])
			f.mintLimits = f.mintLimits[1:]
			writeJSON(w, 429, map[string]string{"error": "rate_limit_exceeded", "message": "Too many registry credentials"})
			return
		}
		f.mints++
		writeJSON(w, 200, f.regCred)
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "deployments" && parts[2] == "logs":
		if f.depLogsFor != nil {
			if l, ok := f.depLogsFor(parts[1]); ok {
				writeJSON(w, 200, l)
				return
			}
		}
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
		f.serveTokens(w, r, parts)
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found", "message": "no route"})
	}
}

func (f *fakeAPI) serveTokens(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == "GET" && len(parts) == 3:
		var list []api.Token
		for _, t := range f.tokens {
			if app := r.URL.Query().Get("app_id"); app == "" || t.AppID == app {
				list = append(list, t)
			}
		}
		writeJSON(w, 200, map[string]any{"tokens": list})
	case r.Method == "DELETE" && len(parts) == 4:
		w.WriteHeader(204)
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found", "message": "no route"})
	}
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

	docker *fakeDocker

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
		docker:    &fakeDocker{},
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
		Docker:         h.docker,
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.sleeps++
			if h.sleeps > 2000 {
				return errors.New("test harness: slept over 2000 times; a wait never ends")
			}
			if h.sleepHook != nil {
				return h.sleepHook(h.sleeps)
			}
			return ctx.Err()
		},
		PreparePrinter: func(p *ui.Printer) {
			p.InTTY, p.TTY = h.tty, h.tty
			// Spinners draw nothing during a test, so output is the same
			// on a slow machine; only their line-clearing on Stop remains.
			p.SpinEvery, p.HeartbeatEvery = time.Hour, time.Hour
			p.ReadPassword = func() (string, error) { return h.password, nil }
			if h.readPassword != nil {
				p.ReadPassword = h.readPassword
			}
		},
	}
	code := app.Run(ctx, args)
	all := h.out.String() + h.err.String()
	for _, tok := range []string{tokCorp, tokLabs, tokApp, tokCorp2, tokRead, tokDeploy} {
		if strings.Contains(all, tok) {
			h.t.Errorf("kx %v printed a whole token", args)
		}
	}
	for _, line := range strings.Split(all, "\n") {
		if strings.Contains(line, regPassword) && !strings.HasPrefix(line, "::add-mask::") {
			h.t.Errorf("kx %v printed the registry password outside ::add-mask::", args)
		}
	}
	return code
}

// norm replaces the test server's address with the public one and drops
// the line-clearing a terminal spinner leaves behind.
func (h *harness) norm(s string) string {
	s = strings.ReplaceAll(s, "\r\x1b[2K", "")
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

func (f *fakeAPI) createApp(w http.ResponseWriter, r *http.Request, appToken bool, who api.WhoAmI) {
	if appToken || (who.Token != nil && !who.Token.Can(api.PermDeploy)) {
		writeJSON(w, 403, map[string]string{"error": "token_not_allowed", "message": "no"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.newApps = append(f.newApps, body)
	name, _ := body["name"].(string)
	for _, a := range f.apps {
		if a.Name == name {
			writeJSON(w, 409, map[string]string{"error": "duplicate_app", "message": "App with this name already exists"})
			return
		}
	}
	port, _ := body["port"].(float64)
	app := api.App{ID: "7a7a7a7a-0000-4000-8000-000000000001", Name: name, Port: int(port), SourceType: "image"}
	f.apps = append(f.apps, app)
	writeJSON(w, 201, app)
}

func (f *fakeAPI) createDeployment(w http.ResponseWriter, r *http.Request, appID string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	key := r.Header.Get("Idempotency-Key")
	f.deployPosts = append(f.deployPosts, deployPost{appID, key, body})
	if f.failPosts > 0 {
		f.failPosts--
		writeJSON(w, 503, map[string]string{"error": "service_unavailable", "message": "try again"})
		return
	}
	for _, d := range f.deployments[appID] {
		if d.ID == "new-"+key {
			writeJSON(w, 200, d)
			return
		}
	}
	found := false
	for i := range f.apps {
		if f.apps[i].ID == appID {
			found = true
			f.apps[i].URL = "https://" + appID + "--acme-corp.apps.example.test"
		}
	}
	if !found {
		writeJSON(w, 404, map[string]string{"error": "app_not_found", "message": "App not found"})
		return
	}
	img, _ := body["image_url"].(string)
	writeJSON(w, 201, f.addDeployment(appID, "new-"+key, img))
}

func (f *fakeAPI) rollback(w http.ResponseWriter, r *http.Request, id string) {
	f.rollbacks = append(f.rollbacks, id)
	if f.rollbackErr != 0 {
		writeJSON(w, f.rollbackErr, map[string]string{"error": "service_unavailable", "message": "try again"})
		return
	}
	for appID, ds := range f.deployments {
		for _, d := range ds {
			if d.ID == id {
				if d.Status != "ready" {
					writeJSON(w, 400, map[string]string{"error": "invalid_deployment_status", "message": "not ready"})
					return
				}
				writeJSON(w, 202, f.addDeployment(appID, "new-rb-"+id, d.ImageURL))
				return
			}
		}
	}
	writeJSON(w, 404, map[string]string{"error": "deployment_not_found", "message": "Deployment not found"})
}

func (f *fakeAPI) addDeployment(appID, id, image string) api.Deployment {
	v := 1
	if ds := f.deployments[appID]; len(ds) > 0 {
		v = ds[0].Version + 1
	}
	d := api.Deployment{ID: id, AppID: appID, Version: v, Status: "pending", ImageURL: image, CreatedAt: f.now}
	f.deployments[appID] = append([]api.Deployment{d}, f.deployments[appID]...)
	return d
}

// findDeployment returns a deployment; ones kx created move along
// f.statuses on every read.
func (f *fakeAPI) findDeployment(id string) *api.Deployment {
	for appID, ds := range f.deployments {
		for i := range ds {
			if ds[i].ID != id {
				continue
			}
			d := &f.deployments[appID][i]
			if strings.HasPrefix(id, "new-") {
				n := min(f.gets[id], len(f.statuses)-1)
				f.gets[id]++
				d.Status = f.statuses[n]
				if d.Status == "failed" {
					d.CustomerError = f.failReason
				}
			}
			return d
		}
	}
	return nil
}

// fakeDocker stands in for Docker in command tests.
type fakeDocker struct {
	mu       sync.Mutex
	calls    []string
	checkErr error
	buildErr error
	pushErrs []error // returned by successive pushes, then success
	onPush   func(cred api.RegistryCredential)
}

func (d *fakeDocker) record(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, s)
}

func (d *fakeDocker) Check(context.Context) error { d.record("check"); return d.checkErr }

func (d *fakeDocker) Build(_ context.Context, o docker.BuildOptions, w io.Writer) error {
	d.record("build " + o.Tag)
	if d.buildErr != nil {
		_, _ = io.WriteString(w, "#5 [2/3] RUN npm ci\n#5 ERROR: npm ci failed\n")
	}
	return d.buildErr
}

func (d *fakeDocker) Tag(_ context.Context, src, dst string) error {
	d.record("tag " + src + " " + dst)
	return nil
}

func (d *fakeDocker) Push(_ context.Context, ref string, cred api.RegistryCredential, _ io.Writer) (string, error) {
	d.record("push " + ref)
	if d.onPush != nil {
		d.onPush(cred)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pushErrs) > 0 {
		err := d.pushErrs[0]
		d.pushErrs = d.pushErrs[1:]
		return "", err
	}
	return testDigest, nil
}

func (d *fakeDocker) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}
