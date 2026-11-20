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
	t.Run("CI tag run", func(t *testing.T) {
		env := ci.Env{IsCI: true, CommitSHA: "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345", Tag: "v0.0.11"}
		got := BuildTags(ctx, env, t.TempDir(), now)
		commit, version := got.Commit, got.Version
		if commit != "3f9c2a1b7d4e" || version != "0.0.11" {
			t.Fatalf("tags = %q, %q", commit, version)
		}
	})
	t.Run("CI tag that isn't a version", func(t *testing.T) {
		env := ci.Env{IsCI: true, CommitSHA: "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345", Tag: "nightly"}
		if version := BuildTags(ctx, env, t.TempDir(), now).Version; version != "" {
			t.Fatalf("version = %q, want none", version)
		}
	})
	t.Run("not a repository", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{}, t.TempDir(), now)
		commit, version := got.Commit, got.Version
		if commit != "local-20261112093005" || version != "" {
			t.Fatalf("tags = %q, %q", commit, version)
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
	commitAll()
	git(t, dir, "tag", "v0.0.9") // an older commit's version
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	sha := commitAll()

	t.Run("no tag on the commit", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{}, dir, now)
		commit, version := got.Commit, got.Version
		if commit != sha[:12] || version != "" {
			t.Fatalf("tags = %q, %q; want %q and no version (v0.0.9 is on another commit)", commit, version, sha[:12])
		}
	})
	git(t, dir, "tag", "nightly")
	git(t, dir, "tag", "v0.0.10")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "tag.gpgsign=false", "tag", "-a", "-m", "release", "v0.0.11")
	t.Run("highest version on the commit, annotated or not", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{}, dir, now)
		commit, version := got.Commit, got.Version
		if commit != sha[:12] || version != "0.0.11" {
			t.Fatalf("tags = %q, %q; want %q, 0.0.11", commit, version, sha[:12])
		}
	})
	t.Run("CI commit read from git", func(t *testing.T) {
		env := ci.Env{IsCI: true, CommitSHA: sha}
		if version := BuildTags(ctx, env, dir, now).Version; version != "0.0.11" {
			t.Fatalf("version = %q, want 0.0.11", version)
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
		commit, version := got.Commit, got.Version
		if commit != sha[:12]+"-dirty-20261112093005" || version != "" {
			t.Fatalf("tags = %q, %q; the image isn't the tagged commit's code", commit, version)
		}
	})
	t.Run("in CI the CI system's commit is taken as it is", func(t *testing.T) {
		got := BuildTags(ctx, ci.Env{IsCI: true, CommitSHA: sha}, dir, now)
		commit, version := got.Commit, got.Version
		if commit != sha[:12] || version != "0.0.11" || got.Revision != sha {
			t.Fatalf("tags = %q, %q; CI tags the commit as it is, like its commit tag", commit, version)
		}
	})
}
