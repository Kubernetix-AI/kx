package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	imageregistry "github.com/kubernetix-ai/kx/internal/registry"
)

const (
	olderDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	commitRef   = "registry.kubernetix.ai/acme-corp/shop-api:3f9c2a1b7d4e"
	versionRef  = "registry.kubernetix.ai/acme-corp/shop-api:0.0.11"
)

// thisBuild is the labels kx puts on an image of ghSHA built from the
// repository root's Dockerfile.
var thisBuild = map[string]string{
	"org.opencontainers.image.revision": ghSHA,
	"ai.kubernetix.kx.context":          ".",
	"ai.kubernetix.kx.dockerfile":       "Dockerfile",
}

// gitCheckout makes the harness folder a real git repository (CI then
// names the commit), so kx can tell where the build sits in it.
func gitCheckout(t *testing.T, h *harness) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := os.RemoveAll(filepath.Join(h.dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", h.dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

func (h *harness) built() bool {
	for _, c := range h.docker.got() {
		if strings.HasPrefix(c, "build ") {
			return true
		}
	}
	return false
}

func TestDeploy_LabelsTheImageWithTheCommit(t *testing.T) {
	h := newHarness(t)
	gitCheckout(t, h)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	got := h.docker.lastLabels()
	for k, v := range thisBuild {
		if got[k] != v {
			t.Fatalf("labels = %v, want %v", got, thisBuild)
		}
	}
}

func TestDeploy_ReRunOfTheSameCommitReusesTheImage(t *testing.T) {
	h := newHarness(t)
	gitCheckout(t, h)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.registry.images[commitRef] = imageregistry.Image{Digest: olderDigest, Labels: thisBuild}
	h.registry.images[versionRef] = imageregistry.Image{Digest: olderDigest, Labels: thisBuild}
	h.expect(h.run("deploy", "--json"), 0)
	if h.built() {
		t.Fatalf("built again: %q", h.docker.got())
	}
	out := h.deployJSON()
	if out["reused"] != true || out["image"] != "registry.kubernetix.ai/acme-corp/shop-api@"+olderDigest ||
		strings.Join(toStrings(out["tags"]), ",") != "3f9c2a1b7d4e,0.0.11" || out["version_tag"] != "0.0.11" {
		t.Fatalf("json = %v", out)
	}
	if !strings.Contains(h.err.String(), "✓ Reused    "+commitRef+"\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if h.api.deployPosts[0].Body["image_url"] != "registry.kubernetix.ai/acme-corp/shop-api@"+olderDigest {
		t.Fatalf("deployed %v", h.api.deployPosts[0].Body["image_url"])
	}
}

func TestDeploy_VersionTagFromTheSameCommitIsReused(t *testing.T) {
	h := newHarness(t)
	gitCheckout(t, h)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.registry.images[versionRef] = imageregistry.Image{Digest: olderDigest, Labels: thisBuild}
	h.expect(h.run("deploy"), 0)
	if h.built() {
		t.Fatalf("built again: %q", h.docker.got())
	}
	if !strings.Contains(strings.Join(h.docker.got(), "\n"), "retag "+commitRef+" "+olderDigest) {
		t.Fatalf("the commit's tag wasn't pointed at the reused image: %q", h.docker.got())
	}
}

func TestDeploy_VersionTagFromAnotherCommitStops(t *testing.T) {
	h := newHarness(t)
	gitCheckout(t, h)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	other := map[string]string{"org.opencontainers.image.revision": strings.Repeat("a", 40)}
	h.registry.images[versionRef] = imageregistry.Image{Digest: olderDigest, Labels: other}
	h.expect(h.run("deploy"), 7)
	if h.built() || len(h.api.deployPosts) != 0 {
		t.Fatalf("built or deployed: %q", h.docker.got())
	}
	if !strings.Contains(h.err.String(), "deploy what 0.0.11 holds: kx deploy --image registry.kubernetix.ai/acme-corp/shop-api@"+olderDigest) {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_RebuildBuilds(t *testing.T) {
	h := newHarness(t)
	gitCheckout(t, h)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.registry.images[commitRef] = imageregistry.Image{Digest: olderDigest, Labels: thisBuild}
	h.registry.images[versionRef] = imageregistry.Image{Digest: olderDigest, Labels: thisBuild}
	h.expect(h.run("deploy", "--rebuild", "--json"), 0)
	if !h.built() {
		t.Fatalf("--rebuild didn't build: %q", h.docker.got())
	}
	out := h.deployJSON()
	if out["reused"] != false || out["image"] != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest || out["version_tag"] != "" {
		t.Fatalf("json = %v; the new build is deployed and 0.0.11 keeps the earlier build", out)
	}
	for _, c := range h.docker.got() {
		if strings.HasPrefix(c, "push "+versionRef) || strings.HasPrefix(c, "retag "+versionRef) {
			t.Fatalf("moved the version tag: %q", c)
		}
	}
}

func TestDeploy_UnlabelledCommitTagIsRebuilt(t *testing.T) {
	h := newHarness(t)
	gitCheckout(t, h)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.registry.images[commitRef] = imageregistry.Image{Digest: olderDigest}
	h.expect(h.run("deploy", "--json"), 0)
	if !h.built() || h.deployJSON()["reused"] != false {
		t.Fatalf("calls %q\n%s", h.docker.got(), h.out.String())
	}
}

func TestDeploy_UncommittedChangesAreAlwaysBuilt(t *testing.T) {
	// On a laptop the folder isn't the commit as it is, so nothing is
	// reused and nothing is labelled with a commit.
	h := newHarness(t)
	gitCheckout(t, h)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	if !h.built() || len(h.docker.lastLabels()) != 0 {
		t.Fatalf("calls %q, labels %v", h.docker.got(), h.docker.lastLabels())
	}
	for _, c := range h.docker.got() {
		if strings.HasPrefix(c, "lookup ") {
			t.Fatalf("looked for an earlier build of a folder that isn't a commit: %q", c)
		}
	}
}
