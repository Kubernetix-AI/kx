package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
)

// errJSON reads kx's --json error object.
func errJSON(t *testing.T, h *harness) map[string]any {
	t.Helper()
	var out struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil || out.Error == nil {
		t.Fatalf("stdout is not a JSON error: %v\n%s", err, h.out.String())
	}
	return out.Error
}

// noteBeforeFix checks that the line about the change comes right after
// what went wrong, before the fix (the last line).
func noteBeforeFix(t *testing.T, h *harness, note string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(h.err.String(), "\n"), "\n")
	if len(lines) < 3 || lines[len(lines)-2] != note {
		t.Fatalf("stderr:\n%s\nwant the second-to-last line %q", h.err.String(), note)
	}
}

// failNewDeploys makes a new deployment fail to go live.
func failNewDeploys(h *harness) {
	h.api.statuses = []string{"pending", "deploying", "failed"}
	h.api.failReason = "your container is crashing on start"
}

// restoreAfterFailure puts shop-api back the way Kubernetix does after a
// failed deploy (replicas, and the names of the env vars), once kx has
// read the app gets times since the deployment failed.
func restoreAfterFailure(h *harness, replicas int, names map[string]string, gets int) {
	failed := false
	h.api.onFailed = func() { failed = true }
	h.api.override = func(r *http.Request, _ string) (int, string, bool) {
		if failed && r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps/"+appShopID {
			if gets--; gets <= 0 {
				h.api.apps[0].Replicas = replicas
				if h.api.envValues == nil {
					h.api.envValues = map[string]map[string]string{}
				}
				h.api.envValues[appShopID] = names
			}
		}
		return 0, "", false
	}
}

func TestConfigure_RefusedAfterSave(t *testing.T) {
	h := configured(t)
	h.api.override = func(r *http.Request, _ string) (int, string, bool) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deployments") {
			return 402, `{"error":"tier_limit_exceeded","limit_name":"max_deployments_per_day","limit":5,"current":5,"message":"Daily deploy limit reached"}`, true
		}
		return 0, "", false
	}
	h.expect(h.run("env", "set", "API_KEY="+secretValue), 5)
	noteBeforeFix(t, h, "The change is saved on shop-api but not deployed; the next deploy applies it.")
	h.noSecrets(secretValue)

	h.expect(h.run("env", "set", "API_KEY="+secretValue, "--json"), 5)
	e := errJSON(t, h)
	if e["saved"] != true || e["deployed"] != false || e["code"] != "deploy_limit_reached" ||
		e["change"] != "The change is saved on shop-api but not deployed; the next deploy applies it." {
		t.Fatalf("error = %v", e)
	}
	if _, ok := e["restored_to"]; ok {
		t.Fatalf("error = %v: nothing was restored", e)
	}
	h.noSecrets(secretValue)
}

func TestConfigure_DeployOutcomeUnknown(t *testing.T) {
	h := configured(t)
	h.api.failPosts = 99 // the deploy POST never gets an answer kx can use
	h.expect(h.run("scale", "3", "--json"), 1)
	e := errJSON(t, h)
	want := "The change is saved on shop-api; kx can't tell whether its deploy started (see kx status). If none did, the next deploy applies it."
	if e["saved"] != true || e["deployed"] != false || e["change"] != want {
		t.Fatalf("error = %v", e)
	}
}

func TestConfigure_FailedAndRestored(t *testing.T) {
	h := configured(t)
	failNewDeploys(h)
	restoreAfterFailure(h, 2, map[string]string{}, 2) // the restore lands a moment after the failure
	h.expect(h.run("scale", "3"), 8)
	noteBeforeFix(t, h, "Kubernetix put shop-api back to v14, so this change was undone.")

	h = configured(t)
	failNewDeploys(h)
	restoreAfterFailure(h, 2, map[string]string{}, 1)
	h.expect(h.run("env", "set", "NEW_FLAG=on", "--json"), 8)
	e := errJSON(t, h)
	if e["saved"] != false || e["deployed"] != false || e["restored_to"] != float64(14) || e["version"] != float64(16) ||
		e["change"] != "Kubernetix put shop-api back to v14, so this change was undone." {
		t.Fatalf("error = %v", e)
	}
}

func TestConfigure_FailedButKept(t *testing.T) {
	// The app changed again since (or Kubernetix had nothing to go back
	// to): the change is still there after kx has looked a few times.
	h := configured(t)
	failNewDeploys(h)
	h.expect(h.run("scale", "3", "--json"), 8)
	e := errJSON(t, h)
	if e["saved"] != true || e["deployed"] != false || e["change"] != "The change is still saved on shop-api but not deployed; the next deploy applies it." {
		t.Fatalf("error = %v", e)
	}
	if _, ok := e["restored_to"]; ok {
		t.Fatalf("error = %v", e)
	}

	// A new name still on the app is the same.
	h = configured(t)
	failNewDeploys(h)
	h.expect(h.run("env", "set", "NEW_FLAG=on"), 8)
	noteBeforeFix(t, h, "The change is still saved on shop-api but not deployed; the next deploy applies it.")
}

func TestConfigure_FailedValueChangeCantBeChecked(t *testing.T) {
	// kx sees names, not values: a new value for a variable the app
	// already has can't be checked, so kx says what Kubernetix does.
	h := configured(t)
	h.api.envValues = map[string]map[string]string{appShopID: {"API_KEY": "old"}}
	failNewDeploys(h)
	h.expect(h.run("env", "set", "API_KEY="+secretValue), 8)
	noteBeforeFix(t, h, "Kubernetix puts shop-api back to v14 after a failed deploy, so this change is most likely undone.")
	h.noSecrets(secretValue)
}

func TestConfigure_WaitEndsBeforeTheOutcome(t *testing.T) {
	h := configured(t)
	h.api.statuses = []string{"pending", "deploying"}
	h.sleepHook = func(int) error { h.now = h.now.Add(time.Minute); return nil }
	h.expect(h.run("scale", "3", "--timeout", "3m", "--json"), 9)
	e := errJSON(t, h)
	if e["saved"] != true || e["deployed"] != false || e["version"] != float64(16) ||
		e["change"] != "The change is saved on shop-api; if v16 doesn't go live, Kubernetix puts the app back, which undoes it." {
		t.Fatalf("error = %v", e)
	}

	h = configured(t)
	h.expect(h.run("scale", "3", "--no-wait"), 0)
	if !strings.Contains(h.err.String(), "If v16 doesn't go live, Kubernetix puts the app back, which undoes this change.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	h = configured(t) // v16 above is still starting: a new kx run would wait for it
	h.expect(h.run("scale", "4", "--no-wait", "--json"), 0)
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["saved"] != true || out["deployed"] != false || out["status"] != "started" {
		t.Fatalf("json = %v: started is not deployed", out)
	}
}

func TestConfigure_JSONSaysSavedAndDeployed(t *testing.T) {
	h := configured(t)
	h.expect(h.run("scale", "3", "--json"), 0)
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["saved"] != true || out["deployed"] != true || out["status"] != "live" {
		t.Fatalf("json = %v", out)
	}

	// Refused before anything was saved: both false, and no line about
	// a change.
	h.api.apps[0].Regions = []api.AppRegion{{Region: "us-central1"}, {Region: "europe-west1"}}
	h.expect(h.run("scale", "4", "--json"), 2)
	e := errJSON(t, h)
	if e["saved"] != false || e["deployed"] != false || e["change"] != nil {
		t.Fatalf("error = %v", e)
	}
	h.expect(h.run("env", "set", "BAD NAME=x", "--json"), 2)
	if e := errJSON(t, h); e["saved"] != false || e["deployed"] != false {
		t.Fatalf("error = %v", e)
	}
}

// Without the Deploy permission kx stops before saving, unless
// --no-deploy says not to deploy.
func TestConfigure_TokenWithoutDeploy(t *testing.T) {
	h := configured(t)
	tok := h.api.who[tokCorp].Token
	tok.Permissions = []string{api.PermConfigure, api.PermRead}
	h.expect(h.run("env", "set", "A=1"), 3)
	want := "The token this computer uses for acme-corp doesn't have the Deploy permission, which kx env needs.\n" +
		"Add --no-deploy to save the change without deploying, or log in again with Deploy ticked: kx login --org acme-corp\n"
	if !strings.HasSuffix(h.err.String(), want) {
		t.Fatalf("stderr:\n%s\nwant it to end with\n%s", h.err.String(), want)
	}
	if len(h.api.envPatches)+len(h.api.deployPosts) != 0 {
		t.Fatal("saved a change kx couldn't deploy")
	}
	h.expect(h.run("env", "set", "A=1", "--no-deploy"), 0)
	if len(h.api.envPatches) != 1 {
		t.Fatal("--no-deploy needs no Deploy permission")
	}
}

func TestConfigure_NothingToChange(t *testing.T) {
	h := configured(t)
	h.api.envValues = map[string]map[string]string{appShopID: {"OLD_FLAG": "x"}}
	h.api.deployments[appShopID][1].Replicas = 2 // v14 runs 2, like the app

	h.expect(h.run("env", "unset", "MISSING", "ALSO_MISSING"), 0)
	if !strings.Contains(h.err.String(), "Nothing to change: shop-api has none of those variables.\n") || strings.Contains(h.err.String(), "MISSING") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	h.expect(h.run("scale", "2"), 0)
	if !strings.Contains(h.err.String(), "Nothing to change: shop-api already runs 2 replicas.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	h.expect(h.run("scale", "2", "--json"), 0)
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["saved"] != false || out["deployed"] != false || out["unchanged"] != true {
		t.Fatalf("json = %v", out)
	}
	if len(h.api.envPatches)+len(h.api.appPuts)+len(h.api.deployPosts) != 0 {
		t.Fatal("changed or deployed the app for nothing")
	}

	// Some names exist: only those are removed, and the others are counted,
	// not named (a name typed here could be a value by mistake).
	h.expect(h.run("env", "unset", "OLD_FLAG", "NOT_THERE", "--no-deploy"), 0)
	if len(h.api.envPatches) != 1 || len(h.api.envPatches[0]["unset"].([]any)) != 1 ||
		!strings.Contains(h.err.String(), "✓ Unset     OLD_FLAG on shop-api\n  1 of the names isn't set on shop-api.\n") || strings.Contains(h.err.String(), "NOT_THERE") {
		t.Fatalf("patches %v, stderr:\n%s", h.api.envPatches, h.err.String())
	}

	// Saved but not deployed yet: the same count is deployed now.
	h.api.apps[0].Replicas = 3
	h.expect(h.run("scale", "3"), 0)
	if len(h.api.deployPosts) != 1 {
		t.Fatalf("deploys = %d: 3 is saved but v14 runs 2", len(h.api.deployPosts))
	}
}

func TestScale_AutoscalingRefusedUnlessNoDeploy(t *testing.T) {
	h := configured(t)
	two := 2
	h.api.apps[0].Autoscaling = &api.Autoscaling{MinReplicas: &two, MaxReplicas: 6}
	h.expect(h.run("scale", "3"), 2)
	if !strings.HasSuffix(h.err.String(), "shop-api autoscales between 2 and 6 replicas, so a replica count has no effect while autoscaling is on.\n"+
		"To save the count for when autoscaling is off: kx scale 3 --no-deploy\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.appPuts)+len(h.api.deployPosts) != 0 {
		t.Fatal("changed the app")
	}
}
