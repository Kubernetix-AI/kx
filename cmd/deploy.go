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
The first run asks which app the folder deploys to and saves it in .kx.json.`,
		Example: `  kx deploy                          build, push, deploy, wait, print the URL
  kx deploy --app web                deploy this folder to another app
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
	f.StringVar(&o.timeout, "timeout", "15m", "how long to wait for the deploy to go live, like 15m or `1h`")
	f.BoolVar(&o.github, "github", false, "report to GitHub: commit status, job summary and step outputs (the Action uses this)")
	f.StringVar(&o.environment, "environment", "production", "with --github, the `name` in the commit status \"Kubernetix / <name>\"")
	return cmd
}

// reportBudget bounds the time kx spends telling GitHub the outcome after
// the job was cancelled: the runner follows its interrupt with a
// termination signal about 7.5 seconds later.
const reportBudget = 5 * time.Second

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
		return 0, &kxerr.Usage{What: "kx deploy --timeout takes a length of time from 10s to 24h, like 15m or 1h.", Fix: "Run: kx deploy --timeout 20m"}
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
		if df, err = deploy.Preflight(ctx, builder, d.path(o.build), d.dockerfilePath()); err != nil {
			return nil, err
		}
		// Before the picker writes .kx.json, which would count as a change.
		tag = deploy.ImageTag(ctx, s.CI, df.Dir, a.Now())
	}
	c, err := s.Client()
	if err != nil {
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
	}

	res, err := deploy.Run(ctx, c, opts, *s.P)
	if res != nil && res.CreatedApp {
		d.rememberAppID(res)
	}
	return res, err
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
// app, a new app (an org token deploying a folder linked to "(new app)"),
// or, on the first run in a folder, the one the person picks.
func (d *deployRun) target(ctx context.Context) (*api.App, string, error) {
	s := d.s
	app, err := s.ResolveApp(ctx)
	if err == nil {
		return app, "", nil
	}
	var apiErr *api.Error
	switch {
	case errors.Is(err, kxerr.ErrNotLinked):
		return d.firstRun(ctx)
	case errors.As(err, &apiErr) && apiErr.Code == "app_not_found" && d.linkedToNewApp():
		who, werr := s.WhoAmI(ctx)
		if werr != nil {
			return nil, "", werr
		}
		if t := who.Token; t != nil && (t.AppScoped() || !t.Can(api.PermDeploy)) {
			return nil, "", err
		}
		return nil, s.Cfg.App, nil
	}
	return nil, "", err
}

// linkedToNewApp reports whether the app comes from a .kx.json that kx
// link wrote for an app that didn't exist yet (it has no app_id).
func (d *deployRun) linkedToNewApp() bool {
	cfg := d.s.Cfg
	return cfg.AppSource == config.SourceProject && cfg.Project != nil && cfg.Project.AppID == "" && cfg.Project.App == cfg.App
}

// firstRun asks which app this folder deploys to and saves the answer.
func (d *deployRun) firstRun(ctx context.Context) (*api.App, string, error) {
	s := d.s
	if !s.P.InTTY || !s.P.TTY {
		fix := "Run: kx deploy --app <app>"
		if s.CI.IsCI {
			fix = "Pass the app with --app <app> (the Action's app input), or commit the .kx.json that kx link writes"
		}
		return nil, "", &kxerr.Usage{What: "This folder isn't linked to an app yet, and kx can't ask which one here.", Fix: fix}
	}
	choice, err := chooseApp(ctx, s, "", "deploy")
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
	s.P.Info("Saved in .kx.json. Next time, just run kx deploy.")
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
