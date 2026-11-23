package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/deploy"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/uuid"
)

// configureFlags are the flags of the commands that change an app's
// configuration (kx env set and unset, kx scale).
type configureFlags struct {
	app      string
	noDeploy bool
	noWait   bool
	timeout  string
}

func addConfigureFlags(cmd *cobra.Command, f *configureFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.app, "app", "", "the `app` (default: the one this folder is linked to)")
	fl.BoolVar(&f.noDeploy, "no-deploy", false, "only save the change; the app's next deploy applies it")
	fl.BoolVar(&f.noWait, "no-wait", false, "start the deploy and return without waiting for it to go live")
	fl.StringVar(&f.timeout, "timeout", defaultTimeout, "how long to wait for the deploy to go live, like 18m or `1h`")
}

// appChange is one change to an app's configuration.
type appChange struct {
	command string // "kx env", "kx scale": for permission messages
	// step is the line kx prints once the change is saved: a label and
	// its detail, e.g. "Set", "API_KEY, LOG_LEVEL on shop-api". It never
	// holds a value.
	step func(app *api.App) (label, detail string)
	// check runs once the app is known, before anything changes (kx scale
	// warns about autoscaling there).
	check func(app *api.App)
	save  func(ctx context.Context, c *api.Client, app *api.App) error
	// out fills in what the change was, for --json.
	out func(o *configureJSON)
}

// configureJSON is what kx env set/unset and kx scale print with --json.
// It never holds a value.
type configureJSON struct {
	App          string   `json:"app"`
	AppID        string   `json:"app_id"`
	Org          string   `json:"org"`
	Set          []string `json:"set,omitempty"`
	Unset        []string `json:"unset,omitempty"`
	Replicas     int      `json:"replicas,omitempty"`
	Deployed     bool     `json:"deployed"`
	DeploymentID string   `json:"deployment_id,omitempty"`
	Version      int      `json:"version,omitempty"`
	Status       string   `json:"status,omitempty"`
	URL          string   `json:"url,omitempty"`
}

// runConfigure saves change on the app and, unless --no-deploy, deploys
// the app as configured and waits like kx deploy. Every check that could
// stop the deploy (the token's permissions, a multi-region app, an earlier
// deploy still running, today's deploys) comes before the change is saved.
func runConfigure(ctx context.Context, a *App, cmd *cobra.Command, f configureFlags, change appChange) error {
	timeout, err := parseTimeout(f.timeout)
	if err != nil {
		return err
	}
	s, err := a.session(cmd, f.app)
	if err != nil {
		return err
	}
	c, err := s.Client(ctx)
	if err != nil {
		return err
	}
	need := []string{api.PermConfigure, api.PermRead}
	if !f.noDeploy {
		need = append(need, api.PermDeploy)
	}
	if err := requirePermissions(ctx, s, change.command, need...); err != nil {
		return err
	}
	app, err := s.ResolveApp(ctx)
	if err != nil {
		return err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return err
	}
	if change.check != nil {
		change.check(app)
	}
	p := s.P
	saved := func(ctx context.Context) error {
		if err := change.save(ctx, c, app); err != nil {
			return err
		}
		label, detail := change.step(app)
		p.Step(label, detail, 0)
		return nil
	}
	out := configureJSON{App: app.Name, AppID: app.ID, Org: org, URL: app.URL}
	change.out(&out)

	if f.noDeploy {
		if err := saved(ctx); err != nil {
			return err
		}
		if a.jsonOut {
			return p.PrintJSON(out)
		}
		p.Note("Saved; the next deploy of %s applies it.", app.Name)
		return nil
	}

	key, err := uuid.V4() // a new deploy each time, also within one CI run
	if err != nil {
		return err
	}
	res, err := deploy.Run(ctx, c, deploy.Options{
		App: app, Org: org, Save: saved,
		IdempotencyKey: key, NoWait: f.noWait, Timeout: timeout,
		Now: a.Now, Sleep: a.Sleep,
		StopCommands: s.CI.Provider == ci.GitHub,
	}, *p)
	if err != nil {
		return err
	}
	out.Deployed, out.DeploymentID, out.Version, out.Status = true, res.DeploymentID, res.Version, res.Status
	if res.URL != "" {
		out.URL = res.URL
	}
	if a.jsonOut {
		return p.PrintJSON(out)
	}
	switch {
	case res.Status == "started":
		p.Info("Check on it: kx status")
	case out.URL != "":
		p.Result(out.URL)
	}
	return nil
}

// requirePermissions stops with TokenLacks when /whoami says the token
// doesn't have perms, which command needs. Without a token (or an answer
// that doesn't say) Kubernetix decides.
func requirePermissions(ctx context.Context, s *Session, command string, perms ...string) error {
	who, err := s.WhoAmI(ctx)
	if err != nil {
		return err
	}
	t := who.Token
	if t == nil {
		return nil
	}
	var missing []string
	for _, p := range perms {
		if !t.Can(p) {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &kxerr.TokenLacks{Permissions: missing, Source: s.Cfg.TokenSource, Org: who.Org.ID, Command: command}
}

// listOf joins names for a step line: "A, B and C" reads as one change, so
// commas only.
func listOf(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

func usageLine(command, what, example string) error {
	return &kxerr.Usage{What: what, Fix: fmt.Sprintf("Run: %s %s", command, example)}
}
