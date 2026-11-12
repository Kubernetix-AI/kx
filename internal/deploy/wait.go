package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
)

const (
	fastPolls       = 15 // polls 2 s apart (the first 30 s), then 5 s apart
	maxPollFailures = 5  // consecutive failed polls kx rides out
	recentDeploys   = 5
)

// defaultSleep waits for d or until ctx ends. Tests replace it.
var defaultSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pollInterval is the wait before poll i: 2 s at first, then 5 s.
func pollInterval(i int) time.Duration {
	if i < fastPolls {
		return 2 * time.Second
	}
	return 5 * time.Second
}

// WaitForPrevious waits while the app's newest deployment is still in
// progress, printing "Waiting for v14 to finish first". Two deploys of one
// app at the same time can finish in the wrong order, so kx never starts
// one while another runs. It returns when the newest
// deployment is ready, failed or rolled back, or when ctx ends.
func WaitForPrevious(ctx context.Context, c *api.Client, appID string, p ui.Printer) error {
	return waitForPrevious(ctx, c, appID, &p, defaultSleep, time.Now, time.Time{})
}

// waitForPrevious is WaitForPrevious with a clock and an optional
// deadline, after which it returns *kxerr.PreviousRunning.
func waitForPrevious(ctx context.Context, c *api.Client, appID string, p *ui.Printer,
	sleep func(context.Context, time.Duration) error, now func() time.Time, deadline time.Time) error {
	deps, err := c.ListDeployments(ctx, appID, recentDeploys)
	if err != nil {
		return err
	}
	if len(deps) > 0 && deleting(deps[0]) {
		return appDeleting(c)
	}
	if len(deps) == 0 || deps[0].Done() {
		return nil
	}
	cur := deps[0]
	start := now()
	p.Info("Waiting for v%d to finish first", cur.Version)
	label := func(d api.Deployment) string { return fmt.Sprintf("Waiting for v%d (%s)", d.Version, d.Phase()) }
	spin := p.Spinner(label(cur))
	defer spin.Stop()
	failures := 0
	for i := 0; ; i++ {
		if !deadline.IsZero() && !now().Before(deadline) {
			return &kxerr.PreviousRunning{Version: cur.Version, After: now().Sub(start)}
		}
		if err := sleep(ctx, pollInterval(i)); err != nil {
			return err
		}
		next, err := c.ListDeployments(ctx, appID, recentDeploys)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if transient(err) && failures < maxPollFailures {
				failures++
				continue
			}
			return err
		}
		failures = 0
		if len(next) > 0 && deleting(next[0]) {
			return appDeleting(c)
		}
		if len(next) == 0 || next[0].Done() {
			return nil
		}
		cur = next[0]
		spin.Update(label(cur))
	}
}

// waitForPrevious waits for an earlier deploy with the deploy's clock and timeout.
func (r *runner) waitForPrevious(ctx context.Context, app *api.App) error {
	err := waitForPrevious(ctx, r.c, app.ID, r.p, r.o.Sleep, r.o.Now, r.o.Now().Add(r.o.Timeout))
	var pr *kxerr.PreviousRunning
	if errors.As(err, &pr) {
		pr.App, pr.After = app.Name, r.o.Timeout
	}
	return err
}

// wait polls dep until it is done or the timeout passes. It returns the
// last state seen and the phases it went through (queued, starting, …).
func (r *runner) wait(ctx context.Context, app *api.App, dep *api.Deployment) (*api.Deployment, []string, error) {
	o := r.o
	start := o.Now()
	cur := dep
	phases := []string{cur.Phase()}
	label := func(d *api.Deployment) string { return fmt.Sprintf("Deploying v%d: %s", d.Version, d.Phase()) }
	spin := r.p.Spinner(label(cur))
	defer spin.Stop()
	failures := 0
	for i := 0; !cur.Done(); i++ {
		if o.Now().Sub(start) >= o.Timeout {
			return cur, phases, &kxerr.WaitTimeout{App: app.Name, Version: cur.Version, After: o.Timeout}
		}
		if err := o.Sleep(ctx, pollInterval(i)); err != nil {
			return cur, phases, stoppedWaiting(ctx, app, cur, err)
		}
		next, err := r.c.GetDeployment(ctx, dep.ID)
		if err != nil {
			if ctx.Err() != nil {
				return cur, phases, stoppedWaiting(ctx, app, cur, err)
			}
			if transient(err) && failures < maxPollFailures {
				failures++
				continue
			}
			return cur, phases, err
		}
		failures = 0
		cur = next
		if deleting(*cur) {
			return cur, phases, appDeleting(r.c)
		}
		if ph := cur.Phase(); ph != phases[len(phases)-1] {
			phases = append(phases, ph)
		}
		spin.Update(label(cur))
	}
	return cur, phases, nil
}

// deleting reports a deployment of an app that is being deleted. Such a
// deployment never becomes ready, so kx stops instead of waiting.
func deleting(d api.Deployment) bool {
	return d.Status == "pending_deletion" || d.Status == "deleting"
}

// appDeleting is the API's own "app is being deleted" error, so kx says it
// the same way whichever way it found out.
func appDeleting(c *api.Client) error {
	return &api.Error{Status: 409, Code: "app_deleting", Ctx: c.Ctx}
}

// stoppedWaiting says the deploy continues when kx is interrupted.
func stoppedWaiting(ctx context.Context, app *api.App, d *api.Deployment, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return &kxerr.StoppedWaiting{App: app.Name, Version: d.Version}
	}
	return err
}

// transient reports whether a failed poll is worth repeating: no answer,
// or a server-side error.
func transient(err error) bool {
	var netErr *api.NetworkError
	var apiErr *api.Error
	return errors.As(err, &netErr) || (errors.As(err, &apiErr) && apiErr.Status >= 500)
}

// tail keeps the last n lines written to it.
type tail struct {
	mu    sync.Mutex
	n     int
	lines []string
	part  string
}

func newTail(n int) *tail { return &tail{n: n} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.ReplaceAll(t.part+string(p), "\r\n", "\n")
	parts := strings.Split(s, "\n")
	t.part = parts[len(parts)-1]
	for _, l := range parts[:len(parts)-1] {
		t.lines = append(t.lines, l)
		if len(t.lines) > t.n {
			t.lines = t.lines[len(t.lines)-t.n:]
		}
	}
	if len(t.part) > 64<<10 { // a runaway line without a newline
		t.part = t.part[len(t.part)-(64<<10):]
	}
	return len(p), nil
}

// Lines returns the kept lines, including an unfinished last one.
func (t *tail) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]string(nil), t.lines...)
	if t.part != "" {
		out = append(out, t.part)
		if len(out) > t.n {
			out = out[1:]
		}
	}
	return out
}
