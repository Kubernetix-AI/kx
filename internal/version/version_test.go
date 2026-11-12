package version

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestString_Stamped(t *testing.T) {
	restore := stamp("1.0.0", "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9a0b1c2", "2026-11-14T10:00:00Z")
	defer restore()

	want := "kx 1.0.0 (3f9c2a1b7d4e, 2026-11-14T10:00:00Z, " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestString_ShortCommitKeptAsIs(t *testing.T) {
	restore := stamp("1.2.3", "abc", "2026-11-14T10:00:00Z")
	defer restore()

	want := "kx 1.2.3 (abc, 2026-11-14T10:00:00Z, " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestString_Unstamped(t *testing.T) {
	restore := stamp("", "", "")
	defer restore()
	defer fakeBuildInfo("(devel)")()

	got := String()
	wantPrefix := "kx dev ("
	if len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("String() = %q, want prefix %q", got, wantPrefix)
	}
	if Get().Version != "dev" {
		t.Fatalf("Get().Version = %q, want dev", Get().Version)
	}
}

func TestGet_Fields(t *testing.T) {
	restore := stamp("1.0.0", "3f9c2a1b7d4e5f60", "2026-11-14T10:00:00Z")
	defer restore()

	info := Get()
	if info.Version != "1.0.0" || info.Commit != "3f9c2a1b7d4e5f60" || info.Date != "2026-11-14T10:00:00Z" {
		t.Fatalf("Get() = %+v", info)
	}
	if info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Fatalf("Get() platform = %s/%s", info.OS, info.Arch)
	}
}

func stamp(v, c, d string) func() {
	ov, oc, od := Version, Commit, Date
	Version, Commit, Date = v, c, d
	return func() { Version, Commit, Date = ov, oc, od }
}

// go install github.com/kubernetix-ai/kx@v1.2.3 has no -ldflags stamp, but
// Go records the module version it built.
func TestGet_GoInstallVersion(t *testing.T) {
	restore := stamp("", "", "")
	defer restore()
	for mainVersion, want := range map[string]string{"v1.2.3": "1.2.3", "v1.0.0-rc.1": "1.0.0-rc.1", "(devel)": "dev", "": "dev"} {
		undo := fakeBuildInfo(mainVersion)
		if got := Get().Version; got != want {
			t.Errorf("module version %q: Version = %q, want %q", mainVersion, got, want)
		}
		undo()
	}
	// An -ldflags stamp always wins.
	defer stamp("2.0.0", "", "")()
	defer fakeBuildInfo("v1.2.3")()
	if got := Get().Version; got != "2.0.0" {
		t.Errorf("stamped Version = %q, want 2.0.0", got)
	}
}

func fakeBuildInfo(mainVersion string) func() {
	old := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/kubernetix-ai/kx", Version: mainVersion}}, true
	}
	return func() { readBuildInfo = old }
}
