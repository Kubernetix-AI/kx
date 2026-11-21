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

// labelled is an image kx built with the labels p gives.
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
	want := map[string]string{LabelRevision: testRevision, LabelContext: ".", LabelDockerfile: "Dockerfile"}
	if len(e.builder.labels) != len(want) {
		t.Fatalf("labels = %v, want %v", e.builder.labels, want)
	}
	for k, v := range want {
		if e.builder.labels[k] != v {
			t.Fatalf("labels = %v, want %v", e.builder.labels, want)
		}
	}
	// No version tag: nothing to look up before or after the build.
	checkCalls(t, e.builder.got(),
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e "+commitRef,
		"push "+commitRef,
	)
}

func TestRun_CommitLabelWithoutTheBuildsPlace(t *testing.T) {
	e := newEnv(t)
	o, _ := e.withVersion("")
	o.Provenance = &Provenance{Revision: testRevision}
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if len(e.builder.labels) != 1 || e.builder.labels[LabelRevision] != testRevision {
		t.Fatalf("labels = %v, want the commit alone", e.builder.labels)
	}
}

func TestRun_AnEarlierBuildOfTheCommitIsNeverReused(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("")
	reg.images[commitRef] = labelled(olderDigest, testProvenance)
	res, err := e.run(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Image != "registry.example.test/acme-corp/shop-api@"+testDigest || strings.Contains(e.errOut.String(), "Reused") {
		t.Fatalf("result %+v; kx builds what is checked out, every time", res)
	}
}

func TestRun_VersionTagIsGivenInTheRegistry(t *testing.T) {
	e := newEnv(t)
	o, _ := e.withProvenance("0.0.11")
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"lookup "+versionRef,
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e "+commitRef,
		"push "+commitRef,
		"lookup "+versionRef,
		"retag "+versionRef+" "+testDigest,
		"lookup "+versionRef,
	)
	if strings.Join(res.Tags, ",") != "3f9c2a1b7d4e,0.0.11" || res.VersionTag != "0.0.11" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(e.errOut.String(), "✓ Pushed    "+versionRef+"\n") {
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
		e := newEnv(t)
		o, reg := e.withProvenance("0.0.11")
		reg.images[versionRef] = held
		_, err := e.run(o)
		var taken *kxerr.TagTaken
		if !errors.As(err, &taken) || taken.Tag != "0.0.11" || taken.Digest != otherDigest || taken.Raced {
			t.Fatalf("%s: Run() = %v, want TagTaken naming what the tag holds", name, err)
		}
		checkCalls(t, e.builder.got(), "lookup "+versionRef)
		if len(e.api.reqs("POST", "/apps/")) != 0 {
			t.Fatalf("%s: deployed", name)
		}
	}
}

func TestRun_VersionTagOnAnEarlierBuildOfThisCommitStays(t *testing.T) {
	// A re-run, or a second run for one tagged push: the tag stays on the
	// first build, and this run deploys its own build, by digest.
	for _, fromFlag := range []bool{false, true} {
		e := newEnv(t)
		o, reg := e.withProvenance("0.0.11")
		o.TagFromFlag = fromFlag
		earlier := testProvenance
		earlier.Dockerfile = "Dockerfile.other" // a build of the same commit, however made
		reg.images[versionRef] = labelled(olderDigest, earlier)
		res, err := e.run(o)
		if err != nil {
			t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
		}
		checkCalls(t, e.builder.got(),
			"lookup "+versionRef,
			"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
			"tag shop-api:3f9c2a1b7d4e "+commitRef,
			"push "+commitRef,
			"lookup "+versionRef,
		)
		if res.Image != "registry.example.test/acme-corp/shop-api@"+testDigest || strings.Join(res.Tags, ",") != "3f9c2a1b7d4e" || res.VersionTag != "" {
			t.Fatalf("result = %+v", res)
		}
		want := "! The version tag 0.0.11 stays on the earlier build of this commit.\n✓ Pushed    registry.example.test/acme-corp/shop-api\n"
		if fromFlag {
			want = "! The tag 0.0.11 stays on the earlier build of this commit.\n"
		}
		if !strings.Contains(e.errOut.String(), want) {
			t.Fatalf("stderr:\n%s\nwant %q", e.errOut.String(), want)
		}
		if len(e.api.reqs("POST", "/apps/"+testAppID+"/deployments")) != 1 {
			t.Fatal("didn't deploy")
		}
	}
}

func TestRun_TwoRunsOfOneCommitBothDeploy(t *testing.T) {
	// The other run tagged its build while this one built: this one leaves
	// the tag there and deploys its own build.
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	reg.images["other run"] = labelled(olderDigest, testProvenance) // so the fake knows its labels
	reg.seq[versionRef] = []string{"", "", olderDigest}
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if res.Image != "registry.example.test/acme-corp/shop-api@"+testDigest || res.VersionTag != "" ||
		!strings.Contains(e.errOut.String(), "stays on the earlier build of this commit") {
		t.Fatalf("result = %+v\n%s", res, e.errOut.String())
	}
}

func TestRun_VersionTagAfterAFailedLookup(t *testing.T) {
	// Before the build a failed lookup only means kx builds; after it, kx
	// looks again and places nothing without an answer.
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	reg.seq[versionRef] = []string{"err", "err"}
	_, err := e.run(o)
	var unchecked *kxerr.TagCheckFailed
	if !errors.As(err, &unchecked) {
		t.Fatalf("Run() = %v, want TagCheckFailed", err)
	}
	for _, c := range e.builder.got() {
		if strings.HasPrefix(c, "retag ") {
			t.Fatalf("tagged without an answer: %q", e.builder.got())
		}
	}

	e = newEnv(t)
	o, reg = e.withProvenance("0.0.11")
	reg.seq[versionRef] = []string{"err"}
	res, err := e.run(o)
	if err != nil || res.VersionTag != "0.0.11" {
		t.Fatalf("Run() = %+v, %v; the look after the build answered, so the tag is given", res, err)
	}
}

func TestRun_TagCheckFailedSaysWhy(t *testing.T) {
	for cause, err := range map[string]error{
		kxerr.CheckRefused:     registry.ErrUnauthorized,
		kxerr.CheckUnavailable: errors.Join(errors.New("registry: registry.example.test answered 502"), registry.ErrUnavailable),
		kxerr.CheckNetwork:     errors.New("registry: registry.example.test: connection reset by peer"),
	} {
		e := newEnv(t)
		o, reg := e.withVersion("0.0.11")
		reg.err = err
		_, got := e.run(o)
		var unchecked *kxerr.TagCheckFailed
		if !errors.As(got, &unchecked) || unchecked.Cause != cause {
			t.Fatalf("Run() = %v, want TagCheckFailed with cause %q", got, cause)
		}
	}
}

func TestRun_StaleCachedCredentialBeforeTheBuild(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withProvenance("0.0.11")
	cached := e.api.cred
	cached.Password = "stale"
	store := &fakeStore{cred: &cached}
	o.Credentials = store
	reg.refuse = "stale"
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if store.forgot != 1 || e.api.mints != 1 || reg.creds[1].Password != testPassword || res.VersionTag != "0.0.11" {
		t.Fatalf("forgot %d, mints %d, second lookup with %q", store.forgot, e.api.mints, reg.creds[1].Password)
	}
}

func TestRun_LongBuildGetsAFreshCredential(t *testing.T) {
	e := newEnv(t)
	o, _ := e.withProvenance("0.0.11")
	e.api.cred.ExpiresAt = e.clock.now.Add(5 * time.Minute)
	if _, err := e.run(o); err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if e.api.mints != 2 {
		t.Fatalf("mints = %d, want 2: the one from before the build runs out too soon to push with", e.api.mints)
	}

	e = newEnv(t)
	o, _ = e.withProvenance("0.0.11")
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if e.api.mints != 1 {
		t.Fatalf("mints = %d, want 1: one credential serves the look and the push", e.api.mints)
	}
}

func TestRun_RejectsBadProvenance(t *testing.T) {
	e := newEnv(t)
	bad := []func(*Options){
		func(o *Options) { o.Provenance = &Provenance{Revision: "abc"} },
		func(o *Options) { o.Provenance = &Provenance{Revision: testRevision, Context: "line\nbreak"} },
		func(o *Options) {
			p := testProvenance
			o.Image, o.Build, o.Builder, o.Provenance = "registry.example.test/acme-corp/x@"+testDigest, nil, nil, &p
		},
	}
	for i, mutate := range bad {
		o := e.options()
		mutate(&o)
		if _, err := e.run(o); err == nil {
			t.Errorf("case %d: Run() accepted bad options", i)
		}
	}
	if p := api.Explain(&kxerr.TagCheckFailed{Image: "r/o/a", Tag: "1.0.0", Cause: kxerr.CheckRefused}); !strings.Contains(p.Fix, "didn't accept") {
		t.Fatalf("problem = %+v", p)
	}
}
