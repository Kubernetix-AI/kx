package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Public checksum test vectors shared with the platform: well formed, never
// issued, so they unlock nothing.
const tok = "kx_0123456789abcdefghijABCDEFGHIJklmnopqrst1zpKRU"

// syncBuffer is a bytes.Buffer safe for the spinner goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTest(in string) (*Printer, *syncBuffer, *syncBuffer) {
	out, errw := &syncBuffer{}, &syncBuffer{}
	p := New(strings.NewReader(in), out, errw, func(string) string { return "" }, false)
	return p, out, errw
}

func TestNew_DetectsNoTTYForBuffers(t *testing.T) {
	p, _, _ := newTest("")
	if p.TTY || p.InTTY || p.Color || p.OutColor {
		t.Fatalf("TTY=%v InTTY=%v Color=%v OutColor=%v for plain buffers", p.TTY, p.InTTY, p.Color, p.OutColor)
	}
}

func TestColorEnabled(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	tests := []struct {
		tty  bool
		env  map[string]string
		want bool
	}{
		{true, nil, true},
		{false, nil, false},
		{true, map[string]string{"NO_COLOR": "1"}, false},
		{true, map[string]string{"NO_COLOR": ""}, true},
		{true, map[string]string{"TERM": "dumb"}, false},
	}
	for _, tt := range tests {
		if got := colorEnabled(tt.tty, env(tt.env)); got != tt.want {
			t.Errorf("colorEnabled(%v, %v) = %v, want %v", tt.tty, tt.env, got, tt.want)
		}
	}
}

func TestPrinter_URLAloneOnStdout(t *testing.T) {
	p, out, errw := newTest("")
	p.Info("Checking %s", "shop-api")
	p.Step("Built", "shop-api:3f9c2a1b7d4e", 72*time.Second)
	p.Note("4 of 5 deploys left today on the Free plan.")
	p.Warn("EXPOSE 80 is not the app's port")
	p.Result("https://shop-api.example.test")
	if out.String() != "https://shop-api.example.test\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	if !strings.Contains(errw.String(), "Checking shop-api") || strings.Contains(errw.String(), "https://shop-api") {
		t.Fatalf("stderr = %q", errw.String())
	}
}

func TestPrinter_StepFormat(t *testing.T) {
	p, _, errw := newTest("")
	p.Step("Checked", "Dockerfile, port 8080", 0)
	p.Step("Built", "shop-api:3f9c2a1b7d4e", 72*time.Second)
	p.Step("Live", "v1  queued → starting → live", 48*time.Second)
	want := "✓ Checked   Dockerfile, port 8080\n" +
		"✓ Built     shop-api:3f9c2a1b7d4e                        1m 12s\n" +
		"✓ Live      v1  queued → starting → live                 48s\n"
	if errw.String() != want {
		t.Fatalf("steps =\n%s\nwant\n%s", errw.String(), want)
	}
}

func TestPrinter_StepFailedFormat(t *testing.T) {
	p, out, errw := newTest("")
	p.StepFailed("Failed", "v15  queued → starting → failed", 62*time.Second)
	p.StepFailed("Failed", "v15", 0)
	want := "✗ Failed    v15  queued → starting → failed" + strings.Repeat(" ", 14) + "1m 2s\n" +
		"✗ Failed    v15\n"
	if errw.String() != want || out.String() != "" {
		t.Fatalf("steps =\n%s\nwant\n%s", errw.String(), want)
	}
}

func TestPrinter_FailText(t *testing.T) {
	p, out, errw := newTest("")
	p.Fail("not_logged_in", "You're not logged in to Kubernetix.", "Run: kx login")
	if out.String() != "" {
		t.Fatalf("stdout = %q", out.String())
	}
	if errw.String() != "You're not logged in to Kubernetix.\nRun: kx login\n" {
		t.Fatalf("stderr = %q", errw.String())
	}
}

func TestPrinter_FailJSON(t *testing.T) {
	p, out, errw := newTest("")
	p.JSON = true
	p.Fail("not_logged_in", "You're not logged in to Kubernetix.", "Run: kx login")
	want := `{"error":{"code":"not_logged_in","message":"You're not logged in to Kubernetix.","fix":"Run: kx login"}}` + "\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
	if errw.String() != "" {
		t.Fatalf("stderr = %q", errw.String())
	}
}

func TestPrinter_JSONStableKeys(t *testing.T) {
	p, out, _ := newTest("")
	if err := p.PrintJSON(map[string]any{"url": "https://x", "app": "shop-api"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\n  \"app\": \"shop-api\",\n  \"url\": \"https://x\"\n}\n" {
		t.Fatalf("json = %q", out.String())
	}
	out2 := &syncBuffer{}
	p.Out = out2
	_ = p.PrintJSONLine(map[string]string{"line": "hello " + tok})
	if out2.String() != "{\"line\":\"hello kx_01234567…\"}\n" {
		t.Fatalf("json line = %q", out2.String())
	}
}

func TestPrinter_RedactsTokensEverywhere(t *testing.T) {
	p, out, errw := newTest("")
	p.Info("token %s", tok)
	p.Note(tok)
	p.Warn(tok)
	p.Step("Saved", tok, 0)
	p.Result(tok)
	p.Fail("x", tok, tok)
	p.Table([]string{"A"}, [][]string{{tok}})
	all := out.String() + errw.String()
	if strings.Contains(all, tok) {
		t.Fatalf("output contains a whole token:\n%s", all)
	}
	if !strings.Contains(all, "kx_01234567…") {
		t.Fatalf("output lost the prefix:\n%s", all)
	}
}

func TestPrinter_SecretIsTheOneException(t *testing.T) {
	p, out, _ := newTest("")
	p.Secret(tok)
	if out.String() != tok+"\n" {
		t.Fatalf("Secret wrote %q", out.String())
	}
}

func TestPrinter_Table(t *testing.T) {
	p, out, _ := newTest("")
	p.Table([]string{"NAME", "STATUS", "URL"}, [][]string{
		{"shop-api", "live", "https://a"},
		{"billing-worker", "failed", ""},
	})
	want := "NAME            STATUS  URL\nshop-api        live    https://a\nbilling-worker  failed\n"
	if out.String() != want {
		t.Fatalf("table =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                       "",
		400 * time.Millisecond:  "0s",
		14 * time.Second:        "14s",
		72 * time.Second:        "1m 12s",
		120 * time.Second:       "2m 0s",
		3*time.Hour + time.Hour: "4h 0m",
		3723 * time.Second:      "1h 2m",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestDate(t *testing.T) {
	if got := Date(time.Date(2026, 12, 11, 23, 30, 0, 0, time.UTC)); got != "11 Dec 2026" {
		t.Fatalf("Date = %q", got)
	}
}

func TestSpinner_HeartbeatWithoutTTY(t *testing.T) {
	p, _, errw := newTest("")
	p.HeartbeatEvery = 15 * time.Millisecond
	s := p.Spinner("Building shop-api")
	time.Sleep(80 * time.Millisecond)
	s.Update("Pushing shop-api")
	time.Sleep(40 * time.Millisecond)
	s.Stop()
	s.Stop() // idempotent
	got := errw.String()
	if strings.Count(got, "\n") < 3 || !strings.Contains(got, "Building shop-api…") || !strings.Contains(got, "Pushing shop-api…") {
		t.Fatalf("heartbeat output = %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Fatalf("non-TTY output has carriage returns: %q", got)
	}
}

func TestSpinner_TTYRedrawsAndClears(t *testing.T) {
	p, _, errw := newTest("")
	p.TTY = true
	p.SpinEvery = 5 * time.Millisecond
	s := p.Spinner("Building")
	time.Sleep(40 * time.Millisecond)
	s.Stop()
	got := errw.String()
	if !strings.Contains(got, "\r") || !strings.HasSuffix(got, "\r\x1b[2K") {
		t.Fatalf("TTY spinner output = %q", got)
	}
}

func TestReadSecret_MasksAndTrims(t *testing.T) {
	p, out, errw := newTest("")
	p.InTTY, p.TTY = true, true
	p.ReadPassword = func() (string, error) { return "  " + tok + "\r\n", nil }
	got, err := p.ReadSecret(context.Background(), "Token: ")
	if err != nil || got != tok {
		t.Fatalf("ReadSecret = %.11s, %v", got, err)
	}
	if errw.String() != "Token: ••••••••••••••••\n" || out.String() != "" {
		t.Fatalf("stderr = %q stdout = %q", errw.String(), out.String())
	}
}

func TestReadSecret_NeedsTTY(t *testing.T) {
	p, _, _ := newTest("")
	if _, err := p.ReadSecret(context.Background(), "Token: "); !errors.Is(err, ErrNoTTY) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadLine(t *testing.T) {
	p, _, _ := newTest("  " + tok + "  \nsecond line\n")
	got, err := p.ReadLine(context.Background())
	if err != nil || got != tok {
		t.Fatalf("ReadLine = %.11s, %v", got, err)
	}
}

func TestChoose(t *testing.T) {
	opts := []string{"shop-api (new app)", "billing-worker", "web"}
	tests := []struct {
		name  string
		keys  string
		def   int
		want  int
		cause error
	}{
		{"enter takes the default", "\r", 0, 0, nil},
		{"down arrow", "\x1b[B\r", 0, 1, nil},
		{"up wraps", "\x1b[A\r", 0, 2, nil},
		{"j and k", "jjk\r", 0, 1, nil},
		{"digit", "3\r", 0, 2, nil},
		{"out of range digit ignored", "9\r", 1, 1, nil},
		{"ctrl-c", "\x03", 0, -1, context.Canceled},
		{"end of input", "", 0, -1, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, out, errw := newTest(tt.keys)
			p.InTTY, p.TTY = true, true
			p.rawMode = func() (func(), error) { return func() {}, nil }
			got, err := p.Choose(context.Background(), "Which app should this folder deploy to?", opts, tt.def)
			if got != tt.want || !errors.Is(err, tt.cause) {
				t.Fatalf("Choose = %d, %v; want %d, %v", got, err, tt.want, tt.cause)
			}
			if out.String() != "" {
				t.Fatalf("picker wrote to stdout: %q", out.String())
			}
			if !strings.HasPrefix(errw.String(), "Which app should this folder deploy to?\r\n") {
				t.Fatalf("stderr = %q", errw.String())
			}
		})
	}
}

func TestChoose_NumberFallback(t *testing.T) {
	p, _, errw := newTest("2\n")
	p.InTTY, p.TTY = true, true
	p.rawMode = func() (func(), error) { return nil, errors.New("no raw mode") }
	got, err := p.Choose(context.Background(), "Pick", []string{"a", "b"}, 0)
	if err != nil || got != 1 {
		t.Fatalf("Choose = %d, %v", got, err)
	}
	if !strings.Contains(errw.String(), "  1) a\n  2) b\nChoose [1]: ") {
		t.Fatalf("stderr = %q", errw.String())
	}
}

func TestChoose_NeedsTTY(t *testing.T) {
	p, _, _ := newTest("\r")
	if _, err := p.Choose(context.Background(), "Pick", []string{"a"}, 0); !errors.Is(err, ErrNoTTY) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfirm(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "yes\n": true, "Y\n": true, "n\n": false, "\n": false, "": false} {
		p, _, errw := newTest(in)
		p.InTTY, p.TTY = true, true
		got, err := p.Confirm(context.Background(), "Revoke kx_01234567?")
		if err != nil || got != want {
			t.Errorf("Confirm(%q) = %v, %v", in, got, err)
		}
		if !strings.HasPrefix(errw.String(), "Revoke kx_01234567? [y/N] ") {
			t.Errorf("prompt = %q", errw.String())
		}
	}
}

func TestPrinter_CopiesShareState(t *testing.T) {
	p, out, _ := newTest("first\nsecond\n")
	byValue := *p
	if line, _ := byValue.ReadLine(context.Background()); line != "first" {
		t.Fatalf("copy read %q", line)
	}
	if line, _ := p.ReadLine(context.Background()); line != "second" {
		t.Fatalf("original read %q after the copy buffered stdin", line)
	}
	byValue.Result("x")
	if out.String() != "x\n" {
		t.Fatalf("copy wrote %q", out.String())
	}
}

func TestSecretJSON_NotRedacted(t *testing.T) {
	p, out, _ := newTest("")
	_ = p.SecretJSON(map[string]string{"token": tok})
	if !strings.Contains(out.String(), tok) {
		t.Fatalf("SecretJSON redacted the token: %q", out.String())
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", time.Minute: "1 minute ago", 2 * time.Minute: "2 minutes ago",
		3 * time.Hour: "3 hours ago", 49 * time.Hour: "2 days ago",
	} {
		if got := Ago(now.Add(-d), now); got != want {
			t.Errorf("Ago(-%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPrinter_TableWithoutHeader(t *testing.T) {
	p, out, _ := newTest("")
	p.Table(nil, [][]string{{"Name", "shop-api"}, {"Replicas", "2"}})
	if out.String() != "Name      shop-api\nReplicas  2\n" {
		t.Fatalf("table = %q", out.String())
	}
}

// blockingReader never returns until closed, like a terminal nobody types in.
type blockingReader struct{ done chan struct{} }

func (b blockingReader) Read([]byte) (int, error) { <-b.done; return 0, errors.New("closed") }

func newBlocked(t *testing.T) (*Printer, *syncBuffer) {
	t.Helper()
	in := blockingReader{done: make(chan struct{})}
	t.Cleanup(func() { close(in.done) })
	errw := &syncBuffer{}
	p := New(in, &syncBuffer{}, errw, func(string) string { return "" }, false)
	p.InTTY, p.TTY = true, true
	return p, errw
}

func cancelSoon() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	return ctx
}

// Ctrl-C at a prompt must stop kx at once, not after the next Enter, and a
// no-echo read must not leave the terminal without echo.
func TestReadSecret_InterruptedRestoresTerminal(t *testing.T) {
	p, errw := newBlocked(t)
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	p.ReadPassword = func() (string, error) { <-unblock; return tok, nil }
	restored := false
	p.saveTerminal = func() func() { return func() { restored = true } }

	got, err := p.ReadSecret(cancelSoon(), "Token: ")
	if !errors.Is(err, context.Canceled) || got != "" {
		t.Fatalf("ReadSecret = %q, %v; want context.Canceled", got, err)
	}
	if !restored {
		t.Fatal("terminal state not restored")
	}
	if errw.String() != "Token: \n" {
		t.Fatalf("stderr = %q", errw.String())
	}
}

func TestPrompts_Interrupted(t *testing.T) {
	t.Run("ReadLine", func(t *testing.T) {
		p, _ := newBlocked(t)
		if _, err := p.ReadLine(cancelSoon()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("Confirm", func(t *testing.T) {
		p, _ := newBlocked(t)
		if ok, err := p.Confirm(cancelSoon(), "Revoke?"); ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("Confirm = %v, %v", ok, err)
		}
	})
	t.Run("Choose by number", func(t *testing.T) {
		p, _ := newBlocked(t)
		p.rawMode = nil
		if i, err := p.Choose(cancelSoon(), "Pick", []string{"a", "b"}, 0); i != -1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("Choose = %d, %v", i, err)
		}
	})
}

// Text from the server (names, statuses, log lines) can't drive the
// terminal: escape sequences, other control characters and bidi overrides
// are dropped. Tabs and line breaks survive in results and log lines.
func TestPrinter_StripsTerminalControlFromServerText(t *testing.T) {
	const evil = "ci\x1b]0;pwned\x07\u202e\u0085x"
	p, out, errw := newTest("")
	p.Info("Created %s (%s).", "kx_AbCdEfGh", evil)
	p.Note("%s", evil)
	p.Warn("%s", evil)
	p.Step("Built", evil, 0)
	p.Table([]string{"NAME"}, [][]string{{evil}, {"tab\there"}})
	p.Result("first\tline\x1b[2J\u2066\nsecond")
	p.LogLine("2026-11-12 09:00:00", "app says \x1b[31mhi\u202e\tthere")
	all := out.String() + errw.String()
	for _, bad := range []string{"\x1b", "\x07", "\u202e", "\u2066", "\u0085"} {
		if strings.Contains(all, bad) {
			t.Errorf("output keeps %q:\n%q", bad, all)
		}
	}
	if !strings.Contains(errw.String(), "Created kx_AbCdEfGh (ci]0;pwnedx).") {
		t.Errorf("stderr = %q", errw.String())
	}
	if !strings.Contains(out.String(), "tab here\n") {
		t.Errorf("a tab inside a table cell must not break the columns: %q", out.String())
	}
	if !strings.Contains(out.String(), "first\tline[2J\nsecond\n") || !strings.Contains(out.String(), "app says [31mhi\tthere\n") {
		t.Errorf("stdout = %q", out.String())
	}
}

// Colour on stdout follows stdout, not stderr: kx logs > app.log from a
// terminal must not write escape codes into the file.
func TestPrinter_LogLineColourFollowsStdout(t *testing.T) {
	p, out, _ := newTest("")
	p.Color, p.OutColor = true, false
	p.LogLine("2026-11-12 09:00:00", "hello")
	if out.String() != "2026-11-12 09:00:00  hello\n" {
		t.Fatalf("redirected stdout = %q", out.String())
	}
	p2, out2, _ := newTest("")
	p2.OutColor = true
	p2.LogLine("2026-11-12 09:00:00", "hello")
	if out2.String() != "\x1b[2m2026-11-12 09:00:00\x1b[0m  hello\n" {
		t.Fatalf("terminal stdout = %q", out2.String())
	}
}
