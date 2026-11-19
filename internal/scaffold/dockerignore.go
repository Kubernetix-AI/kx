package scaffold

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// EnvExposed returns the .env and .env.* files at the top of dir that the
// folder's existing .dockerignore doesn't exclude, so a Dockerfile that
// copies the folder would put them in the image. With no .dockerignore it
// returns nothing: Write adds one that excludes them.
func EnvExposed(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".dockerignore")) //nolint:gosec // G304: the .dockerignore of the folder kx deploys
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	rules := parseDockerignore(data)
	var exposed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (name != ".env" && !strings.HasPrefix(name, ".env.")) {
			continue
		}
		if !rules.excludes(name) {
			exposed = append(exposed, name)
		}
	}
	return exposed, nil
}

// IgnoreEnv adds the lines .env and .env.* to dir's .dockerignore, which
// must exist, keeping its line endings.
func IgnoreEnv(dir string) error {
	p := filepath.Join(dir, ".dockerignore")
	data, err := os.ReadFile(p) //nolint:gosec // G304: the .dockerignore of the folder kx deploys
	if err != nil {
		return err
	}
	nl := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		nl = "\r\n"
	}
	add := ".env" + nl + ".env.*" + nl
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		add = nl + add
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: the .dockerignore of the folder kx deploys
	if err != nil {
		return err
	}
	if _, err := f.WriteString(add); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ignoreRule is one .dockerignore line.
type ignoreRule struct {
	re     *regexp.Regexp
	negate bool // a !pattern: lets back in what an earlier line excluded
}

type ignoreRules []ignoreRule

// parseDockerignore reads a .dockerignore the way Docker does: # starts a
// comment line, blank lines are skipped, a leading ! negates, and each
// pattern is cleaned and made relative to the folder.
func parseDockerignore(data []byte) ignoreRules {
	var rules ignoreRules
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(line)
		negate := strings.HasPrefix(line, "!")
		if negate {
			line = strings.TrimSpace(line[1:])
		}
		if line == "" {
			continue
		}
		line = path.Clean(filepath.ToSlash(line))
		if len(line) > 1 && line[0] == '/' {
			line = line[1:]
		}
		if re, err := ignoreRegexp(line); err == nil {
			rules = append(rules, ignoreRule{re: re, negate: negate})
		}
	}
	return rules
}

// excludes reports whether name, a file at the top of the folder, stays
// out of the build: the last line that matches it decides.
func (rules ignoreRules) excludes(name string) bool {
	excluded := false
	for _, r := range rules {
		if r.negate == excluded && r.re.MatchString(name) {
			excluded = !r.negate
		}
	}
	return excluded
}

// ignoreRegexp turns a .dockerignore pattern into a regular expression,
// as Docker does: * and ? never match a /, ** matches any number of
// folders, and \ escapes the next character.
func ignoreRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch ch := rs[i]; {
		case ch == '*' && i+1 < len(rs) && rs[i+1] == '*':
			i++
			if i+1 < len(rs) && rs[i+1] == '/' {
				i++
			}
			if i+1 == len(rs) {
				b.WriteString(".*")
			} else {
				b.WriteString("(.*/)?")
			}
		case ch == '*':
			b.WriteString("[^/]*")
		case ch == '?':
			b.WriteString("[^/]")
		case ch == '\\' && i+1 < len(rs):
			i++
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		case strings.ContainsRune(`.+()|{}$^\`, ch):
			b.WriteString(`\` + string(ch))
		default:
			b.WriteRune(ch)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
