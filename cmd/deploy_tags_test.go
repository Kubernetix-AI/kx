package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kubernetix-ai/kx/internal/config"
	imageregistry "github.com/kubernetix-ai/kx/internal/registry"
)

// gitlabTagRun makes the harness a GitLab pipeline for the tag tag.
func gitlabTagRun(h *harness, tag string) {
	h.commit()
	for k, v := range map[string]string{"GITLAB_CI": "true", "CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_COMMIT_SHA": ghSHA,
		"CI_COMMIT_TAG": tag, "CI_PROJECT_PATH": "acme/shop-api", "CI_PIPELINE_ID": "55", "CI_JOB_ID": "77", "KX_TOKEN": tokDeploy} {
		h.env[k] = v
	}
}

func (h *harness) deployJSON() map[string]any {
	h.t.Helper()
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		h.t.Fatalf("stdout is not one JSON value: %v\n%s", err, h.out.String())
	}
	return out
}

func TestDeploy_VersionTagFromATagRun(t *testing.T) {
	h := newHarness(t)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--json"), 0)
	want := []string{
		"check",
		"lookup registry.kubernetix.ai/acme-corp/shop-api:0.0.11", // before building: is 0.0.11 another commit's?
		"build shop-api:dc4138a08c09",
		"tag shop-api:dc4138a08c09 registry.kubernetix.ai/acme-corp/shop-api:dc4138a08c09",
		"push registry.kubernetix.ai/acme-corp/shop-api:dc4138a08c09",
		"lookup registry.kubernetix.ai/acme-corp/shop-api:0.0.11",
		"retag registry.kubernetix.ai/acme-corp/shop-api:0.0.11 " + testDigest,
		"lookup registry.kubernetix.ai/acme-corp/shop-api:0.0.11",
	}
	if got := h.docker.got(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(h.err.String(), "✓ Pushed    registry.kubernetix.ai/acme-corp/shop-api:0.0.11\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	out := h.deployJSON()
	if out["version_tag"] != "0.0.11" || strings.Join(toStrings(out["tags"]), ",") != "dc4138a08c09,0.0.11" ||
		out["image"] != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest {
		t.Fatalf("json = %v", out)
	}
	if h.api.deployPosts[0].Body["image_url"] != "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest {
		t.Fatalf("deployed %v, want the image by digest", h.api.deployPosts[0].Body["image_url"])
	}
}

func TestDeploy_TagRunWhoseTagIsntAVersion(t *testing.T) {
	h := newHarness(t)
	gitlabTagRun(h, "nightly")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--json"), 0)
	out := h.deployJSON()
	if out["version_tag"] != "" || strings.Join(toStrings(out["tags"]), ",") != "dc4138a08c09" {
		t.Fatalf("json = %v", out)
	}
	for _, c := range h.docker.got() {
		if strings.HasPrefix(c, "lookup ") {
			t.Fatalf("looked up a tag with no version to push: %q", c)
		}
	}
}

func TestDeploy_VersionTagHoldingAnotherImage(t *testing.T) {
	h := newHarness(t)
	gitlabTagRun(h, "v0.0.11")
	h.dockerfile("FROM scratch\n")
	h.registry.images["registry.kubernetix.ai/acme-corp/shop-api:0.0.11"] = imageregistry.Image{Digest: "sha256:" + strings.Repeat("ee", 32)}
	h.expect(h.run("deploy"), 7)
	lines := strings.Split(strings.TrimRight(h.err.String(), "\n"), "\n")
	got := strings.Join(lines[len(lines)-2:], "\n")
	want := "registry.kubernetix.ai/acme-corp/shop-api:0.0.11 already holds another image, so kx left the tag alone and deployed nothing.\n" +
		"Tag the commit with a new version, or, if you know that image, deploy it: kx deploy --image registry.kubernetix.ai/acme-corp/shop-api@sha256:" + strings.Repeat("ee", 32)
	if got != want {
		t.Fatalf("last lines:\n%s\nwant\n%s", got, want)
	}
	if len(h.api.deployPosts) != 0 {
		t.Fatal("deployed although the version tag holds another image")
	}
}

func TestDeploy_TagFlag(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--tag", "1.4.0-beta.2", "--json"), 0)
	got := h.docker.got()
	want := "lookup registry.kubernetix.ai/acme-corp/shop-api:1.4.0-beta.2\n" +
		"retag registry.kubernetix.ai/acme-corp/shop-api:1.4.0-beta.2 " + testDigest + "\n" +
		"lookup registry.kubernetix.ai/acme-corp/shop-api:1.4.0-beta.2"
	if strings.Join(got[len(got)-3:], "\n") != want {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
	if out := h.deployJSON(); out["version_tag"] != "1.4.0-beta.2" {
		t.Fatalf("json = %v", out)
	}

	// --tag that holds another image: the fix talks about --tag.
	h.registry.images["registry.kubernetix.ai/acme-corp/shop-api:stable"] = imageregistry.Image{Digest: "sha256:" + strings.Repeat("ee", 32)}
	h.expect(h.run("deploy", "--tag", "stable"), 7)
	if !strings.Contains(h.err.String(), "Choose another --tag, or, if you know that image, deploy it: kx deploy --image registry.kubernetix.ai/acme-corp/shop-api@sha256:"+strings.Repeat("ee", 32)) {
		t.Fatalf("stderr:\n%s", h.err.String())
	}

	// A lookup that fails stops the deploy before anything is deployed.
	h.registry.err = errors.New("registry: registry.kubernetix.ai: connection reset by peer")
	posts := len(h.api.deployPosts)
	h.expect(h.run("deploy", "--tag", "1.4.0"), 7)
	if !strings.Contains(h.err.String(), "kx couldn't check whether registry.kubernetix.ai/acme-corp/shop-api:1.4.0 already exists, so nothing was deployed.") || len(h.api.deployPosts) != posts {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_JSONWithoutAVersionTag(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--json"), 0)
	out := h.deployJSON()
	if out["version_tag"] != "" || strings.Join(toStrings(out["tags"]), ",") != "local-20261112090000" {
		t.Fatalf("json = %v", out)
	}
	// --image pushes nothing: tags is an empty list, not null.
	h.expect(h.run("deploy", "--json", "--image", "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest), 0)
	if !strings.Contains(h.out.String(), `"tags": []`) {
		t.Fatalf("stdout:\n%s", h.out.String())
	}
}

func TestDeploy_RepositoryFlag(t *testing.T) {
	h := newHarness(t)
	gitlabTagRun(h, "v0.0.11") // an app token (shop-api) pushing to another repository
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--repository", "shop-frontend"), 0)
	want := []string{
		"check",
		"lookup registry.kubernetix.ai/acme-corp/shop-frontend:0.0.11", // before building: is 0.0.11 another commit's?
		"build shop-api:dc4138a08c09",
		"tag shop-api:dc4138a08c09 registry.kubernetix.ai/acme-corp/shop-frontend:dc4138a08c09",
		"push registry.kubernetix.ai/acme-corp/shop-frontend:dc4138a08c09",
		"lookup registry.kubernetix.ai/acme-corp/shop-frontend:0.0.11",
		"retag registry.kubernetix.ai/acme-corp/shop-frontend:0.0.11 " + testDigest,
		"lookup registry.kubernetix.ai/acme-corp/shop-frontend:0.0.11",
	}
	if got := h.docker.got(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if h.api.deployPosts[0].Body["image_url"] != "registry.kubernetix.ai/acme-corp/shop-frontend@"+testDigest {
		t.Fatalf("deployed %v", h.api.deployPosts[0].Body["image_url"])
	}
	if !strings.Contains(h.err.String(), "✓ Pushed    registry.kubernetix.ai/acme-corp/shop-frontend:0.0.11\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestDeploy_RepositoryFromKxJSON(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	if _, err := config.SaveProject(h.dir, config.Project{App: "shop-api", AppID: appShopID, Org: "acme-corp", Repository: "shop-frontend"}); err != nil {
		t.Fatal(err)
	}
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy"), 0)
	if got := h.docker.got(); got[len(got)-1] != "push registry.kubernetix.ai/acme-corp/shop-frontend:local-20261112090000" {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}

	// --repository wins over .kx.json.
	h.docker.calls = nil
	h.expect(h.run("deploy", "--repository", "web/frontend"), 0)
	if got := h.docker.got(); got[len(got)-1] != "push registry.kubernetix.ai/acme-corp/web/frontend:local-20261112090000" {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}

	// .kx.json's repository goes with its app: another app gets its own name.
	h.docker.calls = nil
	h.expect(h.run("deploy", "--app", "web"), 0)
	if got := h.docker.got(); got[len(got)-1] != "push registry.kubernetix.ai/acme-corp/web:local-20261112090000" {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}

	// --image builds nothing, so .kx.json's repository doesn't matter.
	h.expect(h.run("deploy", "--image", "registry.kubernetix.ai/acme-corp/shop-api@"+testDigest), 0)
}

func TestDeploy_RepositoryThatLooksLikeAnAddress(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.dockerfile("FROM scratch\n")
	// A name with dots is a name when it isn't followed by the organization.
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("deploy", "--repository", "my.app/frontend"), 0)
	if got := h.docker.got(); got[len(got)-1] != "push registry.kubernetix.ai/acme-corp/my.app/frontend:local-20261112090000" {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}

	if _, err := config.SaveProject(h.dir, config.Project{App: "shop-api", AppID: appShopID, Org: "acme-corp", Repository: "registry.kubernetix.ai/acme-corp/shop-frontend"}); err != nil {
		t.Fatal(err)
	}
	h.expect(h.run("deploy"), 2)
	want := "The repository registry.kubernetix.ai/acme-corp/shop-frontend in .kx.json includes the registry's address and organization.\nSet \"repository\" to shop-frontend in .kx.json.\n"
	if !strings.HasSuffix(h.err.String(), want) {
		t.Fatalf("stderr:\n%s\nwant it to end with\n%s", h.err.String(), want)
	}
}

func TestDeploy_RepositoryStartingWithTheOrg(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--repository", "acme-corp/shop-frontend"), 2)
	want := "--repository acme-corp/shop-frontend starts with the organization's name, so kx would push to acme-corp/acme-corp/shop-frontend.\nRun: kx deploy --repository shop-frontend\n"
	if !strings.HasSuffix(h.err.String(), want) {
		t.Fatalf("stderr:\n%s\nwant it to end with\n%s", h.err.String(), want)
	}
	if n := len(h.docker.got()); n != 1 { // only the Docker check
		t.Fatalf("docker calls: %q", h.docker.got())
	}

	if _, err := config.SaveProject(h.dir, config.Project{App: "shop-api", AppID: appShopID, Org: "acme-corp", Repository: "acme-corp/shop-frontend"}); err != nil {
		t.Fatal(err)
	}
	h.expect(h.run("deploy"), 2)
	want = "The repository acme-corp/shop-frontend in .kx.json starts with the organization's name, so kx would push to acme-corp/acme-corp/shop-frontend.\nSet \"repository\" to shop-frontend in .kx.json.\n"
	if !strings.HasSuffix(h.err.String(), want) {
		t.Fatalf("stderr:\n%s\nwant it to end with\n%s", h.err.String(), want)
	}
}

func TestDeploy_TagAndRepositoryFlagErrors(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	image := "registry.kubernetix.ai/acme-corp/shop-api@" + testDigest
	tests := []struct {
		args      []string
		what, fix string
	}{
		{[]string{"deploy", "--tag", "bad tag"}, `"bad tag" isn't an image tag:`, "Run: kx deploy --tag 1.4.0"},
		{[]string{"deploy", "--tag", "-v"}, `"-v" isn't an image tag:`, "Run: kx deploy --tag 1.4.0"},
		{[]string{"deploy", "--tag", strings.Repeat("a", 129)}, "isn't an image tag:", "Run: kx deploy --tag 1.4.0"},
		{[]string{"deploy", "--image", image, "--tag", "1.4.0"}, "--image deploys an image that's already pushed, so it can't be used with --tag or --repository.", "Run: kx deploy --image <image>"},
		{[]string{"deploy", "--image", image, "--repository", "shop-frontend"}, "--image deploys an image that's already pushed, so it can't be used with --tag or --repository.", "Run: kx deploy --image <image>"},
		{[]string{"deploy", "--repository", "Shop Frontend"}, `"Shop Frontend" isn't a repository name:`, "Run: kx deploy --repository shop-frontend"},
		{[]string{"deploy", "--repository", "shop--frontend"}, `"shop--frontend" isn't a repository name:`, "Run: kx deploy --repository shop-frontend"},
		{[]string{"deploy", "--repository", "registry.kubernetix.ai/acme-corp/shop-frontend"}, "--repository takes a repository's name, without the registry's address and organization.", "Run: kx deploy --repository shop-frontend"},
	}
	for _, tt := range tests {
		h.expect(h.run(tt.args...), 2)
		lines := strings.Split(strings.TrimRight(h.err.String(), "\n"), "\n")
		if len(lines) < 2 || !strings.Contains(lines[len(lines)-2], tt.what) || lines[len(lines)-1] != tt.fix {
			t.Errorf("kx %v: stderr =\n%s\nwant %q then %q", tt.args, h.err.String(), tt.what, tt.fix)
		}
	}
	if n := len(h.api.got()); n != 0 {
		t.Fatalf("made %d API calls for flag errors", n)
	}
	if n := len(h.docker.got()); n != 0 {
		t.Fatalf("ran Docker for flag errors: %q", h.docker.got())
	}
}

func TestDeploy_GitHubSummaryShowsTheTags(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	h.env["GITHUB_REF_TYPE"], h.env["GITHUB_REF_NAME"], h.env["GITHUB_REF"] = "tag", "v2.3.0", "refs/tags/v2.3.0"
	h.dockerfile("FROM scratch\n")
	h.expect(h.run("deploy", "--github"), 0)
	sum, _ := os.ReadFile(h.env["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(sum), "| Tags | `dc4138a08c09`, `2.3.0` |") {
		t.Fatalf("summary:\n%s", sum)
	}
}

func TestLink_KeepsTheRepository(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	if _, err := config.SaveProject(h.dir, config.Project{App: "shop-api", AppID: appShopID, Org: "acme-corp", Repository: "shop-frontend"}); err != nil {
		t.Fatal(err)
	}
	h.expect(h.run("link", "web"), 0)
	if !strings.Contains(h.err.String(), "Its images still go to the repository shop-frontend, as .kx.json says.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	_, p, err := config.FindProject(h.dir)
	if err != nil || p == nil || p.App != "web" || p.Repository != "shop-frontend" {
		t.Fatalf(".kx.json = %+v, %v; want web with the repository kept", p, err)
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func TestDeploy_RepositoryWithATagsSegment(t *testing.T) {
	// Kubernetix's image routes read a .../tags/... path as a tag, so such
	// a repository could never be deleted from the dashboard.
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.dockerfile("FROM scratch\n")
	const what = "A repository name can't contain a segment called \"tags\"; Kubernetix's image routes would read it as a tag.\n"
	for _, name := range []string{"tags", "web/tags", "tags/frontend", "a/tags/b"} {
		h.expect(h.run("deploy", "--repository", name), 2)
		if !strings.HasSuffix(h.err.String(), what+"Choose a name without that segment, then run kx deploy again.\n") {
			t.Fatalf("--repository %s: stderr:\n%s", name, h.err.String())
		}
	}
	if len(h.api.got()) != 0 || len(h.docker.got()) != 0 {
		t.Fatal("did something with a refused repository name")
	}
	// A segment that only contains the word is fine.
	h.expect(h.run("deploy", "--repository", "web/tags-api"), 0)

	if _, err := config.SaveProject(h.dir, config.Project{App: "shop-api", AppID: appShopID, Org: "acme-corp", Repository: "web/tags"}); err != nil {
		t.Fatal(err)
	}
	h.expect(h.run("deploy"), 2)
	if !strings.HasSuffix(h.err.String(), what+"Change \"repository\" in .kx.json to a name without that segment.\n") {
		t.Fatalf(".kx.json: stderr:\n%s", h.err.String())
	}
}
