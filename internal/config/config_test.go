package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// Public checksum test vectors shared with the platform: well formed, never
// issued, so they unlock nothing.
const (
	tokCorp = "kx_0123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU"
	tokLabs = "kx_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz2x81PZ"
	tokEnv  = "kx_00000000000000000000000000000000000000002kaqcA"
)

type fixture struct {
	configDir  string
	projectDir string
	env        map[string]string
}

func (f *fixture) getenv(k string) string { return f.env[k] }

// newFixture makes a user config with acme-corp (default) and acme-labs,
// and a git repository whose root holds .kx.json, with the working
// directory two levels below it.
func newFixture(t *testing.T, project string) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		configDir: filepath.Join(root, "config"),
		env:       map[string]string{},
	}
	f.env["KX_CONFIG_DIR"] = f.configDir

	exp := time.Date(2026, 12, 11, 9, 0, 0, 0, time.UTC)
	u := &User{DefaultOrg: "acme-corp", Orgs: map[string]OrgLogin{
		"acme-corp": {Token: tokCorp, ExpiresAt: &exp, User: "sam@acme.dev"},
		"acme-labs": {Token: tokLabs, User: "sam@acme.dev"},
	}}
	if err := SaveUser(filepath.Join(f.configDir, "config.json"), u); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(root, "code", "shop")
	f.projectDir = filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(f.projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if project != "" {
		if err := os.WriteFile(filepath.Join(repo, ProjectFile), []byte(project), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestResolve_Precedence(t *testing.T) {
	project := `{"app":"shop-api","app_id":"6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c","org":"acme-corp"}`
	tests := []struct {
		name    string
		project string
		flags   Flags
		env     map[string]string
		want    Resolved // only the compared fields
	}{
		{
			name: "defaults and user config",
			want: Resolved{
				APIURL: DefaultAPIURL, APIURLSource: SourceDefault,
				Org: "acme-corp", OrgSource: SourceConfig,
				Token: tokCorp, TokenSource: kxerr.SourceLogin,
			},
		},
		{
			name:    "project file gives app, app ID and org",
			project: project,
			want: Resolved{
				APIURL: DefaultAPIURL, APIURLSource: SourceDefault,
				Org: "acme-corp", OrgSource: SourceProject,
				App: "shop-api", AppID: "6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c", AppSource: SourceProject,
				Token: tokCorp, TokenSource: kxerr.SourceLogin,
			},
		},
		{
			name:    "environment beats project and config",
			project: project,
			env: map[string]string{
				"KX_TOKEN": tokEnv, "KX_ORG": "acme-labs", "KX_APP": "web",
				"KX_API_URL": "https://staging.example.test/api/v1",
			},
			want: Resolved{
				APIURL: "https://staging.example.test/api/v1", APIURLSource: SourceEnv,
				Org: "acme-labs", OrgSource: SourceEnv,
				App: "web", AppSource: SourceEnv,
				Token: tokEnv, TokenSource: kxerr.SourceEnv,
			},
		},
		{
			name:    "flags beat environment",
			project: project,
			flags:   Flags{Org: "acme-corp", App: "billing", APIURL: "http://localhost:8080/api/v1"},
			env:     map[string]string{"KX_ORG": "acme-labs", "KX_APP": "web", "KX_API_URL": "https://staging.example.test/api/v1"},
			want: Resolved{
				APIURL: "http://localhost:8080/api/v1", APIURLSource: SourceFlag,
				Org: "acme-corp", OrgSource: SourceFlag,
				App: "billing", AppSource: SourceFlag,
				Token: tokCorp, TokenSource: kxerr.SourceLogin,
			},
		},
		{
			name:    "app flag equal to project app keeps the app ID",
			project: project,
			flags:   Flags{App: "shop-api"},
			want: Resolved{
				APIURL: DefaultAPIURL, APIURLSource: SourceDefault,
				Org: "acme-corp", OrgSource: SourceProject,
				App: "shop-api", AppID: "6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c", AppSource: SourceFlag,
				Token: tokCorp, TokenSource: kxerr.SourceLogin,
			},
		},
		{
			name:    "another org drops the project's app ID",
			project: project,
			flags:   Flags{Org: "acme-labs"},
			want: Resolved{
				APIURL: DefaultAPIURL, APIURLSource: SourceDefault,
				Org: "acme-labs", OrgSource: SourceFlag,
				App: "shop-api", AppSource: SourceProject,
				Token: tokLabs, TokenSource: kxerr.SourceLogin,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.project)
			for k, v := range tt.env {
				f.env[k] = v
			}
			tt.flags.Dir = f.projectDir
			got, err := Resolve(tt.flags, f.getenv)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			check := func(field string, got, want any) {
				t.Helper()
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %v, want %v", field, got, want)
				}
			}
			check("APIURL", got.APIURL, tt.want.APIURL)
			check("APIURLSource", got.APIURLSource, tt.want.APIURLSource)
			check("Org", got.Org, tt.want.Org)
			check("OrgSource", got.OrgSource, tt.want.OrgSource)
			check("App", got.App, tt.want.App)
			check("AppID", got.AppID, tt.want.AppID)
			check("AppSource", got.AppSource, tt.want.AppSource)
			check("TokenSource", got.TokenSource, tt.want.TokenSource)
			if got.Token != tt.want.Token {
				t.Errorf("Token prefix = %.11s, want %.11s", got.Token, tt.want.Token)
			}
		})
	}
}

func TestResolve_OrgPicksToken(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		flags     Flags
		env       map[string]string
		wantOrg   string
		wantToken string
		wantErr   error
	}{
		{name: "default org", wantOrg: "acme-corp", wantToken: tokCorp},
		{name: "folder's org", project: `{"app":"lab","org":"acme-labs"}`, wantOrg: "acme-labs", wantToken: tokLabs},
		{name: "KX_ORG", env: map[string]string{"KX_ORG": "acme-labs"}, wantOrg: "acme-labs", wantToken: tokLabs},
		{name: "--org beats the folder", project: `{"app":"lab","org":"acme-labs"}`, flags: Flags{Org: "acme-corp"}, wantOrg: "acme-corp", wantToken: tokCorp},
		{
			name:    "folder's org without a login",
			project: `{"app":"x","org":"acme-other"}`,
			wantOrg: "acme-other",
			wantErr: &kxerr.OrgNotLoggedIn{Org: "acme-other", LoggedIn: []string{"acme-corp", "acme-labs"}, FromFolder: true},
		},
		{name: "KX_TOKEN wins over every saved login", project: `{"app":"x","org":"acme-other"}`, env: map[string]string{"KX_TOKEN": tokEnv}, wantOrg: "acme-other", wantToken: tokEnv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.project)
			for k, v := range tt.env {
				f.env[k] = v
			}
			tt.flags.Dir = f.projectDir
			got, err := Resolve(tt.flags, f.getenv)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Org != tt.wantOrg {
				t.Errorf("Org = %q, want %q", got.Org, tt.wantOrg)
			}
			if got.Token != tt.wantToken {
				t.Errorf("Token prefix = %.11s, want %.11s", got.Token, tt.wantToken)
			}
			reqErr := got.RequireToken(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
			if !reflect.DeepEqual(reqErr, tt.wantErr) {
				t.Errorf("RequireToken() = %#v, want %#v", reqErr, tt.wantErr)
			}
		})
	}
}

func TestRequireToken(t *testing.T) {
	past := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		r    Resolved
		want error
	}{
		{"nothing saved", Resolved{User: &User{}}, kxerr.ErrNotLoggedIn},
		{"no user config at all", Resolved{}, kxerr.ErrNotLoggedIn},
		{"env token", Resolved{Token: tokEnv, TokenSource: kxerr.SourceEnv}, nil},
		{"expired login", Resolved{Org: "acme-corp", Token: tokCorp, TokenSource: kxerr.SourceLogin, TokenExpiresAt: &past}, &kxerr.LoginEnded{Org: "acme-corp"}},
		{"never expires", Resolved{Org: "acme-corp", Token: tokCorp, TokenSource: kxerr.SourceLogin}, nil},
		{
			"org without login, others exist",
			Resolved{Org: "acme-x", User: &User{Orgs: map[string]OrgLogin{"b": {Token: tokLabs}, "a": {Token: tokCorp}}}},
			&kxerr.OrgNotLoggedIn{Org: "acme-x", LoggedIn: []string{"a", "b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.r.RequireToken(now)
			if !reflect.DeepEqual(got, tt.want) && !errors.Is(got, tt.want) {
				t.Fatalf("RequireToken() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolve_CINeverReadsUserConfig(t *testing.T) {
	f := newFixture(t, "")
	f.env["GITHUB_ACTIONS"] = "true"
	got, err := Resolve(Flags{Dir: f.projectDir}, f.getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got.User != nil || got.Token != "" || !got.CI.IsCI {
		t.Fatalf("Resolve under CI read user config: User=%v token set=%v CI=%v", got.User != nil, got.Token != "", got.CI.IsCI)
	}
	if err := got.RequireToken(time.Now()); !errors.Is(err, kxerr.ErrNotLoggedIn) {
		t.Fatalf("RequireToken() = %v", err)
	}
}

func TestResolve_ProjectFileNeverChangesAPIURL(t *testing.T) {
	f := newFixture(t, `{"app":"x","org":"acme-corp","api_url":"https://evil.example/api/v1"}`)
	got, err := Resolve(Flags{Dir: f.projectDir}, f.getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIURL != DefaultAPIURL {
		t.Fatalf("APIURL = %q", got.APIURL)
	}
}

func TestResolve_InsecureAPIURL(t *testing.T) {
	f := newFixture(t, "")
	f.env["KX_API_URL"] = "http://platform.kubernetix.ai/api/v1"
	_, err := Resolve(Flags{Dir: f.projectDir}, f.getenv)
	var ins *kxerr.InsecureURL
	if !errors.As(err, &ins) || ins.Source != "KX_API_URL" {
		t.Fatalf("Resolve() = %v, want InsecureURL from KX_API_URL", err)
	}
}

func TestFindProject_StopsAtGitRoot(t *testing.T) {
	root := t.TempDir()
	// A .kx.json above the repository must not be picked up.
	if err := os.WriteFile(filepath.Join(root, ProjectFile), []byte(`{"app":"outside"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, p, err := FindProject(sub)
	if err != nil || p != nil || path != "" {
		t.Fatalf("FindProject = %q, %v, %v; want nothing", path, p, err)
	}

	if err := os.WriteFile(filepath.Join(repo, "a", ProjectFile), []byte(`{"app":"mid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path, p, err = FindProject(sub)
	if err != nil || p == nil || p.App != "mid" || path != filepath.Join(repo, "a", ProjectFile) {
		t.Fatalf("FindProject = %q, %+v, %v", path, p, err)
	}
}

func TestFindProject_OutsideGitOnlyChecksTheFolder(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ProjectFile), []byte(`{"app":"up"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, p, err := FindProject(sub); err != nil || p != nil {
		t.Fatalf("FindProject = %+v, %v; want nothing", p, err)
	}
	if _, p, err := FindProject(root); err != nil || p == nil || p.App != "up" {
		t.Fatalf("FindProject(root) = %+v, %v", p, err)
	}
}

// The platform refuses app names with "--" (it separates parts of an app's
// address), whatever their length: kx refuses them before building.
func TestValidAppName_MatchesThePlatform(t *testing.T) {
	long := strings.Repeat("a", 63)
	tests := []struct {
		name string
		ok   bool
	}{
		{"shop-api", true}, {"a", true}, {"a1-b2-c3", true}, {long, true},
		{"my--app", false}, {"a---b", false}, {"-shop", false}, {"shop-", false},
		{"Shop", false}, {"shop_api", false}, {"", false}, {long + "a", false},
	}
	for _, tt := range tests {
		if got := ValidAppName(tt.name); got != tt.ok {
			t.Errorf("ValidAppName(%q) = %v, want %v", tt.name, got, tt.ok)
		}
	}
	if _, err := ParseProject([]byte(`{"app":"my--app"}`)); err == nil {
		t.Error(`ParseProject accepted "my--app"`)
	}
}

func TestAppNameFrom(t *testing.T) {
	tests := map[string]string{
		"shop-api":                "shop-api",
		"My_App":                  "my-app",
		"ShopAPI":                 "shopapi",
		"todo.app":                "todo-app",
		"My Project":              "my-project",
		"  --Weird__ ":            "weird",
		"a--b":                    "a-b",
		"Café 2":                  "caf-2",
		"123":                     "123",
		"___":                     "",
		"日本語":                     "",
		"/":                       "",
		strings.Repeat("ab-", 30): strings.TrimRight(strings.Repeat("ab-", 21), "-"),
	}
	for in, want := range tests {
		got := AppNameFrom(in)
		if got != want {
			t.Errorf("AppNameFrom(%q) = %q, want %q", in, got, want)
		}
		if got != "" && !ValidAppName(got) {
			t.Errorf("AppNameFrom(%q) = %q, which isn't a valid app name", in, got)
		}
	}
}

func TestValidRepository_MatchesTheRegistry(t *testing.T) {
	long := strings.Repeat("a", 128)
	tests := []struct {
		name string
		ok   bool
	}{
		{"shop-api", true}, {"shop-frontend", true}, {"shop_api", true}, {"shop.api", true},
		{"web/frontend", true}, {"a/b/c", true}, {"a1", true}, {long, true},
		{"", false}, {"Shop", false}, {"shop--api", false}, {"shop__api", false}, {"shop-", false},
		{"-shop", false}, {".shop", false}, {"/shop", false}, {"shop/", false}, {"web//frontend", false},
		{"web/../x", false}, {"shop api", false}, {"shop:1", false}, {"shop@sha256", false},
		{long + "a", false},
	}
	for _, tt := range tests {
		if got := ValidRepository(tt.name); got != tt.ok {
			t.Errorf("ValidRepository(%q) = %v, want %v", tt.name, got, tt.ok)
		}
	}
}

func TestParseProject_Repository(t *testing.T) {
	p, err := ParseProject([]byte(`{"app":"shop","repository":"shop-frontend"}`))
	if err != nil || p.Repository != "shop-frontend" {
		t.Fatalf("ParseProject = %+v, %v", p, err)
	}
	if _, err := ParseProject([]byte(`{"app":"shop","repository":"Shop Frontend"}`)); err == nil || !strings.Contains(err.Error(), `"repository"`) {
		t.Fatalf("ParseProject accepted a bad repository (err %v)", err)
	}
}

func TestParseProject_Rejects(t *testing.T) {
	for _, in := range []string{
		``, `[]`, `{"app":"../x"}`, `{"app":"Shop API"}`, `{"app_id":"a/b","app":"x"}`, `{"app":"x","org":"a b"}`,
		`{"app":"x","app_id":"` + string(make([]byte, 100)) + `"}`, `{"app":"x"} trailing`,
		`{"app":"x","repository":"../x"}`, `{"app":"x","repository":"registry.example.test:5000/x"}`,
	} {
		if _, err := ParseProject([]byte(in)); err == nil {
			t.Errorf("ParseProject(%q) accepted", in)
		}
	}
}

func TestSaveUser_ModeAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kx")
	path := filepath.Join(dir, "config.json")
	u := &User{}
	u.AddLogin("acme-corp", OrgLogin{Token: tokCorp, User: "sam@acme.dev"})
	if err := SaveUser(path, u); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !isWindows && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files in %s: %v", dir, entries)
	}
	back, err := LoadUser(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.DefaultOrg != "acme-corp" || back.Orgs["acme-corp"].Token != tokCorp {
		t.Fatalf("round trip lost data: default=%q", back.DefaultOrg)
	}
}

func TestLoadUser_TightensLooseMode(t *testing.T) {
	if isWindows {
		t.Skip("POSIX modes only")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"default_org":"a","orgs":{"a":{"token":"`+tokCorp+`"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUser(path); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestLoadUser_Missing(t *testing.T) {
	u, err := LoadUser(filepath.Join(t.TempDir(), "nope", "config.json"))
	if err != nil || u == nil || len(u.Orgs) != 0 {
		t.Fatalf("LoadUser(missing) = %+v, %v", u, err)
	}
}

func TestUserLogins(t *testing.T) {
	u := &User{}
	u.AddLogin("acme-corp", OrgLogin{Token: tokCorp})
	u.AddLogin("acme-labs", OrgLogin{Token: tokLabs})
	if u.DefaultOrg != "acme-labs" {
		t.Fatalf("DefaultOrg = %q, want the most recent login", u.DefaultOrg)
	}
	if got := u.OrgNames(); !reflect.DeepEqual(got, []string{"acme-corp", "acme-labs"}) {
		t.Fatalf("OrgNames = %v", got)
	}
	u.RemoveLogin("acme-labs")
	if u.DefaultOrg != "acme-corp" {
		t.Fatalf("DefaultOrg after removing the default = %q, want the remaining org", u.DefaultOrg)
	}
	u.RemoveLogin("acme-corp")
	if u.DefaultOrg != "" || len(u.Orgs) != 0 {
		t.Fatalf("after removing all: %+v", u)
	}
}

func TestSaveProject(t *testing.T) {
	dir := t.TempDir()
	path, err := SaveProject(dir, Project{App: "shop-api", AppID: "6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c", Org: "acme-corp"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"app\": \"shop-api\",\n  \"app_id\": \"6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c\",\n  \"org\": \"acme-corp\"\n}\n"
	if string(data) != want {
		t.Fatalf(".kx.json =\n%s\nwant\n%s", data, want)
	}
	if path, err = SaveProject(dir, Project{App: "shop-api", Org: "acme-corp", Repository: "shop-frontend"}); err != nil {
		t.Fatal(err)
	}
	if data, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	want = "{\n  \"app\": \"shop-api\",\n  \"org\": \"acme-corp\",\n  \"repository\": \"shop-frontend\"\n}\n"
	if string(data) != want {
		t.Fatalf(".kx.json =\n%s\nwant\n%s", data, want)
	}
}

func TestUserPath(t *testing.T) {
	p, err := UserPath(func(k string) string {
		if k == "KX_CONFIG_DIR" {
			return "/x/y"
		}
		return ""
	})
	if err != nil || p != filepath.Join("/x/y", "config.json") {
		t.Fatalf("UserPath = %q, %v", p, err)
	}
}

func FuzzParseUser(f *testing.F) {
	f.Add([]byte(`{"api_url":"https://platform.kubernetix.ai/api/v1","default_org":"acme-corp","orgs":{"acme-corp":{"token":"` + tokCorp + `","expires_at":"2026-12-11T09:00:00Z","user":"sam@acme.dev"}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"orgs":null}`))
	f.Add([]byte(`{"orgs":{"":{}}}`))
	f.Add([]byte(`{"orgs":{"a":{"expires_at":"nope"}}}`))
	f.Add([]byte("\x00\xff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, err := ParseUser(data)
		if err != nil {
			return
		}
		if u.Orgs == nil {
			t.Fatal("ParseUser returned nil Orgs")
		}
		for name := range u.Orgs {
			if !validOrg(name) {
				t.Fatalf("accepted invalid org name %q", name)
			}
		}
	})
}

func FuzzParseProject(f *testing.F) {
	f.Add([]byte(`{"app":"shop-api","app_id":"6f1c2a9e-3b7d-4e21-9c0a-5d8e7f6a1b2c","org":"acme-corp"}`))
	f.Add([]byte(`{"app":"x"}`))
	f.Add([]byte(`{"app":"../../etc"}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseProject(data)
		if err != nil {
			return
		}
		if !validApp(p.App) || (p.AppID != "" && !validAppID(p.AppID)) || (p.Org != "" && !validOrg(p.Org)) ||
			(p.Repository != "" && !ValidRepository(p.Repository)) {
			t.Fatalf("accepted invalid project %+v", p)
		}
	})
}

func FuzzAppNameFrom(f *testing.F) {
	for _, s := range []string{"My_App", "ShopAPI", "--a--b--", "日本語", strings.Repeat("x", 100)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, folder string) {
		name := AppNameFrom(folder)
		if name != "" && !ValidAppName(name) {
			t.Fatalf("AppNameFrom(%q) = %q, not a valid app name", folder, name)
		}
	})
}
