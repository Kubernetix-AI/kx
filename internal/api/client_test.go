package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// Public checksum test vectors shared with the platform: well formed, never
// issued, so they unlock nothing.
const testToken = "kx_0123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU"

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/api/v1", testToken)
	c.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return c, srv
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestUserAgent(t *testing.T) {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	if got, want := UserAgent("1.0.0", ""), "kx/1.0.0 ("+plat+")"; got != want {
		t.Errorf("UserAgent = %q, want %q", got, want)
	}
	if got, want := UserAgent("1.0.0", "GitHub Actions"), "kx/1.0.0 ("+plat+"; GitHub Actions)"; got != want {
		t.Errorf("UserAgent = %q, want %q", got, want)
	}
}

func TestClient_SendsAuthAndHeaders(t *testing.T) {
	var got http.Header
	var path string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		writeJSON(w, 200, map[string]any{"id": "app-1", "name": "shop-api"})
	})
	c.UserAgent = "kx/1.0.0 (linux/amd64; GitLab CI)"
	app, err := c.GetApp(context.Background(), "app-1")
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "shop-api" || path != "/api/v1/apps/app-1" {
		t.Fatalf("app = %+v path = %s", app, path)
	}
	if got.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization header not sent as a bearer token")
	}
	if got.Get("User-Agent") != "kx/1.0.0 (linux/amd64; GitLab CI)" {
		t.Errorf("User-Agent = %q", got.Get("User-Agent"))
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q", got.Get("Accept"))
	}
}

func TestClient_EscapesPathSegments(t *testing.T) {
	var raw string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.EscapedPath()
		writeJSON(w, 404, map[string]string{"error": "app_not_found", "message": "App not found"})
	})
	_, _ = c.GetApp(context.Background(), "../orgs/x")
	if raw != "/api/v1/apps/..%2Forgs%2Fx" {
		t.Fatalf("path = %s", raw)
	}
}

func TestClient_RetriesIdempotentOnGatewayErrors(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var n atomic.Int32
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if n.Add(1) < 3 {
					w.WriteHeader(status)
					return
				}
				writeJSON(w, 200, map[string]any{"id": "app-1", "name": "shop-api"})
			})
			if _, err := c.GetApp(context.Background(), "app-1"); err != nil {
				t.Fatalf("GetApp: %v", err)
			}
			if n.Load() != 3 {
				t.Fatalf("requests = %d, want 3", n.Load())
			}
		})
	}
}

func TestClient_GivesUpAfterMaxRetries(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, 503, map[string]string{"error": "token_lookup_unavailable", "message": "try later"})
	})
	_, err := c.GetApp(context.Background(), "app-1")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "token_lookup_unavailable" {
		t.Fatalf("err = %v", err)
	}
	if int(n.Load()) != 1+c.MaxRetries {
		t.Fatalf("requests = %d, want %d", n.Load(), 1+c.MaxRetries)
	}
}

func TestClient_NoRetryOn500OrForPlainPOST(t *testing.T) {
	t.Run("500 on GET", func(t *testing.T) {
		var n atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(500)
		})
		_, _ = c.GetApp(context.Background(), "app-1")
		if n.Load() != 1 {
			t.Fatalf("requests = %d, want 1", n.Load())
		}
	})
	t.Run("503 on POST without a key", func(t *testing.T) {
		var n atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(503)
		})
		_, _ = c.RegistryCredentials(context.Background())
		if n.Load() != 1 {
			t.Fatalf("requests = %d, want 1", n.Load())
		}
	})
}

func TestClient_RetriesPOSTWithIdempotencyKeyReusingIt(t *testing.T) {
	var keys []string
	var bodies []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(keys) == 1 {
			w.WriteHeader(502)
			return
		}
		writeJSON(w, 202, map[string]any{"id": "dep-1", "version": 15, "status": "pending"})
	})
	key := "f1498b64-ba27-5f9a-8704-8b6910db9ac2"
	d, err := c.CreateDeployment(context.Background(), "app-1", CreateDeploymentRequest{ImageURL: "registry.kubernetix.ai/acme-corp/shop-api@sha256:abc"}, key)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 15 || len(keys) != 2 || keys[0] != key || keys[1] != key {
		t.Fatalf("deployment = %+v keys = %v", d, keys)
	}
	if bodies[0] != bodies[1] || !strings.Contains(bodies[1], `"image_url":"registry.kubernetix.ai/acme-corp/shop-api@sha256:abc"`) {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestClient_CreateDeploymentRequiresKey(t *testing.T) {
	c := New("https://platform.kubernetix.ai/api/v1", testToken)
	if _, err := c.CreateDeployment(context.Background(), "app-1", CreateDeploymentRequest{}, ""); err == nil {
		t.Fatal("CreateDeployment without an idempotency key returned no error")
	}
	if _, err := c.Rollback(context.Background(), "dep-1", ""); err == nil {
		t.Fatal("Rollback without an idempotency key returned no error")
	}
}

func TestClient_ParsesErrorEnvelope(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "360")
		writeJSON(w, 401, map[string]any{
			"error": "token_expired", "message": "Token expired",
			"token_prefix": "kx_01234567", "token_name": "laptop", "expires_at": "2027-02-09T00:00:00Z",
		})
	})
	c.Ctx = ErrContext{Org: "acme-corp", TokenSource: kxerr.SourceEnv}
	_, err := c.WhoAmI(context.Background())
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %T %v", err, err)
	}
	want := time.Date(2027, 2, 9, 0, 0, 0, 0, time.UTC)
	if e.Status != 401 || e.Code != "token_expired" || e.Message != "Token expired" || e.TokenPrefix != "kx_01234567" ||
		e.TokenName != "laptop" || e.TokenExpiresAt == nil || !e.TokenExpiresAt.Equal(want) || e.RetryAfter != 360*time.Second ||
		e.Method != "GET" || e.Path != "/whoami" || e.Ctx.Org != "acme-corp" || e.Ctx.TokenSource != kxerr.SourceEnv {
		t.Fatalf("Error = %+v", e)
	}
}

func TestClient_NonJSONErrorBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, "parse error at line 1\x1b[31m red")
	})
	_, err := c.RuntimeLogs(context.Background(), "app-1", LogQuery{Limit: 100})
	var e *Error
	if !errors.As(err, &e) || e.Status != 400 || e.Code != "" {
		t.Fatalf("err = %+v", err)
	}
	if strings.ContainsRune(e.Message, 0x1b) {
		t.Fatalf("message keeps terminal escapes: %q", e.Message)
	}
}

func TestClient_RefusesInsecureBaseURL(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()
	c := New(strings.Replace(srv.URL, "127.0.0.1", "example.com", 1), testToken)
	_, err := c.WhoAmI(context.Background())
	var ins *kxerr.InsecureURL
	if !errors.As(err, &ins) {
		t.Fatalf("err = %v, want InsecureURL", err)
	}
	if n.Load() != 0 {
		t.Fatal("request was sent")
	}
}

func TestClient_RefusesRedirects(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer other.Close()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	})
	_, err := c.WhoAmI(context.Background())
	if err == nil {
		t.Fatal("redirect was followed")
	}
	if leaked.Load() {
		t.Fatal("token sent to the redirect target")
	}
}

func TestClient_ErrorsNeverContainTheToken(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, map[string]string{"error": "invalid_token", "message": "bad " + testToken})
	})
	_, err := c.WhoAmI(context.Background())
	if err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(fmt.Sprintf("%+v", err), testToken) {
		t.Fatalf("error leaks the token: %v", err)
	}
	srv.Close()
	_, err = c.WhoAmI(context.Background())
	var ne *NetworkError
	if !errors.As(err, &ne) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("network error = %v", err)
	}
}

func TestClient_ContextCancelled(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.WhoAmI(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestListApps_Pages(t *testing.T) {
	var offsets []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		off := r.URL.Query().Get("offset")
		var apps []map[string]any
		n := 100
		if off == "100" {
			n = 3
		}
		for i := 0; i < n; i++ {
			apps = append(apps, map[string]any{"id": fmt.Sprintf("id-%s-%d", off, i), "name": fmt.Sprintf("app-%s-%d", off, i)})
		}
		writeJSON(w, 200, map[string]any{"apps": apps, "total": 103, "limit": 100, "offset": off})
	})
	apps, err := c.ListApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 103 || strings.Join(offsets, ",") != "0,100" {
		t.Fatalf("apps = %d offsets = %v", len(apps), offsets)
	}
}

func TestWhoAmI_Shapes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"objects", `{"kind":"token","org":{"id":"acme-corp","name":"Acme"},"user":{"id":"u1","email":"sam@acme.dev"},"token":{"id":"t1","token_prefix":"kx_01234567","name":"laptop","scope":"org","permissions":["deploy","read","registry:push"],"expires_at":"2026-12-11T09:00:00Z","status":"active"},"registry_url":"registry.kubernetix.ai/acme-corp"}`},
		{"strings", `{"kind":"token","org":"acme-corp","user":"sam@acme.dev","token":{"id":"t1","token_prefix":"kx_01234567","name":"laptop","scope":"org","permissions":["deploy","read","registry:push"],"expires_at":"2026-12-11T09:00:00Z","status":"active"},"registry_url":"registry.kubernetix.ai/acme-corp"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			})
			who, err := c.WhoAmI(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if who.Org.ID != "acme-corp" || who.User.Email != "sam@acme.dev" || who.Token == nil ||
				who.Token.ID != "t1" || !who.Token.Can(PermDeploy) || who.Token.ExpiresAt == nil {
				t.Fatalf("WhoAmI = %+v token=%+v", who, who.Token)
			}
		})
	}
}

func TestCreateTokenRequest_JSON(t *testing.T) {
	tests := []struct {
		req  CreateTokenRequest
		want string
	}{
		{CreateTokenRequest{Name: "ci", Permissions: []string{"read"}}, `{"name":"ci","permissions":["read"]}`},
		{CreateTokenRequest{Name: "ci", AppID: "a1", Permissions: []string{"deploy"}, ExpiresInDays: 30}, `{"name":"ci","app_id":"a1","permissions":["deploy"],"expires_in_days":30}`},
		{CreateTokenRequest{Name: "ci", Permissions: []string{"read"}, NeverExpires: true}, `{"name":"ci","permissions":["read"],"expires_in_days":null}`},
	}
	for _, tt := range tests {
		b, err := json.Marshal(tt.req)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tt.want {
			t.Errorf("json = %s, want %s", b, tt.want)
		}
	}
}

func TestTokenEndpoints(t *testing.T) {
	var seen []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch r.Method {
		case "POST":
			writeJSON(w, 201, map[string]any{"id": "t2", "token_prefix": "kx_zzzzzzzz", "name": "ci", "scope": "app", "app_id": "a1", "permissions": []string{"deploy"}, "status": "active", "token": "kx_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz2x81PZ"})
		case "GET":
			writeJSON(w, 200, map[string]any{"tokens": []map[string]any{{"id": "t2", "token_prefix": "kx_zzzzzzzz", "status": "active"}}})
		case "DELETE":
			w.WriteHeader(204)
		}
	})
	ctx := context.Background()
	created, err := c.CreateToken(ctx, "acme-corp", CreateTokenRequest{Name: "ci", AppID: "a1", Permissions: []string{"deploy"}})
	if err != nil || created.Secret == "" || created.ID != "t2" {
		t.Fatalf("CreateToken = %+v, %v", created, err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %s", created, created, created), created.Secret) {
		t.Fatal("CreatedToken formatting shows the secret")
	}
	list, err := c.ListTokens(ctx, "acme-corp", "a1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTokens = %v, %v", list, err)
	}
	if err := c.RevokeToken(ctx, "acme-corp", "t2"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /api/v1/orgs/acme-corp/tokens", "GET /api/v1/orgs/acme-corp/tokens?app_id=a1", "DELETE /api/v1/orgs/acme-corp/tokens/t2"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %v", seen)
	}
}

func TestRegistryCredential_NeverFormatsPassword(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"registry_url": "registry.kubernetix.ai/acme-corp", "username": "kx", "password": "test-registry-password", "expires_at": "2026-11-12T10:00:00Z"})
	})
	cred, err := c.RegistryCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "kx" || cred.Password != "test-registry-password" || cred.Host() != "registry.kubernetix.ai" {
		t.Fatalf("cred user = %q, host = %q", cred.Username, cred.Host())
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{cred, *cred} {
			if s := fmt.Sprintf(verb, v); strings.Contains(s, "test-registry-password") {
				t.Fatalf("%s shows the password: %s", verb, s)
			}
		}
	}
}

func TestRuntimeLogs_MergesStreamsOldestFirst(t *testing.T) {
	var q map[string]string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = map[string]string{}
		for k := range r.URL.Query() {
			q[k] = r.URL.Query().Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"pod":"a"},"values":[["1762855203000000000","third"],["1762855201000000000","first"]]},
			{"stream":{"pod":"b"},"values":[["1762855202000000000","second",{"k":"v"}]]}]}}`)
	})
	start := time.Unix(1762855200, 0)
	lines, err := c.RuntimeLogs(context.Background(), "app-1", LogQuery{Limit: 100, Start: start, Forward: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.Line)
	}
	if strings.Join(got, ",") != "first,second,third" {
		t.Fatalf("lines = %v", got)
	}
	if lines[0].NS != 1762855201000000000 || !lines[0].Time.Equal(time.Unix(0, 1762855201000000000)) {
		t.Fatalf("first line = %+v", lines[0])
	}
	if q["limit"] != "100" || q["start"] != "1762855200000000000" || q["direction"] != "forward" {
		t.Fatalf("query = %v", q)
	}
}

func TestUsage_DeploysToday(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orgs/acme-corp/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"org_id": "acme-corp", "plan": "free", "usage": map[string]any{"deployments_today": map[string]int{"current": 1, "limit": 5}}})
	})
	u, err := c.Usage(context.Background(), "acme-corp")
	if err != nil {
		t.Fatal(err)
	}
	used, limit, ok := u.DeploysToday()
	if !ok || used != 1 || limit != 5 || u.Plan != "free" {
		t.Fatalf("DeploysToday = %d, %d, %v", used, limit, ok)
	}
}

func TestDeploymentPhase(t *testing.T) {
	for status, want := range map[string]string{
		"pending": "queued", "claimed": "queued", "building": "starting", "deploying": "starting",
		"promoting": "starting", "ready": "live", "failed": "failed", "rolled_back": "rolled back", "weird": "weird",
	} {
		if got := (Deployment{Status: status}).Phase(); got != want {
			t.Errorf("Phase(%s) = %s, want %s", status, got, want)
		}
	}
	if !(Deployment{Status: "ready"}).Done() || (Deployment{Status: "building"}).Done() {
		t.Error("Done() wrong")
	}
}

func FuzzParseErrorBody(f *testing.F) {
	f.Add(401, []byte(`{"error":"token_expired","message":"m","token_prefix":"kx_01234567","token_name":"n","expires_at":"2027-02-09T00:00:00Z"}`))
	f.Add(402, []byte(`{"error":"tier_limit_exceeded","message":"m","limit_name":"max_deployments_per_day"}`))
	f.Add(400, []byte(`{"error":{"code":"x"}}`))
	f.Add(500, []byte(`not json`))
	f.Add(429, []byte(``))
	f.Add(403, []byte(`{"error":"x","message":"`+testToken+`"}`))
	f.Fuzz(func(t *testing.T, status int, body []byte) {
		e := parseErrorBody(status, body, http.Header{"Retry-After": []string{"60"}})
		if e == nil {
			t.Fatal("nil error")
		}
		if strings.Contains(e.Message, testToken) {
			t.Fatal("message keeps a token")
		}
		for _, r := range e.Message + e.Code + e.TokenName + e.TokenPrefix {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Fatalf("control or format character %q survived", r)
			}
		}
		if len(e.Message) > 1024 || len(e.Code) > 64 || len(e.TokenPrefix) > 11 {
			t.Fatal("field not bounded")
		}
		_ = Explain(e)
	})
}
