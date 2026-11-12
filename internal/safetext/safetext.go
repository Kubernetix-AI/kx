// Package safetext keeps text kx didn't write (build output, app logs,
// server messages) from being read as a CI workflow command when kx
// prints it.
//
// GitHub Actions acts on a line that starts with "::" after any leading
// whitespace (any Unicode space, not only ASCII), and on the older
// "##[name]" form anywhere in a line; Azure Pipelines acts on "##vso["
// anywhere in a line. Neutralize breaks each of those up while keeping the
// text readable.
package safetext

import (
	"strings"
	"unicode"
)

// isSpace matches what the GitHub runner trims before it looks for "::"
// (.NET's Char.IsWhiteSpace, which is Go's unicode.IsSpace).
func isSpace(r rune) bool { return unicode.IsSpace(r) }

// Neutralize returns s with every line made inert: a leading "::" becomes
// ": :", and "##[" and "##vso[" anywhere become "# #[" and "# #vso[".
func Neutralize(s string) string {
	if !strings.Contains(s, "::") && !strings.Contains(s, "##") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = neutralizeLine(line)
	}
	return strings.Join(lines, "\n")
}

func neutralizeLine(line string) string {
	if t := strings.TrimLeftFunc(line, isSpace); strings.HasPrefix(t, "::") {
		line = line[:len(line)-len(t)] + ": :" + t[2:]
	}
	if !strings.Contains(line, "##") {
		return line
	}
	var b strings.Builder
	for i := 0; i < len(line); {
		if strings.HasPrefix(line[i:], "##[") || hasPrefixFold(line[i:], "##vso[") {
			b.WriteString("# #")
			i += 2
			continue
		}
		b.WriteByte(line[i])
		i++
	}
	return b.String()
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// Commands lists the workflow commands a CI runner would act on in out:
// "::name::…" after any leading Unicode whitespace (honouring GitHub's
// stop-commands blocks), and "##[" or "##vso[" anywhere in a line. Tests
// use it to check that untrusted text issues none.
func Commands(out string) []string {
	var got []string
	stop := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimLeftFunc(line, isSpace)
		if stop != "" {
			if t == "::"+stop+"::" {
				stop = ""
				got = append(got, "resume-commands")
			}
			continue
		}
		if tok, ok := strings.CutPrefix(t, "::stop-commands::"); ok && tok != "" {
			stop = tok
			got = append(got, "stop-commands")
			continue
		}
		if strings.HasPrefix(t, "::") {
			got = append(got, t)
			continue
		}
		if strings.Contains(line, "##[") || strings.Contains(strings.ToLower(line), "##vso[") {
			got = append(got, line)
		}
	}
	return got
}
