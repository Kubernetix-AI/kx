package safetext

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestNeutralize(t *testing.T) {
	evil := []string{
		"::warning::pwned",
		"  ::add-mask::x",
		" ::warning::nbsp",
		"　::stop-commands::tok",
		"  ::error::em space",
		"\t::set-output name=url::https://evil.example.test",
		"build step ##[warning]mid-line legacy",
		"##[group]legacy at start",
		"see ##vso[task.prependpath]/tmp/evil",
		"see ##VSO[task.setvariable variable=x]1",
	}
	for _, line := range evil {
		got := Neutralize(line)
		if cmds := Commands(got); len(cmds) != 0 {
			t.Errorf("Neutralize(%q) = %q, which a runner reads as %q", line, got, cmds)
		}
	}
	for _, ok := range []string{"plain log line", "a::b is fine mid-line", "# heading", "100% ##"} {
		if got := Neutralize(ok); got != ok {
			t.Errorf("Neutralize(%q) = %q, want it unchanged", ok, got)
		}
	}
}

func TestNeutralize_KeepsTheText(t *testing.T) {
	if got := Neutralize(" ::warning::x"); got != " : :warning::x" {
		t.Fatalf("got %q", got)
	}
	if got := Neutralize("a ##[warning] b ##vso[x]"); got != "a # #[warning] b # #vso[x]" {
		t.Fatalf("got %q", got)
	}
}

func FuzzNeutralize(f *testing.F) {
	f.Add("　::stop-commands::x")
	f.Add("##[warning]##vso[x]")
	f.Fuzz(func(t *testing.T, s string) {
		for _, line := range strings.Split(Neutralize(s), "\n") {
			if cmds := Commands(line); len(cmds) != 0 {
				t.Fatalf("Neutralize(%q) still issues %q", s, cmds)
			}
		}
	})
}

func TestStopToken(t *testing.T) {
	lines := []string{"one", "two"}
	a, b := StopToken(lines), StopToken(lines)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) {
		t.Fatalf("StopToken() = %q, want 32 hex characters", a)
	}
	if a == b {
		t.Fatal("StopToken() gave the same token twice")
	}
}

func TestEscapeJSON(t *testing.T) {
	for _, v := range []string{
		"step ##[warning]mid-line",
		"##[group]at the start",
		"see ##vso[task.prependpath]/tmp/x",
		"see ##VSO[task.setvariable variable=x]1",
		"###[error]three",
		"plain # text ## and #[x]",
	} {
		b, err := json.Marshal(map[string]string{"line": v})
		if err != nil {
			t.Fatal(err)
		}
		got := EscapeJSON(string(b))
		if cmds := Commands(got); len(cmds) != 0 {
			t.Errorf("EscapeJSON(%s) = %s, which a runner reads as %q", b, got, cmds)
		}
		var back map[string]string
		if err := json.Unmarshal([]byte(got), &back); err != nil || back["line"] != v {
			t.Errorf("EscapeJSON(%s) = %s, which decodes to %q (%v), want %q", b, got, back["line"], err, v)
		}
	}
	if got := EscapeJSON(`{"line":"a # b"}`); got != `{"line":"a # b"}` {
		t.Fatalf("EscapeJSON changed JSON with nothing to escape: %s", got)
	}
}

func FuzzEscapeJSON(f *testing.F) {
	f.Add("##[warning]##vso[x]")
	f.Add("###[a]##VsO[b]")
	f.Fuzz(func(t *testing.T, v string) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Skip()
		}
		got := EscapeJSON(string(b))
		if cmds := Commands(got); len(cmds) != 0 {
			t.Fatalf("EscapeJSON(%s) still issues %q", b, cmds)
		}
		var back string
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("EscapeJSON(%s) = %s is not JSON: %v", b, got, err)
		}
		var want string
		_ = json.Unmarshal(b, &want)
		if back != want {
			t.Fatalf("EscapeJSON(%s) decodes to %q, want %q", b, back, want)
		}
	})
}
