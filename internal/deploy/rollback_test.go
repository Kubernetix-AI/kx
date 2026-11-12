package deploy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

func img(n string) string {
	return "registry.example.test/acme-corp/shop-api@sha256:" + strings.Repeat(n, 32)
}

func (e *env) setDeployments(ds ...api.Deployment) {
	for i := range ds {
		ds[i].AppID = testAppID
	}
	e.api.deployments[testAppID] = ds
}

func (e *env) rollbackOptions() RollbackOptions {
	return RollbackOptions{
		App: &api.App{ID: testAppID, Name: "shop-api"}, Org: "acme-corp",
		IdempotencyKey: testKey, Now: e.clock.Now, Sleep: e.clock.Sleep,
	}
}

func TestRollback_NewestEarlierReadyWithDifferentImage(t *testing.T) {
	e := newEnv(t)
	e.setDeployments(
		api.Deployment{ID: "dep-16", Version: 16, Status: "failed", ImageURL: img("16")},
		api.Deployment{ID: "dep-15", Version: 15, Status: "ready", ImageURL: img("15")},
		api.Deployment{ID: "dep-14", Version: 14, Status: "ready", ImageURL: img("15")}, // same image as live
		api.Deployment{ID: "dep-13", Version: 13, Status: "failed", ImageURL: img("13")},
		api.Deployment{ID: "dep-12", Version: 12, Status: "ready", ImageURL: img("12")},
	)
	var asked *RollbackPlan
	o := e.rollbackOptions()
	o.Confirm = func(p *RollbackPlan) (bool, error) { asked = p; return true, nil }
	res, err := Rollback(t.Context(), e.client, o, *e.p)
	if err != nil {
		t.Fatalf("Rollback() = %v\n%s", err, e.errOut.String())
	}
	if asked == nil || asked.From.Version != 15 || asked.To.Version != 12 {
		t.Fatalf("plan = %+v", asked)
	}
	posts := e.api.reqs("POST", "/deployments/")
	if len(posts) != 1 || posts[0].Path != "/deployments/dep-12/rollback" || posts[0].Key != testKey {
		t.Fatalf("rollback requests = %+v", posts)
	}
	if res.Version != 17 || res.Status != "live" || res.Image != img("12") || res.URL == "" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(e.errOut.String(), "✓ Started   v17 with the image of v12\n✓ Live      v17  queued → starting → live\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRollback_To(t *testing.T) {
	e := newEnv(t)
	e.setDeployments(
		api.Deployment{ID: "dep-15", Version: 15, Status: "ready", ImageURL: img("15")},
		api.Deployment{ID: "dep-14", Version: 14, Status: "failed", ImageURL: img("14")},
		api.Deployment{ID: "dep-13", Version: 13, Status: "ready", ImageURL: img("13")},
		api.Deployment{ID: "dep-12", Version: 12, Status: "ready", ImageURL: img("12")},
	)
	o := e.rollbackOptions()
	o.To = 12
	if _, err := Rollback(t.Context(), e.client, o, *e.p); err != nil {
		t.Fatal(err)
	}
	if p := e.api.reqs("POST", "/deployments/"); len(p) != 1 || p[0].Path != "/deployments/dep-12/rollback" {
		t.Fatalf("requests = %+v", p)
	}
}

func TestRollback_Refusals(t *testing.T) {
	live := api.Deployment{ID: "dep-15", Version: 15, Status: "ready", ImageURL: img("15")}
	tests := []struct {
		name string
		deps []api.Deployment
		to   int
		want any
	}{
		{"nothing live", []api.Deployment{{ID: "d1", Version: 1, Status: "failed"}}, 0, &kxerr.NothingLive{}},
		{"nothing earlier", []api.Deployment{live, {ID: "d14", Version: 14, Status: "ready", ImageURL: img("15")}}, 0, &kxerr.NoRollbackTarget{}},
		{"to the live version", []api.Deployment{live}, 15, &kxerr.AlreadyLive{}},
		{"to a failed version", []api.Deployment{live, {ID: "d14", Version: 14, Status: "failed", ImageURL: img("14")}}, 14, &kxerr.NotRollbackable{}},
		{"to a missing version", []api.Deployment{live}, 99, &kxerr.NoVersion{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.setDeployments(tt.deps...)
			o := e.rollbackOptions()
			o.To = tt.to
			_, err := Rollback(t.Context(), e.client, o, *e.p)
			if err == nil || !sameType(err, tt.want) {
				t.Fatalf("Rollback() = %v (%T), want %T", err, err, tt.want)
			}
			if len(e.api.reqs("POST", "/")) != 0 {
				t.Fatal("posted a rollback anyway")
			}
		})
	}
}

func sameType(err error, want any) bool {
	switch want.(type) {
	case *kxerr.NothingLive:
		var x *kxerr.NothingLive
		return errors.As(err, &x) && x.App == "shop-api"
	case *kxerr.NoRollbackTarget:
		var x *kxerr.NoRollbackTarget
		return errors.As(err, &x) && x.App == "shop-api"
	case *kxerr.AlreadyLive:
		var x *kxerr.AlreadyLive
		return errors.As(err, &x) && x.Version == 15
	case *kxerr.NotRollbackable:
		var x *kxerr.NotRollbackable
		return errors.As(err, &x) && x.Version == 14
	case *kxerr.NoVersion:
		var x *kxerr.NoVersion
		return errors.As(err, &x) && x.Version == "v99"
	}
	return false
}

func TestRollback_Declined(t *testing.T) {
	e := newEnv(t)
	o := e.rollbackOptions()
	o.Confirm = func(*RollbackPlan) (bool, error) { return false, nil }
	_, err := Rollback(t.Context(), e.client, o, *e.p)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("Rollback() = %v, want ErrDeclined", err)
	}
	if len(e.api.reqs("POST", "/")) != 0 {
		t.Fatal("rolled back after the person said no")
	}
}

func TestRollback_WaitsForPreviousFirst(t *testing.T) {
	e := newEnv(t)
	inFlight := api.Deployment{ID: "dep-15", AppID: testAppID, Version: 15, Status: "deploying", ImageURL: img("15")}
	e.api.lists = [][]api.Deployment{{inFlight}}
	if _, err := Rollback(t.Context(), e.client, e.rollbackOptions(), *e.p); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.errOut.String(), "Waiting for v15 to finish first\n") {
		t.Fatalf("stderr:\n%s", e.errOut.String())
	}
}

func TestRollback_MultiRegionRefused(t *testing.T) {
	e := newEnv(t)
	e.api.apps[testAppID].Regions = []api.AppRegion{{Region: "us"}}
	_, err := Rollback(t.Context(), e.client, e.rollbackOptions(), *e.p)
	var mr *kxerr.MultiRegion
	if !errors.As(err, &mr) {
		t.Fatalf("Rollback() = %v", err)
	}
}

func TestRollback_NeverRetriesThePost(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		e := newEnv(t)
		e.api.deployments[testAppID][1].ImageURL = img("13")
		e.api.rollbackStatus = status
		retries := e.client.MaxRetries
		_, err := Rollback(t.Context(), e.client, e.rollbackOptions(), *e.p)
		var ru *kxerr.RollbackUnconfirmed
		if !errors.As(err, &ru) || ru.App != "shop-api" || ru.Version != 13 {
			t.Fatalf("%d: Rollback() = %v, want RollbackUnconfirmed for v13", status, err)
		}
		if n := len(e.api.reqs("POST", "/deployments/")); n != 1 {
			t.Fatalf("%d: sent the rollback %d times, want exactly once", status, n)
		}
		if e.client.MaxRetries != retries {
			t.Fatalf("the client's retries stayed changed: %d", e.client.MaxRetries)
		}
	}
}

func TestRollback_TimeoutIsUnconfirmed(t *testing.T) {
	e := newEnv(t)
	e.api.deployments[testAppID][1].ImageURL = img("13")
	e.api.rollbackDelay = 300 * time.Millisecond
	e.client.HTTPClient.Timeout = 50 * time.Millisecond
	_, err := Rollback(t.Context(), e.client, e.rollbackOptions(), *e.p)
	var ru *kxerr.RollbackUnconfirmed
	if !errors.As(err, &ru) {
		t.Fatalf("Rollback() = %v, want RollbackUnconfirmed", err)
	}
	if n := len(e.api.reqs("POST", "/deployments/")); n != 1 {
		t.Fatalf("sent the rollback %d times", n)
	}
	p := api.Explain(err)
	if p.Code != "rollback_unconfirmed" || p.Fix != "Check: kx status" || p.Exit != 1 {
		t.Fatalf("problem = %+v", p)
	}
}

func TestRollback_ClearRefusalIsNotUnconfirmed(t *testing.T) {
	e := newEnv(t)
	e.api.deployments[testAppID][1].ImageURL = img("13")
	e.api.rollbackStatus = 400
	_, err := Rollback(t.Context(), e.client, e.rollbackOptions(), *e.p)
	var ru *kxerr.RollbackUnconfirmed
	if errors.As(err, &ru) || err == nil {
		t.Fatalf("Rollback() = %v, want the API's own error", err)
	}
}
