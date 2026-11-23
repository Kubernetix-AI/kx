package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

const (
	testPassword = "test-only-registry-password"
	testDigest   = "sha256:4d3c2b1a00112233445566778899aabbccddeeff00112233445566778899aabb"
	testToken    = "test-only-bearer-token"
	pushToken    = "test-only-push-token"

	ociIndex    = "application/vnd.oci.image.index.v1+json"
	ociManifest = "application/vnd.oci.image.manifest.v1+json"
	ociConfig   = "application/vnd.oci.image.config.v1+json"
)

type stored struct {
	body, mediaType string
}

// fakeRegistry is a registry with token auth: the base and any manifest
// request without a token get a 401 whose challenge points at /v2/token on
// the same host. Manifests are kept by "org/repo:ref" (tag or digest),
// blobs by "org/repo@digest"; a blob GET redirects to /_downloads/ (or to
// blobHost), as registries that hand blobs out from storage do.
type fakeRegistry struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
	scopes   []string
	accepts  []string
	puts     []string // "org/repo:tag media-type"

	manifests   map[string]stored
	blobs       map[string]string
	noDigest    bool   // HEAD answers without Docker-Content-Digest
	badDigest   bool   // Docker-Content-Digest isn't a sha256 digest
	realm       string // overrides the challenge's realm
	basic       bool   // challenge with Basic instead of Bearer
	tokenStatus int    // answer to /v2/token, when not 200
	tokenField  string // "token" (default) or "access_token"
	status      int    // answer to every authorized manifest request, when set
	putStatus   int    // answer to manifest PUTs, when set
	anon404     bool   // manifest requests without a credential get 404, not 401
	anonymous   bool   // no credential needed at all
	blobHost    string // blob GETs redirect here (another server) instead of /_downloads/
	badBlob     bool   // config blobs are served with other bytes than their digest
	pingStatus  int    // answer to the base /v2/, when set
	putDigest   string // Docker-Content-Digest a manifest PUT answers with, when set
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{t: t, manifests: map[string]stored{}, blobs: map[string]string{}}
	f.srv = httptest.NewTLSServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func (f *fakeRegistry) cred() api.RegistryCredential {
	return api.RegistryCredential{RegistryURL: f.host() + "/acme-corp", Username: "oauth2accesstoken", Password: testPassword, ExpiresAt: time.Now().Add(time.Hour)}
}

func (f *fakeRegistry) client() Client { return Client{HTTP: f.srv.Client()} }

func sha(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// putImage stores a linux/amd64 image with these labels under repo:tag
// and returns its digest: an OCI index (with an attestation listed first)
// when index is set, else a single manifest.
func (f *fakeRegistry) putImage(repo, tag string, labels map[string]string, index bool) string {
	cfg, _ := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "config": map[string]any{"Labels": labels}})
	f.blobs[repo+"@"+sha(string(cfg))] = string(cfg)
	man, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": ociManifest,
		"config": map[string]any{"mediaType": ociConfig, "digest": sha(string(cfg)), "size": len(cfg)},
		"layers": []any{},
	})
	top := stored{string(man), ociManifest}
	f.manifests[repo+":"+sha(string(man))] = top
	if index {
		att := `{"schemaVersion":2,"mediaType":"` + ociManifest + `","config":{"mediaType":"application/vnd.in-toto+json","digest":"` + testDigest + `","size":1},"layers":[]}`
		f.manifests[repo+":"+sha(att)] = stored{att, ociManifest}
		idx, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": ociIndex,
			"manifests": []any{
				map[string]any{"mediaType": ociManifest, "digest": sha(att), "size": len(att), "platform": map[string]string{"os": "unknown", "architecture": "unknown"}},
				map[string]any{"mediaType": ociManifest, "digest": sha(string(man)), "size": len(man), "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
			},
		})
		top = stored{string(idx), ociIndex}
		f.manifests[repo+":"+sha(string(idx))] = top
	}
	f.manifests[repo+":"+tag] = top
	return sha(top.body)
}

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
		tok := testToken
		if strings.HasSuffix(r.URL.Query().Get("scope"), ":pull,push") {
			tok = pushToken
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{%q:%q,"expires_in":300}`, field, tok)
		return
	}
	if d, ok := strings.CutPrefix(r.URL.Path, "/_downloads/"); ok {
		// A download link is its own permission.
		for k, body := range f.blobs {
			if strings.HasSuffix(k, "@"+d) {
				if f.badBlob {
					body += " "
				}
				_, _ = io.WriteString(w, body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	auth := r.Header.Get("Authorization")
	authorized := f.anonymous || auth == "Bearer "+testToken || auth == "Bearer "+pushToken
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
		if f.pingStatus != 0 {
			w.WriteHeader(f.pingStatus)
			return
		}
		if !authorized && auth == "" {
			challenge()
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	if name, d, ok := strings.Cut(rest, "/blobs/"); ok {
		if !authorized {
			challenge()
			return
		}
		if _, found := f.blobs[name+"@"+d]; !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		target := "/_downloads/" + d
		if f.blobHost != "" {
			target = f.blobHost + target
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	name, ref, ok := strings.Cut(rest, "/manifests/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !authorized {
		if f.anon404 && auth == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		challenge()
		return
	}
	if r.Method == http.MethodPut {
		if auth != "Bearer "+pushToken && !f.basic {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if f.putStatus != 0 {
			w.WriteHeader(f.putStatus)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.manifests[name+":"+ref] = stored{string(body), r.Header.Get("Content-Type")}
		f.puts = append(f.puts, name+":"+ref+" "+r.Header.Get("Content-Type"))
		w.Header().Set("Docker-Content-Digest", sha(string(body)))
		if f.putDigest != "" {
			w.Header().Set("Docker-Content-Digest", f.putDigest)
		}
		w.WriteHeader(http.StatusCreated)
		return
	}
	f.accepts = append(f.accepts, r.Header.Get("Accept"))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	m, found := f.manifests[name+":"+ref]
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case f.badDigest:
		w.Header().Set("Docker-Content-Digest", "md5:abc")
	case !f.noDigest || r.Method == http.MethodGet:
		w.Header().Set("Docker-Content-Digest", sha(m.body))
	}
	w.Header().Set("Content-Type", m.mediaType)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, m.body)
	}
}

func (f *fakeRegistry) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

var builtLabels = map[string]string{"org.opencontainers.image.revision": "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345"}

func TestLookup_TokenAuth(t *testing.T) {
	f := newFakeRegistry(t)
	want := f.putImage("acme-corp/shop-api", "0.0.11", builtLabels, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Digest != want || got.Labels["org.opencontainers.image.revision"] != builtLabels["org.opencontainers.image.revision"] {
		t.Fatalf("Lookup() = %+v, %v; want %s with its labels", got, err, want)
	}
	reqs := f.got()
	if strings.Join(reqs[:3], "\n") != "GET /v2/\nGET /v2/token\nHEAD /v2/acme-corp/shop-api/manifests/0.0.11" {
		t.Fatalf("requests:\n%s", strings.Join(reqs, "\n"))
	}
	if f.scopes[0] != "repository:acme-corp/shop-api:pull service="+f.host() {
		t.Fatalf("token request = %q, want a pull scope for the repository only", f.scopes[0])
	}
	for _, mt := range []string{ociIndex, ociManifest, "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"} {
		if !strings.Contains(f.accepts[0], mt) {
			t.Errorf("Accept %q lacks %s, so the registry could answer with another manifest's digest", f.accepts[0], mt)
		}
	}
}

func TestLookup_IndexReadsTheLinuxImagesLabels(t *testing.T) {
	f := newFakeRegistry(t)
	want := f.putImage("acme-corp/shop-api", "3f9c2a1b7d4e", builtLabels, true)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:3f9c2a1b7d4e", f.cred())
	if err != nil || got.Digest != want || got.Labels["org.opencontainers.image.revision"] == "" {
		t.Fatalf("Lookup() = %+v, %v; want the index's digest and the linux/amd64 image's labels", got, err)
	}
}

func TestLookup_Unlabelled(t *testing.T) {
	f := newFakeRegistry(t)
	want := f.putImage("acme-corp/shop-api", "0.0.11", nil, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Digest != want || len(got.Labels) != 0 {
		t.Fatalf("Lookup() = %+v, %v; want the digest and no labels", got, err)
	}
}

func TestLookup_BlobFromAnotherHostGetsNoCredential(t *testing.T) {
	var sawAuth []string
	var mu sync.Mutex
	f := newFakeRegistry(t)
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawAuth = append(sawAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		f.ServeHTTP(w, r) // serves /_downloads/
	}))
	t.Cleanup(storage.Close)
	f.blobHost = storage.URL
	f.putImage("acme-corp/shop-api", "0.0.11", builtLabels, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Labels["org.opencontainers.image.revision"] == "" {
		t.Fatalf("Lookup() = %+v, %v", got, err)
	}
	if len(sawAuth) != 1 || sawAuth[0] != "" {
		t.Fatalf("the storage host got Authorization %q; the credential stays with the registry", sawAuth)
	}
}

func TestLookup_ConfigThatDoesntMatchItsDigest(t *testing.T) {
	f := newFakeRegistry(t)
	f.badBlob = true
	f.putImage("acme-corp/shop-api", "0.0.11", builtLabels, false)
	if got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); err == nil {
		t.Fatalf("Lookup() = %+v, want an error: the labels can't be trusted", got)
	}
}

func TestLookup_AuthenticatesEvenWhenAnonymousLooksSayNotFound(t *testing.T) {
	// A registry may answer 404 to anyone without a credential, to hide
	// private repositories; kx must still ask with the credential.
	f := newFakeRegistry(t)
	f.anon404 = true
	want := f.putImage("acme-corp/shop-api", "0.0.11", nil, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Digest != want {
		t.Fatalf("Lookup() = %+v, %v; want the tag's digest", got, err)
	}
}

func TestLookup_AnonymousRegistry(t *testing.T) {
	f := newFakeRegistry(t)
	f.anonymous = true
	want := f.putImage("acme-corp/shop-api", "0.0.11", nil, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Digest != want {
		t.Fatalf("Lookup() = %+v, %v", got, err)
	}
	for _, r := range f.got() {
		if r == "GET /v2/token" {
			t.Fatal("asked for a token the registry didn't want")
		}
	}
}

func TestLookup_MissingTag(t *testing.T) {
	f := newFakeRegistry(t)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Digest != "" || got.Labels != nil {
		t.Fatalf("Lookup() = %+v, %v; want nothing and no error", got, err)
	}
}

func TestLookup_RepositoryWithParts(t *testing.T) {
	f := newFakeRegistry(t)
	want := f.putImage("acme-corp/web/frontend", "1.0.0-rc.1", nil, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/web/frontend:1.0.0-rc.1", f.cred())
	if err != nil || got.Digest != want {
		t.Fatalf("Lookup() = %+v, %v", got, err)
	}
	if f.scopes[0] != "repository:acme-corp/web/frontend:pull service="+f.host() {
		t.Fatalf("scope = %q", f.scopes[0])
	}
}

func TestLookup_WithoutDigestHeaderHashesTheManifest(t *testing.T) {
	f := newFakeRegistry(t)
	f.noDigest = true
	want := f.putImage("acme-corp/shop-api", "0.0.11", builtLabels, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Digest != want {
		t.Fatalf("Lookup() = %+v, %v; want %s", got, err, want)
	}
}

func TestLookup_BadDigestHeader(t *testing.T) {
	f := newFakeRegistry(t)
	f.badDigest = true
	f.putImage("acme-corp/shop-api", "0.0.11", nil, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "reported a digest kx can't read") {
		t.Fatalf("Lookup() = %+v, %v; want the refusal of a digest kx can't compare", got, err)
	}
}

func TestLookup_ManifestThatDoesntMatchItsDigest(t *testing.T) {
	// A registry answering with other bytes than the digest names could
	// hand kx any labels.
	f := newFakeRegistry(t)
	d := f.putImage("acme-corp/shop-api", "0.0.11", builtLabels, false)
	m := f.manifests["acme-corp/shop-api:"+d]
	m.body = strings.Replace(m.body, `"layers":[]`, `"layers":[ ]`, 1)
	f.manifests["acme-corp/shop-api:"+d] = m
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("Lookup() = %+v, %v; want the mismatch refused", got, err)
	}
}

func TestLookup_IndexPicksLinuxAmd64(t *testing.T) {
	f := newFakeRegistry(t)
	arm := map[string]string{"org.opencontainers.image.revision": strings.Repeat("a", 40)}
	armDigest := f.putImage("acme-corp/shop-api", "arm", arm, false)
	f.putImage("acme-corp/shop-api", "amd", builtLabels, false)
	amd := f.manifests["acme-corp/shop-api:amd"]
	idx, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": ociIndex,
		"manifests": []any{
			map[string]any{"mediaType": ociManifest, "digest": armDigest, "size": 1, "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
			map[string]any{"mediaType": ociManifest, "digest": sha(amd.body), "size": len(amd.body), "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
		},
	})
	f.manifests["acme-corp/shop-api:"+sha(string(idx))] = stored{string(idx), ociIndex}
	f.manifests["acme-corp/shop-api:0.0.11"] = stored{string(idx), ociIndex}
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err != nil || got.Labels["org.opencontainers.image.revision"] != builtLabels["org.opencontainers.image.revision"] {
		t.Fatalf("Lookup() = %+v, %v; want the linux/amd64 image's labels, not linux/arm64's", got, err)
	}
}

func TestLookup_BlobRedirectWithoutTLSIsRefused(t *testing.T) {
	f := newFakeRegistry(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.ServeHTTP(w, r) }))
	t.Cleanup(plain.Close)
	f.blobHost = plain.URL
	f.putImage("acme-corp/shop-api", "0.0.11", builtLabels, false)
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "somewhere it doesn't follow") {
		t.Fatalf("Lookup() = %+v, %v; want the redirect to plain HTTP refused", got, err)
	}
}

func TestLookup_BaseThatIsntThere(t *testing.T) {
	// A 404 from the registry's base isn't a registry that needs no login.
	f := newFakeRegistry(t)
	f.pingStatus = http.StatusNotFound
	f.putImage("acme-corp/shop-api", "0.0.11", nil, false)
	f.anonymous = true // even when the tag itself would answer
	got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "answered 404 when kx asked how to log in") {
		t.Fatalf("Lookup() = %+v, %v; want the base's 404 refused", got, err)
	}
}

func TestLookup_ServerTroubleIsUnavailable(t *testing.T) {
	f := newFakeRegistry(t)
	f.status = http.StatusBadGateway
	if _, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Lookup() = %v, want ErrUnavailable", err)
	}
	f.status = 0
	f.pingStatus = http.StatusServiceUnavailable
	if _, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Lookup() with the base down = %v, want ErrUnavailable", err)
	}
}

func TestLookup_AccessTokenField(t *testing.T) {
	f := newFakeRegistry(t)
	f.tokenField = "access_token"
	want := f.putImage("acme-corp/shop-api", "0.0.11", nil, false)
	if got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); err != nil || got.Digest != want {
		t.Fatalf("Lookup() = %+v, %v", got, err)
	}
}

func TestLookup_BasicChallenge(t *testing.T) {
	f := newFakeRegistry(t)
	f.basic = true
	want := f.putImage("acme-corp/shop-api", "0.0.11", builtLabels, false)
	if got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred()); err != nil || got.Digest != want {
		t.Fatalf("Lookup() = %+v, %v", got, err)
	}
}

func TestLookup_NeverSendsTheCredentialElsewhere(t *testing.T) {
	other := newFakeRegistry(t)
	f := newFakeRegistry(t)
	f.realm = other.srv.URL + "/v2/token"
	f.putImage("acme-corp/shop-api", "0.0.11", nil, false)
	_, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "token service on another address") {
		t.Fatalf("Lookup() = %v, want a refusal of the token service on another host", err)
	}
	if n := len(other.got()); n != 0 {
		t.Fatalf("the other host got %d requests", n)
	}

	f.realm = "http://" + f.host() + "/v2/token"
	before := len(f.got())
	_, err = f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil || !strings.Contains(err.Error(), "token service without TLS") {
		t.Fatalf("Lookup() = %v, want a refusal of a token service without TLS", err)
	}
	for _, r := range f.got()[before:] {
		if r == "GET /v2/token" {
			t.Fatal("asked the token service anyway")
		}
	}
}

func TestLookup_CredentialForAnotherRegistry(t *testing.T) {
	f := newFakeRegistry(t)
	cred := f.cred()
	cred.RegistryURL = "registry.example.test/acme-corp"
	if _, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", cred); err == nil {
		t.Fatal("Lookup() used a credential for another registry")
	}
	if n := len(f.got()); n != 0 {
		t.Fatalf("made %d requests", n)
	}
}

func TestLookup_Refused(t *testing.T) {
	f := newFakeRegistry(t)
	f.tokenStatus = http.StatusUnauthorized
	_, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Lookup() = %v, want ErrUnauthorized", err)
	}

	f.tokenStatus = 0
	f.status = http.StatusForbidden
	_, err = f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Lookup() = %v, want ErrUnauthorized", err)
	}
}

func TestLookup_ServerTroubleIsTriedTwice(t *testing.T) {
	f := newFakeRegistry(t)
	f.status = http.StatusServiceUnavailable
	_, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
	if err == nil {
		t.Fatal("Lookup() = nil error for a 503")
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

func TestLookup_BadReferences(t *testing.T) {
	f := newFakeRegistry(t)
	for _, ref := range []string{
		"", "shop-api:0.0.11", f.host() + "/acme-corp/shop-api", f.host() + "/acme-corp/Shop:1",
		f.host() + "/acme-corp/shop-api:bad tag", f.host() + "/acme-corp/../x:1", f.host() + "/acme-corp/shop-api@" + testDigest,
	} {
		if _, err := f.client().Lookup(context.Background(), ref, f.cred()); err == nil {
			t.Errorf("Lookup(%q) accepted", ref)
		}
	}
	if n := len(f.got()); n != 0 {
		t.Fatalf("made %d requests for bad references", n)
	}
}

func TestTag_PointsANewTagAtAnImage(t *testing.T) {
	for _, index := range []bool{false, true} {
		f := newFakeRegistry(t)
		digest := f.putImage("acme-corp/shop-api", "3f9c2a1b7d4e", builtLabels, index)
		if err := f.client().Tag(context.Background(), f.host()+"/acme-corp/shop-api", digest, "0.0.11", f.cred()); err != nil {
			t.Fatalf("Tag() = %v", err)
		}
		got, err := f.client().Lookup(context.Background(), f.host()+"/acme-corp/shop-api:0.0.11", f.cred())
		if err != nil || got.Digest != digest {
			t.Fatalf("after Tag, 0.0.11 = %+v, %v; want %s", got, err, digest)
		}
		wantType := ociManifest
		if index {
			wantType = ociIndex
		}
		if len(f.puts) != 1 || f.puts[0] != "acme-corp/shop-api:0.0.11 "+wantType {
			t.Fatalf("puts = %q", f.puts)
		}
		if f.scopes[0] != "repository:acme-corp/shop-api:pull,push service="+f.host() {
			t.Fatalf("scope = %q, want pull and push on the repository", f.scopes[0])
		}
	}
}

func TestTag_Failures(t *testing.T) {
	f := newFakeRegistry(t)
	digest := f.putImage("acme-corp/shop-api", "3f9c2a1b7d4e", nil, false)
	ctx := context.Background()
	if err := f.client().Tag(ctx, f.host()+"/acme-corp/shop-api", "sha256:"+strings.Repeat("0", 64), "0.0.11", f.cred()); err == nil {
		t.Error("Tag() of an image the repository doesn't have succeeded")
	}
	for _, bad := range [][3]string{
		{f.host() + "/acme-corp/shop-api", "latest", "0.0.11"},      // not a digest
		{f.host() + "/acme-corp/shop-api", digest, "bad tag"},       // not a tag
		{f.host() + "/acme-corp/Shop", digest, "0.0.11"},            // not a repository
		{"registry.example.test/acme-corp/shop-api", digest, "0.0"}, // another registry than the credential's
	} {
		if err := f.client().Tag(ctx, bad[0], bad[1], bad[2], f.cred()); err == nil {
			t.Errorf("Tag(%q) accepted", bad)
		}
	}
	f.putStatus = http.StatusForbidden
	if err := f.client().Tag(ctx, f.host()+"/acme-corp/shop-api", digest, "0.0.11", f.cred()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Tag() refused = %v, want ErrUnauthorized", err)
	}
	f.putStatus = http.StatusBadRequest
	if err := f.client().Tag(ctx, f.host()+"/acme-corp/shop-api", digest, "0.0.11", f.cred()); err == nil {
		t.Error("Tag() = nil for a refused PUT")
	}
	if len(f.puts) != 0 {
		t.Fatalf("puts = %q", f.puts)
	}
	f.putStatus = 0
	f.putDigest = "sha256:" + strings.Repeat("e", 64)
	if err := f.client().Tag(ctx, f.host()+"/acme-corp/shop-api", digest, "0.0.11", f.cred()); err == nil || !strings.Contains(err.Error(), "tagged another image") {
		t.Errorf("Tag() answered with another digest = %v, want an error", err)
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
