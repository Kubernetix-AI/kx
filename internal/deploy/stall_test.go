package deploy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// Replicas as the API reports them: one of the version still serving,
// started before the deploy, and one of the new version.
var (
	oldReplica = api.Replica{Name: "app-shop-7d9f-old", Status: "Running", Ready: true, StartedAt: "2026-11-12T08:00:00Z"}
	newStarted = "2026-11-12T09:00:30Z"
)

func newReplica(status string, ready bool) api.Replica {
	return api.Replica{Name: "app-shop-8e1a-new", Status: status, Ready: ready, StartedAt: newStarted}
}

func repeat(first string, n int, rest ...string) []string {
	out := []string{first}
	for range n {
		out = append(out, rest[0])
	}
	return append(out, rest[1:]...)
}

// warnings records the early signals kx gave and when (on the fake clock).
type warnings struct {
	e     *env
	whats []string
	fixes []string
	at    []time.Duration
	start time.Time
}

func (e *env) watchWarnings(o *Options) *warnings {
	w := &warnings{e: e, start: e.clock.Now()}
	o.OnNotLive = func(what, fix string) {
		w.whats, w.fixes = append(w.whats, what), append(w.fixes, fix)
		w.at = append(w.at, e.clock.Now().Sub(w.start))
	}
	return w
}

const platformGeneric = "Deployment failed. The platform team has been alerted; please retry, or contact support if the issue persists."

// A new version whose container keeps crashing (or never answers its
// health check) gets the likely cause and its own log lines after the
// health checks' window plus a minute, but kx keeps waiting: only the
// platform failing the version ends the deploy with exit 8, and then with
// what kx saw rather than the platform's generic message.
func TestRun_NeverReadyWarnsAndKeepsWaiting(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("pending", 60, "deploying", "failed")
	e.api.failReason = platformGeneric
	e.api.health = defaultHealth()
	e.api.replicas = func(int) []api.Replica { return []api.Replica{oldReplica, newReplica("Running", false)} }
	e.api.runtimeLogs = streams(map[string][]string{
		"app-shop-7d9f-old": {"GET / 200 from the old version"},
		"app-shop-8e1a-new": {"> node server.js", "Error: Cannot find module '/app/server.js'", "::error::not a workflow command"},
	})
	o := e.options()
	o.StopCommands = true
	w := e.watchWarnings(&o)
	res, err := e.run(o)

	if len(w.at) != 1 || w.at[0] < 2*time.Minute || w.at[0] > 3*time.Minute {
		t.Fatalf("early signals at %v, want one after about 2 minutes", w.at)
	}
	if w.whats[0] != "shop-api v15 isn't live yet: for 2 minutes its container has kept crashing or hasn't answered health checks on / at port 8080." ||
		w.fixes[0] != "Make your app start and answer on / at port 8080; the log lines below may say why it hasn't." {
		t.Fatalf("signal = %q / %q", w.whats[0], w.fixes[0])
	}
	if took := e.clock.Now().Sub(w.start); took < 5*time.Minute {
		t.Fatalf("stopped after %v: kx must keep waiting after the signal", took)
	}
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.NotStarting == nil || df.Version != 15 || df.ServingVersion != 14 || df.NotStarting.Port != 8080 || df.NotStarting.Path != "/" {
		t.Fatalf("Run() = %#v", err)
	}
	if res == nil || res.Version != 15 || res.ServingVersion != 14 {
		t.Fatalf("result = %+v", res)
	}
	if p := api.Explain(err); p.Exit != api.ExitDeploy || p.What != "shop-api v15 didn't go live: its container kept crashing or didn't answer health checks on / at port 8080. Version 14 is still serving." {
		t.Fatalf("problem = %+v", p)
	}
	out := e.errOut.String()
	for _, want := range []string{
		"! shop-api v15 isn't live yet: for 2 minutes its container has kept crashing or hasn't answered health checks on / at port 8080.\n  Make your app start and answer on / at port 8080; the log lines below may say why it hasn't.\nThe last 3 log lines of v15:\n",
		"  Error: Cannot find module '/app/server.js'\n",
		"kx keeps waiting until v15 goes live, fails or reaches the 18m timeout; the deploy continues either way.\n",
		"Until v15 goes live or fails, a new deploy of shop-api waits for it.\n",
		"✗ Failed    v15  queued → starting → failed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "old version") {
		t.Fatalf("showed the serving version's log lines:\n%s", out)
	}
	if m := stopRe.FindStringSubmatch(out); m == nil || !strings.Contains(out, "not a workflow command\n::"+m[1]+"::\n") {
		t.Fatalf("app log lines aren't inside a stop-commands block:\n%s", out)
	}
}

// On a terminal, kx says how to stop waiting.
func TestRun_NeverReadySaysCtrlCOnATerminal(t *testing.T) {
	e := newEnv(t)
	e.p.InTTY, e.p.TTY = true, true
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("pending", 40, "deploying", "failed")
	e.api.health = defaultHealth()
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	_, _ = e.run(e.options())
	if !strings.Contains(e.errOut.String(), "kx keeps waiting; press Ctrl-C to stop, the deploy continues.\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

// The early signal never fails a deploy on its own: a container that gets
// ready on a later start, after the signal, is a success.
func TestRun_ReadyAfterTheSignalIsASuccess(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("pending", 34, "deploying", "ready") // live at about 170 s
	e.api.health = defaultHealth()
	e.api.replicas = func(n int) []api.Replica {
		if n < 16 {
			return []api.Replica{oldReplica, newReplica("Running", false)}
		}
		return []api.Replica{oldReplica, newReplica("Running", true)}
	}
	o := e.options()
	w := e.watchWarnings(&o)
	res, err := e.run(o)
	if err != nil || res.Status != "live" || res.Version != 15 {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if len(w.at) != 1 {
		t.Fatalf("early signals at %v, want one before it went live", w.at)
	}
	if !strings.Contains(e.errOut.String(), "✓ Live      v15  queued → starting → live") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

// At the timeout kx exits 9 and repeats the likely cause it saw.
func TestRun_NeverReadyUntilTheTimeout(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = []string{"pending", "deploying"}
	e.api.health = defaultHealth()
	e.api.replicas = func(int) []api.Replica { return []api.Replica{oldReplica, newReplica("Running", false)} }
	o := e.options()
	o.Timeout = 5 * time.Minute
	_, err := e.run(o)
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || wt.NotStarting == nil {
		t.Fatalf("Run() = %#v, want a timeout with the likely cause", err)
	}
	if p := api.Explain(err); p.Exit != api.ExitTimeout || p.What != "shop-api v15 isn't live after 5 minutes: its container keeps crashing or isn't answering health checks on / at port 8080, so kx stopped waiting; the deploy continues." {
		t.Fatalf("problem = %+v", p)
	}
}

// Pending (pulling the image, waiting for room) and a few slow seconds
// before the first answer are a slow start: no signal at all.
func TestRun_SlowStartIsNotAStall(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("pending", 100, "deploying", "ready")
	e.api.health = defaultHealth()
	e.api.replicas = func(n int) []api.Replica {
		switch {
		case n < 20:
			return []api.Replica{oldReplica, newReplica("Pending", false)}
		case n < 30: // running and not ready for about 100 s, less than the 2 minutes allowed
			return []api.Replica{oldReplica, newReplica("Running", false)}
		}
		return []api.Replica{oldReplica, newReplica("Running", true)}
	}
	o := e.options()
	w := e.watchWarnings(&o)
	res, err := e.run(o)
	if err != nil || res.Status != "live" || len(w.at) != 0 || strings.Contains(e.errOut.String(), "isn't live yet") {
		t.Fatalf("Run() = %+v, %v, signals %v\n%s", res, err, w.at, e.errOut.String())
	}
	if e.api.replicaGets < 30 {
		t.Fatalf("looked at replicas %d times", e.api.replicaGets)
	}
}

// The app's own health checks set the window: a startup check that allows
// five minutes means kx waits that long, plus the other checks and a
// minute, before it says anything.
func TestRun_SignalWindowFollowsTheHealthChecks(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("deploying", 120, "deploying", "failed")
	hc := defaultHealth()
	hc.Startup = &api.Probe{HTTPGet: &api.HTTPGet{Path: "/started", Port: 8080}, PeriodSeconds: 10, FailureThreshold: 30}
	e.api.health = hc
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	o := e.options()
	w := e.watchWarnings(&o)
	_, _ = e.run(o)
	if len(w.at) != 1 || w.at[0] < 7*time.Minute || w.at[0] > 8*time.Minute {
		t.Fatalf("signals at %v, want one after about 7 minutes (5 min startup + 1 min + 1 min)", w.at)
	}
}

// A deployment that records no health checks gets kx's default window
// (about two minutes), and the cause names its port but no path.
func TestRun_NoRecordedHealthCheckMeansTheDefaults(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("deploying", 60, "deploying", "failed")
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	o := e.options()
	w := e.watchWarnings(&o)
	_, err := e.run(o)
	if len(w.at) != 1 || w.at[0] > 3*time.Minute {
		t.Fatalf("signals at %v", w.at)
	}
	if p := api.Explain(err); !strings.Contains(p.What, "didn't answer health checks at port 8080.") {
		t.Fatalf("problem = %+v", p)
	}
}

// The platform checks the deployment's port, whatever port an older health
// check stored (for example after the app's port was changed).
func TestRun_SignalNamesTheDeploymentsPort(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("deploying", 40, "deploying", "failed")
	hc := defaultHealth()
	hc.Readiness.HTTPGet = &api.HTTPGet{Path: "/ready", Port: 3000}
	hc.Liveness.HTTPGet = &api.HTTPGet{Path: "/ready", Port: 3000}
	e.api.health = hc
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	o := e.options()
	w := e.watchWarnings(&o)
	_, err := e.run(o)
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.NotStarting == nil || df.NotStarting.Port != 8080 || df.NotStarting.Path != "/ready" {
		t.Fatalf("Run() = %#v", err)
	}
	if len(w.whats) != 1 || !strings.HasSuffix(w.whats[0], "on /ready at port 8080.") || strings.Contains(e.errOut.String(), "3000") {
		t.Fatalf("signal %q\n%s", w.whats, e.errOut.String())
	}
}

// Without a usable answer about replicas (no read access, say) kx keeps
// to the deploy's timeout, gives no cause and stops asking.
func TestRun_ReplicasUnreadableKeepsTheTimeout(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"pending", "deploying"}
	e.api.replicasStatus = 403
	_, err := e.run(e.options())
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || wt.After != 18*time.Minute || wt.NotStarting != nil {
		t.Fatalf("Run() = %#v, want the timeout without a cause", err)
	}
	if e.api.replicaGets != 1 {
		t.Fatalf("asked for replicas %d times after a refusal", e.api.replicaGets)
	}
}

// Without the deployment's creation time kx can't tell new replicas from
// old ones (which may be crashing: that may be why someone deploys), so it
// doesn't judge them.
func TestRun_NoCreationTimeNoSignal(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"deploying"}
	e.api.created = time.Time{}
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	_, err := e.run(e.options())
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || wt.NotStarting != nil || e.api.replicaGets != 0 {
		t.Fatalf("Run() = %v after %d replica checks, want the timeout and none", err, e.api.replicaGets)
	}
}

// Replica data that can't be trusted gives no cause rather than a wrong
// one: replicas of the serving version that are running but not ready
// mean the ready condition is missing (the metrics the API reads lag or
// lack it), and replicas without a phase or start time are skipped.
func TestRun_UntrustworthyReplicaDataGivesNoCause(t *testing.T) {
	for name, reps := range map[string][]api.Replica{
		"no ready data": {{Name: "old", Status: "Running", Ready: false, StartedAt: "2026-11-12T08:00:00Z"}, newReplica("Running", false)},
		"no phase":      {oldReplica, newReplica("Unknown", false)},
		"no start time": {oldReplica, {Name: "new", Status: "Running", Ready: false}},
	} {
		e := newEnv(t)
		e.clock.step = 5 * time.Second
		e.api.statuses = repeat("deploying", 60, "deploying", "failed")
		e.api.failReason = platformGeneric
		e.api.health = defaultHealth()
		e.api.replicas = func(int) []api.Replica { return reps }
		o := e.options()
		w := e.watchWarnings(&o)
		_, err := e.run(o)
		var df *kxerr.DeployFailed
		if !errors.As(err, &df) || df.NotStarting != nil || len(w.at) != 0 {
			t.Fatalf("%s: Run() = %#v, signals %v", name, err, w.at)
		}
	}
}

// A cause that stops being true (the replica got ready) is dropped, so a
// later failure for another reason gets the platform's reason.
func TestRun_CauseClearedWhenTheReplicaGetsReady(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("deploying", 50, "deploying", "failed")
	e.api.failReason = "Your pod ran out of memory."
	e.api.health = defaultHealth()
	e.api.replicas = func(n int) []api.Replica {
		if n < 16 {
			return []api.Replica{newReplica("Running", false)}
		}
		return []api.Replica{newReplica("Running", true)}
	}
	o := e.options()
	w := e.watchWarnings(&o)
	_, err := e.run(o)
	var df *kxerr.DeployFailed
	if len(w.at) != 1 || !errors.As(err, &df) || df.NotStarting != nil || df.Reason != "Your pod ran out of memory." {
		t.Fatalf("signals %v, Run() = %#v", w.at, err)
	}
}

// A rollback that doesn't get ready never advises kx deploy, which would
// redeploy the folder being rolled back from.
func TestRollback_NeverReadyAdvisesAnotherVersion(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.setDeployments(
		api.Deployment{ID: "dep-15", Version: 15, Status: "ready", ImageURL: img("15")},
		api.Deployment{ID: "dep-14", Version: 14, Status: "ready", ImageURL: img("14")},
	)
	e.api.statuses = repeat("deploying", 40, "deploying", "failed")
	e.api.health = defaultHealth()
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	_, err := Rollback(t.Context(), e.client, e.rollbackOptions(), *e.p)
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.NotStarting == nil || !df.NotStarting.Rollback {
		t.Fatalf("Rollback() = %#v", err)
	}
	p := api.Explain(err)
	out := e.errOut.String()
	if p.Fix != "Pick another version to roll back to: kx status" || !strings.Contains(out, "  If it doesn't go live, pick another version to roll back to: kx status\n") {
		t.Fatalf("problem %+v\n%s", p, out)
	}
	if strings.Contains(out+p.Fix, "kx deploy") {
		t.Fatalf("advised kx deploy for a rollback:\n%s\n%s", out, p.Fix)
	}
}

// When Kubernetix starts the deployment again, kx says so once per attempt
// rather than spinning in silence.
func TestRun_SaysWhenKubernetixTriesAgain(t *testing.T) {
	e := newEnv(t)
	e.api.statuses = repeat("pending", 6, "deploying", "ready")
	e.api.retries = []int{0, 0, 0, 1, 1, 1, 2, 2}
	res, err := e.run(e.options())
	if err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	out := e.errOut.String()
	if strings.Count(out, "v15 didn't go live on attempt 1; Kubernetix is trying again (attempt 2).\n") != 1 ||
		strings.Count(out, "v15 didn't go live on attempt 2; Kubernetix is trying again (attempt 3).\n") != 1 {
		t.Fatalf("stderr:\n%s", out)
	}
}

// Log lines come from the stuck replica when the log stream names pods.
// When it doesn't, kx can't tell the new version's lines from an earlier
// one's, so it shows none of them: the deployment's own log, or that the
// version printed nothing.
func TestRun_SignalLogLinesFallBack(t *testing.T) {
	setup := func(t *testing.T, reps ...api.Replica) *env {
		e := newEnv(t)
		e.clock.step = 5 * time.Second
		e.api.statuses = repeat("deploying", 30, "deploying", "failed")
		e.api.health = &api.HealthCheck{} // no health checks: only crashing keeps it from ready
		e.api.replicas = func(int) []api.Replica { return reps }
		return e
	}

	// Named: only the stuck replica's lines, as the new version's.
	e := setup(t, oldReplica, newReplica("Running", false))
	e.api.runtimeLogs = streams(map[string][]string{"app-shop-8e1a-new": {"panic: missing DATABASE_URL"}, "app-shop-7d9f-old": {"GET / 200"}})
	o := e.options()
	w := e.watchWarnings(&o)
	_, err := e.run(o)
	if out := e.errOut.String(); !strings.Contains(out, "The last log line of v15:\n  panic: missing DATABASE_URL\n") || strings.Contains(out, "GET / 200") {
		t.Fatalf("named streams:\n%s", out)
	}
	if len(w.whats) != 1 || w.whats[0] != "shop-api v15 isn't live yet: for 90 seconds its container has kept stopping right after it started." {
		t.Fatalf("signal = %q", w.whats)
	}
	if p := api.Explain(err); p.What != "shop-api v15 didn't go live: its container kept stopping right after it started. Version 14 is still serving." {
		t.Fatalf("problem = %+v", p)
	}

	// Unnamed, with and without an earlier version seen running.
	for _, reps := range [][]api.Replica{{oldReplica, newReplica("Running", false)}, {newReplica("Running", false)}} {
		e = setup(t, reps...)
		e.api.runtimeLogs = streams(map[string][]string{"": {"panic: missing DATABASE_URL"}})
		_, _ = e.run(e.options())
		if out := e.errOut.String(); strings.Contains(out, "panic: missing DATABASE_URL") || !strings.Contains(out, "No output from v15's container.\n") {
			t.Fatalf("unnamed streams, %d replicas:\n%s", len(reps), out)
		}
	}

	// No runtime logs: the deployment's own log.
	e = setup(t, newReplica("Running", false))
	e.api.logs["key-"+testKey] = api.DeploymentLogs{DeploymentLogs: "boot failed: port 8080 in use\n"}
	_, _ = e.run(e.options())
	if !strings.Contains(e.errOut.String(), "The last log line of v15:\n  boot failed: port 8080 in use\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

// A deployment that records no health check names no path: kx can't know
// which check Kubernetix used.
func TestNotLiveYet_NoRecordedHealthCheck_NamesNoPath(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = repeat("pending", 60, "deploying", "failed")
	e.api.failReason = platformGeneric
	e.api.replicas = func(int) []api.Replica { return []api.Replica{oldReplica, newReplica("Running", false)} }
	o := e.options()
	w := e.watchWarnings(&o)
	_, err := e.run(o)
	if len(w.whats) != 1 || !strings.Contains(w.whats[0], "at port 8080") || strings.Contains(w.whats[0], " on /") {
		t.Fatalf("signal = %q", w.whats)
	}
	if w.fixes[0] != "Make your app start and answer health checks at port 8080; the log lines below may say why it hasn't." {
		t.Fatalf("fix = %q", w.fixes[0])
	}
	if p := api.Explain(err); p.What != "shop-api v15 didn't go live: its container kept crashing or didn't answer health checks at port 8080. Version 14 is still serving." {
		t.Fatalf("problem = %+v", p)
	}
}

// After a version fails, kx shows that version's own log lines: never the
// still-serving version's, and never the health probes' and metrics
// scrapers' requests. A version that printed nothing says so.
func TestRun_FailureShowsTheFailedVersionsOwnLogs(t *testing.T) {
	const (
		probe  = `10.8.0.1 - - [12/Nov/2026:09:00:41 +0000] "GET /healthz HTTP/1.1" 200 2 "-" "kube-probe/1.31"`
		scrape = `10.8.0.7 - - [12/Nov/2026:09:00:42 +0000] "GET /metrics HTTP/1.1" 404 153 "-" "vm_promscrape"`
	)
	failFast := func(t *testing.T) *env {
		e := newEnv(t)
		e.api.statuses = []string{"pending", "deploying", "failed"} // fails before kx sees a stuck replica
		e.api.failReason = "Your container is crashing on start. Check the deployment logs for the error message and verify your start command."
		e.api.replicas = func(int) []api.Replica { return []api.Replica{oldReplica} } // the failed pods are gone
		return e
	}

	// Named streams: the pods that aren't the serving version's are the
	// failed version's.
	e := failFast(t)
	e.api.runtimeLogs = streams(map[string][]string{
		"app-shop-7d9f-old": {probe, `10.8.0.3 - - "GET / HTTP/1.1" 200 612`, scrape},
		"app-shop-8e1a-new": {"starting", probe, "panic: missing DATABASE_URL"},
	})
	_, _ = e.run(e.options())
	out := e.errOut.String()
	if !strings.Contains(out, "The last 2 log lines of v15:\n  starting\n  panic: missing DATABASE_URL\n") ||
		strings.Contains(out, "kube-probe") || strings.Contains(out, "vm_promscrape") || strings.Contains(out, `"GET / HTTP/1.1"`) {
		t.Fatalf("stderr:\n%s", out)
	}

	// Unnamed streams: the deployment's own log, without the noise.
	e = failFast(t)
	e.api.runtimeLogs = streams(map[string][]string{"": {`10.8.0.3 - - "GET / HTTP/1.1" 200 612`}})
	e.api.logs["key-"+testKey] = api.DeploymentLogs{DeploymentLogs: probe + "\nnginx: [emerg] mkdir() \"/var/cache/nginx\" failed (13: Permission denied)\n" + scrape + "\n"}
	_, _ = e.run(e.options())
	out = e.errOut.String()
	if !strings.Contains(out, "The last log line of v15:\n  nginx: [emerg] mkdir() \"/var/cache/nginx\" failed (13: Permission denied)\n") ||
		strings.Contains(out, "kube-probe") || strings.Contains(out, "GET / HTTP") {
		t.Fatalf("stderr:\n%s", out)
	}

	// Only noise, or nothing: the version printed nothing kx can show.
	e = failFast(t)
	e.api.logs["key-"+testKey] = api.DeploymentLogs{DeploymentLogs: probe + "\n" + scrape + "\n"}
	_, _ = e.run(e.options())
	if out := e.errOut.String(); !strings.Contains(out, "No output from v15's container.\n") || strings.Contains(out, "kube-probe") {
		t.Fatalf("stderr:\n%s", out)
	}

	// Logs kx can't read: it says nothing about them.
	e = failFast(t)
	e.api.logsStatus = 503
	e.api.runtimeStatus = 503
	_, _ = e.run(e.options())
	if out := e.errOut.String(); strings.Contains(out, "No output") || strings.Contains(out, "log line") {
		t.Fatalf("stderr:\n%s", out)
	}
}
