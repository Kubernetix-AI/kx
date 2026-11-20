package deploy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/registry"
)

const (
	testRevision = "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9012345"
	otherDigest  = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	olderDigest  = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	commitRef    = "registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e"
	versionRef   = "registry.example.test/acme-corp/shop-api:0.0.11"
)

var testProvenance = Provenance{Revision: testRevision, Context: ".", Dockerfile: "Dockerfile"}

// labelled is an image built by kx from the build p describes.
func labelled(digest string, p Provenance) registry.Image {
	return registry.Image{Digest: digest, Labels: p.labels()}
}

// withProvenance is withVersion for a build of testRevision as it is.
func (e *env) withProvenance(version string) (Options, *fakeRegistry) {
	o, reg := e.withVersion(version)
	p := testProvenance
	o.Provenance = &p
	return o, reg
}

func TestRun_BuildLabelsTheImageWithItsCommit(t *testing.T) {
	e := newEnv(t)
	o, _ := e.withProvenance("")
	if _, err := e.run(o); err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	want := map[string]string{
		"org.opencontainers.image.revision": testRevision,
		"ai.kubernetix.kx.context":          ".",
		"ai.kubernetix.kx.dockerfile":       "Dockerfile",
	}
	if len(e.builder.labels) != len(want) {
		t.Fatalf("labels = %v, want %v", e.builder.labels, want)
	}
	for k, v := range want {
		if e.builder.labels[k] != v {
			t.Fatalf("labels = %v, want %v", e.builder.labels, want)
		}
	}
}

func TestRun_ReusesTheImageBuiltForThisCommit(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("")
	reg.images[commitRef] = labelled(olderDigest, testProvenance)
	built := false
	o.OnBuild = func() { built = true }
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(), "lookup "+commitRef)
	if built {
		t.Fatal("OnBuild ran, so GitHub would say Building for an image kx didn't build")
	}
	want := "registry.example.test/acme-corp/shop-api@" + olderDigest
	if res.Image != want || e.api.reqs("POST", "/apps/"+testAppID+"/deployments")[0].Body["image_url"] != want {
		t.Fatalf("deployed %q, want the earlier build %s", res.Image, want)
	}
	if !res.Reused || strings.Join(res.Tags, ",") != "3f9c2a1b7d4e" {
		t.Fatalf("result = %+v", res)
	}
	out := e.errOut.String()
	if !strings.Contains(out, "✓ Checked   Dockerfile, port 8080\n✓ Reused    "+commitRef+"\n✓ Live") || strings.Contains(out, "Built") {
		t.Fatalf("stderr:\n%s", out)
	}
}

func TestRun_ReuseAddsTheMissingVersionTag(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	reg.images[commitRef] = labelled(olderDigest, testProvenance)
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"lookup "+commitRef,
		"lookup "+versionRef,
		"retag "+versionRef+" "+olderDigest,
		"lookup "+versionRef,
	)
	if !res.Reused || strings.Join(res.Tags, ",") != "3f9c2a1b7d4e,0.0.11" || res.VersionTag != "0.0.11" ||
		res.Image != "registry.example.test/acme-corp/shop-api@"+olderDigest {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(e.errOut.String(), "✓ Reused    "+commitRef+"\n✓ Tagged    "+versionRef+"\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_ReRunOfATaggedCommitReusesWithoutTagging(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	reg.images[commitRef] = labelled(olderDigest, testProvenance)
	reg.images[versionRef] = labelled(olderDigest, testProvenance)
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(), "lookup "+commitRef, "lookup "+versionRef)
	if !res.Reused || strings.Join(res.Tags, ",") != "3f9c2a1b7d4e,0.0.11" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRun_ReusesTheVersionTagsImageFromThisCommit(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	reg.images[versionRef] = labelled(olderDigest, testProvenance)
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"lookup "+commitRef,
		"lookup "+versionRef,
		"retag "+commitRef+" "+olderDigest,
	)
	if !res.Reused || res.Image != "registry.example.test/acme-corp/shop-api@"+olderDigest || strings.Join(res.Tags, ",") != "3f9c2a1b7d4e,0.0.11" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(e.errOut.String(), "✓ Reused    "+versionRef+"\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_VersionTagFromAnotherCommitStopsBeforeBuilding(t *testing.T) {
	other := testProvenance
	other.Revision = strings.Repeat("a", 40)
	for name, held := range map[string]registry.Image{
		"another commit": labelled(otherDigest, other),
		"no labels":      {Digest: otherDigest},
	} {
		for _, rebuild := range []bool{false, true} {
			e := newEnv(t)
			o, reg := e.withProvenance("0.0.11")
			o.Rebuild = rebuild
			reg.images[commitRef] = labelled(olderDigest, testProvenance)
			reg.images[versionRef] = held
			_, err := e.run(o)
			var taken *kxerr.TagTaken
			if !errors.As(err, &taken) || taken.Tag != "0.0.11" || taken.Digest != otherDigest {
				t.Fatalf("%s (rebuild %v): Run() = %v, want TagTaken naming what the tag holds", name, rebuild, err)
			}
			checkCalls(t, e.builder.got(), "lookup "+commitRef, "lookup "+versionRef)
			if len(e.api.reqs("POST", "/apps/")) != 0 {
				t.Fatalf("%s: deployed", name)
			}
		}
	}
}

func TestRun_CommitTagThatIsntThisBuildIsRebuilt(t *testing.T) {
	otherFile := testProvenance
	otherFile.Dockerfile = "Dockerfile.dev"
	otherCommit := testProvenance
	otherCommit.Revision = strings.Repeat("a", 40)
	for name, held := range map[string]registry.Image{
		"no labels":           {Digest: olderDigest},
		"another Dockerfile":  labelled(olderDigest, otherFile),
		"another commit":      labelled(olderDigest, otherCommit),
		"labels but no build": {Digest: olderDigest, Labels: map[string]string{"org.opencontainers.image.revision": testRevision}},
	} {
		e := newEnv(t)
		o, reg := e.withProvenance("")
		reg.images[commitRef] = held
		res, err := e.run(o)
		if err != nil {
			t.Fatalf("%s: Run() = %v\n%s", name, err, e.errOut.String())
		}
		checkCalls(t, e.builder.got(),
			"lookup "+commitRef,
			"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
			"tag shop-api:3f9c2a1b7d4e "+commitRef,
			"push "+commitRef,
		)
		if res.Reused || res.Image != "registry.example.test/acme-corp/shop-api@"+testDigest {
			t.Fatalf("%s: result = %+v", name, res)
		}
	}
}

func TestRun_RebuildBuildsAndLeavesTheVersionTag(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	o.Rebuild = true
	reg.images[commitRef] = labelled(olderDigest, testProvenance)
	reg.images[versionRef] = labelled(olderDigest, testProvenance)
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"lookup "+commitRef,
		"lookup "+versionRef,
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e "+commitRef,
		"push "+commitRef,
		"lookup "+versionRef,
	)
	if res.Reused || res.Image != "registry.example.test/acme-corp/shop-api@"+testDigest {
		t.Fatalf("result = %+v; --rebuild deploys the new build", res)
	}
	if strings.Join(res.Tags, ",") != "3f9c2a1b7d4e" || res.VersionTag != "" {
		t.Fatalf("tags = %q, version tag %q; 0.0.11 still holds the earlier build", res.Tags, res.VersionTag)
	}
	if !strings.Contains(e.errOut.String(), versionRef+" holds another build of this commit, so kx leaves it.\n✓ Pushed    registry.example.test/acme-corp/shop-api\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_TwoRunsOfOneCommitBothDeploy(t *testing.T) {
	// The other run pushed the version tag while this one built: it holds
	// another build of this commit, so this run leaves it and deploys.
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	other := labelled(otherDigest, testProvenance)
	reg.images["other"] = other // so the fake knows its labels
	reg.seq[versionRef] = []string{"", "", otherDigest}
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if res.Reused || res.Image != "registry.example.test/acme-corp/shop-api@"+testDigest || res.VersionTag != "" {
		t.Fatalf("result = %+v", res)
	}
	if len(e.api.reqs("POST", "/apps/"+testAppID+"/deployments")) != 1 {
		t.Fatal("didn't deploy")
	}
}

func TestRun_LookupFailureBeforeBuildingBuilds(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("")
	reg.seq[commitRef] = []string{"err"}
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if res.Reused || !strings.Contains(e.errOut.String(), "kx couldn't check whether this commit was built before, so it builds it.\n") {
		t.Fatalf("result %+v, stderr:\n%s", res, e.errOut.String())
	}
}

func TestRun_ReuseTagFailures(t *testing.T) {
	// Tagging the commit is housekeeping: a failure is a warning.
	e := newEnv(t)
	o, reg := e.withProvenance("")
	reg.images[versionRef] = labelled(olderDigest, testProvenance) // reachable only through the version tag
	o.VersionTag = "0.0.11"
	reg.tagErr = errors.New("registry: registry.example.test answered 500 when kx tagged 3f9c2a1b7d4e")
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if !res.Reused || strings.Join(res.Tags, ",") != "0.0.11" ||
		!strings.Contains(e.errOut.String(), "kx couldn't tag "+versionRef+" as 3f9c2a1b7d4e too.") {
		t.Fatalf("result %+v, stderr:\n%s", res, e.errOut.String())
	}

	// Tagging the version is what was asked for: a failure stops the deploy.
	e = newEnv(t)
	o, reg = e.withProvenance("0.0.11")
	reg.images[commitRef] = labelled(olderDigest, testProvenance)
	reg.tagErr = errors.New("registry: registry.example.test answered 500 when kx tagged 0.0.11")
	if _, err := e.run(o); err == nil || len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatalf("Run() = %v; want a failure and no deploy", err)
	}
	if p := api.Explain(err); p.Code == "unexpected" {
		t.Fatalf("problem = %+v", p)
	}
}

func TestRun_LongBuildGetsAFreshCredential(t *testing.T) {
	e := newEnv(t)
	o, _ := e.withProvenance("")
	e.api.cred.ExpiresAt = e.clock.now.Add(5 * time.Minute)
	if _, err := e.run(o); err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if e.api.mints != 2 {
		t.Fatalf("mints = %d, want 2: the one from before the build runs out too soon to push with", e.api.mints)
	}

	e = newEnv(t)
	o, _ = e.withProvenance("")
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if e.api.mints != 1 {
		t.Fatalf("mints = %d, want 1: one credential serves the look and the push", e.api.mints)
	}
}

func TestRun_RejectsBadProvenance(t *testing.T) {
	e := newEnv(t)
	reg := &fakeRegistry{b: e.builder, images: map[string]registry.Image{}}
	bad := []func(*Options){
		func(o *Options) {
			o.Provenance = &Provenance{Revision: "abc", Context: ".", Dockerfile: "Dockerfile"}
			o.Registry = reg
		},
		func(o *Options) { p := testProvenance; o.Provenance = &p }, // nothing to look tags up with
		func(o *Options) {
			p := testProvenance
			o.Image, o.Build, o.Builder, o.Provenance, o.Registry = "registry.example.test/acme-corp/x@"+testDigest, nil, nil, &p, reg
		},
		func(o *Options) {
			o.Image, o.Build, o.Builder, o.Rebuild = "registry.example.test/acme-corp/x@"+testDigest, nil, nil, true
		},
	}
	for i, mutate := range bad {
		o := e.options()
		mutate(&o)
		if _, err := e.run(o); err == nil {
			t.Errorf("case %d: Run() accepted bad options", i)
		}
	}
}
