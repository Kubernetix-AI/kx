package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/config"
)

// statusJSON runs kx status --json and decodes it.
func (h *harness) statusJSON() map[string]any {
	h.t.Helper()
	h.expect(h.run("status", "--json"), 0)
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		h.t.Fatalf("stdout is not one JSON value: %v\n%s", err, h.out.String())
	}
	return out
}

func TestStatus_JSONAlwaysHasTheNewestVersion(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.deployments[appShopID] = []api.Deployment{
		{ID: "dep-16", AppID: appShopID, Version: 16, Status: "ready"},
		{ID: "dep-15", AppID: appShopID, Version: 15, Status: "ready"},
	}
	out := h.statusJSON()
	if fmt.Sprint(out["latest"]) != "map[status:ready version:16]" || out["live_version"] != float64(16) {
		t.Fatalf("json = %v; the newest version is there even when it's the live one", out)
	}
	h.expect(h.run("status"), 0)
	if strings.Contains(h.out.String(), "Newest") {
		t.Fatalf("the table repeats the live version as newest:\n%s", h.out.String())
	}
}

func TestStatus_RolloutWording(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.apps[0].Replicas = 1
	h.api.deployments[appShopID] = []api.Deployment{
		{ID: "dep-4", AppID: appShopID, Version: 4, Status: "ready"},
		{ID: "dep-3", AppID: appShopID, Version: 3, Status: "ready"},
	}
	// The new version's replica is ready; the old one is shutting down.
	h.api.replicas = []api.Replica{{Name: "a", Status: "Running", Ready: true}, {Name: "b", Status: "Running", Ready: false}}
	h.expect(h.run("status"), 0)
	if !strings.Contains(h.out.String(), "Replicas  v4 ready (1 of 1); v3 stopping\n") {
		t.Fatalf("status:\n%s", h.out.String())
	}
	r := h.statusJSON()["replicas"].(map[string]any)
	if r["ready"] != float64(1) || r["total"] != float64(2) || r["desired"] != float64(1) || r["stopping"] != float64(1) || r["stopping_version"] != float64(3) {
		t.Fatalf("replicas = %v", r)
	}

	// A new version on its way while the live one serves.
	h.api.apps[0].Replicas = 2
	h.api.deployments[appShopID] = append([]api.Deployment{{ID: "dep-5", AppID: appShopID, Version: 5, Status: "deploying"}}, h.api.deployments[appShopID]...)
	h.api.replicas = []api.Replica{{Name: "a", Ready: true}, {Name: "b", Ready: true}, {Name: "c", Ready: false}}
	h.expect(h.run("status"), 0)
	if !strings.Contains(h.out.String(), "Replicas  v5 starting; v4 serving (2 of 3 ready)\n") {
		t.Fatalf("status:\n%s", h.out.String())
	}
	if r := h.statusJSON()["replicas"].(map[string]any); r["starting_version"] != float64(5) {
		t.Fatalf("replicas = %v", r)
	}

	// An app that autoscales has no fixed count to compare with.
	h.api.apps[0].Autoscaling = &api.Autoscaling{MaxReplicas: 4}
	h.api.deployments[appShopID] = h.api.deployments[appShopID][1:]
	h.expect(h.run("status"), 0)
	if !strings.Contains(h.out.String(), "Replicas  2 of 3 ready\n") {
		t.Fatalf("status:\n%s", h.out.String())
	}
}

// A count saved without a deploy (kx scale 1 --no-deploy, or the
// dashboard's Save) isn't what runs: kx compares with what the live
// version runs.
func TestStatus_DesiredIsWhatTheLiveVersionRuns(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.apps[0].Replicas = 1
	h.api.deployments[appShopID] = []api.Deployment{
		{ID: "dep-4", AppID: appShopID, Version: 4, Status: "ready", Replicas: 3},
		{ID: "dep-3", AppID: appShopID, Version: 3, Status: "ready", Replicas: 3},
	}
	h.api.replicas = []api.Replica{{Name: "a", Ready: true}, {Name: "b", Ready: true}, {Name: "c", Ready: true}}
	h.expect(h.run("status"), 0)
	if !strings.Contains(h.out.String(), "Replicas  3 of 3 ready\n") || strings.Contains(h.out.String(), "stopping") {
		t.Fatalf("status:\n%s", h.out.String())
	}
	if r := h.statusJSON()["replicas"].(map[string]any); r["desired"] != float64(3) || r["stopping"] != nil {
		t.Fatalf("replicas = %v", r)
	}

	// Autoscaling saved on the app but not deployed: the live version
	// still runs a fixed count.
	h.api.apps[0].Autoscaling = &api.Autoscaling{MaxReplicas: 6}
	if r := h.statusJSON()["replicas"].(map[string]any); r["desired"] != float64(3) {
		t.Fatalf("replicas = %v", r)
	}
	// And the other way round: the live version autoscales.
	h.api.apps[0].Autoscaling = nil
	h.api.deployments[appShopID][0].Autoscaling = &api.Autoscaling{MaxReplicas: 6}
	if r := h.statusJSON()["replicas"].(map[string]any); r["desired"] != nil {
		t.Fatalf("replicas = %v; an autoscaling version has no fixed count", r)
	}
}

func TestApps_VersionIsTheLiveOne(t *testing.T) {
	// Kubernetix's release counter stays 0 for most apps: the version comes
	// from the app's deployments.
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	for i := range h.api.apps {
		h.api.apps[i].LatestReleaseVersion = 0
	}
	h.expect(h.run("apps", "list"), 0)
	if !strings.Contains(h.out.String(), "shop-api  ready   v14 ") || !strings.Contains(h.out.String(), "web       failed  -") {
		t.Fatalf("apps list:\n%s", h.out.String())
	}
	h.expect(h.run("apps", "list", "--json"), 0)
	var out struct {
		Apps []map[string]any `json:"apps"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil || out.Apps[0]["version"] != float64(14) {
		t.Fatalf("json = %s (%v)", h.out.String(), err)
	}
	h.expect(h.run("apps", "get", "shop-api"), 0)
	if !strings.Contains(h.out.String(), "Version   v14\n") {
		t.Fatalf("apps get:\n%s", h.out.String())
	}
}

func TestWhoami_ChecksSavedLogins(t *testing.T) {
	h := newHarness(t)
	u := &config.User{}
	u.AddLogin("acme-labs", config.OrgLogin{Token: tokLabs, User: "sam@acme.dev"})
	u.AddLogin("acme-corp", config.OrgLogin{Token: tokCorp, ExpiresAt: &dec11, User: "sam@acme.dev"})
	h.saveUser(u)
	whoamiFails(h, tokLabs, 99, 401, `{"error":"token_revoked","message":"This API token was revoked."}`)
	h.expect(h.run("whoami"), 0)
	want := "ORG        USER          LOGIN ENDS\n" +
		"acme-corp  sam@acme.dev  11 Dec 2026 (default)\n" +
		"acme-labs  sam@acme.dev  revoked\n"
	if h.out.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", h.out.String(), want)
	}
	if !strings.Contains(h.err.String(), "A login that stopped working needs a new one: kx login --org acme-labs\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	h.expect(h.run("whoami", "--json"), 0)
	var out struct {
		Orgs []map[string]any `json:"orgs"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Orgs[0]["status"] != "active" || out.Orgs[1]["status"] != "revoked" {
		t.Fatalf("json = %s", h.out.String())
	}
}

func TestWhoami_KubernetixUnreachable(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.override = func(*http.Request, string) (int, string, bool) { return 500, `{"error":"internal"}`, true }
	h.expect(h.run("whoami"), 0)
	if !strings.Contains(h.out.String(), "acme-corp  sam@acme.dev  11 Dec 2026 (default, not checked)\n") {
		t.Fatalf("stdout:\n%s", h.out.String())
	}
}
