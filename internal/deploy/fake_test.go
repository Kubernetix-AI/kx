package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/docker"
	"github.com/kubernetix-ai/kx/internal/ui"
)

// testTok is a public checksum test vector: well formed, never issued, so
// it unlocks nothing.
const (
	testAppID    = "6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c"
	testPassword = "test-only-registry-password"
	testDigest   = "sha256:4d3c2b1a00112233445566778899aabbccddeeff00112233445566778899aabb"
	testKey      = "2b1f5d8e-0c4a-5e7b-9f3d-1a2b3c4d5e6f"
	testTok      = "kx_0123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU"
)

type request struct {
	Method, Path, Key string
	Body              map[string]any
}

// fakeAPI is a scripted Kubernetix API for one org.
type fakeAPI struct {
	t  *testing.T
	mu sync.Mutex

	apps        map[string]*api.App
	deployments map[string][]api.Deployment // by app ID, newest first
	// statuses scripts what successive GETs of a new deployment return;
	// the last entry repeats.
	statuses []string
	gets     map[string]int
	// lists scripts successive GET /apps/{id}/deployments answers (for
	// WaitForPrevious); when exhausted, the stored deployments are listed.
	lists     [][]api.Deployment
	listCalls int

	usage      *api.Usage
	cred       api.RegistryCredential
	mints      int
	logs       map[string]api.DeploymentLogs
	logsStatus int // GET /deployments/{id}/logs answers this when set
	// runtimeStatus is what GET /apps/{id}/logs/runtime answers when set.
	runtimeStatus int
	createFail    []int    // statuses POST deployments answers before succeeding
	createErr     []string // [status, code, limit_name, reason (optional)] answered by POST deployments
	// createErrExtra adds fields to createErr's body.
	createErrExtra map[string]any
	getFail        int // GET /deployments/{id} answers this many 503s first
	// getLimited and listLimited answer that many GETs of a deployment, or
	// of the deployment list, with 429 and Retry-After: retryAfter seconds.
	getLimited, listLimited int
	retryAfter              string
	// mintLimits answers successive credential mints with 429 and these
	// Retry-After values, then mints.
	mintLimits []string
	// rollbackStatus, when set, is how every rollback POST is answered;
	// rollbackDelay holds the answer back.
	rollbackStatus int
	rollbackDelay  time.Duration
	failReason     string
	// rolledBackReason is the text a rolled_back status carries (the
	// platform's generic message while it decides what comes next).
	rolledBackReason string
	// onFailed runs when a new deployment first reports failed.
	onFailed    func(f *fakeAPI)
	failCode    string // the error_code a failed deployment carries
	failMessage string // the error_message (the worker's own text) it carries

	// created is the server's clock for new deployments' created_at;
	// health is the health check they carry; retries scripts retry_count
	// along successive GETs, like statuses (the last entry repeats).
	created time.Time
	health  *api.HealthCheck
	retries []int
	// replicas answers GET /apps/{id}/replicas (n counts from 0), or
	// replicasStatus when set; runtimeLogs is the log stream JSON.
	replicas       func(n int) []api.Replica
	replicaGets    int
	replicasStatus int
	runtimeLogs    string

	requests []request
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t: t,
		apps: map[string]*api.App{testAppID: {
			ID: testAppID, Name: "shop-api", URL: "https://" + testAppID + "--acme-corp.apps.example.test", Port: 8080,
		}},
		deployments: map[string][]api.Deployment{testAppID: {
			{ID: "dep-14", AppID: testAppID, Version: 14, Status: "ready", ImageURL: "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat("14", 32)},
			{ID: "dep-13", AppID: testAppID, Version: 13, Status: "ready", ImageURL: "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat("13", 32)},
		}},
		statuses: []string{"pending", "building", "ready"},
		gets:     map[string]int{},
		usage:    &api.Usage{OrgID: "acme-corp", Plan: "free", Usage: map[string]api.UsageItem{"deployments_today": {Current: 1, Limit: 5}}},
		cred:     api.RegistryCredential{RegistryURL: "registry.example.test/acme-corp", Username: "kx-push", Password: testPassword, ExpiresAt: time.Now().Add(time.Hour)},
		logs:     map[string]api.DeploymentLogs{},
		created:  time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	f.requests = append(f.requests, request{r.Method, path, r.Header.Get("Idempotency-Key"), body})
	if r.Header.Get("Authorization") != "Bearer "+testTok {
		writeJSON(w, 401, map[string]string{"error": "invalid_token"})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "apps":
		if a, ok := f.apps[parts[1]]; ok {
			writeJSON(w, 200, a)
			return
		}
		writeJSON(w, 404, map[string]string{"error": "app_not_found"})
	case r.Method == "POST" && path == "/apps":
		name, _ := body["name"].(string)
		for _, a := range f.apps {
			if a.Name == name {
				writeJSON(w, 409, map[string]string{"error": "duplicate_app"})
				return
			}
		}
		id := "new-app-id"
		port, _ := body["port"].(float64)
		f.apps[id] = &api.App{ID: id, Name: name, Port: int(port)}
		writeJSON(w, 201, f.apps[id])
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "apps" && parts[2] == "deployments" && f.listLimited > 0:
		f.listLimited--
		f.limited(w)
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "apps" && parts[2] == "deployments":
		f.listCalls++
		if f.listCalls <= len(f.lists) {
			writeJSON(w, 200, map[string]any{"deployments": f.lists[f.listCalls-1]})
			return
		}
		writeJSON(w, 200, map[string]any{"deployments": f.deployments[parts[1]]})
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "apps" && parts[2] == "deployments":
		if len(f.createFail) > 0 {
			code := f.createFail[0]
			f.createFail = f.createFail[1:]
			writeJSON(w, code, map[string]string{"error": "unavailable"})
			return
		}
		if f.createErr != nil {
			var code int
			fmt.Sscan(f.createErr[0], &code)
			body := map[string]any{"error": f.createErr[1], "limit_name": f.createErr[2], "message": "Plan limit reached"}
			if len(f.createErr) > 3 {
				body["reason"] = f.createErr[3]
			}
			for k, v := range f.createErrExtra {
				body[k] = v
			}
			writeJSON(w, code, body)
			return
		}
		appID := parts[1]
		if f.apps[appID] != nil {
			f.apps[appID].URL = "https://" + appID + "--acme-corp.apps.example.test"
		}
		for _, d := range f.deployments[appID] { // idempotent
			if d.ID == "key-"+r.Header.Get("Idempotency-Key") {
				writeJSON(w, 200, d)
				return
			}
		}
		img, _ := body["image_url"].(string)
		f.addDeployment(w, appID, "key-"+r.Header.Get("Idempotency-Key"), img)
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "deployments" && parts[2] == "rollback":
		if f.rollbackDelay > 0 {
			time.Sleep(f.rollbackDelay)
		}
		if f.rollbackStatus != 0 {
			writeJSON(w, f.rollbackStatus, map[string]string{"error": "unavailable"})
			return
		}
		target := f.find(parts[1])
		if target == nil {
			writeJSON(w, 404, map[string]string{"error": "deployment_not_found"})
			return
		}
		if target.Status != "ready" {
			writeJSON(w, 400, map[string]string{"error": "invalid_deployment_status"})
			return
		}
		f.addDeployment(w, target.AppID, "rb-"+parts[1], target.ImageURL)
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "deployments" && f.getLimited > 0:
		f.getLimited--
		f.limited(w)
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "deployments":
		if f.getFail > 0 {
			f.getFail--
			writeJSON(w, 503, map[string]string{"error": "unavailable"})
			return
		}
		d := f.find(parts[1])
		if d == nil {
			writeJSON(w, 404, map[string]string{"error": "deployment_not_found"})
			return
		}
		writeJSON(w, 200, d)
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "deployments" && parts[2] == "logs":
		if f.logsStatus != 0 {
			writeJSON(w, f.logsStatus, map[string]string{"error": "service_unavailable"})
			return
		}
		writeJSON(w, 200, f.logs[parts[1]])
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "apps" && parts[2] == "replicas":
		n := f.replicaGets
		f.replicaGets++
		switch {
		case f.replicasStatus != 0:
			writeJSON(w, f.replicasStatus, map[string]string{"error": "token_not_allowed"})
		case f.replicas != nil:
			writeJSON(w, 200, f.replicas(n))
		default:
			writeJSON(w, 200, []api.Replica{})
		}
	case r.Method == "GET" && len(parts) == 4 && parts[0] == "apps" && parts[3] == "runtime":
		if f.runtimeStatus != 0 {
			writeJSON(w, f.runtimeStatus, map[string]string{"error": "service_unavailable"})
			return
		}
		body := f.runtimeLogs
		if body == "" {
			body = `{"status":"success","data":{"resultType":"streams","result":[]}}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "orgs" && parts[2] == "usage":
		if f.usage == nil {
			writeJSON(w, 403, map[string]string{"error": "token_not_allowed"})
			return
		}
		writeJSON(w, 200, f.usage)
	case r.Method == "POST" && path == "/registry/credentials" && len(f.mintLimits) > 0:
		w.Header().Set("Retry-After", f.mintLimits[0])
		f.mintLimits = f.mintLimits[1:]
		writeJSON(w, 429, map[string]string{"error": "rate_limit_exceeded", "message": "Too many registry credentials", "token_prefix": "kx_01234567", "token_name": "ci"})
	case r.Method == "POST" && path == "/registry/credentials":
		f.mints++
		writeJSON(w, 200, f.cred)
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found"})
	}
}

// limited answers with the API's rate limit error.
func (f *fakeAPI) limited(w http.ResponseWriter) {
	if f.retryAfter != "" {
		w.Header().Set("Retry-After", f.retryAfter)
	}
	writeJSON(w, 429, map[string]string{"error": "rate_limit_exceeded", "message": "Too many requests"})
}

// addDeployment stores a new pending deployment and answers with it.
func (f *fakeAPI) addDeployment(w http.ResponseWriter, appID, id, image string) {
	v := 1
	if ds := f.deployments[appID]; len(ds) > 0 {
		v = ds[0].Version + 1
	}
	d := api.Deployment{ID: id, AppID: appID, Version: v, Status: "pending", ImageURL: image, Port: 8080, HealthCheck: f.health, CreatedAt: f.created}
	f.deployments[appID] = append([]api.Deployment{d}, f.deployments[appID]...)
	writeJSON(w, 201, d)
}

// find returns a deployment, advancing a new one along f.statuses.
func (f *fakeAPI) find(id string) *api.Deployment {
	for appID, ds := range f.deployments {
		for i := range ds {
			if ds[i].ID != id {
				continue
			}
			d := &f.deployments[appID][i]
			if strings.HasPrefix(id, "key-") || strings.HasPrefix(id, "rb-") {
				n := f.gets[id]
				f.gets[id]++
				if n >= len(f.statuses) {
					n = len(f.statuses) - 1
				}
				was := d.Status
				d.Status = f.statuses[n]
				switch d.Status {
				case "failed":
					d.CustomerError, d.ErrorCode, d.ErrorMessage = f.failReason, f.failCode, f.failMessage
					if was != "failed" && f.onFailed != nil {
						f.onFailed(f)
						d = f.find(id) // the list may have moved
					}
				case "rolled_back":
					d.CustomerError = f.rolledBackReason
				}
				if len(f.retries) > 0 {
					d.RetryCount = f.retries[min(n, len(f.retries)-1)]
				}
			}
			return d
		}
	}
	return nil
}

func (f *fakeAPI) reqs(method, prefix string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.requests {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// defaultHealth is the health check new apps get: GET / on the app's port,
// readiness after 10 s every 5 s, liveness after 30 s every 10 s, three
// failures each.
func defaultHealth() *api.HealthCheck {
	get := &api.HTTPGet{Path: "/", Port: 8080}
	return &api.HealthCheck{
		Liveness:  &api.Probe{HTTPGet: get, InitialDelaySeconds: 30, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3},
		Readiness: &api.Probe{HTTPGet: get, InitialDelaySeconds: 10, PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 3},
	}
}

// streams is the API's runtime log JSON: one stream per pod, lines in order.
func streams(byPod map[string][]string) string {
	var res []string
	ns := int64(1762938000000000000)
	for pod, lines := range byPod {
		var vals []string
		for _, l := range lines {
			ns += 1000
			b, _ := json.Marshal(l)
			vals = append(vals, fmt.Sprintf(`["%d",%s]`, ns, b))
		}
		label := `{}`
		if pod != "" {
			label = fmt.Sprintf(`{"pod":%q,"app":"x"}`, pod)
		}
		res = append(res, fmt.Sprintf(`{"stream":%s,"values":[%s]}`, label, strings.Join(vals, ",")))
	}
	return `{"status":"success","data":{"resultType":"streams","result":[` + strings.Join(res, ",") + `]}}`
}

// fakeBuilder records Docker calls.
type fakeBuilder struct {
	mu       sync.Mutex
	calls    []string
	checkErr error
	buildErr error
	buildOut string
	pushErrs []error // returned by successive pushes, then nil
	pushOut  string
	digest   string
	creds    []api.RegistryCredential
	onPush   func()
	tagErr   error
	labels   map[string]string // the labels of the last build
}

func (b *fakeBuilder) record(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, s)
}

func (b *fakeBuilder) Check(context.Context) error { b.record("check"); return b.checkErr }

func (b *fakeBuilder) Build(_ context.Context, o docker.BuildOptions, w io.Writer) error {
	b.record("build " + o.Tag + " " + o.Dir + " " + o.Dockerfile)
	b.mu.Lock()
	b.labels = o.Labels
	b.mu.Unlock()
	_, _ = io.WriteString(w, b.buildOut)
	return b.buildErr
}

func (b *fakeBuilder) Tag(_ context.Context, src, dst string) error {
	b.record("tag " + src + " " + dst)
	return b.tagErr
}

func (b *fakeBuilder) Push(_ context.Context, ref string, cred api.RegistryCredential, w io.Writer) (string, error) {
	b.record("push " + ref)
	if b.onPush != nil {
		b.onPush()
	}
	b.mu.Lock()
	b.creds = append(b.creds, cred)
	var err error
	if len(b.pushErrs) > 0 {
		err, b.pushErrs = b.pushErrs[0], b.pushErrs[1:]
	}
	b.mu.Unlock()
	_, _ = io.WriteString(w, b.pushOut)
	if err != nil {
		return "", err
	}
	if b.digest == "" {
		return testDigest, nil
	}
	if b.digest == "none" {
		return "", nil
	}
	return b.digest, nil
}

func (b *fakeBuilder) got() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// fakeStore is an in-memory CredentialStore.
type fakeStore struct {
	cred   *api.RegistryCredential
	saved  int
	forgot int
}

func (s *fakeStore) Load() (*api.RegistryCredential, bool) { return s.cred, s.cred != nil }
func (s *fakeStore) Save(c api.RegistryCredential) error   { s.saved++; s.cred = &c; return nil }
func (s *fakeStore) Forget()                               { s.forgot++; s.cred = nil }

// fakeClock advances by step on every Sleep.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	step   time.Duration
	sleeps []time.Duration
	hook   func(n int) error
}

func newClock(step time.Duration) *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC), step: step}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(c.step)
	n := len(c.sleeps)
	hook := c.hook
	c.mu.Unlock()
	if n > 500 {
		return errors.New("fake clock: slept over 500 times; a wait never ends")
	}
	if hook != nil {
		if err := hook(n); err != nil {
			return err
		}
	}
	return ctx.Err()
}

type env struct {
	t       *testing.T
	api     *fakeAPI
	client  *api.Client
	builder *fakeBuilder
	clock   *fakeClock
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	p       *ui.Printer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := newFakeAPI(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := api.New(srv.URL+"/api/v1", testTok)
	c.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	var out, errOut bytes.Buffer
	p := ui.New(strings.NewReader(""), &out, &errOut, func(string) string { return "" }, false)
	return &env{t: t, api: f, client: c, builder: &fakeBuilder{}, clock: newClock(0), out: &out, errOut: &errOut, p: p}
}

func (e *env) options() Options {
	return Options{
		App:            &api.App{ID: testAppID, Name: "shop-api"},
		Org:            "acme-corp",
		Build:          &Dockerfile{Dir: "/code/shop-api", Path: "/code/shop-api/Dockerfile", Port: 8080},
		Tag:            "3f9c2a1b7d4e",
		IdempotencyKey: testKey,
		Builder:        e.builder,
		Now:            e.clock.Now,
		Sleep:          e.clock.Sleep,
	}
}

func (e *env) run(o Options) (*Result, error) {
	e.t.Helper()
	res, err := Run(context.Background(), e.client, o, *e.p)
	e.checkNoSecret()
	return res, err
}

func (e *env) checkNoSecret() {
	e.t.Helper()
	if strings.Contains(e.out.String()+e.errOut.String(), testPassword) {
		e.t.Error("output contains the registry password")
	}
	if strings.Contains(e.out.String()+e.errOut.String(), testTok) {
		e.t.Error("output contains the whole token")
	}
}

// setDefaultSleep swaps the sleep WaitForPrevious uses and returns a
// function that restores it.
func setDefaultSleep(f func(context.Context, time.Duration) error) func() {
	old := defaultSleep
	defaultSleep = f
	return func() { defaultSleep = old }
}
