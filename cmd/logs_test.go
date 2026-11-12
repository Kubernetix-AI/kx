package cmd

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
