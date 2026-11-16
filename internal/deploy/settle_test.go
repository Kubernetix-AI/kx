package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// genericRolledBack is the platform's generic message, which a version
// can carry for a moment while marked rolled_back, before its final status.
const genericRolledBack = "Deployment failed. The platform team has been alerted; please retry, or contact support if the issue persists."

// A rolled_back seen on one poll, followed by failed, is reported as the
// failure with its own message, not as a rollback with the generic one.
func TestRun_MomentaryRolledBackThenFailedReportsTheFailure(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 2 * time.Second
	e.api.statuses = []string{"pending", "deploying", "rolled_back", "failed"}
	e.api.rolledBackReason = genericRolledBack
	e.api.failReason = "your container is crashing on start"
	_, err := e.run(e.options())
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.Reason != "your container is crashing on start" {
		t.Fatalf("Run() = %#v, want DeployFailed with the failure's own reason", err)
	}
	s := e.errOut.String()
	if !strings.Contains(s, "✗ Failed    v15  queued → starting → failed") || strings.Contains(s, "rolled back") {
		t.Fatalf("stderr:\n%s", s)
	}
	if p := api.Explain(err); p.What != "shop-api v15 didn't go live: your container is crashing on start. Version 14 is still serving." || p.Exit != api.ExitDeploy {
		t.Fatalf("problem = %+v", p)
	}
}

// A rolled_back seen for a moment before Kubernetix tries the version
// again doesn't end the wait either.
func TestRun_MomentaryRolledBackThenAnotherAttempt(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 2 * time.Second
	e.api.statuses = []string{"pending", "deploying", "rolled_back", "pending", "deploying", "ready"}
	e.api.retries = []int{0, 0, 0, 1}
	e.api.rolledBackReason = genericRolledBack
	res, err := e.run(e.options())
	if err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	s := e.errOut.String()
	if !strings.Contains(s, "v15 didn't go live on attempt 1; Kubernetix is trying again (attempt 2).") || strings.Contains(s, "rolled back") {
		t.Fatalf("stderr:\n%s", s)
	}
}

// A rolled_back that is still there on a poll at least 10 seconds after kx
// first saw it is final: kx stops and reports it, without waiting for the
// timeout.
func TestRun_RolledBackThatLastsIsFinal(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 2 * time.Second
	e.api.statuses = []string{"pending", "deploying", "rolled_back"}
	e.api.rolledBackReason = genericRolledBack
	_, err := e.run(e.options())
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.Reason != genericRolledBack {
		t.Fatalf("Run() = %#v, want DeployFailed", err)
	}
	gets := 0
	for _, r := range e.api.reqs("GET", "/deployments/") {
		if r.Path == "/deployments/key-"+testKey {
			gets++
		}
	}
	// Polls at 2 s (pending), 4 s (deploying), 6 s (first rolled_back), then
	// every 2 s until 16 s: the eighth poll is 10 s after the first sighting.
	if gets != 8 {
		t.Fatalf("polled %d times, want 8 (rolled_back confirmed 10 s after it was first seen)", gets)
	}
	if s := e.errOut.String(); !strings.Contains(s, "✗ Failed    v15  queued → starting → rolled back") {
		t.Fatalf("stderr:\n%s", s)
	}
}

// Without a message, a lasting rolled_back still says what happened.
func TestRun_RolledBackThatLastsWithoutAMessage(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	e.api.statuses = []string{"pending", "rolled_back"}
	_, err := e.run(e.options())
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) || df.Reason != "Kubernetix rolled it back" {
		t.Fatalf("Run() = %#v", err)
	}
}

// A rolled_back first seen just before the timeout is confirmed (or not)
// by the polls it needs, so kx reports it instead of "still starting".
func TestRun_RolledBackAtTheTimeoutIsStillConfirmed(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"pending", "deploying", "rolled_back"}
	o := e.options()
	o.Timeout = 3 * time.Minute
	_, err := e.run(o)
	var df *kxerr.DeployFailed
	if !errors.As(err, &df) {
		t.Fatalf("Run() = %#v, want DeployFailed", err)
	}

	// The same with failed after it: the failure, not a timeout.
	e = newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"pending", "deploying", "rolled_back", "failed"}
	e.api.failReason = "the image could not be pulled"
	o = e.options()
	o.Timeout = 3 * time.Minute
	if _, err := e.run(o); !errors.As(err, &df) || df.Reason != "the image could not be pulled" {
		t.Fatalf("Run() = %#v, want DeployFailed with its reason", err)
	}

	// A new attempt after it is a deploy that's still starting: timeout.
	e = newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"pending", "deploying", "rolled_back", "pending"}
	e.api.retries = []int{0, 0, 0, 1}
	o = e.options()
	o.Timeout = 3 * time.Minute
	var wt *kxerr.WaitTimeout
	if _, err := e.run(o); !errors.As(err, &wt) {
		t.Fatalf("Run() = %#v, want WaitTimeout", err)
	}
}

// Confirming a rolled_back never keeps kx waiting long past the timeout,
// even when every poll after it is rate limited.
func TestRun_RolledBackAtTheTimeoutDoesntWaitForever(t *testing.T) {
	e := newEnv(t)
	e.clock.step = time.Minute
	e.api.statuses = []string{"pending", "deploying", "rolled_back"}
	e.clock.hook = func(n int) error {
		if n == 4 { // after the poll that first saw rolled_back
			e.api.mu.Lock()
			e.api.getLimited = 1000
			e.api.mu.Unlock()
		}
		return nil
	}
	o := e.options()
	o.Timeout = 3 * time.Minute
	_, err := e.run(o)
	var wt *kxerr.WaitTimeout
	if !errors.As(err, &wt) || len(e.clock.sleeps) != 5 {
		t.Fatalf("Run() = %#v after %d sleeps, want WaitTimeout after 5", err, len(e.clock.sleeps))
	}
}

// The check for an earlier deploy of the app uses the same rule: a
// rolled_back seen for a moment, then a new attempt, is still in progress,
// so kx doesn't start another deploy until it ends.
func TestWaitForPrevious_MomentaryRolledBackKeepsWaiting(t *testing.T) {
	e := newEnv(t)
	prev := func(status string, retry int) api.Deployment {
		return api.Deployment{ID: "dep-15", Version: 15, Status: status, RetryCount: retry}
	}
	e.api.lists = [][]api.Deployment{{prev("deploying", 0)}, {prev("rolled_back", 0)}, {prev("pending", 1)}, {prev("deploying", 1)}, {prev("failed", 1)}}
	restore := setDefaultSleep(func(ctx context.Context, d time.Duration) error { return ctx.Err() })
	defer restore()
	if err := WaitForPrevious(context.Background(), e.client, testAppID, *e.p); err != nil {
		t.Fatal(err)
	}
	if e.api.listCalls != 5 {
		t.Fatalf("listed deployments %d times, want 5 (until v15 failed)", e.api.listCalls)
	}
}

// A rolled_back earlier deploy that stays so for 10 seconds has ended.
func TestWaitForPrevious_RolledBackThatLastsHasEnded(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 2 * time.Second
	rolled := api.Deployment{ID: "dep-15", Version: 15, Status: "rolled_back"}
	for range 20 {
		e.api.lists = append(e.api.lists, []api.Deployment{rolled})
	}
	start := e.clock.Now()
	if err := waitForPrevious(context.Background(), e.client, testAppID, e.p, e.clock.Sleep, e.clock.Now, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if waited := e.clock.Now().Sub(start); waited != 10*time.Second || e.api.listCalls != 6 {
		t.Fatalf("waited %v over %d looks, want 10s over 6", waited, e.api.listCalls)
	}
}

// kx deploy waits out an earlier deploy's momentary rolled_back before it
// starts its own: the deployment request comes after that deploy failed.
func TestRun_WaitsForAnEarlierDeployPastAMomentaryRolledBack(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 2 * time.Second
	prev := func(status string, retry int) api.Deployment {
		return api.Deployment{ID: "dep-15", AppID: testAppID, Version: 15, Status: status, RetryCount: retry}
	}
	e.api.lists = [][]api.Deployment{{prev("deploying", 0)}, {prev("rolled_back", 0)}, {prev("pending", 1)}, {prev("failed", 1)}}
	o := e.options()
	o.Image = "registry.example.test/acme-corp/shop-api@" + testDigest
	if res, err := e.run(o); err != nil || res.Status != "live" {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	lists := 0
	for _, r := range e.api.requests {
		if r.Method == "GET" && r.Path == "/apps/"+testAppID+"/deployments" {
			lists++
		}
		if r.Method == "POST" && r.Path == "/apps/"+testAppID+"/deployments" {
			break
		}
	}
	if lists != 4 {
		t.Fatalf("started the deploy after %d looks at the earlier one, want 4 (after it failed)", lists)
	}
}

// A rolled_back that lasts while kx checks for an earlier deploy, with
// rollback's own wait: the same 10 seconds.
func TestRollback_EarlierRolledBackThatLastsHasEnded(t *testing.T) {
	e := newEnv(t)
	e.clock.step = 5 * time.Second
	rolled := api.Deployment{ID: "dep-15", AppID: testAppID, Version: 15, Status: "rolled_back", ImageURL: img("15")}
	e.api.lists = [][]api.Deployment{{rolled}, {rolled}, {rolled}}
	if _, err := Rollback(t.Context(), e.client, e.rollbackOptions(), *e.p); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.errOut.String(), "Waiting for v15 to finish first\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}
