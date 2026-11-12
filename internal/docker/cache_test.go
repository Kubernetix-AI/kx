package docker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Public checksum test vectors: well formed, never issued, so they unlock
// nothing.
const (
	cacheTokA = "kx_0123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU"
	cacheTokB = "kx_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz2x81PZ"
	cacheAPI  = "https://platform.kubernetix.ai/api/v1"
)

func TestCredentialCache_SaveLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c := NewCredentialCache(dir, cacheAPI, cacheTokA, func() time.Time { return now })
	cred := testCred()
	cred.ExpiresAt = now.Add(time.Hour)

	if _, ok := c.Load(); ok {
		t.Fatal("Load() found a credential in an empty cache")
	}
	if err := c.Save(cred); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Load()
	if !ok || got.Password != cred.Password || got.Username != cred.Username || got.RegistryURL != cred.RegistryURL || !got.ExpiresAt.Equal(cred.ExpiresAt) {
		t.Fatalf("Load() = %v, %v", got, ok)
	}

	fi, err := os.Stat(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("cache file mode %o, want 600", fi.Mode().Perm())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm() != 0o700 {
			t.Errorf("cache folder mode %o, want 700", di.Mode().Perm())
		}
	}
	if strings.Contains(c.Path, cacheTokA) || strings.Contains(c.Path, cacheTokA[3:11]) {
		t.Errorf("cache file name reveals the token: %s", filepath.Base(c.Path))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("cache folder holds %d files, want 1 (no temporary files left)", len(entries))
	}

	c.Forget()
	if _, ok := c.Load(); ok {
		t.Fatal("Load() after Forget() found a credential")
	}
}

func TestCredentialCache_HonoursExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c := NewCredentialCache(dir, cacheAPI, cacheTokA, func() time.Time { return now })
	tests := []struct {
		left time.Duration
		ok   bool
	}{
		{time.Hour, true},
		{16 * time.Minute, true},
		{14 * time.Minute, false}, // too little left for a long push
		{-time.Minute, false},
	}
	for _, tt := range tests {
		cred := testCred()
		cred.ExpiresAt = now.Add(tt.left)
		if err := c.Save(cred); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Load(); ok != tt.ok {
			t.Errorf("%v left: Load() ok = %v, want %v", tt.left, ok, tt.ok)
		}
	}
}

func TestCredentialCache_RemovesExpiredCredential(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c := NewCredentialCache(t.TempDir(), cacheAPI, cacheTokA, func() time.Time { return now })
	cred := testCred()
	cred.ExpiresAt = now.Add(10 * time.Minute) // still valid, too short to use
	if err := c.Save(cred); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Load(); ok {
		t.Fatal("used a credential with 10 minutes left")
	}
	if _, err := os.Stat(c.Path); err != nil {
		t.Fatal("removed a credential that hasn't expired yet")
	}
	cred.ExpiresAt = now.Add(-time.Second)
	if err := c.Save(cred); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Load(); ok {
		t.Fatal("used an expired credential")
	}
	if _, err := os.Stat(c.Path); !os.IsNotExist(err) {
		t.Fatal("an expired credential stayed on disk")
	}
}

func TestCredentialCache_PerTokenAndAPI(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	clock := func() time.Time { return now }
	a := NewCredentialCache(dir, cacheAPI, cacheTokA, clock)
	cred := testCred()
	cred.ExpiresAt = now.Add(time.Hour)
	if err := a.Save(cred); err != nil {
		t.Fatal(err)
	}
	// Another token (which may not be allowed to push) never sees it, and
	// neither does the same token against another API.
	if _, ok := NewCredentialCache(dir, cacheAPI, cacheTokB, clock).Load(); ok {
		t.Error("a different token loaded the cached credential")
	}
	if _, ok := NewCredentialCache(dir, "https://staging.example.test/api/v1", cacheTokA, clock).Load(); ok {
		t.Error("the same token against another API loaded the cached credential")
	}
}

func TestCredentialCache_IgnoresDamagedFile(t *testing.T) {
	dir := t.TempDir()
	c := NewCredentialCache(dir, cacheAPI, cacheTokA, time.Now)
	for _, body := range []string{"", "{", `{"registry_url":"registry.example.test/a","username":"u","password":"","expires_at":"2099-01-01T00:00:00Z"}`, `{"registry_url":"bad host/x","username":"u","password":"p","expires_at":"2099-01-01T00:00:00Z"}`, strings.Repeat("x", 70000)} {
		if err := os.WriteFile(c.Path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := c.Load(); ok {
			t.Errorf("Load() accepted a damaged file: %v", got)
		}
	}
}

func FuzzParseCachedCredential(f *testing.F) {
	f.Add([]byte(`{"registry_url":"registry.example.test/acme-corp","username":"kx-push","password":"p","expires_at":"2099-01-01T00:00:00Z"}`))
	f.Add([]byte(`{"expires_at":"not a time"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		cred, err := parseCachedCredential(data)
		if err != nil {
			return
		}
		if cred.Username == "" || cred.Password == "" || !registryURLRe.MatchString(cred.RegistryURL) {
			t.Fatalf("parseCachedCredential accepted an incomplete credential: %v", cred)
		}
	})
}
