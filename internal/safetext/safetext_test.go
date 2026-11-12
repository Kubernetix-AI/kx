package safetext

import (
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
