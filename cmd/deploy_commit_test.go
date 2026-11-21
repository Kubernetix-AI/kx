package cmd

import (
	"os"
	"strings"
	"testing"

	imageregistry "github.com/kubernetix-ai/kx/internal/registry"
)

const (
	otherSHA   = "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345"
	olderImage = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	versionRef = "registry.kubernetix.ai/acme-corp/shop-api:0.0.11"
)

func (h *harness) built() bool {
	for _, c := range h.docker.got() {
		if strings.HasPrefix(c, "build ") {
			return true
		}
	}
	return false
}

func TestDeploy_TagsWhatIsCheckedOutNotTheCICommit(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	// The run names another commit, and a tag of that commit.
	h.env["GITHUB_SHA"], h.env["GITHUB_REF_TYPE"], h.env["GITHUB_REF_NAME"] = otherSHA, "tag", "v9.9.9"
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 0)
	if !strings.Contains(h.err.String(), "GitHub Actions names commit 3f9c2a1b7d4e, but this folder is at dc4138a08c09; kx builds and tags what is checked out.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	calls := strings.Join(h.docker.got(), "\n")
	if !strings.Contains(calls, "push registry.kubernetix.ai/acme-corp/shop-api:dc4138a08c09") || strings.Contains(calls, "9.9.9") || strings.Contains(calls, "3f9c2a1b7d4e") {
		t.Fatalf("calls:\n%s\nwant HEAD's tag and not the CI commit's version", calls)
	}
	if got := h.docker.lastLabels()["org.opencontainers.image.revision"]; got != ghSHA {
		t.Fatalf("revision label = %q, want HEAD %s", got, ghSHA)
	}
	// The commit status and the summary still name the CI system's commit.
	sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(sum), "| Commit | `3f9c2a1b7d4e` |") {
		t.Fatalf("summary:\n%s", sum)
	}
}

func TestDeploy_CIWithoutACheckout(t *testing.T) {
	h := newHarness(t)
	for k, v := range map[string]string{"GITLAB_CI": "true", "CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_COMMIT_SHA": otherSHA,
		"CI_PROJECT_PATH": "acme/shop-api", "CI_PIPELINE_ID": "55", "CI_JOB_ID": "77", "KX_TOKEN": tokDeploy} {
		h.env[k] = v
	}
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	if !strings.Contains(h.err.String(), "GitLab CI names commit 3f9c2a1b7d4e, but this folder isn't a git checkout, so kx tags the image local-20261112090000.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.docker.lastLabels()) != 0 {
		t.Fatalf("labels = %v; without a checkout kx can't say which commit it built", h.docker.lastLabels())
	}
}

func TestDeploy_LabelsTheImageWithTheCommit(t *testing.T) {
	h := newHarness(t)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	want := map[string]string{
		"org.opencontainers.image.revision": ghSHA,
		"ai.kubernetix.kx.context":          ".",
		"ai.kubernetix.kx.dockerfile":       "Dockerfile",
	}
	got := h.docker.lastLabels()
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	}
}

func TestDeploy_VersionTagOnAnEarlierBuildOfThisCommit(t *testing.T) {
	// A re-run, or a second run for one tagged push: kx builds again,
	// deploys the new build by digest and leaves 0.0.11 where it is.
	h := newHarness(t)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.registry.images[versionRef] = imageregistry.Image{Digest: olderImage, Labels: map[string]string{"org.opencontainers.image.revision": ghSHA}}
	h.expect(h.run("deploy", "--json"), 0)
	if !h.built() {
		t.Fatalf("didn't build: %q", h.docker.got())
	}
	if !strings.Contains(h.err.String(), "! The version tag 0.0.11 stays on the earlier build of this commit.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	out := h.deployJSON()
	if out["image"] != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest || out["version_tag"] != "" ||
		strings.Join(toStrings(out["tags"]), ",") != "dc4138a08c09" {
		t.Fatalf("json = %v", out)
	}
	if _, has := out["reused"]; has {
		t.Fatalf("json = %v; kx never reuses a build", out)
	}
	for _, c := range h.docker.got() {
		if strings.HasPrefix(c, "retag ") {
			t.Fatalf("moved the version tag: %q", c)
		}
	}
}

func TestDeploy_VersionTagFromAnotherCommitStopsBeforeBuilding(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"another commit": {"org.opencontainers.image.revision": otherSHA},
		"no labels":      nil,
	} {
		h := newHarness(t)
		gitlabTagRun(h, "v0.0.11")
		h.dockerfile("FROM scratch\n")
		h.registry.images[versionRef] = imageregistry.Image{Digest: olderImage, Labels: labels}
		h.expect(h.run("deploy"), 7)
		if h.built() || len(h.api.deployPosts) != 0 {
			t.Fatalf("%s: built or deployed: %q", name, h.docker.got())
		}
		if !strings.Contains(h.err.String(), "deploy what 0.0.11 holds: kx deploy --image registry.kubernetix.ai/acme-corp/shop-api@"+olderImage) {
			t.Fatalf("%s: stderr:\n%s", name, h.err.String())
		}
	}
}

func TestDeploy_NoRebuildFlag(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("deploy", "--rebuild"), 2)
}
