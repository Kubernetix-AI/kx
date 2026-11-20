package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/deploy"
	"github.com/kubernetix-ai/kx/internal/docker"
	"github.com/kubernetix-ai/kx/internal/github"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
	"github.com/kubernetix-ai/kx/internal/uuid"
	"github.com/kubernetix-ai/kx/internal/version"
)

func init() { register(newDeployCmd) }

type deployOpts struct {
	app         string
	image       string
	build       string
	dockerfile  string
	timeout     string
	environment string
	noWait      bool
	github      bool
}

func newDeployCmd(a *App) *cobra.Command {
	var o deployOpts
	cmd := &cobra.Command{
		Use:     "deploy",
		Short:   "Build this folder and put it live",
		GroupID: groupStart,
		Long: `Build this folder's Dockerfile for linux/amd64, push the image, deploy it and wait
until it's live. The URL comes last, alone on standard output, so URL=$(kx deploy) works.
The first run logs in if needed and saves the app the folder deploys to in .kx.json,
asking which one only when there's a choice.`,
		Example: `  kx deploy                          build, push, deploy, wait, print the URL
  kx deploy --app web                deploy this folder to another app (or create it)
  kx deploy --image registry.kubernetix.ai/acme-corp/shop-api@sha256:...   skip the build
  kx deploy --build ./api --dockerfile ./api/Dockerfile.prod
  kx deploy --no-wait                start the deploy and return`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploy(cmd.Context(), a, cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.app, "app", "", "the `app` (default: the one this folder is linked to)")
	f.StringVar(&o.image, "image", "", "deploy this `image`, already pushed, instead of building one")
	f.StringVar(&o.build, "build", ".", "the `folder` to build")
	f.StringVar(&o.dockerfile, "dockerfile", "", "the `Dockerfile` to build with (default: the one in the build folder)")
	f.BoolVar(&o.noWait, "no-wait", false, "start the deploy and return without waiting for it to go live")
	f.StringVar(&o.timeout, "timeout", defaultTimeout, "how long to wait for the deploy to go live, like 18m or `1h`")
	f.BoolVar(&o.github, "github", false, "report to GitHub: commit status, job summary and step outputs (the Action uses this)")
	f.StringVar(&o.environment, "environment", "production", "with --github, the `name` in the commit status \"Kubernetix / <name>\"")
	return cmd
}

// reportBudget bounds the time kx spends telling GitHub the outcome after
// the job was cancelled: the runner follows its interrupt with a
// termination signal about 7.5 seconds later.
const reportBudget = 5 * time.Second

// defaultTimeout is --timeout's default, deploy.DefaultTimeout in minutes.
var defaultTimeout = fmt.Sprintf("%dm", deploy.DefaultTimeout/time.Minute)

// ciMintWait is how long kx waits in CI for the registry credential limit
// (one credential every 6 minutes per token once its burst is used): in CI
// nothing would run the deploy again, so waiting beats failing the job.
const ciMintWait = 6*time.Minute + 30*time.Second

var (
	imageRefRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,511}$`)
	environmentRe = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,100}$`)
)

func runDeploy(ctx context.Context, a *App, cmd *cobra.Command, o deployOpts) error {
	env := ci.Detect(a.Getenv)
	// GitHub mode starts first, so even a mistake in how kx was called
	// shows on the run page; outside Actions it is refused, so workflow
	// commands never reach an ordinary terminal.
	var gh *github.Reporter
	if o.github {
		if env.Provider != ci.GitHub {
			return &kxerr.Usage{What: "kx deploy --github only works inside a GitHub Actions job.", Fix: "Run: kx deploy"}
		}
		ua := api.UserAgent(version.Get().Version, env.Name)
		if strings.TrimSpace(o.environment) == "" || !environmentRe.MatchString(o.environment) {
			err := &kxerr.Usage{What: "--environment needs a name of up to 100 characters on one line.", Fix: "Run: kx deploy --github --environment production"}
			// No status: its context would name an environment nobody asked for.
			reportEarly(ctx, github.New(a.Getenv, printerLines{a.printer()}, "production", ua), env, err, false)
			return err
		}
		gh = github.New(a.Getenv, printerLines{a.printer()}, o.environment, ua)
	}
	fail := func(err error) error {
		if gh != nil {
			reportEarly(ctx, gh, env, err, !env.SkipsDeploy())
		}
		return err
	}

	timeout, err := parseTimeout(o.timeout)
	if err != nil {
		return fail(err)
	}
	if o.image != "" {
		if cmd.Flags().Changed("build") || cmd.Flags().Changed("dockerfile") {
			return fail(&kxerr.Usage{What: "--image deploys an image that's already built, so it can't be used with --build or --dockerfile.", Fix: "Run: kx deploy --image <image>"})
		}
		if !imageRefRe.MatchString(o.image) {
			return fail(&kxerr.Usage{What: fmt.Sprintf("%q isn't an image name.", o.image), Fix: "Run: kx deploy --image registry.kubernetix.ai/<org>/<app>@sha256:<digest>"})
		}
	}

	// Pull and merge request runs never deploy; say so before needing a
	// token, since forks get no secrets. Every other run deploys.
	if env.EventUnknown() {
		return fail(&kxerr.Usage{
			What: "GITHUB_EVENT_NAME is empty, so kx can't tell whether this run is for a pull request, and it doesn't deploy.",
			Fix:  "Run kx deploy in a workflow step, where GitHub sets GITHUB_EVENT_NAME.",
		})
	}
	if env.SkipsDeploy() {
		p := a.printer()
		if a.jsonOut {
			return p.PrintJSON(struct {
				Skipped bool   `json:"skipped"`
				Reason  string `json:"reason"`
			}{true, ci.SkipMessage})
		}
		p.Info("%s", ci.SkipMessage)
		return nil
	}

	s, err := a.session(cmd, o.app)
	if err != nil {
		return fail(err)
	}
	d := &deployRun{a: a, s: s, o: o, gh: gh, timeout: timeout, start: a.Now()}
	res, err := d.run(ctx)
	if d.gh != nil {
		d.report(ctx, res, err)
	}
	if err != nil {
		return err
	}
	return d.print(res)
}

// reportEarly tells GitHub about a failure before kx got as far as a
// deploy: the ::error:: annotation, a failure status when status is true,
// and a summary saying nothing was deployed.
func reportEarly(ctx context.Context, gh *github.Reporter, env ci.Env, err error, status bool) {
	prob := api.Explain(err)
	gh.Error(prob.What, prob.Fix)
	if status {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportBudget)
		defer cancel()
		_ = gh.SetStatus(sctx, github.StateFailure, prob.What, "")
	}
	if werr := gh.WriteSummary(github.Summary{Failed: true, What: prob.What, Fix: prob.Fix, Commit: env.CommitSHA,
		Note: "Nothing was deployed, so what was live before is still live."}); werr != nil {
		gh.Warning("kx couldn't write the job summary: " + werr.Error())
	}
}

func parseTimeout(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < 10*time.Second || d > 24*time.Hour {
		return 0, &kxerr.Usage{What: "kx deploy --timeout takes a length of time from 10s to 24h, like 18m or 1h.", Fix: "Run: kx deploy --timeout 20m"}
	}
	return d, nil
}

// deployRun is one kx deploy.
type deployRun struct {
	a       *App
	s       *Session
	o       deployOpts
	gh      *github.Reporter
	timeout time.Duration
	start   time.Time
	appName string
}

func (d *deployRun) run(ctx context.Context) (*deploy.Result, error) {
	a, s, o := d.a, d.s, d.o
	builder := a.docker()

	// Local checks first: no API call before kx knows it can build.
	var df *deploy.Dockerfile
	var tag string
	if o.image == "" {
		var err error
		df, err = deploy.Preflight(ctx, builder, d.path(o.build), d.dockerfilePath())
		var none *kxerr.NoDockerfile
		if errors.As(err, &none) {
			if err := d.writeDockerfile(ctx, none.Dir); err != nil {
				return nil, err
			}
			df, err = deploy.Preflight(ctx, builder, d.path(o.build), d.dockerfilePath())
		}
		if err != nil {
			return nil, err
		}
		// Before the picker writes .kx.json, which would count as a change.
		tag = deploy.ImageTag(ctx, s.CI, df.Dir, a.Now())
	}
	c, err := s.Client(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.checkPermissions(ctx); err != nil {
		return nil, err
	}
	app, newApp, err := d.target(ctx)
	if err != nil {
		return nil, err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return nil, err
	}
	d.appName = newApp
	if app != nil {
		d.appName = app.Name
	}
	d.linkNamedApp(app, org)
	key := s.CI.IdempotencyKey(d.appName)
	if key == "" {
		if key, err = uuid.V4(); err != nil {
			return nil, err
		}
	}

	opts := deploy.Options{
		App: app, NewApp: newApp, Org: org,
		Image: o.image, Build: df, Tag: tag,
		IdempotencyKey: key, NoWait: o.noWait, Timeout: d.timeout,
		Builder: builder, Now: a.Now, Sleep: a.Sleep,
		StopCommands: s.CI.Provider == ci.GitHub,
	}
	if s.CI.IsCI {
		opts.MintWait = ciMintWait
	} else if s.P.InTTY && s.P.TTY {
		opts.Ask, opts.Confirm = s.P.Ask, s.P.Confirm
	}
	// Only a login on this computer gets a cache: a KX_TOKEN usually means
	// CI, including systems kx doesn't recognise, where other jobs may
	// share the user's cache folder.
	if o.image == "" && !s.CI.IsCI && s.Cfg.TokenSource == kxerr.SourceLogin {
		if dir, err := config.CacheDir(a.Getenv); err == nil {
			opts.Credentials = docker.NewCredentialCache(dir, s.Cfg.APIURL, s.Cfg.Token, a.Now)
		}
	}
	if gh := d.gh; gh != nil {
		opts.OnCredential = gh.MaskCredential
		opts.OnBuild = func() {
			target := ""
			if app != nil {
				target = s.DashboardURL("/apps/" + url.PathEscape(app.ID))
			}
			_ = gh.SetStatus(ctx, github.StatePending, "Building", target)
		}
		opts.OnDeploy = func(app *api.App, dep *api.Deployment) {
			_ = gh.SetStatus(ctx, github.StatePending, fmt.Sprintf("Deploying v%d", dep.Version), s.DashboardURL("/apps/"+url.PathEscape(app.ID)))
		}
		// A likely cause while kx keeps waiting is a warning: the deploy
		// may still go live.
		opts.OnNotLive = func(what, fix string) { gh.Warning(what + "\n" + fix) }
	}

	res, err := deploy.Run(ctx, c, opts, *s.P)
	if res != nil && res.CreatedApp {
		d.rememberAppID(res)
	}
	return res, err
}

// checkPermissions stops before building when /whoami says the token
// can't do what this deploy needs: read the app and its deployments,
// deploy, and, when kx builds, push the image.
func (d *deployRun) checkPermissions(ctx context.Context) error {
	who, err := d.s.WhoAmI(ctx)
	if err != nil {
		return err
	}
	t := who.Token
	if t == nil {
		return nil // not a token: Kubernetix decides
	}
	need := []string{api.PermDeploy, api.PermRead}
	if d.o.image == "" {
		need = append(need, api.PermPush)
	}
	var missing []string
	for _, p := range need {
		if !t.Can(p) {
			missing = append(missing, p)
		}
	}
	switch {
	case len(missing) == 0:
		return nil
	case len(missing) == 1 && missing[0] == api.PermPush:
		return kxerr.ErrNoPushPermission
	}
	return &kxerr.TokenLacks{Permissions: missing, Source: d.s.Cfg.TokenSource, Org: who.Org.ID}
}

// ownBuild reports whether kx builds the working folder's own Dockerfile,
// as a plain kx deploy there does: no --dockerfile, and --build is the
// folder itself.
func (d *deployRun) ownBuild() bool {
	return d.o.dockerfile == "" && d.path(d.o.build) == filepath.Clean(d.s.Cfg.Dir)
}

// path resolves a --build or --dockerfile value against the working folder.
func (d *deployRun) path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(d.s.Cfg.Dir, p)
}

// dockerfilePath resolves --dockerfile like docker build -f, from the
// working folder; when nothing is there but the build folder has it, that
// one is used (the Action describes its dockerfile input that way).
func (d *deployRun) dockerfilePath() string {
	p := d.o.dockerfile
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	fromWD := d.path(p)
	if _, err := os.Stat(fromWD); err != nil {
		if inBuild := filepath.Join(d.path(d.o.build), p); fileExists(inBuild) {
			return inBuild
		}
	}
	return fromWD
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// target finds the app to deploy: the linked or named app, the token's
// app, a new app (an org token deploying a folder linked to "(new app)",
// or naming one that doesn't exist yet and saying to create it), or, on
// the first run in a folder, the one the person picks.
func (d *deployRun) target(ctx context.Context) (*api.App, string, error) {
	s := d.s
	app, err := s.ResolveApp(ctx)
	if err == nil {
		return app, "", nil
	}
	var apiErr *api.Error
	notFound := errors.As(err, &apiErr) && apiErr.Code == "app_not_found"
	switch {
	case errors.Is(err, kxerr.ErrNotLinked):
		return d.firstRun(ctx)
	case notFound && (d.linkedToNewApp() || d.namedApp()):
		who, werr := s.WhoAmI(ctx)
		if werr != nil {
			return nil, "", werr
		}
		if t := who.Token; t != nil && (t.AppScoped() || !t.Can(api.PermDeploy)) {
			return nil, "", err
		}
		if d.linkedToNewApp() {
			return nil, s.Cfg.App, nil
		}
		return d.offerNewApp(ctx, err)
	}
	return nil, "", err
}

// namedApp reports whether the app was named by --app or KX_APP (as an
// app name kx could create).
func (d *deployRun) namedApp() bool {
	cfg := d.s.Cfg
	return (cfg.AppSource == config.SourceFlag || cfg.AppSource == config.SourceEnv) && config.ValidAppName(cfg.App)
}

// offerNewApp asks whether to create the app that --app or KX_APP names
// and that doesn't exist (notFound says so). The name was typed, maybe
// mistyped, so the default is no: Enter alone creates nothing. In CI it
// stays an error; without a terminal the fix says how to create it.
func (d *deployRun) offerNewApp(ctx context.Context, notFound error) (*api.App, string, error) {
	s, name := d.s, d.s.Cfg.App
	if s.CI.IsCI {
		return nil, "", notFound
	}
	what := api.Explain(notFound).What // There's no app called web in acme-corp.
	if !s.P.InTTY || !s.P.TTY {
		return nil, "", &kxerr.Usage{What: what, Fix: "Create it: kx link " + name + ", then kx deploy"}
	}
	yes, err := s.P.Confirm(ctx, what+" Create it?")
	if err != nil {
		return nil, "", err
	}
	if !yes {
		return nil, "", &kxerr.Usage{What: "Nothing was deployed: " + strings.ToLower(what[:1]) + what[1:], Fix: "See your apps: kx apps list"}
	}
	return nil, name, nil
}

// linkNamedApp saves the app named by --app in .kx.json when the folder
// has none, so the next kx deploy there needs no --app. Not in CI (the
// checkout is thrown away), not for --image, which names no project
// folder, and not when --build or --dockerfile builds something other
// than the folder's own Dockerfile, which a plain kx deploy would build
// next time. A new app's ID is added once it exists (rememberAppID). Not
// being able to save it is only a warning: nobody asked for the file.
func (d *deployRun) linkNamedApp(app *api.App, org string) {
	s := d.s
	if s.Cfg.AppSource != config.SourceFlag || s.Cfg.Project != nil || s.CI.IsCI || d.o.image != "" || !d.ownBuild() {
		return
	}
	proj := config.Project{App: d.appName, Org: org}
	if app != nil {
		proj.AppID = app.ID
	}
	if _, err := config.SaveProject(s.Cfg.Dir, proj); err != nil {
		s.P.Warn("kx couldn't save %s in .kx.json: %s.", d.appName, fileErrReason(err))
		return
	}
	s.P.Info("Saved in .kx.json. Next time, just run kx deploy.")
	s.P.Info("")
}

// linkedToNewApp reports whether the app comes from a .kx.json that kx
// link wrote for an app that didn't exist yet (it has no app_id).
func (d *deployRun) linkedToNewApp() bool {
	cfg := d.s.Cfg
	return cfg.AppSource == config.SourceProject && cfg.Project != nil && cfg.Project.AppID == "" && cfg.Project.App == cfg.App
}

// firstRun finds the app this folder deploys to, asking when there is a
// choice, and saves it in .kx.json. In CI it never chooses: a fresh
// checkout without .kx.json would otherwise create an app.
func (d *deployRun) firstRun(ctx context.Context) (*api.App, string, error) {
	s := d.s
	if s.CI.IsCI {
		return nil, "", &kxerr.Usage{
			What: "This folder isn't linked to an app yet, and kx can't ask which one here.",
			Fix:  "Pass the app with --app <app> (the Action's app input), or commit the .kx.json that kx link writes",
		}
	}
	choice, err := chooseApp(ctx, s, "", "deploy", s.P.InTTY && s.P.TTY)
	if err != nil {
		return nil, "", err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return nil, "", err
	}
	proj := config.Project{App: choice.newApp, Org: org}
	if choice.app != nil {
		proj.App, proj.AppID = choice.app.Name, choice.app.ID
	}
	if _, err := config.SaveProject(s.Cfg.Dir, proj); err != nil {
		return nil, "", fmt.Errorf("saving .kx.json: %w", err)
	}
	switch {
	case choice.unasked && choice.newApp != "":
		s.P.Info("Creating a new app called %s. Saved in .kx.json.", choice.newApp)
	case !d.ownBuild():
		// A plain kx deploy would build the folder's own Dockerfile.
		s.P.Info("Saved in .kx.json.")
	default:
		s.P.Info("Saved in .kx.json. Next time, just run kx deploy.")
	}
	s.P.Info("")
	return choice.app, choice.newApp, nil
}

// rememberAppID adds a newly created app's ID to the folder's .kx.json,
// on laptops only (CI checkouts are thrown away).
func (d *deployRun) rememberAppID(res *deploy.Result) {
	s := d.s
	if s.CI.IsCI {
		return
	}
	path, p, err := config.FindProject(s.Cfg.Dir)
	if err != nil || p == nil || p.App != res.App || p.AppID != "" {
		return
	}
	p.AppID = res.AppID
	if _, err := config.SaveProject(filepath.Dir(path), *p); err != nil {
		s.P.Warn("kx couldn't save %s's ID in .kx.json: %s", res.App, err)
	}
}

type deployJSON struct {
	App          string `json:"app"`
	AppID        string `json:"app_id"`
	Org          string `json:"org"`
	URL          string `json:"url"`
	DeploymentID string `json:"deployment_id"`
	Version      int    `json:"version"`
	Status       string `json:"status"`
	Image        string `json:"image"`
	CreatedApp   bool   `json:"created_app"`
}

func (d *deployRun) print(res *deploy.Result) error {
	p := d.s.P
	if d.a.jsonOut {
		return p.PrintJSON(deployJSON{res.App, res.AppID, res.Org, res.URL, res.DeploymentID, res.Version, res.Status, res.Image, res.CreatedApp})
	}
	switch {
	case res.URL != "":
		p.Result(res.URL)
	case res.Status == "started":
		p.Info("Check on it: kx status")
	default:
		p.Warn("%s is live, but Kubernetix hasn't given it a URL yet.", res.App)
		p.Info("Check on it: kx status")
	}
	return nil
}

// report tells GitHub how the deploy went: the commit status, the job
// summary, the step outputs and, on failure, an ::error:: annotation.
func (d *deployRun) report(ctx context.Context, res *deploy.Result, err error) {
	gh, s := d.gh, d.s
	took := d.a.Now().Sub(d.start)
	sum := github.Summary{App: d.appName, Commit: s.CI.CommitSHA, Took: took}
	var out github.Outputs
	dashboard := ""
	if res != nil {
		sum.App, sum.Version, sum.URL, sum.Image = res.App, res.Version, res.URL, res.Image
		if res.Version > 0 {
			out = github.Outputs{URL: res.URL, DeploymentID: res.DeploymentID, Version: fmt.Sprintf("v%d", res.Version), Image: res.Image}
		}
		if res.AppID != "" {
			dashboard = s.DashboardURL("/apps/" + url.PathEscape(res.AppID))
		}
	}
	// The job may be cancelled; GitHub should still hear the outcome.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportBudget)
	defer cancel()

	if res != nil && res.Replayed {
		sum.Note = fmt.Sprintf("This run already deployed v%d, so nothing new was started.", res.Version)
	}
	if err == nil {
		desc, target := "Live in "+ui.Duration(took), res.URL
		if took < time.Second {
			desc = "Live in 0s"
		}
		if res.Status == "started" {
			desc, target, sum.Started = fmt.Sprintf("Started v%d; kx didn't wait for it to go live", res.Version), dashboard, true
		}
		if target == "" {
			target = dashboard
		}
		_ = gh.SetStatus(sctx, github.StateSuccess, desc, target)
		d.writeReport(sum, out)
		return
	}

	prob := api.Explain(err)
	gh.Error(prob.What, prob.Fix)
	_ = gh.SetStatus(sctx, github.StateFailure, prob.What, dashboard)
	sum.Failed, sum.What, sum.Fix = true, prob.What, prob.Fix
	var failed *kxerr.DeployFailed
	switch {
	case res == nil || res.Version == 0:
		sum.Note = "Nothing was deployed, so what was live before is still live."
	case errors.As(err, &failed) && failed.ServingVersion == 0:
		sum.Note = "No earlier version of " + failed.App + " is serving."
	}
	d.writeReport(sum, out)
}

func (d *deployRun) writeReport(sum github.Summary, out github.Outputs) {
	if err := d.gh.WriteSummary(sum); err != nil {
		d.gh.Warning("kx couldn't write the job summary: " + err.Error())
	}
	if err := d.gh.SetOutputs(out); err != nil {
		d.gh.Warning("kx couldn't set the step outputs: " + err.Error())
	}
}

// printerLines writes workflow commands through the printer, one line at
// a time, so they never interleave with progress lines.
type printerLines struct{ p *ui.Printer }

func (w printerLines) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		w.p.Info("%s", line)
	}
	return len(b), nil
}
