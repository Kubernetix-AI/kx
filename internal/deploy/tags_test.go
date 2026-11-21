package deploy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/docker"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/registry"
)

// fakeRegistry answers tag lookups and records them, and the tags it
// adds, in the builder's call list, so tests see the order of lookups,
// tags and pushes. A lookup takes the next answer in seq for the reference
// (a digest, or "err"), then images, then digests (unlabelled), then what
// the builder pushed under it (with the build's labels), as a registry
// would.
type fakeRegistry struct {
	mu      sync.Mutex
	b       *fakeBuilder
	images  map[string]registry.Image // image reference → what it holds
	digests map[string]string         // image reference → digest of an unlabelled image
	seq     map[string][]string       // image reference → successive digests ("err" fails)
	err     error                     // every lookup fails
	tagErr  error                     // every Tag fails
	refuse  string                    // lookups with this password are refused
	creds   []api.RegistryCredential
}

func (r *fakeRegistry) Lookup(_ context.Context, ref string, cred api.RegistryCredential) (registry.Image, error) {
	r.b.record("lookup " + ref)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creds = append(r.creds, cred)
	if r.err != nil {
		return registry.Image{}, r.err
	}
	if r.refuse != "" && cred.Password == r.refuse {
		return registry.Image{}, registry.ErrUnauthorized
	}
	if next := r.seq[ref]; len(next) > 0 {
		r.seq[ref] = next[1:]
		if next[0] == "err" {
			return registry.Image{}, errors.New("registry: connection reset by peer")
		}
		return r.imageOf(next[0]), nil
	}
	if img, ok := r.images[ref]; ok {
		return img, nil
	}
	if d, ok := r.digests[ref]; ok {
		return registry.Image{Digest: d}, nil
	}
	for _, c := range r.b.got() {
		if c == "push "+ref {
			return r.builtImage(), nil
		}
	}
	return registry.Image{}, nil
}

func (r *fakeRegistry) Tag(_ context.Context, repo, digest, tag string, _ api.RegistryCredential) error {
	r.b.record("retag " + repo + ":" + tag + " " + digest)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tagErr != nil {
		return r.tagErr
	}
	r.images[repo+":"+tag] = r.imageOf(digest)
	return nil
}

// imageOf is the image with digest d that the registry already knows.
func (r *fakeRegistry) imageOf(d string) registry.Image {
	for _, img := range r.images {
		if img.Digest == d {
			return img
		}
	}
	if built := r.builtImage(); built.Digest == d {
		return built
	}
	return registry.Image{Digest: d}
}

func (r *fakeRegistry) builtImage() registry.Image {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	d := r.b.digest
	if d == "" {
		d = testDigest
	}
	return registry.Image{Digest: d, Labels: r.b.labels}
}

func (e *env) withVersion(version string) (Options, *fakeRegistry) {
	reg := &fakeRegistry{b: e.builder, images: map[string]registry.Image{}, digests: map[string]string{}, seq: map[string][]string{}}
	o := e.options()
	o.VersionTag, o.Registry = version, reg
	return o, reg
}

func checkCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRun_VersionTagIsPushedToo(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withVersion("0.0.11")
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"lookup registry.example.test/acme-corp/shop-api:0.0.11",
		"retag registry.example.test/acme-corp/shop-api:0.0.11 "+testDigest,
		"lookup registry.example.test/acme-corp/shop-api:0.0.11",
	)
	if len(reg.creds) != 2 || reg.creds[0].Password != testPassword {
		t.Fatalf("the lookup should use the push credential, got %+v", reg.creds)
	}
	// Deployed by digest, as always.
	wantImage := "registry.example.test/acme-corp/shop-api@" + testDigest
	if res.Image != wantImage || e.api.reqs("POST", "/apps/"+testAppID+"/deployments")[0].Body["image_url"] != wantImage {
		t.Fatalf("deployed %q", res.Image)
	}
	if strings.Join(res.Tags, ",") != "3f9c2a1b7d4e,0.0.11" || res.VersionTag != "0.0.11" {
		t.Fatalf("tags = %q, version tag %q", res.Tags, res.VersionTag)
	}
	if !strings.Contains(e.errOut.String(), "✓ Built     shop-api:3f9c2a1b7d4e\n✓ Pushed    registry.example.test/acme-corp/shop-api:0.0.11\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_VersionTagAlreadyHoldingThisImageIsKept(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withVersion("0.0.11")
	reg.digests["registry.example.test/acme-corp/shop-api:0.0.11"] = testDigest
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"lookup registry.example.test/acme-corp/shop-api:0.0.11",
	)
	if !strings.Contains(e.errOut.String(), "0.0.11 already holds this image.\n✓ Pushed    registry.example.test/acme-corp/shop-api:0.0.11\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
	if strings.Join(res.Tags, ",") != "3f9c2a1b7d4e,0.0.11" || len(e.api.reqs("POST", "/apps/"+testAppID+"/deployments")) != 1 {
		t.Fatalf("tags %q; the deploy should go on", res.Tags)
	}
}

func TestRun_VersionTagHoldingAnotherImageStops(t *testing.T) {
	for _, fromFlag := range []bool{false, true} {
		e := newEnv(t)
		o, reg := e.withVersion("0.0.11")
		o.TagFromFlag = fromFlag
		reg.digests["registry.example.test/acme-corp/shop-api:0.0.11"] = "sha256:" + strings.Repeat("ee", 32)
		res, err := e.run(o)
		var taken *kxerr.TagTaken
		if !errors.As(err, &taken) || taken.Image != "registry.example.test/acme-corp/shop-api" || taken.Tag != "0.0.11" ||
			taken.FromFlag != fromFlag || taken.Digest != "sha256:"+strings.Repeat("ee", 32) {
			t.Fatalf("Run() = %v, want TagTaken for shop-api:0.0.11 (from flag %v)", err, fromFlag)
		}
		if p := api.Explain(err); p.Code != "tag_taken" || p.Exit != api.ExitPush {
			t.Fatalf("problem = %+v", p)
		}
		for _, c := range e.builder.got() {
			if (strings.HasSuffix(c, ":0.0.11") && !strings.HasPrefix(c, "lookup ")) || strings.HasPrefix(c, "retag ") {
				t.Fatalf("kx touched the existing tag: %q", c)
			}
		}
		if n := len(e.api.reqs("POST", "/apps/")); n != 0 || res != nil && res.DeploymentID != "" {
			t.Fatal("deployed although the version tag holds another image")
		}
		if strings.Contains(e.errOut.String(), "✓ Pushed") {
			t.Fatalf("printed Pushed before stopping:\n%s", e.errOut.String())
		}
	}
}

func TestRun_VersionTagLookupFailureStops(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withVersion("0.0.11")
	reg.err = errors.New("registry: registry.example.test: connection reset by peer")
	_, err := e.run(o)
	var unchecked *kxerr.TagCheckFailed
	if !errors.As(err, &unchecked) || unchecked.Image != "registry.example.test/acme-corp/shop-api" || unchecked.Tag != "0.0.11" {
		t.Fatalf("Run() = %v, want TagCheckFailed", err)
	}
	if p := api.Explain(err); p.Code != "tag_check_failed" {
		t.Fatalf("problem = %+v", p)
	}
	for _, c := range e.builder.got() {
		if strings.HasPrefix(c, "retag ") {
			t.Fatalf("tagged the version without knowing what it holds: %q", e.builder.got())
		}
	}
	if len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatal("deployed after the lookup failed")
	}
}

func TestRun_VersionTagLookupInterrupted(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withVersion("0.0.11")
	reg.err = context.Canceled
	_, err := e.run(o)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want the interruption itself", err)
	}
}

func TestRun_VersionTagRefusedByTheRegistryStops(t *testing.T) {
	// A registry that keeps its tags refuses the version tag when another
	// image took it after kx looked.
	e := newEnv(t)
	o, reg := e.withVersion("0.0.11")
	reg.tagErr = errors.New("registry: registry.example.test answered 400 when kx tagged 0.0.11")
	reg.seq[versionRef] = []string{"", otherDigest}
	_, err := e.run(o)
	var taken *kxerr.TagTaken
	if !errors.As(err, &taken) || taken.Tag != "0.0.11" || !taken.Raced || taken.Digest != otherDigest {
		t.Fatalf("Run() = %v, want TagTaken after the race", err)
	}
	if p := api.Explain(err); !strings.Contains(p.What, "was given another image while kx was tagging it") {
		t.Fatalf("problem = %+v", p)
	}
	if len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatal("deployed after the version tag was refused")
	}
}

func TestRun_VersionTagPushFailure(t *testing.T) {
	e := newEnv(t)
	o, reg := e.withVersion("0.0.11")
	reg.tagErr = errors.New("registry: registry.example.test answered 400 when kx tagged 0.0.11")
	_, err := e.run(o)
	if p := api.Explain(err); p.Code != "push_failed" {
		t.Fatalf("problem = %+v (%v)", p, err)
	}
	if len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatal("deployed after the version tag failed")
	}
}

func TestRun_VersionTagMovedByAnotherRunWhilePushing(t *testing.T) {
	// Another run pushed the same new tag at the same moment, and its image
	// is what the tag holds now: that run deploys, this one stops.
	e := newEnv(t)
	o, reg := e.withVersion("0.0.11")
	reg.seq["registry.example.test/acme-corp/shop-api:0.0.11"] = []string{"", "sha256:" + strings.Repeat("ee", 32)}
	_, err := e.run(o)
	var taken *kxerr.TagTaken
	if !errors.As(err, &taken) || taken.Digest != "sha256:"+strings.Repeat("ee", 32) || !taken.Raced {
		t.Fatalf("Run() = %v, want TagTaken with the other run's digest", err)
	}
	if len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatal("deployed although the version tag holds the other run's image")
	}
}

func TestRun_VersionTagUnconfirmedAfterPushWarnsAndDeploys(t *testing.T) {
	for _, after := range []string{"err", ""} {
		e := newEnv(t)
		o, reg := e.withVersion("0.0.11")
		reg.seq["registry.example.test/acme-corp/shop-api:0.0.11"] = []string{"", after}
		res, err := e.run(o)
		if err != nil {
			t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
		}
		if !strings.Contains(e.errOut.String(), "kx couldn't confirm that registry.example.test/acme-corp/shop-api:0.0.11 holds the image it deploys.") {
			t.Fatalf("stderr:\n%s", e.errOut.String())
		}
		if res.VersionTag != "0.0.11" || len(e.api.reqs("POST", "/apps/"+testAppID+"/deployments")) != 1 {
			t.Fatalf("result %+v; the push worked, so the deploy goes on", res)
		}
	}
}

func TestRun_CommitRePushedThenVersion(t *testing.T) {
	e := newEnv(t)
	e.clock.now = time.Date(2026, 11, 14, 8, 15, 30, 0, time.UTC)
	o, _ := e.withVersion("0.0.11")
	e.builder.pushErrs = []error{&kxerr.PushFailed{Err: kxerr.ErrTagExists}}
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e-20261114081530",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e-20261114081530",
		"lookup registry.example.test/acme-corp/shop-api:0.0.11",
		"retag registry.example.test/acme-corp/shop-api:0.0.11 "+testDigest,
		"lookup registry.example.test/acme-corp/shop-api:0.0.11",
	)
	if strings.Join(res.Tags, ",") != "3f9c2a1b7d4e-20261114081530,0.0.11" {
		t.Fatalf("tags = %q", res.Tags)
	}
}

func TestRun_TagSameAsCommitTagIsPushedOnce(t *testing.T) {
	e := newEnv(t)
	o, _ := e.withVersion("3f9c2a1b7d4e")
	o.TagFromFlag = true
	res, err := e.run(o)
	if err != nil {
		t.Fatal(err)
	}
	checkCalls(t, e.builder.got(),
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
	)
	if strings.Join(res.Tags, ",") != "3f9c2a1b7d4e" {
		t.Fatalf("tags = %q", res.Tags)
	}
}

func TestRun_NoVersionTag(t *testing.T) {
	e := newEnv(t)
	res, err := e.run(e.options())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Tags, ",") != "3f9c2a1b7d4e" || res.VersionTag != "" {
		t.Fatalf("tags = %q, version tag %q", res.Tags, res.VersionTag)
	}
}

func TestRun_Repository(t *testing.T) {
	e := newEnv(t)
	o, _ := e.withVersion("0.0.11")
	o.Repository = "shop-frontend"
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	checkCalls(t, e.builder.got(),
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-frontend:3f9c2a1b7d4e",
		"push registry.example.test/acme-corp/shop-frontend:3f9c2a1b7d4e",
		"lookup registry.example.test/acme-corp/shop-frontend:0.0.11",
		"retag registry.example.test/acme-corp/shop-frontend:0.0.11 "+testDigest,
		"lookup registry.example.test/acme-corp/shop-frontend:0.0.11",
	)
	wantImage := "registry.example.test/acme-corp/shop-frontend@" + testDigest
	if res.Image != wantImage || e.api.reqs("POST", "/apps/"+testAppID+"/deployments")[0].Body["image_url"] != wantImage {
		t.Fatalf("deployed %q", res.Image)
	}
	if !strings.Contains(e.errOut.String(), "✓ Pushed    registry.example.test/acme-corp/shop-frontend:0.0.11\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_RepositoryWithParts(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.Repository = "web/frontend"
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if got := e.builder.got(); got[len(got)-1] != "push registry.example.test/acme-corp/web/frontend:3f9c2a1b7d4e" {
		t.Fatalf("calls: %q", got)
	}
	if !strings.Contains(e.errOut.String(), "✓ Pushed    registry.example.test/acme-corp/web/frontend\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_RepositoryForANewApp(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp, o.Repository = nil, "web", "shop-frontend"
	if _, err := e.run(o); err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	created := e.api.reqs("POST", "/apps")
	if len(created) == 0 || created[0].Body["image_url"] != "registry.example.test/acme-corp/shop-frontend@"+testDigest {
		t.Fatalf("create app = %+v", created)
	}
	if !strings.Contains(e.errOut.String(), "✓ Built     web:3f9c2a1b7d4e\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_RepositoryWithARefusedCachedCredential(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.Repository = "shop-frontend"
	cached := e.api.cred
	cached.Password = "cached-password"
	o.Credentials = &fakeStore{cred: &cached}
	e.builder.pushErrs = []error{&kxerr.PushFailed{Err: docker.ErrUnauthorized}}
	if _, err := e.run(o); err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	got := e.builder.got()
	if got[len(got)-1] != "push registry.example.test/acme-corp/shop-frontend:3f9c2a1b7d4e" || e.api.mints != 1 {
		t.Fatalf("calls %q, mints %d", got, e.api.mints)
	}
}

func TestRun_RejectsBadTagOptions(t *testing.T) {
	e := newEnv(t)
	reg := &fakeRegistry{b: e.builder, images: map[string]registry.Image{}}
	bad := []func(*Options){
		func(o *Options) { o.VersionTag, o.Registry = "bad tag", reg },
		func(o *Options) { o.VersionTag = "0.0.11" }, // nothing to look tags up with
		func(o *Options) { o.Repository = "Shop Frontend" },
		func(o *Options) {
			o.Image, o.Build, o.Builder, o.Repository = "registry.example.test/acme-corp/x@"+testDigest, nil, nil, "x"
		},
		func(o *Options) {
			o.Image, o.Build, o.Builder, o.VersionTag, o.Registry = "registry.example.test/acme-corp/x@"+testDigest, nil, nil, "1.0.0", reg
		},
	}
	for i, mutate := range bad {
		o := e.options()
		mutate(&o)
		if _, err := e.run(o); err == nil {
			t.Errorf("case %d: Run() accepted bad options", i)
		}
	}
	if len(e.api.requests) != 0 || len(e.builder.got()) != 0 {
		t.Fatal("did something with bad options")
	}
}
