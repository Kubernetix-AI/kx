package scaffold

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Folders EnvExposed doesn't look in: they hold no .env file of the
// app's own, and can be large.
var skipFolders = map[string]bool{".git": true, "node_modules": true, ".venv": true}

// maxWalk bounds how many entries EnvExposed looks at.
const maxWalk = 100_000

// ignoreState says what kind of .dockerignore a folder has.
type ignoreState int

const (
	ignoreNone   ignoreState = iota // there is none
	ignoreOK                        // a regular file kx could read
	ignoreBroken                    // something Docker can't read: a link to nothing, a folder
)

// loadDockerignore reads dir's .dockerignore.
func loadDockerignore(dir string) (ignoreRules, ignoreState) {
	p := filepath.Join(dir, ".dockerignore")
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil, ignoreNone
	}
	data, err := readFile(dir, ".dockerignore")
	if err != nil {
		return nil, ignoreBroken
	}
	return parseDockerignore(data), ignoreOK
}

// DockerignoreBroken reports whether dir has something called
// .dockerignore that Docker can't read (a link to nothing, a folder), so
// it keeps nothing out of the image, and kx can neither use nor replace it.
func DockerignoreBroken(dir string) bool {
	_, state := loadDockerignore(dir)
	return state == ignoreBroken
}

// EnvExposed returns the .env and .env.* files in dir and its subfolders
// (as slash-separated paths relative to dir) that the folder's existing
// .dockerignore doesn't exclude, so a Dockerfile that copies the folder
// would put them in the image. With no .dockerignore it returns nothing:
// Write adds one that excludes them. A broken .dockerignore excludes
// nothing.
func EnvExposed(dir string) ([]string, error) {
	rules, state := loadDockerignore(dir)
	if state == ignoreNone {
		return nil, nil
	}
	return contextFiles(dir, rules, isEnvFile)
}

// contextFiles returns the files in dir and its subfolders whose names
// match, and that rules don't exclude: what Docker would send with the
// build. Paths are slash-separated and relative to dir. It skips .git,
// node_modules and .venv, and stops after maxWalk entries.
func contextFiles(dir string, rules ignoreRules, match func(name string) bool) ([]string, error) {
	// Without a ! line, nothing in an excluded folder can be let back
	// in, so the walk can skip it.
	prune := !rules.hasNegation()
	var found []string
	seen := 0
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // a folder kx can't read: Docker would fail on it too
		}
		if seen++; seen > maxWalk {
			return fs.SkipAll
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			if skipFolders[e.Name()] || (prune && rules.excludes(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if match(e.Name()) && !rules.excludes(rel) {
			found = append(found, rel)
		}
		return nil
	})
	return found, err
}

// EnvListed reports whether dir's .dockerignore keeps the .env at the top
// of the folder out of the image.
func EnvListed(dir string) bool {
	rules, state := loadDockerignore(dir)
	return state == ignoreOK && rules.excludes(".env")
}

func isEnvFile(name string) bool { return name == ".env" || strings.HasPrefix(name, ".env.") }

// envLines are what IgnoreEnv adds: .env files at any depth, the top of
// the folder included.
var envLines = []string{"**/.env", "**/.env.*"}

// IgnoreEnv adds the lines **/.env and **/.env.* to dir's .dockerignore,
// which must be a regular file, keeping its line endings.
func IgnoreEnv(dir string) error {
	p := filepath.Join(dir, ".dockerignore")
	data, err := readFile(dir, ".dockerignore")
	if err != nil {
		return err
	}
	nl := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		nl = "\r\n"
	}
	add := strings.Join(envLines, nl) + nl
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

func (rules ignoreRules) hasNegation() bool {
	for _, r := range rules {
		if r.negate {
			return true
		}
	}
	return false
}

// excludes reports whether rel (slash-separated, relative to the folder)
// stays out of the build, as Docker decides: a line matches the path or
// any folder above it, and the last line that matches decides.
func (rules ignoreRules) excludes(rel string) bool {
	parts := strings.Split(rel, "/")
	parents := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		parents = append(parents, strings.Join(parts[:i], "/"))
	}
	excluded := false
	for _, r := range rules {
		if r.negate != excluded {
			continue
		}
		match := r.re.MatchString(rel)
		for _, p := range parents {
			if match {
				break
			}
			match = r.re.MatchString(p)
		}
		if match {
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
