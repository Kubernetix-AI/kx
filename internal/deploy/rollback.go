package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
)

// ErrDeclined means the person answered no to the rollback question.
var ErrDeclined = errors.New("rollback declined")

// RollbackPlan is what a rollback would do: redeploy To's image as a new
// version, replacing From.
type RollbackPlan struct {
	App  *api.App
	From *api.Deployment // the live version
	To   *api.Deployment // the version whose image comes back
}

// RollbackOptions describe one rollback.
type RollbackOptions struct {
	App *api.App
	Org string
	// To is the version to roll back to; 0 means the newest earlier
	// version that went live with a different image.
	To int
	// Confirm is asked before anything changes; nil means yes.
	Confirm        func(*RollbackPlan) (bool, error)
	IdempotencyKey string
	Timeout        time.Duration // 0 means DefaultTimeout
	Now            func() time.Time
	Sleep          func(ctx context.Context, d time.Duration) error
	StopCommands   bool // see Options.StopCommands
}

// Rollback waits for any deploy in progress, picks the version to roll
// back to, asks Confirm, then redeploys that version's image and waits
// until it is live.
func Rollback(ctx context.Context, c *api.Client, o RollbackOptions, p ui.Printer) (*Result, error) {
	if o.App == nil {
		return nil, errors.New("deploy: Rollback needs an app")
	}
	if o.IdempotencyKey == "" {
		return nil, errors.New("deploy: an idempotency key is required")
	}
	// Image only satisfies prepare's build checks: a rollback builds nothing.
	r := &runner{c: c, p: &p, rollback: true, o: Options{App: o.App, Org: o.Org, IdempotencyKey: o.IdempotencyKey, Timeout: o.Timeout, Now: o.Now, Sleep: o.Sleep, Image: "rollback", StopCommands: o.StopCommands}}
	if err := r.o.prepare(); err != nil {
		return nil, err
	}
	c.Ctx.App = o.App.Name
	app, err := c.GetApp(ctx, o.App.ID)
	if err != nil {
		return nil, err
	}
	if app.MultiRegion() {
		return nil, &kxerr.MultiRegion{App: app.Name}
	}
	if err := r.waitForPrevious(ctx, app); err != nil {
		return nil, err
	}
	plan, err := planRollback(ctx, c, app, o.To)
	if err != nil {
		return nil, err
	}
	if o.Confirm != nil {
		ok, err := o.Confirm(plan)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrDeclined
		}
	}

	// The server doesn't deduplicate rollbacks by key, so the request is
	// sent once: a retry could start a second rollback. A copy of the
	// client keeps the caller's retry setting for everything else.
	once := *c
	once.MaxRetries = 0
	dep, err := once.Rollback(ctx, plan.To.ID, o.IdempotencyKey)
	if err != nil {
		if unanswered(err) {
			return nil, &kxerr.RollbackUnconfirmed{App: app.Name, Version: plan.To.Version}
		}
		return nil, err
	}
	res := &Result{App: app.Name, AppID: app.ID, Org: o.Org, URL: app.URL, DeploymentID: dep.ID, Version: dep.Version, Status: dep.Status, Image: plan.To.ImageURL}
	r.p.Step("Started", fmt.Sprintf("v%d with the image of v%d", dep.Version, plan.To.Version), 0)
	return r.finish(ctx, app, dep, res)
}

// planRollback finds the live version and the one to roll back to.
func planRollback(ctx context.Context, c *api.Client, app *api.App, to int) (*RollbackPlan, error) {
	deps, err := c.ListDeployments(ctx, app.ID, 100)
	if err != nil {
		return nil, err
	}
	var live *api.Deployment
	for i := range deps {
		if deps[i].Status == api.StatusReady {
			live = &deps[i]
			break
		}
	}
	if live == nil {
		return nil, &kxerr.NothingLive{App: app.Name}
	}
	plan := &RollbackPlan{App: app, From: live}
	if to > 0 {
		for i := range deps {
			if deps[i].Version == to {
				plan.To = &deps[i]
				break
			}
		}
		switch {
		case plan.To == nil:
			return nil, &kxerr.NoVersion{App: app.Name, Version: fmt.Sprintf("v%d", to)}
		case plan.To.ID == live.ID:
			return nil, &kxerr.AlreadyLive{App: app.Name, Version: to}
		case plan.To.Status != api.StatusReady:
			return nil, &kxerr.NotRollbackable{App: app.Name, Version: to}
		}
		return plan, nil
	}
	for i := range deps {
		d := &deps[i]
		if d.Status == api.StatusReady && d.Version < live.Version && d.ImageURL != live.ImageURL {
			plan.To = d
			return plan, nil
		}
	}
	return nil, &kxerr.NoRollbackTarget{App: app.Name}
}

// unanswered reports whether a request may have reached Kubernetix without
// kx learning the outcome: a gateway error, a timeout, a broken
// connection, or kx being interrupted while it waited.
func unanswered(err error) bool {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var netErr *api.NetworkError
	return errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
