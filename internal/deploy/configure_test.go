package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kubernetix-ai/kx/internal/api"
)

// configureOptions deploys shop-api as configured after save.
func (e *env) configureOptions(save func(context.Context) error) Options {
	return Options{
		App:            &api.App{ID: testAppID, Name: "shop-api"},
		Org:            "acme-corp",
		Save:           save,
		IdempotencyKey: testKey,
		Now:            e.clock.Now,
		Sleep:          e.clock.Sleep,
	}
}

func TestRun_SaveThenDeployAsConfigured(t *testing.T) {
	e := newEnv(t)
	var order []string
	save := func(context.Context) error {
		order = append(order, "save after "+strings.Join(paths(e.api.requests), ","))
		return nil
	}
	res, err := e.run(e.configureOptions(save))
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, e.errOut.String())
	}
	// Every check comes first (the app, an earlier deploy, today's
	// deploys), then the change is saved, then the app is deployed.
	if len(order) != 1 || order[0] != "save after GET /apps/"+testAppID+",GET /apps/"+testAppID+"/deployments,GET /orgs/acme-corp/usage" {
		t.Fatalf("save ran %q", order)
	}
	posts := e.api.reqs("POST", "/apps/"+testAppID+"/deployments")
	if len(posts) != 1 || len(posts[0].Body) != 0 || posts[0].Key != testKey {
		t.Fatalf("deploy request = %+v, want an empty body: the app as configured", posts)
	}
	if len(e.builder.got()) != 0 {
		t.Fatalf("docker ran: %q", e.builder.got())
	}
	if res.Status != "live" || res.Version != 15 {
		t.Fatalf("result = %+v", res)
	}
	out := e.errOut.String()
	if strings.Contains(out, "Checked") || strings.Contains(out, "Built") || !strings.Contains(out, "✓ Live      v15  queued → starting → live\n") {
		t.Fatalf("stderr:\n%s", out)
	}
}

func TestRun_SaveFailureDeploysNothing(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("refused")
	_, err := e.run(e.configureOptions(func(context.Context) error { return boom }))
	if !errors.Is(err, boom) || len(e.api.reqs("POST", "/apps/")) != 0 {
		t.Fatalf("Run() = %v; want the save's error and no deploy", err)
	}
}

func TestRun_NothingSavedWhenTheDeployCantHappen(t *testing.T) {
	saved := false
	save := func(context.Context) error { saved = true; return nil }

	e := newEnv(t)
	e.api.apps[testAppID].Regions = []api.AppRegion{{Region: "us-central1"}, {Region: "europe-west1"}}
	if _, err := e.run(e.configureOptions(save)); err == nil || saved {
		t.Fatalf("Run() = %v, saved %v; a multi-region app can't be deployed by kx, so nothing is saved", err, saved)
	}

	e = newEnv(t)
	e.api.usage = &api.Usage{OrgID: "acme-corp", Plan: "free", Usage: map[string]api.UsageItem{"deployments_today": {Current: 5, Limit: 5}}}
	if _, err := e.run(e.configureOptions(save)); err == nil || saved {
		t.Fatalf("Run() = %v, saved %v; with no deploys left today nothing is saved", err, saved)
	}
}

func TestRun_SaveRejectsBadOptions(t *testing.T) {
	e := newEnv(t)
	save := func(context.Context) error { return nil }
	bad := []func(*Options){
		func(o *Options) { o.Image = "registry.example.test/acme-corp/shop-api@" + testDigest },
		func(o *Options) { o.App, o.NewApp = nil, "web" },
		func(o *Options) { o.Build = &Dockerfile{Dir: "/code", Path: "/code/Dockerfile"} },
		func(o *Options) { o.VersionTag = "1.0.0" },
		func(o *Options) { o.Repository = "shop-frontend" },
		func(o *Options) { o.Provenance = &Provenance{Revision: testRevision} },
	}
	for i, mutate := range bad {
		o := e.configureOptions(save)
		mutate(&o)
		if _, err := e.run(o); err == nil {
			t.Errorf("case %d: Run() accepted bad options", i)
		}
	}
	if len(e.api.requests) != 0 {
		t.Fatal("made API calls with bad options")
	}
}

func paths(reqs []request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}
