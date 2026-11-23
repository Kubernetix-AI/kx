package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetix-ai/kx/internal/api"
)

const (
	secretValue  = "s3cr3t-value-never-printed"
	secretValue2 = "another-secret-value-9f2c"
)

// canConfigure gives tok the configure permission.
func (h *harness) canConfigure(tok string) {
	t := h.api.who[tok].Token
	t.Permissions = append(t.Permissions, api.PermConfigure)
}

// configured is a linked laptop whose login can configure.
func configured(t *testing.T) *harness {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.canConfigure(tokCorp)
	return h
}

// noSecrets fails if a value appears anywhere kx wrote.
func (h *harness) noSecrets(values ...string) {
	h.t.Helper()
	for _, v := range values {
		if strings.Contains(h.out.String()+h.err.String(), v) {
			h.t.Fatalf("kx printed a value:\nstdout:\n%s\nstderr:\n%s", h.out.String(), h.err.String())
		}
	}
}

func TestEnvSet_SavesThenDeploys(t *testing.T) {
	h := configured(t)
	h.expect(h.run("env", "set", "LOG_LEVEL=debug", "API_KEY="+secretValue), 0)
	if len(h.api.envPatches) != 1 || fmt.Sprint(h.api.envPatches[0]) != "map[set:map[API_KEY:"+secretValue+" LOG_LEVEL:debug]]" {
		t.Fatalf("PATCH env bodies = %v", h.api.envPatches)
	}
	if len(h.api.deployPosts) != 1 || len(h.api.deployPosts[0].Body) != 0 {
		t.Fatalf("deploys = %+v, want one with an empty body (the app as configured)", h.api.deployPosts)
	}
	want := "✓ Set       API_KEY, LOG_LEVEL on shop-api\n✓ Live      v16  queued → starting → live\n"
	if !strings.Contains(h.err.String(), want) {
		t.Fatalf("stderr:\n%s\nwant\n%s", h.err.String(), want)
	}
	if h.out.String() != shopURL+"\n" {
		t.Fatalf("stdout = %q, want the URL alone", h.out.String())
	}
	if len(h.docker.got()) != 0 {
		t.Fatalf("docker ran: %q", h.docker.got())
	}
	h.noSecrets(secretValue)
}

func TestEnvSet_NoDeploy(t *testing.T) {
	h := configured(t)
	h.expect(h.run("env", "set", "FEATURE_X=on", "--no-deploy"), 0)
	if len(h.api.envPatches) != 1 || len(h.api.deployPosts) != 0 {
		t.Fatalf("patches %v, deploys %d", h.api.envPatches, len(h.api.deployPosts))
	}
	if !strings.Contains(h.err.String(), "✓ Set       FEATURE_X on shop-api\n  Saved; the next deploy of shop-api applies it.\n") || h.out.String() != "" {
		t.Fatalf("stderr:\n%s\nstdout %q", h.err.String(), h.out.String())
	}
}

func TestEnvSet_FromFileAndEnvironment(t *testing.T) {
	h := configured(t)
	file := filepath.Join(h.dir, ".env.production")
	body := "\ufeff# production\n\nexport\tDATABASE_URL=\"postgres://u:" + secretValue + "@db/app\"\nGREETING='hello world'\nEMPTY=\nPLAIN = spaced value # a comment\nHASH=a#b\nRAW=${HOME}/x\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.env["STRIPE_KEY"] = secretValue2
	h.expect(h.run("env", "set", "--from-file", ".env.production", "STRIPE_KEY", "--no-deploy", "--allow-empty"), 0)
	got := h.api.envPatches[0]["set"].(map[string]any)
	want := map[string]string{"DATABASE_URL": "postgres://u:" + secretValue + "@db/app", "GREETING": "hello world", "EMPTY": "", "PLAIN": "spaced value",
		"HASH": "a#b", "RAW": "${HOME}/x", "STRIPE_KEY": secretValue2}
	if len(got) != len(want) {
		t.Fatalf("set = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("set[%s] = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	h.noSecrets(secretValue, secretValue2)
}

func TestEnvSet_JSON(t *testing.T) {
	h := configured(t)
	h.expect(h.run("env", "set", "API_KEY="+secretValue, "--json"), 0)
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, h.out.String())
	}
	if out["app"] != "shop-api" || fmt.Sprint(out["set"]) != "[API_KEY]" || out["deployed"] != true || out["version"] != float64(16) || out["url"] != shopURL {
		t.Fatalf("json = %v", out)
	}
	h.noSecrets(secretValue)
}

func TestEnvSet_BadArgumentsNeverShowAValue(t *testing.T) {
	h := configured(t)
	file := filepath.Join(h.dir, "bad.env")
	if err := os.WriteFile(file, []byte("GOOD=1\n"+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args      []string
		what, fix string
	}{
		{[]string{"env", "set"}, "kx env set needs at least one NAME=VALUE.", "Run: kx env set LOG_LEVEL=debug"},
		{[]string{"env", "set", "=" + secretValue}, "Argument 1 isn't NAME=VALUE: names use letters, digits and _, and don't start with a digit.", "Run: kx env set LOG_LEVEL=debug"},
		{[]string{"env", "set", "OK=1", "BAD NAME=" + secretValue}, "Argument 2 isn't NAME=VALUE: names use letters, digits and _, and don't start with a digit.", "Run: kx env set LOG_LEVEL=debug"},
		{[]string{"env", "set", "SPRING.PROFILE=" + secretValue}, "Argument 1 isn't NAME=VALUE: names use letters, digits and _, and don't start with a digit.", "Run: kx env set LOG_LEVEL=debug"},
		{[]string{"env", "set", strings.Repeat("N", 129) + "=x"}, "Argument 1 isn't NAME=VALUE: names use letters, digits and _, and don't start with a digit.", "Run: kx env set LOG_LEVEL=debug"},
		{[]string{"env", "set", secretValue}, "Argument 1 isn't NAME=VALUE, nor the name of a variable in kx's environment.", "Run: kx env set LOG_LEVEL=debug"},
		{[]string{"env", "set", "A=1", "A=2"}, "A is given twice.", "Give each variable once."},
		{[]string{"env", "set", "--from-file", "bad.env"}, "Line 2 of bad.env isn't NAME=VALUE.", "Fix that line, then run the command again."},
		{[]string{"env", "set", "--from-file", "missing.env"}, "kx can't read missing.env.", "Check the --from-file path, then run the command again."},
		{[]string{"env", "unset"}, "kx env unset needs at least one name.", "Run: kx env unset OLD_FLAG"},
		{[]string{"env", "unset", "A=" + secretValue}, "Argument 1 isn't a name: kx env unset takes names only.", "Run: kx env unset OLD_FLAG"},
		{[]string{"env", "unset", secretValue}, "Argument 1 isn't a name: kx env unset takes names only.", "Run: kx env unset OLD_FLAG"},
	}
	for _, tt := range tests {
		h.expect(h.run(tt.args...), 2)
		lines := strings.Split(strings.TrimRight(h.err.String(), "\n"), "\n")
		if len(lines) < 2 || lines[len(lines)-2] != tt.what || lines[len(lines)-1] != tt.fix {
			t.Errorf("kx %v: stderr =\n%s\nwant %q then %q", tt.args[:min(len(tt.args), 2)], h.err.String(), tt.what, tt.fix)
		}
		h.noSecrets(secretValue)
	}
	if len(h.api.envPatches) != 0 || len(h.api.deployPosts) != 0 {
		t.Fatal("changed the app after a bad argument")
	}
}

func TestEnvUnset(t *testing.T) {
	h := configured(t)
	h.api.envValues = map[string]map[string]string{appShopID: {"OLD_FLAG": "1", "LEGACY_URL": "x", "KEEP": "y"}}
	h.expect(h.run("env", "unset", "OLD_FLAG", "LEGACY_URL", "--no-deploy"), 0)
	if fmt.Sprint(h.api.envPatches) != "[map[unset:[OLD_FLAG LEGACY_URL]]]" {
		t.Fatalf("PATCH env bodies = %v", h.api.envPatches)
	}
	if !strings.Contains(h.err.String(), "✓ Unset     LEGACY_URL, OLD_FLAG on shop-api\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestEnvList_NamesOnly(t *testing.T) {
	h := configured(t)
	h.api.envValues = map[string]map[string]string{appShopID: {"LOG_LEVEL": "debug", "API_KEY": secretValue}}
	h.expect(h.run("env", "list"), 0)
	if h.out.String() != "API_KEY\nLOG_LEVEL\n" {
		t.Fatalf("stdout = %q, want the names, one per line", h.out.String())
	}
	if !strings.Contains(h.err.String(), "Values are hidden: kx only sees the names.") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	h.expect(h.run("env", "--json"), 0) // kx env alone lists
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out["names"]) != "[API_KEY LOG_LEVEL]" || out["values_hidden"] != true || out["app"] != "shop-api" {
		t.Fatalf("json = %v", out)
	}
	h.noSecrets(secretValue)

	h.api.envValues = nil
	h.expect(h.run("env", "list"), 0)
	if h.out.String() != "" || !strings.Contains(h.err.String(), "shop-api has no environment variables.") {
		t.Fatalf("stdout %q, stderr:\n%s", h.out.String(), h.err.String())
	}
}

func TestEnvAndScale_TokenWithoutConfigure(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11) // deploy, read, push: no configure
	h.link("shop-api", appShopID, "acme-corp")
	for _, args := range [][]string{{"env", "set", "A=" + secretValue}, {"env", "unset", "A"}, {"scale", "3"}} {
		h.expect(h.run(args...), 3)
		want := "The token this computer uses for acme-corp doesn't have the Configure permission, which kx " + args[0] + " needs.\n" +
			"Log in again and keep Configure ticked: kx login --org acme-corp\n"
		if !strings.HasSuffix(h.err.String(), want) {
			t.Fatalf("kx %v: stderr =\n%s\nwant it to end with\n%s", args, h.err.String(), want)
		}
	}
	if len(h.api.envPatches)+len(h.api.appPuts)+len(h.api.deployPosts) != 0 {
		t.Fatal("changed the app with a token that can't configure")
	}

	// In CI the fix is a token from the dashboard's API tokens page: a
	// deploy token's form never offers Configure.
	h.env["KX_TOKEN"], h.env["CI"] = tokCorp, "true"
	h.expect(h.run("scale", "3"), 3)
	if !strings.HasSuffix(h.err.String(), "Create an API token with Configure (Settings → API tokens) and replace the KX_TOKEN secret.\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestEnv_ServerRefusesConfigure(t *testing.T) {
	// /whoami says the token can configure, the platform says no (say the
	// permission was taken away since): its refusal gets the same fix.
	h := configured(t)
	h.api.override = func(r *http.Request, _ string) (int, string, bool) {
		if r.Method == "PATCH" {
			return 403, `{"error":"token_not_allowed","message":"This API token doesn't have the configure permission."}`, true
		}
		return 0, "", false
	}
	h.expect(h.run("env", "set", "A=1", "--no-deploy"), 3)
	if !strings.HasSuffix(h.err.String(), "This token doesn't have the Configure permission.\nLog in again and keep Configure ticked: kx login --org acme-corp\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestScale(t *testing.T) {
	h := configured(t)
	h.expect(h.run("scale", "3"), 0)
	if fmt.Sprint(h.api.appPuts) != "[map[replicas:3]]" || len(h.api.deployPosts) != 1 || len(h.api.deployPosts[0].Body) != 0 {
		t.Fatalf("puts %v, deploys %+v", h.api.appPuts, h.api.deployPosts)
	}
	if !strings.Contains(h.err.String(), "✓ Scaled    shop-api to 3 replicas\n✓ Live      v16") || h.out.String() != shopURL+"\n" {
		t.Fatalf("stderr:\n%s\nstdout %q", h.err.String(), h.out.String())
	}

	h.expect(h.run("scale", "1", "--no-deploy", "--json"), 0)
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["replicas"] != float64(1) || out["deployed"] != false {
		t.Fatalf("json = %v", out)
	}
}

func TestScale_BadCounts(t *testing.T) {
	h := configured(t)
	h.expect(h.run("scale", "-2"), 2) // a flag to the command line; still a usage error
	for _, n := range []string{"0", "two", "1.5", ""} {
		h.expect(h.run("scale", n), 2)
		if !strings.Contains(h.err.String(), "kx scale takes how many replicas to run, a whole number from 1 up.\nRun: kx scale 2\n") {
			t.Fatalf("kx scale %q: stderr:\n%s", n, h.err.String())
		}
	}
	if len(h.api.appPuts) != 0 {
		t.Fatal("changed the app")
	}
}

func TestScale_Autoscaling(t *testing.T) {
	h := configured(t)
	two := 2
	h.api.apps[0].Autoscaling = &api.Autoscaling{MinReplicas: &two, MaxReplicas: 6}
	h.expect(h.run("scale", "3", "--no-deploy"), 0)
	if !strings.Contains(h.err.String(), "! shop-api autoscales between 2 and 6 replicas, so a replica count has no effect while autoscaling is on. The count applies once autoscaling is off.\n") ||
		fmt.Sprint(h.api.appPuts) != "[map[replicas:3]]" {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestScale_PlanLimit(t *testing.T) {
	h := configured(t)
	h.api.override = func(r *http.Request, _ string) (int, string, bool) {
		if r.Method == "PUT" {
			return 402, `{"error":"tier_limit_exceeded","limit_name":"max_replicas_per_app","limit":2,"current":2,"message":"Plan limit reached"}`, true
		}
		return 0, "", false
	}
	h.expect(h.run("scale", "5"), 5)
	if len(h.api.deployPosts) != 0 {
		t.Fatal("deployed after the change was refused")
	}
}

func TestEnvSet_MultiRegionAppSavesNothing(t *testing.T) {
	h := configured(t)
	h.api.apps[0].Regions = []api.AppRegion{{Region: "us-central1"}, {Region: "europe-west1"}}
	h.expect(h.run("env", "set", "A=1"), 2)
	if len(h.api.envPatches) != 0 {
		t.Fatal("saved a change kx then couldn't deploy")
	}
	h.expect(h.run("env", "set", "A=1", "--no-deploy"), 0)
	if len(h.api.envPatches) != 1 {
		t.Fatal("--no-deploy should save it")
	}
}
