package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// ProjectFile is the name of the per-folder link file.
const ProjectFile = ".kx.json"

const maxConfigBytes = 1 << 20

var isWindows = runtime.GOOS == "windows"

// Project is the content of .kx.json.
type Project struct {
	App   string `json:"app"`
	AppID string `json:"app_id,omitempty"`
	Org   string `json:"org,omitempty"`
}

// OrgLogin is one organization's saved login.
type OrgLogin struct {
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expires_at"` // null: the token never expires
	User      string     `json:"user,omitempty"`
}

// User is kx's own settings file.
type User struct {
	APIURL     string              `json:"api_url,omitempty"`
	DefaultOrg string              `json:"default_org,omitempty"`
	Orgs       map[string]OrgLogin `json:"orgs"`
}

var (
	appRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	appIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	orgRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
)

// validApp matches the platform's rule for app names: lowercase letters,
// digits and single hyphens, at most 63 characters ("--" separates parts
// of an app's address, so the platform refuses it).
func validApp(s string) bool   { return appRe.MatchString(s) && !strings.Contains(s, "--") }
func validAppID(s string) bool { return appIDRe.MatchString(s) }
func validOrg(s string) bool   { return orgRe.MatchString(s) && s != "." && s != ".." }

// ValidAppName reports whether s can be an app name.
func ValidAppName(s string) bool { return validApp(s) }

// ValidOrgName reports whether s can be an organization name.
func ValidOrgName(s string) bool { return validOrg(s) }

// UserPath returns the settings file path: $KX_CONFIG_DIR/config.json, or
// os.UserConfigDir()/kx/config.json.
func UserPath(getenv func(string) string) (string, error) {
	if d := getenv(EnvConfigDir); d != "" {
		return filepath.Join(d, "config.json"), nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "kx", "config.json"), nil
}

// CacheDir returns the folder for cached, short-lived data:
// os.UserCacheDir()/kx (or $KX_CONFIG_DIR/cache). It is not created.
func CacheDir(getenv func(string) string) (string, error) {
	if d := getenv(EnvConfigDir); d != "" {
		return filepath.Join(d, "cache"), nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "kx"), nil
}

// ParseUser decodes and validates a settings file.
func ParseUser(data []byte) (*User, error) {
	var u User
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, errors.New("not valid JSON")
	}
	if u.Orgs == nil {
		u.Orgs = map[string]OrgLogin{}
	}
	for name := range u.Orgs {
		if !validOrg(name) {
			return nil, errors.New("an organization name is not valid")
		}
	}
	if u.DefaultOrg != "" && !validOrg(u.DefaultOrg) {
		return nil, errors.New("default_org is not valid")
	}
	return &u, nil
}

// LoadUser reads the settings file. A missing file is an empty config. A
// file other users can read is tightened to 0600.
func LoadUser(path string) (*User, error) {
	f, err := os.Open(path) //nolint:gosec // G304: kx's own settings file
	if errors.Is(err, fs.ErrNotExist) {
		return &User{Orgs: map[string]OrgLogin{}}, nil
	}
	if err != nil {
		return nil, &kxerr.BadConfig{Path: path, Reason: "cannot open it"}
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && !isWindows && st.Mode().Perm()&0o077 != 0 {
		_ = f.Chmod(0o600) // the file just opened and checked, not whatever path names now
	}
	data, err := readLimited(f)
	if err != nil {
		return nil, &kxerr.BadConfig{Path: path, Reason: err.Error()}
	}
	u, err := ParseUser(data)
	if err != nil {
		return nil, &kxerr.BadConfig{Path: path, Reason: err.Error()}
	}
	return u, nil
}

// SaveUser writes the settings file atomically with mode 0600.
func SaveUser(path string, u *User) error {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

// AddLogin saves org's login and makes org the default.
func (u *User) AddLogin(org string, l OrgLogin) {
	if u.Orgs == nil {
		u.Orgs = map[string]OrgLogin{}
	}
	u.Orgs[org] = l
	u.DefaultOrg = org
}

// RemoveLogin deletes org's login. If it was the default, the first
// remaining org (alphabetically) becomes the default.
func (u *User) RemoveLogin(org string) {
	delete(u.Orgs, org)
	if u.DefaultOrg == org {
		u.DefaultOrg = ""
		if names := u.OrgNames(); len(names) > 0 {
			u.DefaultOrg = names[0]
		}
	}
}

// ParseProject decodes and validates a .kx.json file.
func ParseProject(data []byte) (*Project, error) {
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, errors.New("not valid JSON")
	}
	if !validApp(p.App) {
		return nil, errors.New(`"app" must be an app name like shop-api`)
	}
	if p.AppID != "" && !validAppID(p.AppID) {
		return nil, errors.New(`"app_id" is not valid`)
	}
	if p.Org != "" && !validOrg(p.Org) {
		return nil, errors.New(`"org" is not valid`)
	}
	return &p, nil
}

// FindProject looks for .kx.json from dir upwards. Inside a git repository
// it stops at the repository root; outside one it checks dir only.
func FindProject(dir string) (string, *Project, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	dirs := []string{dir}
	if root := gitRoot(dir); root != "" {
		dirs = dirs[:0]
		for d := dir; ; d = filepath.Dir(d) {
			dirs = append(dirs, d)
			if d == root || filepath.Dir(d) == d {
				break
			}
		}
	}
	for _, d := range dirs {
		path := filepath.Join(d, ProjectFile)
		f, err := os.Open(path) //nolint:gosec // G304: the folder's .kx.json, read and validated
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, &kxerr.BadConfig{Path: path, Project: true, Reason: "cannot read it"}
		}
		data, err := readLimited(f)
		_ = f.Close()
		if err != nil {
			return "", nil, &kxerr.BadConfig{Path: path, Project: true, Reason: err.Error()}
		}
		p, err := ParseProject(data)
		if err != nil {
			return "", nil, &kxerr.BadConfig{Path: path, Project: true, Reason: err.Error()}
		}
		return path, p, nil
	}
	return "", nil, nil
}

// SaveProject writes dir/.kx.json and returns its path.
func SaveProject(dir string, p Project) (string, error) {
	if _, err := ParseProject(mustJSON(p)); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, ProjectFile)
	return path, writeAtomic(path, append(data, '\n'), 0o644)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func gitRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil {
		return nil, errors.New("cannot read it")
	}
	if len(data) > maxConfigBytes {
		return nil, errors.New("it is too large")
	}
	return data, nil
}

// writeAtomic writes data to a temporary file next to path and renames it
// into place, so a crash never leaves a half-written file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot write in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil && !isWindows {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
