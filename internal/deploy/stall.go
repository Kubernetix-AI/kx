package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
)

// Kubernetix keeps trying a new version that doesn't get ready for many
// minutes (it waits for readiness and starts it again), longer than kx's
// default wait. So while it waits, kx also watches the app's replicas: a
// replica of the new version that runs but stays not ready for longer than
// its health checks allow is probably crashing on start or not answering
// them. kx then says so at once, with that replica's last log lines, and
// keeps waiting: a container can still get ready on a later start, so
// only the platform failing the version, or the timeout, ends the wait.
// A replica that is still pending (its image being pulled, or waiting for
// room) counts as a slow start.
const (
	replicaCheckEvery = 10 * time.Second
	// stallGrace is added to the health checks' own window.
	stallGrace    = time.Minute
	minStallAfter = 90 * time.Second
)

// defaultChecks stands in, for timing only, for the health checks of a
// deployment that records none: how long its containers may take to get
// ready before kx says they aren't.
var defaultChecks = &api.HealthCheck{
	Liveness:  &api.Probe{HTTPGet: &api.HTTPGet{Path: "/"}, InitialDelaySeconds: 30, PeriodSeconds: 10, FailureThreshold: 3},
	Readiness: &api.Probe{HTTPGet: &api.HTTPGet{Path: "/"}, InitialDelaySeconds: 10, PeriodSeconds: 5, FailureThreshold: 3},
}

// stalled is what the replicas say right now: which replicas of the new
// version have been running and not ready for longer than allowed, and the
// longest time kx saw one so.
type stalled struct {
	pods  []string
	after time.Duration
}

// stallWatch looks at the app's replicas while a deployment rolls out.
type stallWatch struct {
	c     *api.Client
	appID string
	// since is when the deployment was created: replicas started before
	// it run an earlier version.
	since time.Time
	after time.Duration // how long a running replica may stay not ready
	next  time.Time
	// running is when kx first saw each replica of the new version
	// running but not ready (kx's clock).
	running map[string]time.Time
	off     bool // replicas can't be read: only the timeout applies
}

func newStallWatch(c *api.Client, appID string, d *api.Deployment) *stallWatch {
	return &stallWatch{
		c: c, appID: appID,
		since:   d.CreatedAt.Truncate(time.Second),
		after:   stallAfter(d.HealthCheck),
		running: map[string]time.Time{},
		// Without the deployment's start, kx can't tell the new version's
		// replicas from the ones serving now.
		off: d.CreatedAt.IsZero(),
	}
}

// stallAfter is how long a replica may run without getting ready: the
// startup check's window, then the longer of the liveness and readiness
// windows (the time to the first restart, or to being marked not ready),
// plus a minute; at least 90 seconds.
func stallAfter(hc *api.HealthCheck) time.Duration {
	if hc == nil {
		hc = defaultChecks
	}
	window := hc.Startup.Window() + max(hc.Liveness.Window(), hc.Readiness.Window())
	return max(window+stallGrace, minStallAfter)
}

// check looks at the replicas, at most every 10 seconds while d is being
// rolled out. ok is false when it learned nothing this time (not due, no
// answer, or replica data it can't trust); otherwise st lists the
// replicas stuck right now, nil if none.
func (w *stallWatch) check(ctx context.Context, now time.Time, d *api.Deployment) (st *stalled, ok bool) {
	rolling := d.Status == api.StatusDeploying || d.Status == api.StatusPromoting
	if w.off || !rolling || now.Before(w.next) {
		return nil, false
	}
	w.next = now.Add(replicaCheckEvery)
	reps, err := w.c.Replicas(ctx, w.appID)
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 {
			w.off = true
		}
		return nil, false
	}
	// Replicas of the version still serving are ready. If none of the
	// running ones says so, the ready condition is missing from the answer
	// (it may come from metrics that lag or lack it): no judgment.
	older, olderReady := false, false
	for _, rep := range reps {
		if started, err := time.Parse(time.RFC3339, rep.StartedAt); err == nil && started.Before(w.since) && rep.Status == "Running" {
			older = true
			olderReady = olderReady || rep.Ready
		}
	}
	if older && !olderReady {
		return nil, false
	}
	seen := map[string]bool{}
	var s stalled
	for _, rep := range reps {
		// Skipped: older replicas, unknown phase or start time, pending
		// (a slow start) and ready ones.
		started, err := time.Parse(time.RFC3339, rep.StartedAt)
		if err != nil || started.Before(w.since) || rep.Status != "Running" || rep.Ready {
			continue
		}
		seen[rep.Name] = true
		first, known := w.running[rep.Name]
		if !known {
			w.running[rep.Name] = now
			continue
		}
		if took := now.Sub(first); took >= w.after {
			s.pods = append(s.pods, rep.Name)
			s.after = max(s.after, took)
		}
	}
	for name := range w.running {
		if !seen[name] {
			delete(w.running, name) // ready, or gone
		}
	}
	if len(s.pods) == 0 {
		return nil, true
	}
	return &s, true
}

// likelyCause turns what the replicas say into what kx tells the person:
// crashing, or not answering the health check on its path at the
// deployment's port (the port the platform checks, whatever port an older
// health check stored). A deployment that records no health check gets
// Kubernetix's own, whose path kx doesn't know, so it names none.
func (r *runner) likelyCause(app *api.App, d *api.Deployment, st *stalled) *kxerr.NotStarting {
	ns := &kxerr.NotStarting{After: st.after, Port: d.Port, Rollback: r.rollback}
	if ns.Port == 0 {
		ns.Port = app.Port
	}
	hc := d.HealthCheck
	if hc == nil {
		ns.HealthCheck = true
		return ns
	}
	for _, p := range []*api.Probe{hc.Readiness, hc.Liveness, hc.Startup} {
		if p != nil && p.HTTPGet != nil {
			ns.HealthCheck, ns.Path = true, p.HTTPGet.Path
			break
		}
	}
	return ns
}

// signalNotLive prints the likely cause the moment kx sees it: the two
// lines, the stuck replicas' last log lines, and that kx keeps waiting.
func (r *runner) signalNotLive(ctx context.Context, app *api.App, d *api.Deployment, ns *kxerr.NotStarting) {
	what, fix := api.NotLiveYet(app.Name, d.Version, ns)
	r.p.Warn("%s", what)
	r.p.Note("%s", fix)
	r.o.OnNotLive(what, fix)
	r.showLogs(ctx, app, d)
	if r.p.InTTY {
		r.p.Info("kx keeps waiting; press Ctrl-C to stop, the deploy continues.")
	} else {
		r.p.Info("kx keeps waiting until v%d goes live, fails or reaches the %s timeout; the deploy continues either way.", d.Version, shortDuration(r.o.Timeout))
	}
	r.p.Info("Until v%d goes live or fails, a new deploy of %s waits for it.", d.Version, app.Name)
}

// showLogs prints the last log lines of the stuck replicas, when kx saw
// any, else of the deployment. Lines from a log stream that doesn't name
// replicas could be any version's, so they are headed as the app's recent
// lines, never as the new version's.
func (r *runner) showLogs(ctx context.Context, app *api.App, d *api.Deployment) {
	var lines []string
	named := true
	if r.stuck != nil {
		lines, named = r.replicaLogs(ctx, app, d, r.stuck.pods)
	}
	if len(lines) == 0 {
		lines, named = r.deploymentLogs(ctx, d), true
	}
	switch {
	case len(lines) == 0:
		return
	case named:
		r.p.Info("The last %d log lines of v%d:", len(lines), d.Version)
	default:
		r.p.Info("Recent log lines from %s (kx can't tell which version printed them):", app.Name)
	}
	r.showUntrusted(lines, func(l string) { r.p.Note("%s", l) })
}

// replicaLogs returns the last log lines the stuck replicas printed since
// the deployment started, and whether the log stream names replicas. When
// it doesn't, every line since then counts, whichever version printed it.
func (r *runner) replicaLogs(ctx context.Context, app *api.App, d *api.Deployment, pods []string) (lines []string, named bool) {
	logs, err := r.c.RuntimeLogs(ctx, app.ID, api.LogQuery{Limit: 500, Start: d.CreatedAt})
	if err != nil {
		return nil, false
	}
	stuck := map[string]bool{}
	for _, p := range pods {
		stuck[p] = true
	}
	for _, l := range logs {
		named = named || l.Pod != ""
	}
	var text []string
	for _, l := range logs {
		if !named || stuck[l.Pod] {
			text = append(text, l.Line)
		}
	}
	return lastLines(joinLines(text), shownLogLines), named
}

// shortDuration writes 18m, 1h or 90s.
func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return ui.Duration(d)
}
