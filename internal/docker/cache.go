package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
)

const (
	// cacheMinValidity is how long a cached credential must still be valid
	// to be used: a slow upload must not outlive it.
	cacheMinValidity = 15 * time.Minute
	maxCacheBytes    = 64 << 10
)

var registryURLRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]+([._-]+[a-z0-9]+)*)*$`)

// CredentialCache keeps the registry credential one token minted, on a
// laptop only, so deploys in a row don't run into the hourly limit on new
// credentials. Each token (and API) gets its own file, named by a hash, so
// a token that may not push never picks up another token's credential.
// The folder is 0700 and the file 0600; kx never uses the cache in CI.
type CredentialCache struct {
	Path string
	Now  func() time.Time
}

// NewCredentialCache returns the cache for token at apiURL, kept in dir
// (usually os.UserCacheDir()/kx).
func NewCredentialCache(dir, apiURL, token string, now func() time.Time) *CredentialCache {
	sum := sha256.Sum256([]byte(apiURL + "\x00" + token))
	if now == nil {
		now = time.Now
	}
	return &CredentialCache{Path: filepath.Join(dir, "registry-"+hex.EncodeToString(sum[:16])+".json"), Now: now}
}

// Load returns the cached credential if there is one with at least 15
// minutes left. An expired one is deleted.
func (c *CredentialCache) Load() (*api.RegistryCredential, bool) {
	f, err := os.Open(c.Path)
	if err != nil {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCacheBytes+1))
	_ = f.Close()
	if err != nil || len(data) > maxCacheBytes {
		return nil, false
	}
	cred, err := parseCachedCredential(data)
	if err != nil {
		return nil, false
	}
	now := c.Now()
	if !now.Before(cred.ExpiresAt) {
		c.Forget() // expired: no reason to keep it on disk
		return nil, false
	}
	if !now.Add(cacheMinValidity).Before(cred.ExpiresAt) {
		return nil, false
	}
	return cred, true
}

// Save stores cred, replacing any earlier one atomically.
func (c *CredentialCache) Save(cred api.RegistryCredential) error {
	data, err := json.Marshal(cred) //nolint:gosec // G117: storing the credential is the point; the file is 0600 in a 0700 folder
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil && runtime.GOOS != "windows" { //nolint:gosec // G302: a folder needs x to be usable; 0700 is owner-only
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, c.Path)
}

// Forget removes the cached credential (for example after the registry
// refused it).
func (c *CredentialCache) Forget() { _ = os.Remove(c.Path) }

func parseCachedCredential(data []byte) (*api.RegistryCredential, error) {
	var cred api.RegistryCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, err
	}
	if cred.Username == "" || cred.Password == "" || !registryURLRe.MatchString(cred.RegistryURL) || cred.ExpiresAt.IsZero() {
		return nil, errors.New("incomplete credential")
	}
	return &cred, nil
}
