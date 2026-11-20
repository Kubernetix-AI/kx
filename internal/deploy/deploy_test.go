package deploy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/docker"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/safetext"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func TestWaitForPrevious_Blocks(t *testing.T) {
	e := newEnv(t)
	inFlight := api.Deployment{ID: "dep-15", Version: 15, Status: "building"}
	ready := inFlight
	ready.Status = "ready"
	e.api.lists = [][]api.Deployment{{inFlight}, {inFlight}, {ready}}
	var slept []time.Duration
	restore := setDefaultSleep(func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil })
	defer restore()

	if err := WaitForPrevious(context.Background(), e.client, testAppID, *e.p); err != nil {
		t.Fatalf("WaitForPrevious() = %v", err)
	}
	if e.api.listCalls != 3 {
		t.Fatalf("listed deployments %d times, want 3 (blocked until v15 finished)", e.api.listCalls)
	}
	if len(slept) != 2 || slept[0] != 2*time.Second {
		t.Fatalf("slept %v, want two 2 s polls", slept)
	}
	if !strings.Contains(e.errOut.String(), "Waiting for v15 to finish first\n") {
		t.Fatalf("stderr = %q", e.errOut.String())
	}
	if reqs := e.api.reqs("GET", "/apps/"+testAppID+"/deployments"); len(reqs) == 0 {
		t.Fatal("no deployment list requests")
	}
}

func TestWaitForPrevious_NothingInFlight(t *testing.T) {
	e := newEnv(t)
	restore := setDefaultSleep(func(context.Context, time.Duration) error { t.Fatal("slept with nothing in flight"); return nil })
	defer restore()
	if err := WaitForPrevious(context.Background(), e.client, testAppID, *e.p); err != nil {
		t.Fatal(err)
	}
	if e.errOut.Len() != 0 {
		t.Fatalf("printed %q", e.errOut.String())
	}
	if q := e.api.requests[0]; q.Path != "/apps/"+testAppID+"/deployments" {
		t.Fatalf("first request %s", q.Path)
	}
}

func TestPollInterval_TwoThenFive(t *testing.T) {
	var got []time.Duration
	for i := 0; i < 18; i++ {
		got = append(got, pollInterval(i))
	}
	for i, d := range got {
		want := 2 * time.Second
		if i >= 15 {
			want = 5 * time.Second
		}
		if d != want {
			t.Fatalf("poll %d waits %v, want %v", i, d, want)
		}
	}
}

func TestRun_GivesUpOnAStuckPreviousDeploy(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	stuck := api.Deployment{ID: "dep-15", Version: 15, Status: "deploying"}
	for i := 0; i < 30; i++ {
		e.api.lists = append(e.api.lists, []api.Deployment{stuck})
	}
	_, err := e.run(e.options())
	var pr *kxerr.PreviousRunning
	if !errors.As(err, &pr) || pr.App != "shop-api" || pr.Version != 15 || pr.After != 18*time.Minute {
		t.Fatalf("Run() = %v, want PreviousRunning for shop-api v15 after 18m", err)
	}
	if calls := e.builder.got(); len(calls) != 0 {
		t.Fatalf("built while v15 was still deploying: %v", calls)
	}
}

func TestRun_ChecksAgainForADeployStartedDuringTheBuild(t *testing.T) {
	e := newEnv(t)
	done := api.Deployment{ID: "dep-14", Version: 14, Status: "ready"}
	started := api.Deployment{ID: "dep-15", Version: 15, Status: "building"} // e.g. from the dashboard
	finished := started
	finished.Status = "ready"
	e.api.lists = [][]api.Deployment{{done}, {started}, {finished}}
	if _, err := e.run(e.options()); err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, q := range e.api.requests {
		switch {
		case q.Method == "GET" && q.Path == "/apps/"+testAppID+"/deployments":
			seq = append(seq, "list")
		case q.Method == "POST" && q.Path == "/apps/"+testAppID+"/deployments":
			seq = append(seq, "post")
		case q.Method == "POST" && q.Path == "/registry/credentials":
			seq = append(seq, "mint")
		}
	}
	if strings.Join(seq, ",") != "list,mint,list,list,post" {
		t.Fatalf("request order = %v, want a second check (and wait) right before the deployment", seq)
	}
	out := e.errOut.String()
	if i, j := strings.Index(out, "✓ Pushed"), strings.Index(out, "Waiting for v15 to finish first"); i < 0 || j < i {
		t.Fatalf("stderr:\n%s", out)
	}
}

func TestRun_AppBeingDeletedStopsAtOnce(t *testing.T) {
	for _, status := range []string{"pending_deletion", "deleting"} {
		e := newEnv(t)
		e.api.lists = [][]api.Deployment{{{ID: "dep-14", Version: 14, Status: status}}}
		_, err := e.run(e.options())
		if p := api.Explain(err); p.Code != "app_deleting" || p.What != "shop-api is being deleted, so it can't be deployed." {
			t.Fatalf("%s before the build: %+v", status, p)
		}
		if len(e.clock.sleeps) != 0 || len(e.builder.got()) != 0 {
			t.Fatalf("%s: waited or built for an app being deleted", status)
		}

		e = newEnv(t)
		e.clock.step = time.Minute
		e.api.statuses = []string{"pending", status}
		_, err = e.run(e.options())
		if p := api.Explain(err); p.Code != "app_deleting" {
			t.Fatalf("%s while waiting: %+v (want no 15-minute wait)", status, p)
		}
		if n := len(e.clock.sleeps); n > 2 {
			t.Fatalf("%s: polled %d times", status, n)
		}
	}
}

func TestRun_IdempotencyKeyReused(t *testing.T) {
	e := newEnv(t)
	e.api.createFail = []int{503, 502}
	res, err := e.run(e.options())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	posts := e.api.reqs("POST", "/apps/"+testAppID+"/deployments")
	if len(posts) != 3 {
		t.Fatalf("POSTed %d times, want 3 (two retries)", len(posts))
	}
	for i, p := range posts {
		if p.Key != testKey {
			t.Fatalf("attempt %d sent Idempotency-Key %q, want %q every time", i+1, p.Key, testKey)
		}
	}
	if res.Version != 15 {
		t.Fatalf("version = %d", res.Version)
	}
}

func TestRun_BuildPushDeploy(t *testing.T) {
	e := newEnv(t)
	res, err := e.run(e.options())
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	wantCalls := []string{
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
	}
	if got := e.builder.got(); strings.Join(got, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("docker calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantCalls, "\n"))
	}
	post := e.api.reqs("POST", "/apps/"+testAppID+"/deployments")[0]
	wantImage := "registry.example.test/acme-corp/shop-api@" + testDigest
	if post.Body["image_url"] != wantImage {
		t.Fatalf("image_url = %v, want %s", post.Body["image_url"], wantImage)
	}
	if res.Image != wantImage || res.Version != 15 || res.DeploymentID == "" || res.Status != "live" ||
		res.URL != "https://"+testAppID+"--acme-corp.apps.example.test" || res.AppID != testAppID || res.CreatedApp {
		t.Fatalf("result = %+v", res)
	}
	want := "✓ Checked   Dockerfile, port 8080\n" +
		"✓ Built     shop-api:3f9c2a1b7d4e\n" +
		"✓ Pushed    registry.example.test/acme-corp/shop-api\n" +
		"✓ Live      v15  queued → starting → live\n" +
		"  4 of 5 deploys left today on the Free plan.\n"
	if e.errOut.String() != want {
		t.Fatalf("stderr:\n%s\nwant:\n%s", e.errOut.String(), want)
	}
	if e.out.Len() != 0 {
		t.Fatalf("Run printed to stdout: %q (the command prints the URL)", e.out.String())
	}
}

func TestRun_ElapsedTimes(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 2 * time.Second // each poll's sleep moves the clock 2 s
	if _, err := e.run(e.options()); err != nil {
		t.Fatal(err)
	}
	live := strings.Split(e.errOut.String(), "\n")[3]
	if !strings.HasPrefix(live, "✓ Live      v15  queued → starting → live") || !strings.HasSuffix(live, " 6s") {
		t.Fatalf("live line = %q, want three 2 s polls (6s)", live)
	}
}

func TestRun_DeploysLeftOnlyWhenFew(t *testing.T) {
	e := newEnv(t)
	e.api.usage = &api.Usage{Plan: "pro", Usage: map[string]api.UsageItem{"deployments_today": {Current: 10, Limit: 50}}}
	if _, err := e.run(e.options()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.errOut.String(), "deploys left") {
		t.Fatalf("printed deploys left with 40 remaining:\n%s", e.errOut.String())
	}
}

func TestRun_NewApp(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 3000
	res, err := e.run(o)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	creates := e.api.reqs("POST", "/apps")
	var create *request
	for i := range creates {
		if creates[i].Path == "/apps" {
			create = &creates[i]
		}
	}
	if create == nil {
		t.Fatal("no POST /apps")
	}
	b := create.Body
	if b["name"] != "billing-worker" || b["source_type"] != "image" || b["port"] != float64(3000) ||
		b["image_url"] != "registry.example.test/acme-corp/billing-worker@"+testDigest {
		t.Fatalf("POST /apps body = %v", b)
	}
	if !res.CreatedApp || res.AppID != "new-app-id" || res.Version != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(e.errOut.String(), "✓ Pushed    registry.example.test/acme-corp/billing-worker\n✓ Created   app billing-worker\n✓ Live      v1  ") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
	if e.api.listCalls != 0 {
		t.Fatal("waited for a previous deploy of an app that doesn't exist yet")
	}
}

func TestRun_NewAppWithoutExposeUses8080(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 0
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if e.api.reqs("POST", "/apps")[0].Body["port"] != float64(8080) {
		t.Fatalf("port = %v", e.api.reqs("POST", "/apps")[0].Body["port"])
	}
	if !strings.Contains(e.errOut.String(), "has no EXPOSE line, so kx assumes your app listens on port 8080.\n  Kubernetix checks that it answers on / there; if it listens on another port, add EXPOSE <port> to your Dockerfile.\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_PortWarnings(t *testing.T) {
	tests := []struct {
		exposed int
		want    string
	}{
		{3000, "The Dockerfile exposes port 3000, but shop-api listens on port 8080."},
		{80, "Your Dockerfile exposes port 80."},
		{8080, ""},
		{0, ""},
	}
	for _, tt := range tests {
		e := newEnv(t)
		o := e.options()
		o.Build.Port = tt.exposed
		if _, err := e.run(o); err != nil {
			t.Fatal(err)
		}
		if tt.want == "" && strings.Contains(e.errOut.String(), "!") {
			t.Errorf("EXPOSE %d: unexpected warning:\n%s", tt.exposed, e.errOut.String())
		}
		if tt.want != "" && !strings.Contains(e.errOut.String(), tt.want) {
			t.Errorf("EXPOSE %d: stderr lacks %q:\n%s", tt.exposed, tt.want, e.errOut.String())
		}
	}
}

func TestRun_MultiRegionRefused(t *testing.T) {
	e := newEnv(t)
	e.api.apps[testAppID].Regions = []api.AppRegion{{Region: "us"}, {Region: "eu"}}
	_, err := e.run(e.options())
	var mr *kxerr.MultiRegion
	if !errors.As(err, &mr) || mr.App != "shop-api" {
		t.Fatalf("Run() = %v, want MultiRegion", err)
	}
	if len(e.builder.got()) != 0 || len(e.api.reqs("POST", "/")) != 0 {
		t.Fatal("did something for a multi-region app")
	}
}

func TestRun_ImageSkipsBuild(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.Image, o.Build, o.Tag, o.Builder = "registry.example.test/acme-corp/shop-api@"+testDigest, nil, "", nil
	res, err := e.run(o)
	if err != nil {
		t.Fatal(err)
	}
	if e.api.mints != 0 {
		t.Fatal("minted a registry credential for --image")
	}
	if got := e.api.reqs("POST", "/apps/"+testAppID+"/deployments")[0].Body["image_url"]; got != o.Image || res.Image != o.Image {
		t.Fatalf("image_url = %v", got)
	}
	if !strings.HasPrefix(e.errOut.String(), "✓ Checked   image registry.example.test/acme-corp/shop-api@sha256:") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestMasksRegistryCredential_BeforeDocker(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	var events []string
	o.OnCredential = func(c api.RegistryCredential) {
		if c.Password != testPassword {
			t.Errorf("OnCredential got another credential")
		}
		events = append(events, "mask")
	}
	e.builder.onPush = func() { events = append(events, "push") }
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "mask,push" {
		t.Fatalf("events = %v, want the credential masked before Docker sees it", events)
	}
}

func TestRun_UsesCachedCredential(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	cached := e.api.cred
	cached.Password = "cached-password"
	store := &fakeStore{cred: &cached}
	o.Credentials = store
	masked := 0
	o.OnCredential = func(api.RegistryCredential) { masked++ }
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if e.api.mints != 0 || e.builder.creds[0].Password != "cached-password" || masked != 1 {
		t.Fatalf("mints %d, pushed with cached=%v, masked %d", e.api.mints, e.builder.creds[0].Password == "cached-password", masked)
	}
}

func TestRun_SavesMintedCredential(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	store := &fakeStore{}
	o.Credentials = store
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if e.api.mints != 1 || store.saved != 1 {
		t.Fatalf("mints %d, saved %d", e.api.mints, store.saved)
	}
}

func TestRun_RefusedCachedCredentialIsReplaced(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	cached := e.api.cred
	cached.Password = "stale"
	store := &fakeStore{cred: &cached}
	o.Credentials = store
	e.builder.pushErrs = []error{&kxerr.PushFailed{Err: docker.ErrUnauthorized}}
	if _, err := e.run(o); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if store.forgot != 1 || e.api.mints != 1 || len(e.builder.creds) != 2 || e.builder.creds[1].Password != testPassword {
		t.Fatalf("forgot %d, mints %d, pushes %d", store.forgot, e.api.mints, len(e.builder.creds))
	}
}

func TestRun_RefusedFreshCredentialFails(t *testing.T) {
	e := newEnv(t)
	e.builder.pushErrs = []error{&kxerr.PushFailed{Err: docker.ErrUnauthorized}}
	e.builder.pushOut = "unauthorized: authentication required\n"
	_, err := e.run(e.options())
	var pf *kxerr.PushFailed
	if !errors.As(err, &pf) || e.api.mints != 1 {
		t.Fatalf("Run() = %v, mints %d", err, e.api.mints)
	}
	if !strings.Contains(e.errOut.String(), "unauthorized: authentication required") {
		t.Fatalf("Docker's push output not shown:\n%s", e.errOut.String())
	}
}

func TestRun_TagAlreadyPushedIsUploadedUnderANewTag(t *testing.T) {
	e := newEnv(t)
	e.clock.now = time.Date(2026, 11, 14, 8, 15, 30, 0, time.UTC)
	e.builder.pushErrs = []error{&kxerr.PushFailed{Err: kxerr.ErrTagExists}}
	e.builder.digest = "sha256:" + strings.Repeat("cd", 32)
	res, err := e.run(e.options())
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	want := []string{
		"build shop-api:3f9c2a1b7d4e /code/shop-api /code/shop-api/Dockerfile",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e",
		"tag shop-api:3f9c2a1b7d4e registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e-20261114081530",
		"push registry.example.test/acme-corp/shop-api:3f9c2a1b7d4e-20261114081530",
	}
	if got := e.builder.got(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("docker calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Only the digest of kx's own push is deployed, never what the existing tag holds.
	wantImage := "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat("cd", 32)
	if res.Image != wantImage || e.api.reqs("POST", "/apps/"+testAppID+"/deployments")[0].Body["image_url"] != wantImage {
		t.Fatalf("deployed %q", res.Image)
	}
	if !strings.Contains(e.errOut.String(), "✓ Built     shop-api:3f9c2a1b7d4e\nThis commit was already pushed; uploaded it as 3f9c2a1b7d4e-20261114081530.\n✓ Pushed    registry.example.test/acme-corp/shop-api\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_TagRefusalIsNotACredentialProblem(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	cached := e.api.cred
	cached.Password = "cached-password"
	store := &fakeStore{cred: &cached}
	o.Credentials = store
	e.builder.pushErrs = []error{&kxerr.PushFailed{Err: kxerr.ErrTagExists}}
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if store.forgot != 0 || e.api.mints != 0 {
		t.Fatalf("forgot %d, minted %d: an existing tag must not cost a new credential", store.forgot, e.api.mints)
	}
	if len(e.builder.creds) != 2 || e.builder.creds[1].Password != "cached-password" {
		t.Fatal("the re-tagged push should use the same cached credential")
	}
}

func TestRun_TagFailureIsABuildProblem(t *testing.T) {
	e := newEnv(t)
	e.builder.tagErr = errors.New("docker tag: Error response from daemon: No such image: shop-api:3f9c2a1b7d4e")
	_, err := e.run(e.options())
	p := api.Explain(err)
	if p.Code != "build_failed" || p.Exit != 6 {
		t.Fatalf("problem = %+v, want build_failed (exit 6), not a push or network problem", p)
	}
	if !strings.Contains(e.errOut.String(), "No such image: shop-api:3f9c2a1b7d4e") {
		t.Fatalf("the error isn't shown above the fix:\n%s", e.errOut.String())
	}
}

func TestRun_TagAlreadyPushedTwiceFails(t *testing.T) {
	e := newEnv(t)
	e.builder.pushErrs = []error{&kxerr.PushFailed{Err: kxerr.ErrTagExists}, &kxerr.PushFailed{Err: kxerr.ErrTagExists}}
	_, err := e.run(e.options())
	if !errors.Is(err, kxerr.ErrTagExists) {
		t.Fatalf("Run() = %v", err)
	}
	if n := len(e.builder.creds); n != 2 {
		t.Fatalf("pushed %d times, want 2 (one re-tag, no loop)", n)
	}
	if len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatal("deployed after the upload failed")
	}
}

func TestRun_NoDigestMeansNoDeploy(t *testing.T) {
	e := newEnv(t)
	e.builder.digest = "none"
	_, err := e.run(e.options())
	if !errors.Is(err, kxerr.ErrNoDigest) {
		t.Fatalf("Run() = %v, want ErrNoDigest", err)
	}
	if len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatal("deployed a tag without knowing which image it holds")
	}
	if p := api.Explain(err); p.Exit != 7 {
		t.Fatalf("exit %d", p.Exit)
	}
}

func TestRun_BuildFailureShowsDockerOutput(t *testing.T) {
	e := newEnv(t)
	var lines []string
	for i := 1; i <= 150; i++ {
		lines = append(lines, fmt.Sprintf("#%d step output", i))
	}
	e.builder.buildOut = strings.Join(lines, "\n") + "\nERROR: failed to solve: \x1b[31mexit code: 1\x1b[0m\n"
	e.builder.buildErr = &kxerr.BuildFailed{Err: errors.New("exit status 1")}
	_, err := e.run(e.options())
	var bf *kxerr.BuildFailed
	if !errors.As(err, &bf) {
		t.Fatalf("Run() = %v", err)
	}
	s := e.errOut.String()
	if !strings.Contains(s, "ERROR: failed to solve: [31mexit code: 1[0m") && !strings.Contains(s, "ERROR: failed to solve: exit code: 1") {
		t.Fatalf("stderr lacks the error:\n%s", s)
	}
	if strings.Contains(s, "\x1b") {
		t.Fatal("Docker output reached the terminal with escape sequences")
	}
	if strings.Contains(s, "#1 step output\n") || !strings.Contains(s, "#150 step output") {
		t.Fatal("expected only the last lines of Docker's output")
	}
	if e.api.mints != 0 || len(e.api.reqs("POST", "/apps")) != 0 {
		t.Fatal("did API work after the build failed")
	}
}

func TestRun_DeployLimitCheckedBeforeBuild(t *testing.T) {
	e := newEnv(t)
	e.api.usage = &api.Usage{Plan: "free", Usage: map[string]api.UsageItem{"deployments_today": {Current: 5, Limit: 5}}}
	_, err := e.run(e.options())
	var dl *kxerr.DeployLimit
	if !errors.As(err, &dl) || dl.Org != "acme-corp" || dl.Plan != "free" || dl.Limit != 5 {
		t.Fatalf("Run() = %#v", err)
	}
	if len(e.builder.got()) != 0 {
		t.Fatal("built although no deploys are left today")
	}
	if p := api.Explain(err); !strings.HasPrefix(p.What, "acme-corp has used all 5 deploys the Free plan includes today") {
		t.Fatalf("message = %q", p.What)
	}
	// kx can't know the next plan's limit, so the fix names no number.
	if p := api.Explain(err); p.Fix != "Upgrade for more deploys a day: platform.kubernetix.ai/plans" || p.Exit != 5 {
		t.Fatalf("fix = %q, exit %d", p.Fix, p.Exit)
	}
}

func TestRun_ServerDeployLimitNamesPlan(t *testing.T) {
	e := newEnv(t)
	e.api.createErr = []string{"402", "tier_limit_exceeded", "max_deployments_per_day"}
	e.api.usage = &api.Usage{Plan: "free", Usage: map[string]api.UsageItem{"deployments_today": {Current: 4, Limit: 5}}}
	_, err := e.run(e.options())
	var dl *kxerr.DeployLimit
	if !errors.As(err, &dl) || dl.Plan != "free" || dl.Limit != 5 {
		t.Fatalf("Run() = %#v", err)
	}
	if p := api.Explain(err); p.Fix != "Upgrade for more deploys a day: platform.kubernetix.ai/plans" {
		t.Fatalf("fix = %q", p.Fix)
	}
}

func TestRun_FailurePrintsReasonAndTwentyLogLines(t *testing.T) {
	e := newEnv(t)
	e.api.statuses = []string{"pending", "deploying", "failed"}
	e.api.failReason = "your container is crashing on start"
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("log line %02d", i))
	}
	e.api.logs["key-"+testKey] = api.DeploymentLogs{DeploymentLogs: strings.Join(lines, "\n") + "\n"}
	res, err := e.run(e.options())
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.App != "shop-api" || df.Version != 15 || df.ServingVersion != 14 || df.Reason != "your container is crashing on start" {
		t.Fatalf("Run() = %#v", err)
	}
	if res == nil || res.Version != 15 || res.DeploymentID == "" || res.ServingVersion != 14 {
		t.Fatalf("result after failure = %+v", res)
	}
	s := e.errOut.String()
	if strings.Contains(s, "log line 10") || !strings.Contains(s, "log line 11") || !strings.Contains(s, "log line 30") {
		t.Fatalf("want exactly the last 20 log lines:\n%s", s)
	}
	if !strings.Contains(s, "✗ Failed    v15  queued → starting → failed") {
		t.Fatalf("stderr:\n%s", s)
	}
	if p := api.Explain(err); p.What != "shop-api v15 didn't go live: your container is crashing on start. Version 14 is still serving." {
		t.Fatalf("message = %q", p.What)
	}
}

func TestRun_UntrustedLinesCannotBeWorkflowCommands(t *testing.T) {
	e := newEnv(t)
	e.api.statuses = []string{"pending", "failed"}
	e.api.logs["key-"+testKey] = api.DeploymentLogs{DeploymentLogs: "GET /::error::owned 404\n::stop-commands::x\n  ::add-mask::y\n\u00a0::warning::nbsp\n\u3000::add-mask::wide\nstep ##[warning]mid-line\nsee ##vso[task.prependpath]/tmp/x\nfine line\n"}
	_, err := e.run(e.options())
	if err == nil {
		t.Fatal("want a failure")
	}
	if cmds := safetext.Commands(e.errOut.String()); len(cmds) != 0 {
		t.Fatalf("app log lines reached the output as workflow commands: %q", cmds)
	}
	if !strings.Contains(e.errOut.String(), "GET /::error::owned 404") || !strings.Contains(e.errOut.String(), "fine line") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}

	e = newEnv(t)
	e.builder.buildOut = "::error::from a Dockerfile\n"
	e.builder.buildErr = &kxerr.BuildFailed{Err: errors.New("exit status 1")}
	_, _ = e.run(e.options())
	if cmds := safetext.Commands(e.errOut.String()); len(cmds) != 0 {
		t.Fatalf("Docker output reached the output as workflow commands: %q", cmds)
	}
}

var stopRe = regexp.MustCompile(`(?m)^::stop-commands::([0-9a-f]{32})$`)

func TestRun_StopCommandsAroundUntrustedBlocksOnGitHub(t *testing.T) {
	e := newEnv(t)
	e.api.statuses = []string{"pending", "failed"}
	e.api.logs["key-"+testKey] = api.DeploymentLogs{DeploymentLogs: "::warning::a\nboot failed\n"}
	o := e.options()
	o.StopCommands = true
	if _, err := e.run(o); err == nil {
		t.Fatal("want a failure")
	}
	out := e.errOut.String()
	m := stopRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stop-commands line:\n%s", out)
	}
	if !strings.Contains(out, "boot failed\n::"+m[1]+"::\n") {
		t.Fatalf("the block isn't closed right after the log lines:\n%s", out)
	}
	if got := safetext.Commands(out); strings.Join(got, ",") != "stop-commands,resume-commands" {
		t.Fatalf("runner would act on %q", got)
	}

	e = newEnv(t)
	e.builder.buildOut = "\u00a0::error::from a Dockerfile\n"
	e.builder.buildErr = &kxerr.BuildFailed{Err: errors.New("exit status 1")}
	o = e.options()
	o.StopCommands = true
	_, _ = e.run(o)
	if got := safetext.Commands(e.errOut.String()); strings.Join(got, ",") != "stop-commands,resume-commands" {
		t.Fatalf("runner would act on %q\n%s", got, e.errOut.String())
	}
	if m2 := stopRe.FindStringSubmatch(e.errOut.String()); m2 == nil || m2[1] == m[1] {
		t.Fatal("each block needs its own random token")
	}
}

func TestRun_Timeout(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"pending", "deploying"}
	o := e.options()
	res, err := e.run(o)
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || wt.App != "shop-api" || wt.Version != 15 || wt.After != 18*time.Minute {
		t.Fatalf("Run() = %#v", err)
	}
	if res == nil || res.Version != 15 {
		t.Fatalf("result = %+v", res)
	}
	p := api.Explain(err)
	if p.What != "shop-api v15 is still starting after 18 minutes, so kx stopped waiting; the deploy continues." || p.Fix != "Check on it: kx status" || p.Exit != 9 {
		t.Fatalf("problem = %+v", p)
	}
}

// The default wait outlasts the platform's own verdict on a version that
// never gets ready (up to 3 attempts of 5 minutes each), so kx reports
// that verdict instead of giving up first.
func TestDefaultTimeout_OutlastsThePlatformsRetries(t *testing.T) {
	if DefaultTimeout != 18*time.Minute || DefaultTimeout <= 3*5*time.Minute {
		t.Fatalf("DefaultTimeout = %v, want 18m", DefaultTimeout)
	}
}

func TestRun_CustomTimeout(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"deploying"}
	o := e.options()
	o.Timeout = 3 * time.Minute
	_, err := e.run(o)
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || wt.After != 3*time.Minute {
		t.Fatalf("Run() = %v", err)
	}
}

func TestRun_InterruptedWhileWaiting(t *testing.T) {
	e := newEnv(t)
	e.api.statuses = []string{"deploying"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.clock.hook = func(n int) error {
		if n == 2 {
			cancel()
		}
		return nil
	}
	o := e.options()
	_, err := Run(ctx, e.client, o, *e.p)
	var sw *kxerr.StoppedWaiting
	if !errors.As(err, &sw) || sw.App != "shop-api" || sw.Version != 15 {
		t.Fatalf("Run() = %v, want StoppedWaiting", err)
	}
}

func TestRun_KeepsPollingThroughBriefOutages(t *testing.T) {
	e := newEnv(t)
	e.api.getFail = 8 // the client retries 3 times per poll, so two polls fail
	res, err := e.run(e.options())
	if err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
}

// A rate limit while waiting is not a deploy failure: kx waits as long as
// Retry-After asks and polls again.
func TestRun_RateLimitWhileWaitingIsWaitedOut(t *testing.T) {
	e := newEnv(t)
	e.api.getLimited, e.api.retryAfter = 3, "7"
	res, err := e.run(e.options())
	if err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if n := countSleeps(e.clock, 7*time.Second); n != 3 {
		t.Fatalf("slept %v, want three 7 s waits (Retry-After)", e.clock.sleeps)
	}

	// Without Retry-After kx waits 5 seconds.
	e = newEnv(t)
	e.api.getLimited = 8 // more than the failures kx rides out for outages
	if res, err := e.run(e.options()); err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if n := countSleeps(e.clock, 5*time.Second); n < 8 {
		t.Fatalf("slept %v, want 5 s after each 429", e.clock.sleeps)
	}
}

func TestRun_RateLimitWhileWaitingForAnEarlierDeploy(t *testing.T) {
	e := newEnv(t)
	inFlight := api.Deployment{ID: "dep-15", Version: 15, Status: "deploying"}
	done := inFlight
	done.Status = "ready"
	e.api.lists = [][]api.Deployment{{inFlight}, {done}}
	e.api.retryAfter = "9"
	e.clock.hook = func(n int) error {
		if n == 1 {
			e.api.mu.Lock()
			e.api.listLimited = 2
			e.api.mu.Unlock()
		}
		return nil
	}
	if res, err := e.run(e.options()); err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if n := countSleeps(e.clock, 9*time.Second); n != 2 {
		t.Fatalf("slept %v, want two 9 s waits", e.clock.sleeps)
	}
}

// In CI kx waits out the registry credential limit (up to MintWait) with a
// progress line, instead of failing the job.
func TestRun_CIWaitsOutTheUploadLimit(t *testing.T) {
	e := newEnv(t)
	e.api.mintLimits = []string{"240"}
	o := e.options()
	o.MintWait = 6*time.Minute + 30*time.Second
	res, err := e.run(o)
	if err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if countSleeps(e.clock, 4*time.Minute) != 1 || e.api.mints != 1 {
		t.Fatalf("slept %v, minted %d", e.clock.sleeps, e.api.mints)
	}
	if !strings.Contains(e.errOut.String(), "This token has uploaded images too often in the last hour, so kx waits 4m 0s before uploading.\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}

	// Two refusals still within the budget.
	e = newEnv(t)
	e.api.mintLimits = []string{"120", "200"}
	o = e.options()
	o.MintWait = 6*time.Minute + 30*time.Second
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}

	// A wait past the budget fails at once with the usual message.
	e = newEnv(t)
	e.api.mintLimits = []string{"300", "300"}
	o = e.options()
	o.MintWait = 6*time.Minute + 30*time.Second
	_, err = e.run(o)
	if p := api.Explain(err); p.Code != "push_rate_limited" || p.Fix != "Wait 5 minutes, then run kx deploy again." {
		t.Fatalf("Run() = %v (%+v)", err, p)
	}
	if countSleeps(e.clock, 5*time.Minute) != 1 || len(e.api.reqs("POST", "/apps/"+testAppID+"/deployments")) != 0 {
		t.Fatalf("slept %v", e.clock.sleeps)
	}
}

// On a laptop (MintWait 0) the limit fails at once and says how long to wait.
func TestRun_LaptopDoesNotWaitForTheUploadLimit(t *testing.T) {
	e := newEnv(t)
	e.api.mintLimits = []string{"240"}
	_, err := e.run(e.options())
	if p := api.Explain(err); p.Code != "push_rate_limited" || p.Fix != "Wait 4 minutes, then run kx deploy again." {
		t.Fatalf("Run() = %v (%+v)", err, p)
	}
	if len(e.clock.sleeps) != 0 {
		t.Fatalf("slept %v", e.clock.sleeps)
	}
}

func countSleeps(c *fakeClock, d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sleeps {
		if s == d {
			n++
		}
	}
	return n
}

// A second kx deploy of the app inside one CI run attempt sends the same
// idempotency key, and Kubernetix hands back the deployment that run
// already started. kx says so and reports that deployment's image, not
// the one it just pushed.
func TestRun_ReplayOfAnEarlierDeploymentIsReported(t *testing.T) {
	e := newEnv(t)
	earlier := "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat("00", 32)
	e.api.deployments[testAppID] = append([]api.Deployment{{ID: "key-" + testKey, AppID: testAppID, Version: 15, Status: "ready", ImageURL: earlier}}, e.api.deployments[testAppID]...)
	res, err := e.run(e.options())
	if err != nil || res.Version != 15 || res.Image != earlier || !res.Replayed {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if !strings.Contains(e.errOut.String(), "This run already deployed v15, so nothing new was started; kx reports that deploy.\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}

	// A new deployment is not a replay.
	e = newEnv(t)
	res, err = e.run(e.options())
	if err != nil || res.Replayed || !strings.HasSuffix(res.Image, testDigest) || strings.Contains(e.errOut.String(), "already deployed") {
		t.Fatalf("Run() = %+v, %v\n%s", res, err, e.errOut.String())
	}
}

func TestSameDigest(t *testing.T) {
	a := "registry.kubernetix.ai/acme-corp/shop-api@" + testDigest
	tests := []struct {
		x, y  string
		known bool
		same  bool
	}{
		{a, a, true, true},
		{a, "registry.example.test/acme-corp/shop-api@" + testDigest, true, true}, // shown under another host
		{a, "registry.kubernetix.ai/acme-corp/shop-api@sha256:" + strings.Repeat("00", 32), true, false},
		{a, "nginx:latest", false, false},
		{"", a, false, false},
	}
	for _, tt := range tests {
		same, known := sameDigest(tt.x, tt.y)
		if same != tt.same || known != tt.known {
			t.Errorf("sameDigest(%q, %q) = %v, %v; want %v, %v", tt.x, tt.y, same, known, tt.same, tt.known)
		}
	}
}

func TestRun_NoWait(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.NoWait = true
	res, err := e.run(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "started" || res.Version != 15 || len(e.api.reqs("GET", "/deployments/")) != 0 {
		t.Fatalf("result %+v, polled %d times", res, len(e.api.reqs("GET", "/deployments/")))
	}
	if !strings.Contains(e.errOut.String(), "✓ Started   v15") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRun_OnDeployAndOnBuild(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	var events []string
	o.OnBuild = func() { events = append(events, "build") }
	o.OnDeploy = func(app *api.App, d *api.Deployment) {
		events = append(events, fmt.Sprintf("deploy %s v%d", app.Name, d.Version))
	}
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "build,deploy shop-api v15" {
		t.Fatalf("events = %v", events)
	}
}

func TestRun_RejectsBadOptions(t *testing.T) {
	e := newEnv(t)
	bad := []func(*Options){
		func(o *Options) { o.IdempotencyKey = "" },
		func(o *Options) { o.App, o.NewApp = nil, "" },
		func(o *Options) { o.NewApp = "web" }, // both
		func(o *Options) { o.Build = nil },
		func(o *Options) { o.Builder = nil },
		func(o *Options) { o.Tag = "bad tag" },
		func(o *Options) { o.App, o.NewApp = nil, "Not An App" },
	}
	for i, mutate := range bad {
		o := e.options()
		mutate(&o)
		if _, err := e.run(o); err == nil {
			t.Errorf("case %d: Run() accepted bad options", i)
		}
	}
	if len(e.api.requests) != 0 {
		t.Fatal("made API calls with bad options")
	}
}

// answers returns an Ask that gives these answers in turn and records
// each question.
func answers(asked *[]string, list ...string) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, q, def string) (string, error) {
		*asked = append(*asked, q+" ["+def+"]")
		if len(list) == 0 {
			return "", ui.ErrNoChoice
		}
		a := list[0]
		list = list[1:]
		if a == "" {
			a = def
		}
		return a, nil
	}
}

func TestRun_NoExpose_AsksPortOnce(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 0
	var asked []string
	o.Ask = answers(&asked, "3000")
	if _, err := e.run(o); err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	if len(asked) != 1 || asked[0] != "Which port does your app listen on? [8080]" {
		t.Fatalf("asked %q", asked)
	}
	if port := e.api.reqs("POST", "/apps")[0].Body["port"]; port != float64(3000) {
		t.Fatalf("port = %v", port)
	}
	if strings.Contains(e.errOut.String(), "EXPOSE") || !strings.Contains(e.errOut.String(), "✓ Checked   Dockerfile, port 3000\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}

	// Once the app exists, its port is known: no question.
	e = newEnv(t)
	o = e.options()
	o.Build.Port = 0
	asked = nil
	o.Ask = answers(&asked, "3000")
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Fatalf("asked %q for an app that exists", asked)
	}
}

func TestRun_NoExpose_BadAnswersAskAgain(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 0
	var asked []string
	o.Ask = answers(&asked, "http", "0", "65536", "", "")
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(e.errOut.String(), "Type a port number from 1 to 65535.\n"); n != 3 || len(asked) != 4 {
		t.Fatalf("asked %d times, %d retries:\n%s", len(asked), n, e.errOut.String())
	}
	if port := e.api.reqs("POST", "/apps")[0].Body["port"]; port != float64(8080) {
		t.Fatalf("port = %v, want the default", port)
	}

	// Input that ends without an answer stops before anything is built.
	e = newEnv(t)
	o = e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 0
	o.Ask = answers(&asked)
	if _, err := e.run(o); !errors.Is(err, context.Canceled) || len(e.builder.got()) != 0 {
		t.Fatalf("Run() = %v, docker calls %v", err, e.builder.got())
	}
}

// Without a terminal (or in CI) kx can't ask: it says what it assumes, as
// before.
func TestRun_NoExpose_NoTerminal_KeepsWarning(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 0
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	want := "! Your Dockerfile has no EXPOSE line, so kx assumes your app listens on port 8080.\n" +
		"  Kubernetix checks that it answers on / there; if it listens on another port, add EXPOSE <port> to your Dockerfile.\n"
	if !strings.Contains(e.errOut.String(), want) {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

const expose80 = "! Your Dockerfile exposes port 80. Apps on Kubernetix don't run as root, so they may not be able to listen on ports below 1024.\n" +
	"  Make your app listen on 8080 and change the line to EXPOSE 8080.\n"

func TestRun_Expose80_AsksAndStopsByDefault(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 80
	var asked []string
	o.Confirm = func(_ context.Context, q string) (bool, error) { asked = append(asked, q); return false, nil }
	_, err := e.run(o)
	if p := api.Explain(err); p.What != "Nothing was deployed, because your app may not be able to listen on port 80." ||
		p.Fix != "Change the line to EXPOSE 8080, then run kx deploy again." || p.Exit != api.ExitUsage {
		t.Fatalf("Run() = %v: %+v", err, p)
	}
	if len(asked) != 1 || asked[0] != "Deploy anyway?" || !strings.HasPrefix(e.errOut.String(), expose80) {
		t.Fatalf("asked %q, stderr:\n%s", asked, e.errOut.String())
	}
	if len(e.builder.got()) != 0 || len(e.api.reqs("POST", "/apps")) != 0 {
		t.Fatalf("docker %v, API %v", e.builder.got(), e.api.reqs("POST", "/"))
	}

	// An existing app too.
	e = newEnv(t)
	o = e.options()
	o.Build.Port = 443
	o.Confirm = func(context.Context, string) (bool, error) { return false, nil }
	if _, err := e.run(o); api.Explain(err).What != "Nothing was deployed, because your app may not be able to listen on port 443." {
		t.Fatalf("Run() = %v", err)
	}
	if len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatal("deployed after no")
	}
}

func TestRun_Expose80_Yes_Deploys(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 80
	o.Confirm = func(context.Context, string) (bool, error) { return true, nil }
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if port := e.api.reqs("POST", "/apps")[0].Body["port"]; port != float64(80) {
		t.Fatalf("port = %v", port)
	}
}

func TestRun_Expose80_CantAsk_WarnsAndDeploys(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.App, o.NewApp = nil, "billing-worker"
	o.Build.Port = 80
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.errOut.String(), expose80) || len(e.api.reqs("POST", "/apps/")) != 1 {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

// An app that is live on that port shows it works there: kx warns but
// doesn't ask on every deploy.
func TestRun_Expose80_AppLiveOnThatPort_DoesNotAsk(t *testing.T) {
	e := newEnv(t)
	e.api.apps[testAppID].Port, e.api.apps[testAppID].Status = 80, "ready"
	o := e.options()
	o.Build.Port = 80
	o.Confirm = func(context.Context, string) (bool, error) { t.Fatal("asked"); return false, nil }
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.errOut.String(), expose80) {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}

	// Not live there yet: kx asks.
	e = newEnv(t)
	e.api.apps[testAppID].Port, e.api.apps[testAppID].Status = 80, "failed"
	o = e.options()
	o.Build.Port = 80
	asked := false
	o.Confirm = func(context.Context, string) (bool, error) { asked = true; return true, nil }
	if _, err := e.run(o); err != nil || !asked {
		t.Fatalf("Run() = %v, asked %v", err, asked)
	}
}

// The ceiling on tries a day (failed deploys included) is worded apart
// from the daily deploy limit, which kx status would contradict.
func TestRun_ServerTriesCeilingNamesPlan(t *testing.T) {
	e := newEnv(t)
	e.api.createErr = []string{"402", "tier_limit_exceeded", "max_deployments_per_day", "too_many_tries"}
	e.api.usage = &api.Usage{Plan: "free", Usage: map[string]api.UsageItem{"deployments_today": {Current: 2, Limit: 5}}}
	_, err := e.run(e.options())
	p := api.Explain(err)
	want := api.Problem{Code: "deploy_limit_reached",
		What: "acme-corp has started 25 deploys today, failed ones included, which is the most the Free plan allows in a day.",
		Fix:  "More are available at 00:00 UTC; fix what makes the deploys fail before trying again.", Exit: api.ExitLimit}
	if p != want {
		t.Fatalf("Explain() = %+v\nwant %+v", p, want)
	}
}
