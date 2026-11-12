package cmd

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/safetext"
)

func TestLogs_LastLinesOldestFirst(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	var q map[string]string
	h.api.runtimeLogs = func(got map[string]string) string {
		q = got
		return logsJSON([2]string{"1762938002000000000", "second"}, [2]string{"1762938001000000000", "first \x1b[31mred\x1b[0m"})
	}
	h.expect(h.run("logs"), 0)
	wantStart := strconv.FormatInt(nov12.Add(-time.Hour).UnixNano(), 10)
	if q["limit"] != "100" || q["direction"] != "backward" || q["start"] != wantStart {
		t.Fatalf("query = %v (want start %s)", q, wantStart)
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "first [31mred[0m") || !strings.HasSuffix(lines[1], "second") {
		t.Fatalf("stdout = %q", h.out.String())
	}
	if strings.ContainsRune(h.out.String(), 0x1b) {
		t.Fatal("log output keeps terminal escape characters")
	}
	if !strings.HasPrefix(lines[0], time.Unix(0, 1762938001000000000).Local().Format("2006-01-02 15:04:05")) {
		t.Fatalf("line has no timestamp: %q", lines[0])
	}
}

func TestLogs_Since(t *testing.T) {
	for since, d := range map[string]time.Duration{"30m": 30 * time.Minute, "2d": 48 * time.Hour, "90s": 90 * time.Second} {
		h := newHarness(t)
		h.loginAs("acme-corp", tokCorp, &dec11)
		var q map[string]string
		h.api.runtimeLogs = func(got map[string]string) string { q = got; return logsJSON() }
		h.expect(h.run("logs", "--app", "shop-api", "--since", since), 0)
		if q["start"] != strconv.FormatInt(nov12.Add(-d).UnixNano(), 10) {
			t.Errorf("--since %s: start = %s", since, q["start"])
		}
	}
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("logs", "--app", "shop-api", "--since", "yesterday"), 2)
}

func TestLogsFollow_ForwardFromLast(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")

	var mu sync.Mutex
	var queries []map[string]string
	h.api.runtimeLogs = func(q map[string]string) string {
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, q)
		switch len(queries) {
		case 1: // the last lines
			return logsJSON([2]string{"1762938001000000000", "one"}, [2]string{"1762938002000000000", "two"})
		case 2: // forward from the last line: it comes back, plus a new one
			return logsJSON([2]string{"1762938002000000000", "two"}, [2]string{"1762938003000000000", "three"})
		default: // nothing new
			return logsJSON([2]string{"1762938003000000000", "three"})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.sleepHook = func(n int) error {
		if n >= 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	h.expect(h.runCtx(ctx, "logs", "--follow"), 0)

	var got []string
	for _, l := range strings.Split(strings.TrimSpace(h.out.String()), "\n") {
		got = append(got, l[strings.LastIndex(l, " ")+1:])
	}
	if strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("lines = %v\nstdout:\n%s", got, h.out.String())
	}
	if len(queries) < 3 {
		t.Fatalf("follow made %d queries", len(queries))
	}
	if queries[1]["direction"] != "forward" || queries[1]["start"] != "1762938002000000000" {
		t.Fatalf("first follow query = %v, want forward from the last line", queries[1])
	}
	if queries[2]["start"] != "1762938003000000000" {
		t.Fatalf("second follow query = %v, want forward from the newest line", queries[2])
	}
	if h.err.String() != "" {
		t.Fatalf("Ctrl-C while following printed: %q", h.err.String())
	}
}

func TestLogs_JSONLines(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.api.runtimeLogs = func(map[string]string) string { return logsJSON([2]string{"1762938001000000000", "hello"}) }
	h.expect(h.run("logs", "--app", "shop-api", "--json"), 0)
	want := `{"time":"` + time.Unix(0, 1762938001000000000).UTC().Format(time.RFC3339Nano) + `","line":"hello"}` + "\n"
	if h.out.String() != want {
		t.Fatalf("stdout = %q, want %q", h.out.String(), want)
	}
}

func TestLogs_Deployment(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("logs", "--deployment", "v15"), 0)
	if h.out.String() != "Error: listen EADDRINUSE :8080\nexit 1\n" {
		t.Fatalf("stdout = %q", h.out.String())
	}
	if !strings.Contains(h.err.String(), "shop-api v15 failed: your container is crashing on start.") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	if !strings.Contains(strings.Join(h.api.got(), "\n"), "GET /api/v1/deployments/dep-15/logs") {
		t.Fatal("did not fetch the deployment's logs")
	}
}

func TestLogs_DeploymentErrors(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("logs", "--deployment", "v99"), 4)
	if !strings.HasPrefix(h.err.String(), "shop-api has no v99.\n") {
		t.Fatalf("stderr = %q", h.err.String())
	}
	h.expect(h.run("logs", "--deployment", "v15", "--follow"), 2)
}

func TestLogs_NotLinked(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.expect(h.run("logs"), 2)
	if h.err.String() != "This folder isn't linked to an app yet.\nRun: kx link\n" {
		t.Fatalf("stderr = %q", h.err.String())
	}
}

// hostileLogLines are app output lines that try to issue CI workflow
// commands: the modern form after Unicode spaces, the legacy forms
// mid-line, and ones hidden after a line break or behind a control
// character kx removes.
var hostileLogLines = []string{
	"::error::forged",
	"　::add-mask::wide",
	"step ##[warning]mid-line",
	"see ##vso[task.prependpath]/tmp/x",
	"two lines\n::warning::embedded",
	"\x00::error::after-nul",
	"a break\n\x00::error::after-break",
	"::stop-commands::guess",
}

var stopCommandsRe = regexp.MustCompile(`(?m)^::stop-commands::([0-9a-f]{32})$`)

// commandNames lists what a runner would act on in out, by command name.
func commandNames(out string) string {
	var names []string
	for _, c := range safetext.Commands(out) {
		name, _, _ := strings.Cut(strings.TrimPrefix(c, "::"), "::")
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

func TestLogs_AppOutputCannotIssueWorkflowCommands(t *testing.T) {
	var pairs [][2]string
	for i, l := range hostileLogLines {
		pairs = append(pairs, [2]string{strconv.Itoa(1762938001000000000 + i), l})
	}
	reason := "crashed ##[error]forged reason"
	setUp := func(t *testing.T, github bool) *harness {
		h := newHarness(t)
		if github {
			githubJob(h)
		} else {
			h.loginAs("acme-corp", tokCorp, &dec11)
		}
		h.api.runtimeLogs = func(map[string]string) string { return logsJSON(pairs...) }
		h.api.deployments[appShopID][0].CustomerError = reason
		h.api.depLogsFor = func(string) (api.DeploymentLogs, bool) {
			return api.DeploymentLogs{BuildLogs: strings.Join(hostileLogLines[:3], "\n"), DeploymentLogs: strings.Join(hostileLogLines[3:], "\n")}, true
		}
		return h
	}
	for _, args := range [][]string{{"logs", "--app", "shop-api"}, {"logs", "--app", "shop-api", "--deployment", "v15"}} {
		for _, github := range []bool{false, true} {
			h := setUp(t, github)
			h.expect(h.run(args...), 0)
			want := ""
			if github {
				want = "stop-commands,resume-commands"
			}
			if got := commandNames(h.out.String()); got != want {
				t.Fatalf("%v github=%v: a runner would act on %q in stdout:\n%s", args, github, got, h.out.String())
			}
			if got := commandNames(h.err.String()); got != "" {
				t.Fatalf("%v github=%v: a runner would act on %q in stderr:\n%s", args, github, got, h.err.String())
			}
			for _, s := range []string{"forged", "wide", "mid-line", "embedded", "after-nul", "after-break"} {
				if !strings.Contains(h.out.String(), s) {
					t.Fatalf("%v: the app's text %q is missing from stdout:\n%s", args, s, h.out.String())
				}
			}
			if github {
				m := stopCommandsRe.FindStringSubmatch(h.out.String())
				if m == nil || !strings.HasPrefix(h.out.String(), m[0]+"\n") || !strings.HasSuffix(h.out.String(), "\n::"+m[1]+"::\n") {
					t.Fatalf("%v: the app's output isn't inside one stop-commands block on stdout:\n%s", args, h.out.String())
				}
			}
		}
	}

	// --json keeps the text exactly as before, but encoded so a runner
	// can't read a command in it.
	h := setUp(t, true)
	h.expect(h.run("logs", "--app", "shop-api", "--json"), 0)
	if got := commandNames(h.out.String()); got != "" {
		t.Fatalf("--json: a runner would act on %q in:\n%s", got, h.out.String())
	}
	dec := json.NewDecoder(strings.NewReader(h.out.String()))
	for _, l := range hostileLogLines {
		var back struct{ Line string }
		if err := dec.Decode(&back); err != nil || back.Line != cleanLogText(l) {
			t.Fatalf("--json line = %q (%v), want %q", back.Line, err, cleanLogText(l))
		}
	}
	h = setUp(t, true)
	h.expect(h.run("logs", "--app", "shop-api", "--deployment", "v15", "--json"), 0)
	if got := commandNames(h.out.String()); got != "" {
		t.Fatalf("--deployment --json: a runner would act on %q in:\n%s", got, h.out.String())
	}
	var dep struct {
		Reason    string `json:"reason"`
		BuildLogs string `json:"build_logs"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &dep); err != nil || dep.Reason != reason || dep.BuildLogs != strings.Join(hostileLogLines[:3], "\n") {
		t.Fatalf("--deployment --json = %+v (%v)", dep, err)
	}
}

func TestLogsFollow_EachBatchInItsOwnStopCommandsBlock(t *testing.T) {
	h := newHarness(t)
	githubJob(h)
	var mu sync.Mutex
	n := 0
	h.api.runtimeLogs = func(map[string]string) string {
		mu.Lock()
		defer mu.Unlock()
		n++
		switch n {
		case 1:
			return logsJSON([2]string{"1762938001000000000", "::error::one"})
		case 2:
			return logsJSON([2]string{"1762938001000000000", "::error::one"}, [2]string{"1762938002000000000", "##[warning]two"})
		default:
			return logsJSON([2]string{"1762938002000000000", "##[warning]two"})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.sleepHook = func(n int) error {
		if n >= 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	h.expect(h.runCtx(ctx, "logs", "--app", "shop-api", "--follow"), 0)
	if got := commandNames(h.out.String()); got != "stop-commands,resume-commands,stop-commands,resume-commands" {
		t.Fatalf("a runner would act on %q in:\n%s", got, h.out.String())
	}
	if m := stopCommandsRe.FindAllStringSubmatch(h.out.String(), -1); len(m) != 2 || m[0][1] == m[1][1] {
		t.Fatalf("want two blocks with different tokens:\n%s", h.out.String())
	}
}
