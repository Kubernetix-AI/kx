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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/docker"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/safetext"
	"github.com/kubernetix-ai/kx/internal/ui"
)

// DefaultTimeout is how long kx waits for a deploy to go live.
const DefaultTimeout = 15 * time.Minute

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
	// Preflight and Tag from ImageTag.
	Build *Dockerfile
	Tag   string

	// IdempotencyKey is sent with the deployment request and reused on
	// every retry, so a retry never deploys twice.
	IdempotencyKey string
	NoWait         bool
	Timeout        time.Duration // 0 means DefaultTimeout

	Builder     Builder
	Credentials CredentialStore // nil: mint a new credential (always in CI)

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// OnCredential sees every registry credential before anything else
	// does (GitHub mode masks it). OnBuild runs before the build and
	// OnDeploy once the deployment exists.
	OnCredential func(api.RegistryCredential)
	OnBuild      func()
	OnDeploy     func(app *api.App, d *api.Deployment)

	// StopCommands wraps every block of untrusted output (Docker's, the
	// app's logs) in a GitHub ::stop-commands:: block. Set it whenever kx
	// runs in GitHub Actions, in any mode: the runner reads every line.
	StopCommands bool
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
	switch {
	case o.IdempotencyKey == "":
		return errors.New("deploy: an idempotency key is required")
	case (o.App == nil) == (o.NewApp == ""):
		return errors.New("deploy: give exactly one of App and NewApp")
	case o.NewApp != "" && !config.ValidAppName(o.NewApp):
		return fmt.Errorf("deploy: %q is not an app name", o.NewApp)
	case o.Image == "" && (o.Build == nil || o.Builder == nil):
		return errors.New("deploy: building needs Build and Builder")
	case o.Image == "" && !tagRe.MatchString(o.Tag):
		return fmt.Errorf("deploy: %q is not an image tag", o.Tag)
	}
	return nil
}

type runner struct {
	c *api.Client
	o Options
	p *ui.Printer
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

	port := r.check(app)
	if app != nil {
		if err := r.waitForPrevious(ctx, app); err != nil {
			return nil, err
		}
	}
	if err := r.deploysLeftBefore(ctx); err != nil {
		return nil, err
	}

	image := o.Image
	if image == "" {
		built, err := r.buildAndPush(ctx, res.App)
		if err != nil {
			return nil, err
		}
		image = built
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
	}

	if !res.CreatedApp && o.Image == "" {
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

// check prints the Checked line and port warnings, and returns the port a
// new app gets.
func (r *runner) check(app *api.App) int {
	o := r.o
	name := o.NewApp
	if app != nil {
		name = app.Name
	}
	if o.Image != "" {
		r.p.Step("Checked", "image "+o.Image, 0)
		if app == nil {
			r.p.Warn("kx can't see which port the image listens on, so %s will send traffic to port %d.", name, defaultPort)
			return defaultPort
		}
		return app.Port
	}
	exposed := o.Build.Port
	port := exposed
	if app != nil && app.Port > 0 {
		port = app.Port
	}
	if port == 0 {
		port = defaultPort
		if app == nil {
			r.p.Warn("Your Dockerfile has no EXPOSE line, so kx assumes your app listens on port %d.", defaultPort)
		}
	}
	if app != nil && exposed > 0 && app.Port > 0 && exposed != app.Port {
		r.p.Warn("The Dockerfile exposes port %d, but %s listens on port %d.", exposed, name, app.Port)
		r.p.Note("Kubernetix sends traffic to port %d; make your app listen there, or change the app's port in the dashboard.", app.Port)
	}
	if exposed > 0 && exposed < 1024 {
		r.p.Warn("Your Dockerfile exposes port %d. Apps on Kubernetix don't run as root, so they may not be able to listen on ports below 1024.", exposed)
	}
	r.p.Step("Checked", fmt.Sprintf("%s, port %d", filepath.Base(o.Build.Path), port), 0)
	return port
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
// plan and its limit.
func (r *runner) deployLimit(ctx context.Context, err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "tier_limit_exceeded" || apiErr.LimitName != "max_deployments_per_day" {
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

func (r *runner) buildAndPush(ctx context.Context, name string) (string, error) {
	o := r.o
	o.OnBuild()
	local := name + ":" + o.Tag
	out := newTail(shownDockerLines)
	start := o.Now()
	spin := r.p.Spinner("Building " + local)
	err := o.Builder.Build(ctx, docker.BuildOptions{Dir: o.Build.Dir, Dockerfile: o.Build.Path, Tag: local}, out)
	spin.Stop()
	if err != nil {
		r.showDocker(err, out)
		return "", err
	}
	r.p.Step("Built", local, o.Now().Sub(start))

	start = o.Now()
	cred, cached, err := r.credential(ctx)
	if err != nil {
		return "", err
	}
	repo := cred.RegistryURL + "/" + name
	digest, out, err := r.push(ctx, local, repo, o.Tag, cred)
	if err != nil && cached && errors.Is(err, docker.ErrUnauthorized) {
		// The cached credential stopped working early: get a new one.
		o.Credentials.Forget()
		if cred, err = r.mint(ctx); err == nil {
			if cred.RegistryURL+"/"+name != repo {
				err = errors.New("the registry address changed; run kx deploy again")
			} else {
				digest, out, err = r.push(ctx, local, repo, o.Tag, cred)
			}
		}
	}
	if err != nil && errors.Is(err, kxerr.ErrTagExists) {
		// Immutable tags: the commit was pushed before (a re-run, or a
		// rebuild with different bytes). Upload under a new tag, once. kx
		// never deploys what the existing tag holds: anyone who can push to
		// the organization's registry could have put it there.
		tag := o.Tag + "-" + o.Now().UTC().Format("20060102150405")
		r.p.Info("This commit was already pushed; uploaded it as %s.", tag)
		digest, out, err = r.push(ctx, local, repo, tag, cred)
	}
	if err != nil {
		r.showDocker(err, out)
		return "", err
	}
	if digest == "" {
		// Deploying the tag could run an image someone else pushed under it.
		return "", &kxerr.PushFailed{Err: kxerr.ErrNoDigest}
	}
	r.p.Step("Pushed", repo, o.Now().Sub(start))
	return repo + "@" + digest, nil
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
	cred, err := r.c.RegistryCredentials(ctx)
	if err != nil {
		return nil, err
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
func (r *runner) failed(ctx context.Context, app *api.App, d *api.Deployment) *kxerr.DeployFailed {
	if logs, err := r.c.DeploymentLogs(ctx, d.ID); err == nil {
		lines := lastLines(logs.BuildLogs+"\n"+logs.DeploymentLogs, shownLogLines)
		if len(lines) > 0 {
			r.p.Info("The last %d log lines of v%d:", len(lines), d.Version)
			r.showUntrusted(lines, func(l string) { r.p.Note("%s", l) })
		}
	}
	serving := 0
	if deps, err := r.c.ListDeployments(ctx, app.ID, 20); err == nil {
		for _, other := range deps {
			if other.Status == api.StatusReady && other.ID != d.ID {
				serving = other.Version
				break
			}
		}
	}
	reason := d.CustomerError
	if reason == "" && d.Status == api.StatusRolledBack {
		reason = "Kubernetix rolled it back"
	}
	return &kxerr.DeployFailed{App: app.Name, Version: d.Version, ServingVersion: serving, Reason: reason}
}

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
		tok := stopToken(lines)
		r.p.Info("::stop-commands::%s", tok)
		defer r.p.Info("::%s::", tok)
	}
	for _, l := range lines {
		print(l)
	}
}

// stopToken is a random token that occurs in none of lines.
func stopToken(lines []string) string {
	for {
		b := make([]byte, 16)
		_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
		tok := hex.EncodeToString(b)
		if !strings.Contains(strings.Join(lines, "\n"), tok) {
			return tok
		}
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
