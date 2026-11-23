package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	// prepare runs once the app is known, before anything changes. It
	// returns an error to refuse the change, or nothing, the line to print
	// when the change would change nothing (then kx neither saves nor
	// deploys). It may be nil.
	prepare func(ctx context.Context, c *api.Client, app *api.App) (nothing string, err error)
	// step is the line kx prints once the change is saved: a label and
	// its detail, e.g. "Set", "API_KEY, LOG_LEVEL on shop-api". It never
	// holds a value. note, when set, returns a line to print under it.
	step func(app *api.App) (label, detail string)
	note func(app *api.App) string
	save func(ctx context.Context, c *api.Client, app *api.App) error
	// undone, given the app as it was before the change, returns the check
	// kx makes on the app after a failed deploy: whether the change is gone
	// again, and whether kx can tell at all (it sees variable names, never
	// values). It may be nil.
	undone func(before *api.App) func(fresh *api.App) (undone, known bool)
	// out fills in what the change was, for --json.
	out func(o *configureJSON)
}

// configureJSON is what kx env set/unset and kx scale print with --json.
// Saved means the change is on the app; Deployed that a deployment with
// it went live; Unchanged that it would have changed nothing, so kx did
// nothing. It never holds a value.
type configureJSON struct {
	App          string   `json:"app"`
	AppID        string   `json:"app_id"`
	Org          string   `json:"org"`
	Set          []string `json:"set,omitempty"`
	Unset        []string `json:"unset,omitempty"`
	Replicas     int      `json:"replicas,omitempty"`
	Saved        bool     `json:"saved"`
	Deployed     bool     `json:"deployed"`
	Unchanged    bool     `json:"unchanged,omitempty"`
	DeploymentID string   `json:"deployment_id,omitempty"`
	Version      int      `json:"version,omitempty"`
	Status       string   `json:"status,omitempty"`
	URL          string   `json:"url,omitempty"`
}

// asChange marks an error of kx env set/unset or kx scale that came
// before anything was saved, so --json says saved and deployed are false.
func asChange(err error) error {
	if err == nil {
		return nil
	}
	if ch := (*kxerr.Change)(nil); errors.As(err, &ch) {
		return err
	}
	return &kxerr.Change{Err: err}
}

// runConfigure saves change on the app and, unless --no-deploy, deploys
// the app as configured and waits like kx deploy. Every check kx can make
// (the token's permissions, a multi-region app, an earlier deploy still
// running, today's deploys) comes before the change is saved; an error
// after it says what became of the change (afterSave).
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
	p := s.P
	out := configureJSON{App: app.Name, AppID: app.ID, Org: org, URL: app.URL}
	if change.prepare != nil {
		nothing, err := change.prepare(ctx, c, app)
		if err != nil {
			return err
		}
		if nothing != "" {
			change.out(&out)
			out.Unchanged = true
			if a.jsonOut {
				return p.PrintJSON(out)
			}
			p.Info("%s", nothing)
			return nil
		}
	}
	isSaved := false
	saved := func(ctx context.Context) error {
		if err := change.save(ctx, c, app); err != nil {
			return err
		}
		isSaved = true
		label, detail := change.step(app)
		p.Step(label, detail, 0)
		if change.note != nil {
			if n := change.note(app); n != "" {
				p.Note("%s", n)
			}
		}
		return nil
	}
	change.out(&out)

	if f.noDeploy {
		if err := saved(ctx); err != nil {
			return err
		}
		out.Saved = true
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
		if !isSaved {
			return err
		}
		return afterSave(ctx, a, c, app, change, res, err)
	}
	out.Saved, out.Deployed = true, res.Status == "live"
	out.DeploymentID, out.Version, out.Status = res.DeploymentID, res.Version, res.Status
	if res.URL != "" {
		out.URL = res.URL
	}
	if a.jsonOut {
		return p.PrintJSON(out)
	}
	switch {
	case res.Status == "started":
		p.Info("If v%d doesn't go live, Kubernetix puts the app back, which undoes this change.", res.Version)
		p.Info("Check on it: kx status")
	case out.URL != "":
		p.Result(out.URL)
	}
	return nil
}

// afterSave turns err, from a deploy kx started after saving change on app,
// into a *kxerr.Change that says what became of the change: refused, and
// still saved for the next deploy; started, with the outcome not seen; or
// failed, after which Kubernetix puts the app back to the version still
// serving, undoing the change (kx looks, when it can tell).
func afterSave(ctx context.Context, a *App, c *api.Client, app *api.App, change appChange, res *deploy.Result, err error) error {
	ch := &kxerr.Change{Err: err, Saved: true}
	if res != nil {
		ch.DeploymentID, ch.Version = res.DeploymentID, res.Version
	}
	stillSaved := fmt.Sprintf("The change is still saved on %s but not deployed; the next deploy applies it.", app.Name)
	var failed *kxerr.DeployFailed
	switch {
	case errors.As(err, &failed):
		ch.Version = failed.Version
		back := "its last working settings"
		if failed.ServingVersion > 0 {
			back = fmt.Sprintf("v%d", failed.ServingVersion)
		}
		switch undone, known := confirmUndone(ctx, a, c, app, change); {
		case known && undone:
			ch.Saved, ch.RestoredTo = false, failed.ServingVersion
			ch.Note = fmt.Sprintf("Kubernetix put %s back to %s, so this change was undone.", app.Name, back)
		case !known && failed.ServingVersion > 0:
			ch.Saved, ch.RestoredTo = false, failed.ServingVersion
			ch.Note = fmt.Sprintf("Kubernetix puts %s back to %s after a failed deploy, so this change is most likely undone.", app.Name, back)
		default:
			ch.Note = stillSaved
		}
	case ch.DeploymentID != "":
		ch.Note = fmt.Sprintf("The change is saved on %s; if v%d doesn't go live, Kubernetix puts the app back, which undoes it.", app.Name, ch.Version)
	case outcomeUnknown(err):
		ch.Note = fmt.Sprintf("The change is saved on %s; kx can't tell whether its deploy started (see kx status). If none did, the next deploy applies it.", app.Name)
	default:
		ch.Note = fmt.Sprintf("The change is saved on %s but not deployed; the next deploy applies it.", app.Name)
	}
	return ch
}

// undoChecks are the waits between kx's looks at an app after a failed
// deploy: Kubernetix puts the app back just after the deploy fails.
var undoChecks = []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second}

// confirmUndone reads app until change.undone sees the change gone, or kx
// has looked len(undoChecks) times. known is false when kx can't tell.
func confirmUndone(ctx context.Context, a *App, c *api.Client, app *api.App, change appChange) (undone, known bool) {
	if change.undone == nil {
		return false, false
	}
	check := change.undone(app)
	for _, wait := range undoChecks {
		if wait > 0 && a.Sleep(ctx, wait) != nil {
			return false, false
		}
		fresh, err := c.GetApp(ctx, app.ID)
		if err != nil {
			return false, false
		}
		undone, known := check(fresh)
		if !known || undone {
			return undone, known
		}
	}
	return false, true
}

// outcomeUnknown reports whether err leaves open whether the request it
// came from went through: no answer, or a server error.
func outcomeUnknown(err error) bool {
	var netErr *api.NetworkError
	var apiErr *api.Error
	return errors.As(err, &netErr) || (errors.As(err, &apiErr) && apiErr.Status >= 500) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
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
