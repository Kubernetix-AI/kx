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

	// A poll answered with 429 waits for Retry-After, between these.
	minRateLimitWait = 5 * time.Second
	maxRateLimitWait = time.Minute

	// rolledBackLasts is how long a rolled_back status must last before kx
	// believes it (see finality).
	rolledBackLasts = 10 * time.Second
)

// finality tells when a deployment kx keeps looking at has stopped
// changing. Ready and failed are final at once. Kubernetix can mark a
// version rolled_back for a moment, with a generic message, before it
// fails it with the real reason or tries it again, so a rolled_back is
// final only when kx sees it again on a look at least rolledBackLasts
// after the first, with nothing else in between.
type finality struct {
	since time.Time // when kx first saw the current rolled_back; zero if none
	id    string    // which deployment, and which attempt of it
	retry int
}

// final records a look at d, taken at now, and reports whether d is final.
func (f *finality) final(d api.Deployment, now time.Time) bool {
	if !d.RolledBack() {
		f.since = time.Time{}
		return d.Done()
	}
	if f.since.IsZero() || d.ID != f.id || d.RetryCount != f.retry {
		f.since, f.id, f.retry = now, d.ID, d.RetryCount
	}
	return now.Sub(f.since) >= rolledBackLasts
}

// confirming reports, at now, that the last look showed a rolled_back kx
// doesn't believe yet and the looks that settle it are still due. The
// waits let those looks run past their timeout, so a version rolled back
// right at the timeout is reported as such: by at most rolledBackLasts
// plus one rate-limited poll.
func (f *finality) confirming(done bool, now time.Time) bool {
	return !done && !f.since.IsZero() && now.Sub(f.since) < rolledBackLasts+maxRateLimitWait
}

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
// deployment is ready, failed or rolled back (for at least 10 seconds, see
// finality), or when ctx ends.
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
	var fin finality
	if len(deps) == 0 || fin.final(deps[0], now()) {
		return nil
	}
	cur := deps[0]
	start := now()
	p.Info("Waiting for v%d to finish first", cur.Version)
	label := func(d api.Deployment) string { return fmt.Sprintf("Waiting for v%d (%s)", d.Version, d.Phase()) }
	spin := p.Spinner(label(cur))
	defer spin.Stop()
	failures := 0
	var limited time.Duration
	for i := 0; ; i++ {
		if !deadline.IsZero() && !now().Before(deadline) && !fin.confirming(false, now()) {
			return &kxerr.PreviousRunning{Version: cur.Version, After: now().Sub(start)}
		}
		if err := sleep(ctx, max(pollInterval(i), limited)); err != nil {
			return err
		}
		limited = 0
		next, err := c.ListDeployments(ctx, appID, recentDeploys)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if wait, ok := rateLimited(err); ok {
				limited = wait
				continue
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
		if len(next) == 0 || fin.final(next[0], now()) {
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
// When the new version's replicas run but don't get ready (see
// stallWatch) it says so once, with the likely cause, and keeps waiting;
// the cause is kept in r.likely for a later failure or timeout. It also
// says when Kubernetix starts the deployment again after an attempt that
// didn't go live.
func (r *runner) wait(ctx context.Context, app *api.App, dep *api.Deployment) (*api.Deployment, []string, error) {
	o := r.o
	start := o.Now()
	cur := dep
	phases := []string{cur.Phase()}
	watch := newStallWatch(r.c, app.ID, dep)
	attempt := dep.RetryCount + 1
	label := func(d *api.Deployment) string {
		if attempt > 1 {
			return fmt.Sprintf("Deploying v%d: %s (attempt %d)", d.Version, d.Phase(), attempt)
		}
		return fmt.Sprintf("Deploying v%d: %s", d.Version, d.Phase())
	}
	spin := r.p.Spinner(label(cur))
	defer func() { spin.Stop() }()
	signalled := false
	failures := 0
	var limited time.Duration
	var fin finality
	done := fin.final(*cur, o.Now())
	for i := 0; !done; i++ {
		if o.Now().Sub(start) >= o.Timeout && !fin.confirming(done, o.Now()) {
			return cur, phases, &kxerr.WaitTimeout{App: app.Name, Version: cur.Version, After: o.Timeout, NotStarting: r.likely}
		}
		if err := o.Sleep(ctx, max(pollInterval(i), limited)); err != nil {
			return cur, phases, stoppedWaiting(ctx, app, cur, err)
		}
		limited = 0
		next, err := r.c.GetDeployment(ctx, dep.ID)
		if err != nil {
			if ctx.Err() != nil {
				return cur, phases, stoppedWaiting(ctx, app, cur, err)
			}
			if wait, ok := rateLimited(err); ok {
				limited = wait
				continue
			}
			if transient(err) && failures < maxPollFailures {
				failures++
				continue
			}
			return cur, phases, err
		}
		failures = 0
		cur = next
		done = fin.final(*cur, o.Now())
		if deleting(*cur) {
			return cur, phases, appDeleting(r.c)
		}
		if fin.confirming(done, o.Now()) {
			// It may yet fail with its real reason or start again: show
			// nothing until it lasts.
			continue
		}
		if ph := cur.Phase(); ph != phases[len(phases)-1] {
			phases = append(phases, ph)
		}
		if n := cur.RetryCount + 1; n > attempt && !done {
			spin.Println("v%d didn't go live on attempt %d; Kubernetix is trying again (attempt %d).", cur.Version, n-1, n)
			attempt = n
		}
		spin.Update(label(cur))
		st, ok := watch.check(ctx, o.Now(), cur)
		if !ok {
			continue
		}
		r.stuck, r.likely = st, nil
		if st == nil {
			continue // nothing stuck now: no cause to report
		}
		r.likely = r.likelyCause(app, cur, st)
		if !signalled {
			signalled = true
			spin.Stop()
			r.signalNotLive(ctx, app, cur, r.likely)
			spin = spin.Resume()
		}
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

// rateLimited reports a 429 and how long to wait before the next poll:
// the answer's Retry-After, at least 5 seconds and at most a minute. A
// rate limit says nothing about the deploy, so the wait loops ride it out
// for as long as their timeout allows.
func rateLimited(err error) (time.Duration, bool) {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		return 0, false
	}
	return min(max(apiErr.RetryAfter, minRateLimitWait), maxRateLimitWait), true
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
