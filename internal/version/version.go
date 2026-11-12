// Package version holds the build stamp of the kx binary.
//
// Release builds set the three variables with -ldflags:
//
//	-X github.com/kubernetix-ai/kx/internal/version.Version=1.0.0
//	-X github.com/kubernetix-ai/kx/internal/version.Commit=<full sha>
//	-X github.com/kubernetix-ai/kx/internal/version.Date=<RFC3339>
//
// Without them (go install, go build), Get falls back to what the Go
// toolchain embeds: the module version and the VCS details, and finally to
// "dev" and "unknown".
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set at link time by release builds.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Info is the resolved build stamp.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// readBuildInfo is debug.ReadBuildInfo; tests replace it.
var readBuildInfo = debug.ReadBuildInfo

// Get returns the build stamp with fallbacks applied.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if info.Version == "" || info.Commit == "" || info.Date == "" {
		if bi, ok := readBuildInfo(); ok {
			if v := bi.Main.Version; info.Version == "" && v != "" && v != "(devel)" {
				info.Version = strings.TrimPrefix(v, "v")
			}
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if info.Commit == "" {
						info.Commit = s.Value
					}
				case "vcs.time":
					if info.Date == "" {
						info.Date = s.Value
					}
				}
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
	return info
}

// String renders the stamp the way `kx version` prints it:
// kx <ver> (<commit12>, <date>, <os>/<arch>).
func String() string {
	i := Get()
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return fmt.Sprintf("kx %s (%s, %s, %s/%s)", i.Version, commit, i.Date, i.OS, i.Arch)
}
