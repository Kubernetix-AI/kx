package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
)

const (
	testPassword = "test-only-registry-password"
	testDigest   = "sha256:4d3c2b1a00112233445566778899aabbccddeeff00112233445566778899aabb"
	testToken    = "test-only-bearer-token"
)

// fakeRegistry is a registry with token auth: a manifest request without
// a token gets a 401 whose challenge points at /v2/token on the same host.
type fakeRegistry struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
	scopes   []string
	accepts  []string

	manifests   map[string]string // "org/repo:tag" → manifest body
	noDigest    bool              // HEAD answers without Docker-Content-Digest
	badDigest   bool              // Docker-Content-Digest isn't a sha256 digest
	realm       string            // overrides the challenge's realm
	basic       bool              // challenge with Basic instead of Bearer
	tokenStatus int               // answer to /v2/token, when not 200
	tokenField  string            // "token" (default) or "access_token"
	status      int               // answer to every authorized manifest request, when set
	anon404     bool              // manifest requests without a credential get 404, not 401
	anonymous   bool              // no credential needed at all
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{t: t, manifests: map[string]string{}}
	f.srv = httptest.NewTLSServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func (f *fakeRegistry) cred() api.RegistryCredential {
	return api.RegistryCredential{RegistryURL: f.host() + "/acme-corp", Username: "oauth2accesstoken", Password: testPassword, ExpiresAt: time.Now().Add(time.Hour)}
}

func (f *fakeRegistry) client() Client { return Client{HTTP: f.srv.Client()} }

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/v2/token" {
		user, pass, ok := r.BasicAuth()
		f.scopes = append(f.scopes, r.URL.Query().Get("scope")+" service="+r.URL.Query().Get("service"))
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			return
		}
		if !ok || user != "oauth2accesstoken" || pass != testPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		field := f.tokenField
		if field == "" {
			field = "token"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{%q:%q,"expires_in":300}`, field, testToken)
		return
	}
	authorized := f.anonymous || r.Header.Get("Authorization") == "Bearer "+testToken
	if f.basic {
		user, pass, okBasic := r.BasicAuth()
		authorized = okBasic && user == "oauth2accesstoken" && pass == testPassword
	}
	challenge := func() {
		realm := f.realm
		if realm == "" {
			realm = f.srv.URL + "/v2/token"
		}
		if f.basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		} else {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service=%q`, realm, f.host()))
		}
		w.WriteHeader(http.StatusUnauthorized)
	}
	if r.URL.Path == "/v2/" {
		if !authorized && r.Header.Get("Authorization") == "" {
			challenge()
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	name, tag, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !authorized {
		if f.anon404 && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		challenge()
		return
	}
	f.accepts = append(f.accepts, r.Header.Get("Accept"))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	body, found := f.manifests[name+":"+tag]
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sum := sha256.Sum256([]byte(body))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	switch {
	case f.badDigest:
		w.Header().Set("Docker-Content-Digest", "md5:abc")
	case !f.noDigest || r.Method == http.MethodGet:
		w.Header().Set("Docker-Content-Digest", digest)
	}
	w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeRegistry) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestDigest_TokenAuth(t *testing.T) {
	f := newFakeRegistry(t)
	const body = `{"schemaVersion":2}`
	f.manifests["acme-corp/shop-api:0.0.11"] = body
	got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got != digestOf(body) {
		t.Fatalf("Digest() = %q, %v; want %q", got, err, digestOf(body))
	}
	want := []string{"GET /v2/", "GET /v2/token", "HEAD /v2/acme-corp/shop-api/manifests/0.0.11"}
	if strings.Join(f.got(), "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant\n%s", strings.Join(f.got(), "\n"), strings.Join(want, "\n"))
	}
	if f.scopes[0] != "repository:acme-corp/shop-api:pull service="+f.host() {
		t.Fatalf("token request = %q, want a pull scope for the repository only", f.scopes[0])
	}
	for _, mt := range []string{
		"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json",
	} {
		if !strings.Contains(f.accepts[0], mt) {
			t.Errorf("Accept %q lacks %s, so the registry could answer with another manifest's digest", f.accepts[0], mt)
		}
	}
}

func TestDigest_AuthenticatesEvenWhenAnonymousLooksSayNotFound(t *testing.T) {
	// A registry may answer 404 to anyone without a credential, to hide
	// private repositories; kx must still ask with the credential.
	f := newFakeRegistry(t)
	f.anon404 = true
	f.manifests["acme-corp/shop-api:0.0.11"] = "m"
	got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got != digestOf("m") {
		t.Fatalf("Digest() = %q, %v; want the tag's digest", got, err)
	}
}

func TestDigest_AnonymousRegistry(t *testing.T) {
	f := newFakeRegistry(t)
	f.anonymous = true
	f.manifests["acme-corp/shop-api:0.0.11"] = "m"
	got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got != digestOf("m") {
		t.Fatalf("Digest() = %q, %v", got, err)
	}
	for _, r := range f.got() {
		if r == "GET /v2/token" {
			t.Fatal("asked for a token the registry didn't want")
		}
	}
}

func TestDigest_MissingTag(t *testing.T) {
	f := newFakeRegistry(t)
	got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got != "" {
		t.Fatalf("Digest() = %q, %v; want no digest and no error", got, err)
	}
}

func TestDigest_RepositoryWithParts(t *testing.T) {
	f := newFakeRegistry(t)
	f.manifests["acme-corp/web/frontend:1.0.0-rc.1"] = "m"
	got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/web/frontend:1.0.0-rc.1", f.cred())
	if err != nil || got != digestOf("m") {
		t.Fatalf("Digest() = %q, %v", got, err)
	}
	if f.scopes[0] != "repository:acme-corp/web/frontend:pull service="+f.host() {
		t.Fatalf("scope = %q", f.scopes[0])
	}
}

func TestDigest_WithoutDigestHeaderHashesTheManifest(t *testing.T) {
	f := newFakeRegistry(t)
	f.noDigest = true
	f.manifests["acme-corp/shop-api:0.0.11"] = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`
	got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got != digestOf(f.manifests["acme-corp/shop-api:0.0.11"]) {
		t.Fatalf("Digest() = %q, %v", got, err)
	}
}

func TestDigest_BadDigestHeader(t *testing.T) {
	f := newFakeRegistry(t)
	f.badDigest = true
	f.manifests["acme-corp/shop-api:0.0.11"] = "m"
	if got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); err == nil {
		t.Fatalf("Digest() = %q, want an error for a digest kx can't compare", got)
	}
}

func TestDigest_AccessTokenField(t *testing.T) {
	f := newFakeRegistry(t)
	f.tokenField = "access_token"
	f.manifests["acme-corp/shop-api:0.0.11"] = "m"
	if got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); err != nil || got != digestOf("m") {
		t.Fatalf("Digest() = %q, %v", got, err)
	}
}

func TestDigest_BasicChallenge(t *testing.T) {
	f := newFakeRegistry(t)
	f.basic = true
	f.manifests["acme-corp/shop-api:0.0.11"] = "m"
	if got, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); err != nil || got != digestOf("m") {
		t.Fatalf("Digest() = %q, %v", got, err)
	}
}

func TestDigest_NeverSendsTheCredentialElsewhere(t *testing.T) {
	other := newFakeRegistry(t)
	f := newFakeRegistry(t)
	f.realm = other.srv.URL + "/v2/token"
	f.manifests["acme-corp/shop-api:0.0.11"] = "m"
	_, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "token service on another address") {
		t.Fatalf("Digest() = %v, want a refusal of the token service on another host", err)
	}
	if n := len(other.got()); n != 0 {
		t.Fatalf("the other host got %d requests", n)
	}

	f.realm = "http://" + f.host() + "/v2/token"
	before := len(f.got())
	_, err = f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "token service without TLS") {
		t.Fatalf("Digest() = %v, want a refusal of a token service without TLS", err)
	}
	for _, r := range f.got()[before:] {
		if r == "GET /v2/token" {
			t.Fatal("asked the token service anyway")
		}
	}
}

func TestDigest_CredentialForAnotherRegistry(t *testing.T) {
	f := newFakeRegistry(t)
	cred := f.cred()
	cred.RegistryURL = "registry.example.test/acme-corp"
	if _, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", cred); err == nil {
		t.Fatal("Digest() used a credential for another registry")
	}
	if n := len(f.got()); n != 0 {
		t.Fatalf("made %d requests", n)
	}
}

func TestDigest_Refused(t *testing.T) {
	f := newFakeRegistry(t)
	f.tokenStatus = http.StatusUnauthorized
	_, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Digest() = %v, want ErrUnauthorized", err)
	}

	f.tokenStatus = 0
	f.status = http.StatusForbidden
	_, err = f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Digest() = %v, want ErrUnauthorized", err)
	}
}

func TestDigest_ServerTroubleIsTriedTwice(t *testing.T) {
	f := newFakeRegistry(t)
	f.status = http.StatusServiceUnavailable
	_, err := f.client().Digest(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil {
		t.Fatal("Digest() = nil error for a 503")
	}
	if strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("the error shows a secret: %v", err)
	}
	heads := 0
	for _, r := range f.got() {
		if strings.HasPrefix(r, "HEAD ") {
			heads++
		}
	}
	if heads != 2 { // two tries with the token
		t.Fatalf("HEAD requests = %d, want 2:\n%s", heads, strings.Join(f.got(), "\n"))
	}
}

func TestDigest_BadReferences(t *testing.T) {
	f := newFakeRegistry(t)
	for _, ref := range []string{
		"", "shop-api:0.0.11", f.host() + "/acme-corp/shop-api", f.host() + "/acme-corp/Shop:1",
		f.host() + "/acme-corp/shop-api:bad tag", f.host() + "/acme-corp/../x:1", f.host() + "/acme-corp/shop-api@" + testDigest,
	} {
		if _, err := f.client().Digest(context.Background(), ref, f.cred()); err == nil {
			t.Errorf("Digest(%q) accepted", ref)
		}
	}
	if n := len(f.got()); n != 0 {
		t.Fatalf("made %d requests for bad references", n)
	}
}

func TestParseChallenge(t *testing.T) {
	tests := []struct {
		in     string
		scheme string
		params map[string]string
	}{
		{`Bearer realm="https://r.example.test/v2/token",service="r.example.test"`, "bearer",
			map[string]string{"realm": "https://r.example.test/v2/token", "service": "r.example.test"}},
		{`bearer realm="https://r.example.test/token", scope="repository:a/b:pull,push", service=r.example.test`, "bearer",
			map[string]string{"realm": "https://r.example.test/token", "scope": "repository:a/b:pull,push", "service": "r.example.test"}},
		{`Bearer realm="https://r.example.test/t\"x",service=""`, "bearer", map[string]string{"realm": `https://r.example.test/t"x`, "service": ""}},
		{`Basic realm="Registry"`, "basic", map[string]string{"realm": "Registry"}},
		{``, "", map[string]string{}},
		{`Bearer realm="unterminated`, "bearer", map[string]string{"realm": "unterminated"}},
	}
	for _, tt := range tests {
		scheme, params := parseChallenge(tt.in)
		if scheme != tt.scheme || fmt.Sprint(params) != fmt.Sprint(tt.params) {
			t.Errorf("parseChallenge(%q) = %q, %v; want %q, %v", tt.in, scheme, params, tt.scheme, tt.params)
		}
	}
}
