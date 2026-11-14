package deploy

import (
	"context"
	"errors"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// Kubernetix keeps a new version that never gets ready for many minutes
// (it waits for readiness and tries again), longer than kx's default wait.
// So while it waits, kx also watches the app's replicas: a replica of the
// new version that runs but stays not ready for longer than its health
// checks allow is crashing on start or not answering them, and waiting
// longer won't change that. A replica that is still pending (its image
// being pulled, or waiting for room) counts as a slow start.
const (
	replicaCheckEvery = 10 * time.Second
	// stallGrace is added to the health checks' own window.
	stallGrace    = time.Minute
	minStallAfter = 90 * time.Second
	// unknownChecks is the window kx assumes when the deployment doesn't
	// say how its containers are checked.
	unknownChecks = 4 * time.Minute
)

// stalled is the wait loop's verdict that the new version isn't going to
// get ready: which replicas are stuck, and for how long kx saw them so.
type stalled struct {
	pods  []string
	after time.Duration
}

func (s *stalled) Error() string { return "the new version's replicas never got ready" }

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
	window := unknownChecks
	if hc != nil {
		window = hc.Startup.Window() + max(hc.Liveness.Window(), hc.Readiness.Window())
	}
	return max(window+stallGrace, minStallAfter)
}

// check looks at the replicas, at most every 10 seconds while d is being
// rolled out, and returns a verdict when a replica of the new version has
// been running and not ready for longer than allowed.
func (w *stallWatch) check(ctx context.Context, now time.Time, d *api.Deployment) *stalled {
	rolling := d.Status == api.StatusDeploying || d.Status == api.StatusPromoting
	if w.off || !rolling || now.Before(w.next) {
		return nil
	}
	w.next = now.Add(replicaCheckEvery)
	reps, err := w.c.Replicas(ctx, w.appID)
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 {
			w.off = true
		}
		return nil
	}
	seen := map[string]bool{}
	var st stalled
	for _, rep := range reps {
		started, err := time.Parse(time.RFC3339, rep.StartedAt)
		if err != nil || started.Before(w.since) || rep.Status != "Running" || rep.Ready {
			continue
		}
		seen[rep.Name] = true
		first, ok := w.running[rep.Name]
		if !ok {
			w.running[rep.Name] = now
			continue
		}
		if took := now.Sub(first); took >= w.after {
			st.pods = append(st.pods, rep.Name)
			st.after = max(st.after, took)
		}
	}
	for name := range w.running {
		if !seen[name] {
			delete(w.running, name) // ready, or gone
		}
	}
	if len(st.pods) == 0 {
		return nil
	}
	return &st
}

// notStarting explains a version whose replicas never got ready: the
// stuck replicas' last log lines, that Kubernetix keeps trying, and the
// likely causes kx can see (crashing, or not answering the health check
// at its path and port).
func (r *runner) notStarting(ctx context.Context, app *api.App, d *api.Deployment, st *stalled) *kxerr.DeployFailed {
	lines := r.replicaLogs(ctx, app, d, st.pods)
	if len(lines) == 0 {
		lines = r.deploymentLogs(ctx, d)
	}
	if len(lines) > 0 {
		r.p.Info("The last %d log lines of v%d:", len(lines), d.Version)
		r.showUntrusted(lines, func(l string) { r.p.Note("%s", l) })
	}
	r.p.Info("Kubernetix keeps trying v%d for a few more minutes; kx status shows how it ends.", d.Version)
	ns := &kxerr.NotStarting{After: st.after, Port: d.Port}
	if ns.Port == 0 {
		ns.Port = app.Port
	}
	if hc := d.HealthCheck; hc == nil {
		ns.HealthCheck = true // not known: new apps get an HTTP check on the app's port
	} else {
		for _, p := range []*api.Probe{hc.Readiness, hc.Liveness, hc.Startup} {
			if p != nil && p.HTTPGet != nil {
				ns.HealthCheck, ns.Path = true, p.HTTPGet.Path
				if p.HTTPGet.Port > 0 {
					ns.Port = p.HTTPGet.Port
				}
				break
			}
		}
	}
	return &kxerr.DeployFailed{App: app.Name, Version: d.Version, ServingVersion: r.serving(ctx, app, d), NotStarting: ns}
}

// replicaLogs returns the last log lines the stuck replicas printed since
// the deployment started. When the log stream doesn't name replicas, every
// line since then counts.
func (r *runner) replicaLogs(ctx context.Context, app *api.App, d *api.Deployment, pods []string) []string {
	logs, err := r.c.RuntimeLogs(ctx, app.ID, api.LogQuery{Limit: 500, Start: d.CreatedAt})
	if err != nil {
		return nil
	}
	stuck := map[string]bool{}
	for _, p := range pods {
		stuck[p] = true
	}
	named := false
	for _, l := range logs {
		named = named || l.Pod != ""
	}
	var text []string
	for _, l := range logs {
		if !named || stuck[l.Pod] {
			text = append(text, l.Line)
		}
	}
	return lastLines(joinLines(text), shownLogLines)
}
