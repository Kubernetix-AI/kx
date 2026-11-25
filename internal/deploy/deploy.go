// Package deploy is kx deploy's flow after the app is known: refuse
// multi-region apps, check the Dockerfile's port, wait for an earlier
// deploy of the same app to finish, build and push the image (or take
// --image), create the app if it is new, start the deployment with an
// idempotency key, and wait until it is live, printing Checked, Built,
// Pushed and Live with elapsed times. It also rolls back.
//
// Progress goes to the printer's standard error. Run never prints the
// URL; the command does, last and alone on standard output.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/docker"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/registry"
	"github.com/kubernetix-ai/kx/internal/safetext"
	"github.com/kubernetix-ai/kx/internal/ui"
)

// DefaultTimeout is how long kx waits for a deploy (or rollback) to go
// live. Kubernetix tries a version that doesn't get ready up to 3 times, 5
// minutes each, so its own verdict arrives before kx would give up.
const DefaultTimeout = 18 * time.Minute

// defaultPort is the port kx gives a new app when nothing says otherwise.
const defaultPort = 8080

const (
	shownDockerLines = 100 // Docker output shown when a step fails
	shownLogLines    = 20  // deployment log lines shown when a deploy fails
)

// Builder is the Docker side of a deploy; docker.Engine implements it.
type Builder interface {
	Check(ctx context.Context) error
	Build(ctx context.Context, o docker.BuildOptions, w io.Writer) error
	Tag(ctx context.Context, src, dst string) error
	Push(ctx context.Context, ref string, cred api.RegistryCredential, w io.Writer) (string, error)
}

// Registry looks tags up and points tags at images the registry already
// has; registry.Client implements it. Lookup returns an empty Image when
// there is no such tag.
type Registry interface {
	Lookup(ctx context.Context, ref string, cred api.RegistryCredential) (registry.Image, error)
	Tag(ctx context.Context, repo, digest, tag string, cred api.RegistryCredential) error
}

// credentialMargin is how much time a registry credential minted before
// the build must have left to push with afterwards.
const credentialMargin = 10 * time.Minute

// CredentialStore keeps a registry credential between runs (laptops
// only); docker.CredentialCache implements it.
type CredentialStore interface {
	Load() (*api.RegistryCredential, bool)
	Save(api.RegistryCredential) error
	Forget()
}

// Options describe one deploy.
type Options struct {
	// App is the app to deploy. For a new app leave it nil and set NewApp
	// (org tokens only; the command decides).
	App    *api.App
	NewApp string
	Org    string

	// Image deploys an image that is already pushed and skips the build.
	Image string
	// Build and Tag are needed when Image is empty: Build comes from
	// Preflight and Tag from BuildTags (or ImageTag).
	Build *Dockerfile
	Tag   string
	// VersionTag is a second tag for the image kx builds: the commit's
	// version from BuildTags, or --tag (then TagFromFlag is set). kx looks
	// it up first and never moves a tag that holds another image.
	// Registry does the looking up.
	VersionTag  string
	TagFromFlag bool
	Registry    Registry
	// Repository is the repository in the organization's registry that kx
	// pushes to; "" means the app's name.
	Repository string
	// Save, when set, changes the app's configuration (its env vars or
	// replicas) instead of building: Run makes every check first (the app,
	// an earlier deploy, today's deploys), then calls Save, then deploys
	// the app as configured (an empty deploy request) and waits as for
	// any deploy. Nothing is saved when a check fails. App must be set;
	// Image, Build, VersionTag, Repository and Provenance must not.
	Save func(ctx context.Context) error

	// Provenance, when the build is of a commit as it is (ProvenanceOf),
	// goes on the image as labels. Its commit also tells a version tag on
	// an earlier build of the same commit, which kx leaves where it is,
	// from one on another commit, which stops the deploy before the build.
	Provenance *Provenance

	// IdempotencyKey is sent with the deployment request and reused on
	// every retry, so a retry never deploys twice.
	IdempotencyKey string
	NoWait         bool
	Timeout        time.Duration // 0 means DefaultTimeout

	Builder     Builder
	Credentials CredentialStore // nil: mint a new credential (always in CI)
	// MintWait is how long kx may wait in all for the registry credential
	// limit to allow a new credential (CI, where nothing would retry the
	// job); 0 fails at once and says how long to wait (laptops).
	MintWait time.Duration

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// OnCredential sees every registry credential before anything else
	// does (GitHub mode masks it). OnBuild runs before the build,
	// OnCreated (when set) just after kx created the app, and OnDeploy
	// once the deployment exists.
	OnCredential func(api.RegistryCredential)
	OnBuild      func()
	OnCreated    func(app *api.App)
	OnDeploy     func(app *api.App, d *api.Deployment)

	// StopCommands wraps every block of untrusted output (Docker's, the
	// app's logs) in a GitHub ::stop-commands:: block. Set it whenever kx
	// runs in GitHub Actions, in any mode: the runner reads every line.
	StopCommands bool

	// OnNotLive sees the two lines kx prints, while it keeps waiting, when
	// the new version's containers aren't getting ready (GitHub mode makes
	// them a warning annotation).
	OnNotLive func(what, fix string)

	// Ask and Confirm put a question to the person before anything is
	// built: the port a new app listens on when the Dockerfile doesn't
	// say, and whether to deploy a port below 1024. Leave them nil when
	// nobody can answer (no terminal, or CI); kx then warns and goes on.
	Ask     func(ctx context.Context, question, def string) (string, error)
	Confirm func(ctx context.Context, question string) (bool, error)
}

// Result is what a deploy did. After a failure Run still returns the
// Result when a deployment was created, so CI can report it.
type Result struct {
	App, AppID, Org string
	URL             string
	DeploymentID    string
	Version         int
	Status          string // "live", "started" (NoWait) or the final status
	Image           string // what was deployed; by digest when kx built it
	CreatedApp      bool
	ServingVersion  int // after a failed deploy: the version still serving, 0 if none
	// Tags are the tags kx pushed the image under, the commit's first;
	// VersionTag is the second one, if any (the version or --tag). Both
	// are empty with Image.
	Tags       []string
	VersionTag string
	// Replayed means Kubernetix handed back a deployment this CI run had
	// already started (same idempotency key); Image is that one's.
	Replayed bool
}

var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (o *Options) prepare() error {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = defaultSleep
	}
	if o.OnCredential == nil {
		o.OnCredential = func(api.RegistryCredential) {}
	}
	if o.OnBuild == nil {
		o.OnBuild = func() {}
	}
	if o.OnDeploy == nil {
		o.OnDeploy = func(*api.App, *api.Deployment) {}
	}
	if o.OnNotLive == nil {
		o.OnNotLive = func(string, string) {}
	}
	switch {
	case o.IdempotencyKey == "":
		return errors.New("deploy: an idempotency key is required")
	case (o.App == nil) == (o.NewApp == ""):
		return errors.New("deploy: give exactly one of App and NewApp")
	case o.NewApp != "" && !config.ValidAppName(o.NewApp):
		return fmt.Errorf("deploy: %q is not an app name", o.NewApp)
	case o.Save != nil && (o.App == nil || o.Image != "" || o.Build != nil || o.VersionTag != "" || o.Repository != "" || o.Provenance != nil):
		return errors.New("deploy: Save deploys an existing app as configured, and nothing else")
	case o.Save == nil && o.Image == "" && (o.Build == nil || o.Builder == nil):
		return errors.New("deploy: building needs Build and Builder")
	case o.Save == nil && o.Image == "" && !tagRe.MatchString(o.Tag):
		return fmt.Errorf("deploy: %q is not an image tag", o.Tag)
	case o.Image != "" && (o.VersionTag != "" || o.Repository != ""):
		return errors.New("deploy: VersionTag and Repository are for images kx builds")
	case o.VersionTag != "" && !tagRe.MatchString(o.VersionTag):
		return fmt.Errorf("deploy: %q is not an image tag", o.VersionTag)
	case o.VersionTag != "" && o.Registry == nil:
		return errors.New("deploy: a version tag needs Registry")
	case o.Repository != "" && !config.ValidRepository(o.Repository):
		return fmt.Errorf("deploy: %q is not a repository name", o.Repository)
	case o.Image != "" && o.Provenance != nil:
		return errors.New("deploy: Provenance is for images kx builds")
	case o.Provenance != nil && !o.Provenance.valid():
		return errors.New("deploy: Provenance needs a full commit and label-safe paths")
	}
	return nil
}

type runner struct {
	c *api.Client
	o Options
	p *ui.Printer

	rollback bool // the deployment is a rollback (changes the advice)
	// What the replicas said at the last look while waiting: the stuck
	// ones, and the likely cause kx tells the person (nil when nothing is
	// stuck).
	stuck  *stalled
	likely *kxerr.NotStarting
}

// Run deploys and waits (unless NoWait) until the deployment is live,
// failed, or Timeout passes. Errors are kx errors that api.Explain
// words: DeployFailed, WaitTimeout, PreviousRunning, StoppedWaiting,
// BuildFailed, PushFailed, DeployLimit, MultiRegion or an API error.
func Run(ctx context.Context, c *api.Client, o Options, p ui.Printer) (*Result, error) {
	if err := o.prepare(); err != nil {
		return nil, err
	}
	r := &runner{c: c, o: o, p: &p}
	return r.deploy(ctx)
}

func (r *runner) deploy(ctx context.Context) (*Result, error) {
	o := r.o
	res := &Result{Org: o.Org, App: o.NewApp}
	app := o.App
	if app != nil {
		r.c.Ctx.App = app.Name
		fresh, err := r.c.GetApp(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		if fresh.MultiRegion() {
			return nil, &kxerr.MultiRegion{App: fresh.Name}
		}
		app = fresh
		res.App, res.AppID, res.URL = app.Name, app.ID, app.URL
	} else {
		r.c.Ctx.App = o.NewApp
	}

	port := 0
	if o.Save == nil {
		var err error
		if port, err = r.check(ctx, app); err != nil {
			return nil, err
		}
	}
	if app != nil {
		if err := r.waitForPrevious(ctx, app); err != nil {
			return nil, err
		}
	}
	if err := r.deploysLeftBefore(ctx); err != nil {
		return nil, err
	}

	image := o.Image
	if o.Save != nil {
		if err := o.Save(ctx); err != nil {
			return nil, err
		}
	} else if image == "" {
		built, tags, version, err := r.image(ctx, res.App)
		if err != nil {
			return nil, err
		}
		image, res.Tags, res.VersionTag = built, tags, version
	}
	res.Image = image

	if app == nil {
		created, err := r.c.CreateApp(ctx, api.CreateAppRequest{Name: o.NewApp, SourceType: "image", ImageURL: image, Port: port})
		if err != nil {
			return nil, err
		}
		app = created
		res.AppID, res.CreatedApp = app.ID, true
		r.p.Step("Created", "app "+app.Name, 0)
		if o.OnCreated != nil {
			o.OnCreated(app)
		}
	}

	if !res.CreatedApp && o.Image == "" && o.Save == nil {
		// The build took a while: check again that nothing (a dashboard
		// deploy, another computer) started meanwhile, so two deploys of
		// the app never run at once from kx's side.
		if err := r.waitForPrevious(ctx, app); err != nil {
			return res, err
		}
	}
	dep, err := r.c.CreateDeployment(ctx, app.ID, api.CreateDeploymentRequest{ImageURL: image}, o.IdempotencyKey)
	if err != nil {
		return res, r.deployLimit(ctx, err)
	}
	res.DeploymentID, res.Version, res.Status = dep.ID, dep.Version, dep.Status
	if o.Save != nil {
		res.Image = dep.ImageURL // the image the app is configured with
	}
	if same, known := sameDigest(dep.ImageURL, image); dep.Replayed || (known && !same) {
		// Same key, earlier deployment: say so, and report what it deployed.
		if o.Save != nil {
			// A change's deploy has a key of its own: the earlier deployment
			// came from this same command, a try that got through.
			r.p.Info("Kubernetix had already started v%d for this change, so kx follows that one.", dep.Version)
		} else {
			r.p.Info("This run already deployed v%d, so nothing new was started; kx reports that deploy.", dep.Version)
		}
		res.Replayed = true
		if dep.ImageURL != "" {
			res.Image = dep.ImageURL
		}
	}
	o.OnDeploy(app, dep)
	if o.NoWait {
		r.p.Step("Started", fmt.Sprintf("v%d; kx isn't waiting for it to go live", dep.Version), 0)
		res.Status = "started"
		return res, nil
	}
	return r.finish(ctx, app, dep, res)
}

// finish waits for dep and reports the outcome.
func (r *runner) finish(ctx context.Context, app *api.App, dep *api.Deployment, res *Result) (*Result, error) {
	start := r.o.Now()
	final, phases, err := r.wait(ctx, app, dep)
	if final != nil {
		res.Status = final.Status
	}
	if err != nil {
		return res, err
	}
	took := r.o.Now().Sub(start)
	path := strings.Join(phases, " → ")
	if final.Status != api.StatusReady {
		r.p.StepFailed("Failed", fmt.Sprintf("v%d  %s", final.Version, path), took)
		failed := r.failed(ctx, app, final)
		res.ServingVersion = failed.ServingVersion
		return res, failed
	}
	r.p.Step("Live", fmt.Sprintf("v%d  %s", final.Version, path), took)
	res.Status = "live"
	r.deploysLeftAfter(ctx)
	if fresh, err := r.c.GetApp(ctx, app.ID); err == nil && fresh.URL != "" {
		res.URL = fresh.URL
	}
	return res, nil
}

// sameDigest compares two image references by digest; known is false when
// either has none. The API may show an image under another registry host
// than the one kx sent, but never with another digest.
func sameDigest(a, b string) (same, known bool) {
	_, da, okA := strings.Cut(a, "@")
	_, db, okB := strings.Cut(b, "@")
	if !okA || !okB || !strings.HasPrefix(da, "sha256:") || !strings.HasPrefix(db, "sha256:") {
		return false, false
	}
	return da == db, true
}

// check prints the Checked line and port warnings, and returns the port a
// new app gets. It asks, when it can, for the port of a new app whose
// Dockerfile has no EXPOSE line, and whether to deploy a port below 1024;
// a no to that stops the deploy before anything is built.
func (r *runner) check(ctx context.Context, app *api.App) (int, error) {
	o := r.o
	name := o.NewApp
	if app != nil {
		name = app.Name
	}
	if o.Image != "" {
		r.p.Step("Checked", "image "+o.Image, 0)
		if app == nil {
			r.p.Warn("kx can't see which port the image listens on, so %s will send traffic to port %d.", name, defaultPort)
			return defaultPort, nil
		}
		return app.Port, nil
	}
	exposed := o.Build.Port
	port := exposed
	if app != nil && app.Port > 0 {
		port = app.Port
	}
	switch {
	case port == 0 && app == nil && o.Ask != nil:
		asked, err := r.askPort(ctx)
		if err != nil {
			return 0, err
		}
		port = asked
		if port < 1024 {
			r.p.Warn("Apps on Kubernetix don't run as root, so they may not be able to listen on ports below 1024.")
			r.p.Note("Make your app listen on %d.", defaultPort)
			if err := r.deployAnyway(ctx, port, fmt.Sprintf("Make your app listen on %d, then run kx deploy again.", defaultPort)); err != nil {
				return 0, err
			}
		}
	case port == 0:
		port = defaultPort
		if app == nil {
			r.p.Warn("Your Dockerfile has no EXPOSE line, so kx assumes your app listens on port %d.", defaultPort)
			r.p.Note("Kubernetix checks that it answers on / there; if it listens on another port, add EXPOSE <port> to your Dockerfile.")
		}
	}
	if app != nil && exposed > 0 && app.Port > 0 && exposed != app.Port {
		r.p.Warn("The Dockerfile exposes port %d, but %s listens on port %d.", exposed, name, app.Port)
		r.p.Note("Kubernetix sends traffic to port %d; make your app listen there, or change the app's port in the dashboard.", app.Port)
	}
	if exposed > 0 && exposed < 1024 {
		r.p.Warn("Your Dockerfile exposes port %d. Apps on Kubernetix don't run as root, so they may not be able to listen on ports below 1024.", exposed)
		r.p.Note("Make your app listen on %d and change the line to EXPOSE %d.", defaultPort, defaultPort)
		// An app that is live on that port shows it can listen there.
		liveThere := app != nil && app.Port == exposed && app.Status == api.StatusReady
		if !liveThere {
			if err := r.deployAnyway(ctx, exposed, fmt.Sprintf("Change the line to EXPOSE %d, then run kx deploy again.", defaultPort)); err != nil {
				return 0, err
			}
		}
	}
	r.p.Step("Checked", fmt.Sprintf("%s, port %d", filepath.Base(o.Build.Path), port), 0)
	return port, nil
}

// deployAnyway asks, when kx can, whether to deploy an app that may not be
// able to listen on port, which is below 1024; no stops the deploy, with
// fix. Without a way to ask, the deploy goes on.
func (r *runner) deployAnyway(ctx context.Context, port int, fix string) error {
	if r.o.Confirm == nil {
		return nil
	}
	yes, err := r.o.Confirm(ctx, "Deploy anyway?")
	if err != nil {
		return err
	}
	if !yes {
		return &kxerr.Usage{What: fmt.Sprintf("Nothing was deployed, because your app may not be able to listen on port %d.", port), Fix: fix}
	}
	return nil
}

// askPort asks which port a new app listens on, until the answer is a
// port. Input that ends without one stops kx, like Ctrl-C.
func (r *runner) askPort(ctx context.Context) (int, error) {
	for {
		answer, err := r.o.Ask(ctx, "Which port does your app listen on?", strconv.Itoa(defaultPort))
		if errors.Is(err, ui.ErrNoChoice) {
			return 0, context.Canceled
		}
		if err != nil {
			return 0, err
		}
		if port, err := strconv.Atoi(answer); err == nil && port >= 1 && port <= 65535 {
			return port, nil
		}
		r.p.Info("Type a port number from 1 to 65535.")
	}
}

// deploysLeftBefore stops before building when today's deploys are used
// up. Usage is optional: without read access kx just tries.
func (r *runner) deploysLeftBefore(ctx context.Context) error {
	if r.o.Org == "" {
		return nil
	}
	u, err := r.c.Usage(ctx, r.o.Org)
	if err != nil {
		return nil // a courtesy check: the API enforces the limit anyway
	}
	if used, limit, ok := u.DeploysToday(); ok && limit > 0 && used >= limit {
		return &kxerr.DeployLimit{Org: r.o.Org, Plan: u.Plan, Limit: limit, Now: r.o.Now()}
	}
	return nil
}

// deploysLeftAfter prints "4 of 5 deploys left today" when five or fewer
// remain.
func (r *runner) deploysLeftAfter(ctx context.Context) {
	if r.o.Org == "" {
		return
	}
	u, err := r.c.Usage(ctx, r.o.Org)
	if err != nil {
		return
	}
	used, limit, ok := u.DeploysToday()
	if !ok || limit <= 0 {
		return
	}
	left := max(limit-used, 0)
	if left > 5 {
		return
	}
	line := fmt.Sprintf("%d of %d deploys left today", left, limit)
	if u.Plan != "" {
		line += " on the " + strings.ToUpper(u.Plan[:1]) + u.Plan[1:] + " plan"
	}
	r.p.Note("%s.", line)
}

// deployLimit turns the API's daily-limit error into one that names the
// plan and its limit. The ceiling on deploys started a day (failed ones
// included) stays the API's error: only Kubernetix knows that ceiling.
func (r *runner) deployLimit(ctx context.Context, err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "tier_limit_exceeded" || apiErr.LimitName != "max_deployments_per_day" {
		return err
	}
	if apiErr.Reason == "too_many_tries" {
		if apiErr.Ctx.Org == "" {
			apiErr.Ctx.Org = r.o.Org
		}
		return err
	}
	limit := &kxerr.DeployLimit{Org: r.o.Org, Now: r.o.Now()}
	if r.o.Org != "" {
		if u, uerr := r.c.Usage(ctx, r.o.Org); uerr == nil {
			if _, l, ok := u.DeploysToday(); ok {
				limit.Limit = l
			}
			limit.Plan = u.Plan
		}
	}
	return limit
}

// image builds the image as name:<tag> and pushes it to the repository
// (o.Repository, or name) under the commit's tag, then gives it the
// version tag when it can. It returns the image by digest, the tags that
// name it, and the version tag if it is one of them.
func (r *runner) image(ctx context.Context, name string) (string, []string, string, error) {
	o := r.o
	repoName := name
	if o.Repository != "" {
		repoName = o.Repository
	}
	var cred *api.RegistryCredential
	var cached bool
	if v := o.VersionTag; v != "" && v != o.Tag && o.Provenance != nil {
		// A version tag already on another commit's image would stop the
		// deploy after the build: look first. Without an answer, the look
		// after the build decides.
		var err error
		if cred, cached, err = r.credential(ctx); err != nil {
			return "", nil, "", err
		}
		var held registry.Image
		held, cred, cached, err = r.lookup(ctx, cred.RegistryURL+"/"+repoName+":"+v, cred, cached)
		if ctx.Err() != nil {
			return "", nil, "", ctx.Err()
		}
		if err == nil && held.Digest != "" && !o.Provenance.sameCommit(held.Labels) {
			return "", nil, "", &kxerr.TagTaken{Image: cred.RegistryURL + "/" + repoName, Tag: v, Digest: held.Digest, FromFlag: o.TagFromFlag}
		}
	}
	return r.buildAndPush(ctx, name, repoName, cred, cached)
}

// lookup looks ref up with cred; a cached credential the registry refuses
// is replaced once, as for a push.
func (r *runner) lookup(ctx context.Context, ref string, cred *api.RegistryCredential, cached bool) (registry.Image, *api.RegistryCredential, bool, error) {
	img, err := r.o.Registry.Lookup(ctx, ref, *cred)
	if err == nil || !cached || !errors.Is(err, registry.ErrUnauthorized) {
		return img, cred, cached, err
	}
	r.o.Credentials.Forget()
	fresh, err := r.mint(ctx)
	if err != nil {
		return registry.Image{}, cred, cached, err
	}
	img, err = r.o.Registry.Lookup(ctx, ref, *fresh)
	return img, fresh, false, err
}

// buildAndPush builds the image as name:<tag>, with its provenance as
// labels, and pushes it to repoName under the commit's tag, then gives it
// the version tag. cred, from before the build, is used if it still has
// time left.
func (r *runner) buildAndPush(ctx context.Context, name, repoName string, cred *api.RegistryCredential, cached bool) (string, []string, string, error) {
	o := r.o
	o.OnBuild()
	local := name + ":" + o.Tag
	out := newTail(shownDockerLines)
	start := o.Now()
	spin := r.p.Spinner("Building " + local)
	err := o.Builder.Build(ctx, docker.BuildOptions{Dir: o.Build.Dir, Dockerfile: o.Build.Path, Tag: local, Labels: o.Provenance.labels()}, out)
	spin.Stop()
	if err != nil {
		r.showDocker(err, out)
		return "", nil, "", err
	}
	r.p.Step("Built", local, o.Now().Sub(start))

	start = o.Now()
	if cred == nil || (!cred.ExpiresAt.IsZero() && cred.ExpiresAt.Before(o.Now().Add(credentialMargin))) {
		if cred, cached, err = r.credential(ctx); err != nil {
			return "", nil, "", err
		}
	}
	repo := cred.RegistryURL + "/" + repoName
	pushed := o.Tag
	digest, out, err := r.push(ctx, local, repo, pushed, cred)
	if err != nil && cached && errors.Is(err, docker.ErrUnauthorized) {
		// The cached credential stopped working early: get a new one.
		o.Credentials.Forget()
		if cred, err = r.mint(ctx); err == nil {
			if cred.RegistryURL+"/"+repoName != repo {
				err = errors.New("the registry address changed; run kx deploy again")
			} else {
				digest, out, err = r.push(ctx, local, repo, pushed, cred)
			}
		}
	}
	if err != nil && errors.Is(err, kxerr.ErrTagExists) {
		// Immutable tags: the commit was pushed before (a re-run, or a
		// rebuild with different bytes). Upload under a new tag, once. kx
		// never deploys what the existing tag holds: anyone who can push to
		// the organization's registry could have put it there.
		pushed = o.Tag + "-" + o.Now().UTC().Format("20060102150405")
		r.p.Info("This commit was already pushed; uploaded it as %s.", pushed)
		digest, out, err = r.push(ctx, local, repo, pushed, cred)
	}
	if err != nil {
		r.showDocker(err, out)
		return "", nil, "", err
	}
	if digest == "" {
		// Deploying the tag could run an image someone else pushed under it.
		return "", nil, "", &kxerr.PushFailed{Err: kxerr.ErrNoDigest}
	}
	tags, shown, version := []string{pushed}, repo, ""
	if v := o.VersionTag; v != "" && v != o.Tag {
		held, err := r.version(ctx, repo, v, digest, cred)
		if err != nil {
			return "", nil, "", err
		}
		if held {
			tags, shown, version = append(tags, v), repo+":"+v, v
		}
	}
	r.p.Step("Pushed", shown, o.Now().Sub(start))
	return repo + "@" + digest, tags, version, nil
}

// version gives the image digest, which kx deploys, the version tag tag
// when it can do so without moving it, and reports whether the tag names
// digest afterwards. It always looks first; without an answer it places
// nothing. A free tag is pointed at digest in the registry (no second
// upload), then looked at again, since the registry's tags can change at
// any moment. A tag already naming digest is fine. A tag on another image
// is handled by occupied.
func (r *runner) version(ctx context.Context, repo, tag, digest string, cred *api.RegistryCredential) (bool, error) {
	o := r.o
	ref := repo + ":" + tag
	cur, err := o.Registry.Lookup(ctx, ref, *cred)
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case errors.Is(err, context.Canceled):
		return false, err
	case err != nil:
		return false, &kxerr.TagCheckFailed{Image: repo, Tag: tag, Cause: checkCause(err), Err: err}
	case cur.Digest == digest:
		r.p.Info("%s already holds this image.", ref)
		return true, nil
	case cur.Digest != "":
		return r.occupied(repo, tag, cur, false)
	}
	if err := o.Registry.Tag(ctx, repo, digest, tag, *cred); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Look at what the tag holds now: the request may have landed
		// although its answer was an error, or a registry that keeps its
		// tags refused it because another run got there first.
		after, lerr := o.Registry.Lookup(ctx, ref, *cred)
		switch {
		case lerr == nil && after.Digest == digest:
			return true, nil
		case lerr == nil && after.Digest != "":
			return r.occupied(repo, tag, after, true)
		}
		return false, &kxerr.PushFailed{Err: err}
	}
	after, err := o.Registry.Lookup(ctx, ref, *cred)
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case err == nil && after.Digest == digest:
		return true, nil
	case err == nil && after.Digest != "":
		return r.occupied(repo, tag, after, true)
	}
	r.p.Warn("kx couldn't confirm that %s holds the image it deploys.", ref)
	return true, nil
}

// occupied handles a version tag naming another image than the one kx
// deploys. When that image says it was built from this commit (a re-run,
// or a second run for one tagged push), the tag stays where it is, with a
// warning, and the deploy goes on. Anything else (another commit, or an
// image without the label) stops the deploy; raced says kx had just placed
// the tag when it found another image there.
func (r *runner) occupied(repo, tag string, img registry.Image, raced bool) (bool, error) {
	if r.o.Provenance.sameCommit(img.Labels) {
		what := "The version tag"
		if r.o.TagFromFlag {
			what = "The tag"
		}
		r.p.Warn("%s %s stays on the earlier build of this commit.", what, tag)
		return false, nil
	}
	return false, &kxerr.TagTaken{Image: repo, Tag: tag, Digest: img.Digest, FromFlag: r.o.TagFromFlag, Raced: raced}
}

// checkCause says why a tag lookup failed, for the fix kx suggests.
func checkCause(err error) string {
	switch {
	case errors.Is(err, registry.ErrUnauthorized):
		return kxerr.CheckRefused
	case errors.Is(err, registry.ErrUnavailable):
		return kxerr.CheckUnavailable
	}
	return kxerr.CheckNetwork
}

// push gives the local image the registry name repo:tag and pushes it. It
// returns the digest Docker reported for this push ("" if none) and the end
// of Docker's output, for the caller to show if it gives up.
func (r *runner) push(ctx context.Context, local, repo, tag string, cred *api.RegistryCredential) (string, *tail, error) {
	out := newTail(shownDockerLines)
	ref := repo + ":" + tag
	if err := r.o.Builder.Tag(ctx, local, ref); err != nil {
		if ctx.Err() != nil {
			return "", out, ctx.Err()
		}
		// The built image isn't in the engine (for example a builder that
		// keeps results in its cache): a build problem, shown above the fix.
		r.p.Info("%s", untrusted(err.Error()))
		return "", out, &kxerr.BuildFailed{Err: err}
	}
	spin := r.p.Spinner("Pushing " + repo)
	digest, err := r.o.Builder.Push(ctx, ref, *cred, out)
	spin.Stop()
	return digest, out, err
}

// credential returns a cached credential (laptops) or mints one.
func (r *runner) credential(ctx context.Context) (*api.RegistryCredential, bool, error) {
	if s := r.o.Credentials; s != nil {
		if cred, ok := s.Load(); ok {
			r.o.OnCredential(*cred)
			return cred, true, nil
		}
	}
	cred, err := r.mint(ctx)
	return cred, false, err
}

func (r *runner) mint(ctx context.Context) (*api.RegistryCredential, error) {
	var cred *api.RegistryCredential
	var waited time.Duration
	for {
		var err error
		if cred, err = r.c.RegistryCredentials(ctx); err == nil {
			break
		}
		wait, limited := mintLimited(err)
		if !limited || waited+wait > r.o.MintWait {
			return nil, err
		}
		if err := r.waitToMint(ctx, wait); err != nil {
			return nil, err
		}
		waited += wait
	}
	r.o.OnCredential(*cred) // first: GitHub mode masks it before anything can print it
	if cred.Username == "" || cred.Password == "" || cred.RegistryURL == "" {
		return nil, errors.New("the registry credential from Kubernetix is incomplete")
	}
	if s := r.o.Credentials; s != nil {
		if err := s.Save(*cred); err != nil {
			r.p.Warn("kx couldn't save the registry credential for next time: %s", err)
		}
	}
	return cred, nil
}

// mintLimited reports the registry credential limit (429) and the wait the
// answer's Retry-After asks for; an answer without one isn't waited out.
func mintLimited(err error) (time.Duration, bool) {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 429 || apiErr.RetryAfter <= 0 {
		return 0, false
	}
	return apiErr.RetryAfter, true
}

// waitToMint waits out the registry credential limit, saying so and
// showing progress (heartbeat lines in CI).
func (r *runner) waitToMint(ctx context.Context, wait time.Duration) error {
	r.p.Info("This token has uploaded images too often in the last hour, so kx waits %s before uploading.", ui.Duration(wait))
	spin := r.p.Spinner("Waiting to upload")
	defer spin.Stop()
	return r.o.Sleep(ctx, wait)
}

// showDocker prints the end of Docker's output when a Docker step failed
// (not when kx was interrupted).
func (r *runner) showDocker(err error, out *tail) {
	var bf *kxerr.BuildFailed
	var pf *kxerr.PushFailed
	if !errors.As(err, &bf) && !errors.As(err, &pf) {
		return
	}
	r.showUntrusted(out.Lines(), func(l string) { r.p.Info("%s", l) })
}

// failed explains a deployment that did not go live: the platform's
// reason, the last 20 log lines, and the version still serving.
// When kx saw why its containers weren't getting ready, the log lines are
// the stuck replicas' and the error carries that cause.
func (r *runner) failed(ctx context.Context, app *api.App, d *api.Deployment) *kxerr.DeployFailed {
	r.showLogs(ctx, app, d)
	reason := d.CustomerError
	if reason == "" && d.Status == api.StatusRolledBack {
		reason = "Kubernetix rolled it back"
	}
	serving, restoredAs := r.serving(ctx, app, d)
	return &kxerr.DeployFailed{App: app.Name, Version: d.Version, ServingVersion: serving, RestoredAs: restoredAs, Reason: reason, Code: d.FailureClass(), NotStarting: r.likely}
}

// deploymentLogs returns the last 20 lines of d's build and rollout logs,
// without probe and scraper noise, and whether kx could read them.
func (r *runner) deploymentLogs(ctx context.Context, d *api.Deployment) ([]string, bool) {
	logs, err := r.c.DeploymentLogs(ctx, d.ID)
	if err != nil {
		return nil, false
	}
	all := lastLines(logs.BuildLogs+"\n"+logs.DeploymentLogs, 1<<20)
	return lastLines(joinLines(withoutNoise(all)), shownLogLines), true
}

// serving returns the version still serving after d failed, 0 if none.
func (r *runner) serving(ctx context.Context, app *api.App, d *api.Deployment) (version, restoredAs int) {
	deps, err := r.c.ListDeployments(ctx, app.ID, 20)
	if err != nil {
		return 0, 0
	}
	for _, other := range deps {
		if other.Status != api.StatusReady || other.ID == d.ID {
			continue
		}
		// Kubernetix may record putting the earlier version back as a new
		// version: name the version it put back.
		if rb := other.Rollback; rb != nil && rb.Automatic && rb.FailedDeploymentID == d.ID && rb.TargetVersion > 0 {
			return rb.TargetVersion, other.Version
		}
		return other.Version, 0
	}
	return 0, 0
}

func joinLines(lines []string) string { return strings.Join(lines, "\n") }

func lastLines(s string, n int) []string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// showUntrusted prints lines kx didn't write (build output, app logs) so
// they can't act as CI workflow commands: each line is cleaned and
// neutralised, and in GitHub Actions the whole block sits inside a
// ::stop-commands:: block with a fresh random token.
func (r *runner) showUntrusted(lines []string, print func(string)) {
	if len(lines) == 0 {
		return
	}
	for i := range lines {
		lines[i] = untrusted(lines[i])
	}
	if r.o.StopCommands {
		tok := safetext.StopToken(lines)
		r.p.Info("::stop-commands::%s", tok)
		defer r.p.Info("::%s::", tok)
	}
	for _, l := range lines {
		print(l)
	}
}

// untrusted makes a line of build or app output safe to print: no control
// characters, and nothing a CI runner would read as a workflow command.
func untrusted(s string) string { return safetext.Neutralize(cleanText(s)) }

// cleanText drops control characters (keeping tabs), so build and app
// output can't move the cursor or retitle the terminal.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}
