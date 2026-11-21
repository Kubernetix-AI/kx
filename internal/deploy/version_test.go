package deploy

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/ci"
)

func TestVersionOf(t *testing.T) {
	tests := []struct{ tag, want string }{
		{"v1.2.3", "1.2.3"},
		{"1.2.3", "1.2.3"},
		{"v0.0.11", "0.0.11"},
		{"v1.0.0-rc.1", "1.0.0-rc.1"},
		{"2.0.0-beta-2.x", "2.0.0-beta-2.x"},
		{"v10.20.30", "10.20.30"},
		{"V1.2.3", ""},                            // only a lowercase v
		{"v1.2", ""},                              // three numbers
		{"1.2.3.4", ""},                           // three numbers
		{"v01.2.3", ""},                           // no leading zeros
		{"1.2.3-rc.01", ""},                       // nor in a numeric pre-release part
		{"1.2.3+build.5", ""},                     // a registry tag can't hold +
		{"1.2.3-", ""},                            // an empty pre-release
		{"1.2.3-rc..1", ""},                       // an empty pre-release part
		{"release-1.2.3", ""},                     // not a version
		{"v1.2.3\n", ""},                          // nothing around it
		{"latest", ""},                            // not a version
		{"1.2.3-" + strings.Repeat("a", 130), ""}, // longer than a registry tag
	}
	for _, tt := range tests {
		if got := versionOf(tt.tag); got != tt.want {
			t.Errorf("versionOf(%q) = %q, want %q", tt.tag, got, tt.want)
		}
	}
}

func TestHighestVersion(t *testing.T) {
	tests := []struct {
		tags []string
		want string
	}{
		{nil, ""},
		{[]string{"latest", "prod"}, ""},
		{[]string{"v0.0.11"}, "0.0.11"},
		{[]string{"v0.0.9", "v0.0.11", "v0.0.10"}, "0.0.11"},
		{[]string{"v1.0.0-rc.1", "v1.0.0"}, "1.0.0"},
		{[]string{"v1.0.0-rc.2", "v1.0.0-rc.10"}, "1.0.0-rc.10"},
		{[]string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1"}, "1.0.0-rc.1"},
		{[]string{"1.0.0-alpha.beta", "1.0.0-alpha.1"}, "1.0.0-alpha.beta"}, // letters sort after numbers
		{[]string{"1.0.0-alpha", "1.0.0-alpha.1"}, "1.0.0-alpha.1"},         // more parts sort later
		{[]string{"v2.0.0", "1.99.99"}, "2.0.0"},
		{[]string{"v1.2.3", "1.2.3"}, "1.2.3"},
		{[]string{"99999999999999999999.0.0", "100000000000000000000.0.0"}, "100000000000000000000.0.0"},
		{[]string{"nightly", "v3.1.4", "deploy-7"}, "3.1.4"},
	}
	for _, tt := range tests {
		if got := highestVersion(tt.tags); got != tt.want {
			t.Errorf("highestVersion(%q) = %q, want %q", tt.tags, got, tt.want)
		}
	}
}

func TestBuildTags(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 30, 5, 0, time.UTC)
	ctx := context.Background()
	t.Run("not a repository, in CI too", func(t *testing.T) {
		env := ci.Env{IsCI: true, CommitSHA: "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345", Tag: "v0.0.11"}
		got := BuildTags(ctx, env, t.TempDir(), now)
		if got.Commit != "local-20261112093005" || got.Version != "" || got.Revision != "" || got.CICommit != env.CommitSHA {
			t.Fatalf("tags = %+v; without a checkout kx can't tell what it builds", got)
		}
	})
	t.Run("not a repository", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{}, t.TempDir(), now)
		if got.Commit != "local-20261112093005" || got.Version != "" || got.CICommit != "" {
			t.Fatalf("tags = %+v", got)
		}
	})
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	commitAll := func() string {
		git(t, dir, "add", ".")
		git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "x")
		return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	}
	git(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	older := commitAll()
	git(t, dir, "tag", "v0.0.9") // an older commit's version
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	sha := commitAll()

	t.Run("no tag on the commit", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{}, dir, now)
		if got.Commit != sha[:12] || got.Version != "" || got.Revision != sha {
			t.Fatalf("tags = %+v; want %q and no version (v0.0.9 is on another commit)", got, sha[:12])
		}
	})
	t.Run("the CI system's tag of this commit", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{IsCI: true, CommitSHA: sha, Tag: "v0.0.11"}, dir, now)
		if got.Version != "0.0.11" || got.CICommit != "" {
			t.Fatalf("tags = %+v", got)
		}
	})
	t.Run("the CI system names another commit", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{IsCI: true, CommitSHA: older, Tag: "v0.0.9"}, dir, now)
		if got.Commit != sha[:12] || got.Revision != sha || got.Version != "" || got.CICommit != older {
			t.Fatalf("tags = %+v; want HEAD's tags, and the CI commit only reported", got)
		}
	})
	t.Run("the CI system's commit in capitals is the same commit", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{IsCI: true, CommitSHA: strings.ToUpper(sha)}, dir, now)
		if got.CICommit != "" || got.Revision != sha {
			t.Fatalf("tags = %+v", got)
		}
	})
	git(t, dir, "tag", "nightly")
	git(t, dir, "tag", "v0.0.10")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "tag.gpgsign=false", "tag", "-a", "-m", "release", "v0.0.11")
	t.Run("highest version on the commit, annotated or not", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{}, dir, now)
		if got.Commit != sha[:12] || got.Version != "0.0.11" {
			t.Fatalf("tags = %+v; want %q, 0.0.11", got, sha[:12])
		}
	})
	t.Run("CI tag and git tags together", func(t *testing.T) {
		env := ci.Env{IsCI: true, CommitSHA: sha, Tag: "v0.0.12-rc.1"}
		if version := BuildTags(ctx, env, dir, now).Version; version != "0.0.12-rc.1" {
			t.Fatalf("version = %q, want 0.0.12-rc.1, the highest of the CI's tag and git's", version)
		}
	})
	t.Run("uncommitted changes get no version", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "new.txt"), "x")
		got := BuildTags(ctx, ci.Env{}, dir, now)
		if got.Commit != sha[:12]+"-dirty-20261112093005" || got.Version != "" || got.Revision != "" {
			t.Fatalf("tags = %+v; the image isn't the tagged commit's code", got)
		}
	})
	t.Run("in CI the checkout is built as it is", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{IsCI: true, CommitSHA: sha}, dir, now)
		if got.Commit != sha[:12] || got.Version != "0.0.11" || got.Revision != sha {
			t.Fatalf("tags = %+v", got)
		}
	})
}
