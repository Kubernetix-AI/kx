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

// A new version whose container keeps crashing (or never answers its
// health check) is reported after the health checks' window plus a
// minute, not after the 15-minute wait, with the reason, the new
// replica's own log lines and the version still serving.
func TestRun_ContainerThatNeverGetsReadyStopsEarly(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = []string{"pending", "deploying"}
	e.api.health = defaultHealth()
	e.api.replicas = func(int) []api.Replica { return []api.Replica{oldReplica, newReplica("Running", false)} }
	e.api.runtimeLogs = streams(map[string][]string{
		"app-shop-7d9f-old": {"GET / 200 from the old version"},
		"app-shop-8e1a-new": {"> node server.js", "Error: Cannot find module '/app/server.js'", "::error::not a workflow command"},
	})
	start := e.clock.Now()
	o := e.options()
	o.StopCommands = true
	res, err := e.run(o)

	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.NotStarting == nil || df.Version != 15 || df.ServingVersion != 14 {
		t.Fatalf("Run() = %#v, want DeployFailed for v15 that never got ready", err)
	}
	if took := e.clock.Now().Sub(start); took < 2*time.Minute || took > 3*time.Minute {
		t.Fatalf("stopped after %v, want about 2 minutes (60 s of health checks plus a minute)", took)
	}
	if n := df.NotStarting; n.After < 2*time.Minute || n.Path != "/" || n.Port != 8080 || !n.HealthCheck {
		t.Fatalf("NotStarting = %+v", n)
	}
	if res == nil || res.Version != 15 || res.ServingVersion != 14 {
		t.Fatalf("result = %+v", res)
	}
	p := api.Explain(err)
	if p.Exit != api.ExitDeploy || p.What != "shop-api v15 didn't go live: for 2 minutes its container kept crashing or wasn't answering health checks on / at port 8080. Version 14 is still serving." {
		t.Fatalf("problem = %+v", p)
	}
	out := e.errOut.String()
	for _, want := range []string{
		"✗ Failed    v15  queued → starting",
		"The last 3 log lines of v15:\n",
		"  Error: Cannot find module '/app/server.js'\n",
		"Kubernetix keeps trying v15 for a few more minutes; kx status shows how it ends.\n",
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

// Pending (pulling the image, waiting for room) and a few slow seconds
// before the first answer are a slow start, not a stall.
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
	res, err := e.run(e.options())
	if err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if e.api.replicaGets < 30 {
		t.Fatalf("looked at replicas %d times", e.api.replicaGets)
	}
}

// The app's own health checks set the window: a startup probe that allows
// five minutes means kx waits that long, plus the other checks and a
// minute, before calling it a stall.
func TestRun_StallWindowFollowsTheHealthChecks(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = []string{"deploying"}
	hc := defaultHealth()
	hc.Startup = &api.Probe{HTTPGet: &api.HTTPGet{Path: "/started", Port: 8080}, PeriodSeconds: 10, FailureThreshold: 30}
	e.api.health = hc
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	start := e.clock.Now()
	_, err := e.run(e.options())
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.NotStarting == nil {
		t.Fatalf("Run() = %v", err)
	}
	if took := e.clock.Now().Sub(start); took < 7*time.Minute || took > 8*time.Minute {
		t.Fatalf("stopped after %v, want about 7 minutes (5 min startup + 1 min + 1 min)", took)
	}
}

// Without a usable answer about replicas (no read access, say) kx keeps
// to the deploy's timeout and stops asking.
func TestRun_ReplicasUnreadableKeepsTheTimeout(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"pending", "deploying"}
	e.api.replicasStatus = 403
	_, err := e.run(e.options())
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || wt.After != 15*time.Minute {
		t.Fatalf("Run() = %v, want the timeout", err)
	}
	if e.api.replicaGets != 1 {
		t.Fatalf("asked for replicas %d times after a refusal", e.api.replicaGets)
	}
}

// Without the deployment's creation time kx can't tell new replicas from
// old ones (which may be crashing: that may be why someone deploys), so it
// doesn't judge them.
func TestRun_NoCreationTimeNoStall(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"deploying"}
	e.api.created = time.Time{}
	e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
	_, err := e.run(e.options())
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || e.api.replicaGets != 0 {
		t.Fatalf("Run() = %v after %d replica checks, want the timeout and none", err, e.api.replicaGets)
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

// Log lines come from the stuck replica when the log stream names pods;
// otherwise all recent lines, and without any, the deployment's own log.
func TestRun_NeverReadyLogLinesFallBack(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := newEnv(t)
		e.clock.step = 5 * time.Second
		e.api.statuses = []string{"deploying"}
		e.api.health = &api.HealthCheck{} // no health checks: only crashing keeps it from ready
		e.api.replicas = func(int) []api.Replica { return []api.Replica{newReplica("Running", false)} }
		return e
	}

	e := setup(t)
	e.api.runtimeLogs = streams(map[string][]string{"": {"panic: missing DATABASE_URL"}})
	_, err := e.run(e.options())
	if !strings.Contains(e.errOut.String(), "  panic: missing DATABASE_URL\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
	if p := api.Explain(err); p.What != "shop-api v15 didn't go live: for 2 minutes its container kept stopping right after it started. Version 14 is still serving." {
		t.Fatalf("problem = %+v", p)
	}

	e = setup(t)
	e.api.logs["key-"+testKey] = api.DeploymentLogs{DeploymentLogs: "boot failed: port 8080 in use\n"}
	_, _ = e.run(e.options())
	if !strings.Contains(e.errOut.String(), "  boot failed: port 8080 in use\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}
