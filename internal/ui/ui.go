// Package ui is how kx talks to people: progress and prompts on stderr,
// results on stdout (for deploy, the URL alone, so URL=$(kx deploy)
// works), a spinner and colour only on terminals, NO_COLOR honoured,
// --json output with stable keys, and every line scrubbed so a whole
// token is never printed (only its kx_ prefix).
package ui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/kubernetix-ai/kx/internal/safetext"
)

// ErrNoTTY means a prompt was needed but there is no terminal to ask on.
var ErrNoTTY = errors.New("no terminal to prompt on")

// Printer writes kx's output. Create it with New; tests may flip the
// exported fields. A Printer may be passed by value: copies share the lock
// and the stdin buffer.
type Printer struct {
	In  io.Reader
	Out io.Writer // stdout: results only
	Err io.Writer // stderr: progress, prompts, errors

	JSON     bool // --json: results and errors as JSON on stdout
	Color    bool // colour on stderr
	OutColor bool // colour on stdout (stdout is a terminal)
	TTY      bool // stderr is a terminal: spinner, redraws
	InTTY    bool // stdin is a terminal: prompts

	SpinEvery      time.Duration // spinner frame interval on a TTY
	HeartbeatEvery time.Duration // progress line interval without a TTY

	// ReadPassword reads one line from the terminal without echo.
	ReadPassword func() (string, error)

	rawMode      func() (restore func(), err error)
	saveTerminal func() (restore func())
	in           *lineReader
	mu           *sync.Mutex
}

// lineReader buffers stdin once for every copy of a Printer.
type lineReader struct {
	src io.Reader
	r   *bufio.Reader
}

// New builds a Printer. getenv is usually os.Getenv.
func New(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, jsonMode bool) *Printer {
	p := &Printer{
		In: stdin, Out: stdout, Err: stderr,
		JSON:           jsonMode,
		TTY:            isTerminal(stderr),
		InTTY:          isTerminal(stdin),
		SpinEvery:      100 * time.Millisecond,
		HeartbeatEvery: 10 * time.Second,
		in:             &lineReader{},
		mu:             &sync.Mutex{},
	}
	p.Color = colorEnabled(p.TTY, getenv)
	p.OutColor = colorEnabled(isTerminal(stdout), getenv)
	if f, ok := stdin.(*os.File); ok {
		fd := int(f.Fd())
		p.ReadPassword = func() (string, error) {
			b, err := term.ReadPassword(fd)
			return string(b), err
		}
		p.rawMode = func() (func(), error) {
			old, err := term.MakeRaw(fd)
			if err != nil {
				return nil, err
			}
			return func() { _ = term.Restore(fd, old) }, nil
		}
		p.saveTerminal = func() func() {
			old, err := term.GetState(fd)
			if err != nil {
				return func() {}
			}
			return func() { _ = term.Restore(fd, old) }
		}
	}
	return p
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func colorEnabled(tty bool, getenv func(string) string) bool {
	if !tty || getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return false
	}
	return true
}

var tokenRe = regexp.MustCompile(`kx_[0-9A-Za-z]{40,}`)

// Redact replaces every whole kx_ token in s with its 11-character prefix
// and an ellipsis.
func Redact(s string) string {
	return tokenRe.ReplaceAllStringFunc(s, func(t string) string { return t[:11] + "…" })
}

func (p *Printer) writeErr(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = io.WriteString(p.Err, Redact(s))
}

func (p *Printer) writeOut(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = io.WriteString(p.Out, Redact(s))
}

// CleanField makes a value from outside kx (a name, status, email or URL
// from the API) safe to show inside one line: tabs and line breaks become
// spaces, and other control characters (C0 and C1, so no terminal escape
// sequences) and Unicode format characters such as bidi overrides are
// removed.
func CleanField(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
}

// CleanText makes text from outside kx that may span lines (log lines,
// results) safe to print: tabs and line breaks stay, other control
// characters and the bidi controls that reorder how text is displayed are
// removed. Other format characters stay, so emoji sequences survive.
func CleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n':
			return r
		case unicode.IsControl(r) || isBidiControl(r):
			return -1
		}
		return r
	}, s)
}

func isBidiControl(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// cleanArgs applies CleanField to the string arguments of a format call,
// which is where values from the API end up.
func cleanArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if s, ok := a.(string); ok {
			a = CleanField(s)
		}
		out[i] = a
	}
	return out
}

// Info prints a progress line on stderr.
func (p *Printer) Info(format string, args ...any) {
	p.writeErr(fmt.Sprintf(format, cleanArgs(args)...) + "\n")
}

// Note prints an indented detail line on stderr.
func (p *Printer) Note(format string, args ...any) {
	p.writeErr("  " + fmt.Sprintf(format, cleanArgs(args)...) + "\n")
}

// Warn prints a warning on stderr.
func (p *Printer) Warn(format string, args ...any) {
	p.writeErr(p.paint(yellow, "!") + " " + fmt.Sprintf(format, cleanArgs(args)...) + "\n")
}

// Step prints a finished step: "✓ Built     shop-api:3f9c   1m 12s".
// elapsed 0 leaves the time off.
func (p *Printer) Step(label, detail string, elapsed time.Duration) {
	p.writeErr(p.paint(green, "✓") + " " + stepLine(label, detail, elapsed) + "\n")
}

// stepLine lays out a step's label, detail and time, cleaning the label
// and detail of terminal control text first.
func stepLine(label, detail string, elapsed time.Duration) string {
	label, detail = CleanField(label), CleanField(detail)
	line := fmt.Sprintf("%-10s%s", label, detail)
	if elapsed > 0 {
		line = fmt.Sprintf("%-10s%-45s%s", label, detail, Duration(elapsed))
	}
	return strings.TrimRight(line, " ")
}

// StepFailed prints a step that failed: "✗ Failed    v15  …   1m 2s".
func (p *Printer) StepFailed(label, detail string, elapsed time.Duration) {
	p.writeErr(p.paint(red, "✗") + " " + stepLine(label, detail, elapsed) + "\n")
}

// Result prints a result on stdout (for deploy, the URL alone).
func (p *Printer) Result(line string) {
	p.writeOut(CleanText(line) + "\n")
}

// LogLine prints one line of an app's output on stdout after its
// timestamp, which is dimmed only when stdout is a colour terminal.
func (p *Printer) LogLine(stamp, line string) {
	p.writeOut(p.paintOut(dim, CleanField(stamp)) + "  " + CleanText(line) + "\n")
}

// PrintJSON writes v as indented JSON on stdout (keys sorted for maps,
// struct order otherwise), redacting tokens.
func (p *Printer) PrintJSON(v any) error { return p.encode(v, "  ") }

// PrintJSONLine writes v as one line of JSON (for streams such as logs).
func (p *Printer) PrintJSONLine(v any) error { return p.encode(v, "") }

func (p *Printer) encode(v any, indent string) error {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return err
	}
	// Values often hold text kx didn't write (app logs, server messages).
	p.writeOut(safetext.EscapeJSON(b.String()))
	return nil
}

// ErrorJSON is the --json shape of a failure.
type ErrorJSON struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inside of ErrorJSON.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

// Fail prints a problem: two lines on stderr, or with --json
// {"error":{"code","message","fix"}} on stdout.
func (p *Printer) Fail(code, what, fix string) {
	what, fix = CleanField(what), CleanField(fix)
	if p.JSON {
		_ = p.PrintJSONLine(ErrorJSON{ErrorBody{Code: code, Message: what, Fix: fix}})
		return
	}
	p.writeErr(p.paint(bold, what) + "\n" + fix + "\n")
}

// Table prints rows under headers on stdout, aligned in columns. With no
// headers it prints aligned "key  value" rows.
func (p *Printer) Table(headers []string, rows [][]string) {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	row := func(cells []string) string {
		clean := make([]string, len(cells))
		for i, c := range cells {
			clean[i] = CleanField(c)
		}
		return strings.Join(clean, "\t")
	}
	if len(headers) > 0 {
		fmt.Fprintln(w, row(headers))
	}
	for _, r := range rows {
		fmt.Fprintln(w, row(r))
	}
	_ = w.Flush()
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	p.writeOut(strings.Join(lines, "\n") + "\n")
}

// Duration formats an elapsed time as "14s", "1m 12s" or "1h 2m".
func Duration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// Date formats t as "11 Dec 2026" in UTC.
func Date(t time.Time) string { return t.UTC().Format("2 Jan 2006") }

const (
	bold   = "1"
	dim    = "2"
	red    = "31"
	green  = "32"
	yellow = "33"
)

func (p *Printer) paint(code, s string) string {
	if !p.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p *Printer) paintOut(code, s string) string {
	if !p.OutColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Dim greys s out when stderr is a colour terminal. Use it only for text
// written to stderr.
func (p *Printer) Dim(s string) string { return p.paint(dim, s) }

// Bold makes s bold when stderr is a colour terminal. Use it only for text
// written to stderr.
func (p *Printer) Bold(s string) string { return p.paint(bold, s) }

// Ago describes how long before now t was: "just now", "2 minutes ago",
// "3 hours ago", "5 days ago".
func Ago(t, now time.Time) string {
	d := now.Sub(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/(24*time.Hour)), "day")
}
