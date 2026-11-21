package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/procenv"
)

const maxDockerfileBytes = 1 << 20

// Dockerfile is what Preflight found out about the build.
type Dockerfile struct {
	Dir  string // the build context, absolute
	Path string // the Dockerfile, absolute
	Port int    // the first port the final stage EXPOSEs, 0 if none
}

// Preflight checks, before any API call, that dir has a Dockerfile (or
// that dockerfile exists) and that Docker can build Linux images.
func Preflight(ctx context.Context, b Builder, dir, dockerfile string) (*Dockerfile, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, &kxerr.Usage{What: "There's no folder " + abs + " to build.", Fix: "Check the --build path, then run kx deploy again."}
	}
	var path string
	var data []byte
	if dockerfile != "" {
		if path, err = filepath.Abs(dockerfile); err != nil {
			return nil, err
		}
		if data, err = readDockerfile(path); err != nil {
			return nil, &kxerr.Usage{What: "There's no Dockerfile at " + path + ".", Fix: "Check the --dockerfile path, then run kx deploy again."}
		}
	} else {
		for _, name := range []string{"Dockerfile", "dockerfile"} {
			path = filepath.Join(abs, name)
			if data, err = readDockerfile(path); err == nil {
				break
			}
		}
		if err != nil {
			return nil, &kxerr.NoDockerfile{Dir: abs}
		}
	}
	if err := b.Check(ctx); err != nil {
		return nil, err
	}
	return &Dockerfile{Dir: abs, Path: path, Port: ExposedPort(data)}, nil
}

func readDockerfile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the Dockerfile kx was asked to build
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("%s is not a file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxDockerfileBytes))
	if err != nil {
		return nil, err
	}
	return data, nil
}

var (
	escapeDirectiveRe = regexp.MustCompile("^#\\s*escape\\s*=\\s*([\\\\`])\\s*$")
	heredocRe         = regexp.MustCompile(`(?:^|[^<])<<(-?)["']?([A-Za-z_][A-Za-z0-9_]*)["']?`)
	portRe            = regexp.MustCompile(`^([0-9]{1,5})(?:-[0-9]{1,5})?(?:/(tcp|udp|sctp))?$`)
)

// ExposedPort returns the first TCP port the Dockerfile's final stage
// EXPOSEs, or 0. Ports written as variables are skipped.
func ExposedPort(dockerfile []byte) int {
	text := strings.ReplaceAll(string(dockerfile), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	escape := `\`
	// Parser directives come first, before any instruction or blank line.
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "#") {
			break
		}
		if m := escapeDirectiveRe.FindStringSubmatch(t); m != nil {
			escape = m[1]
		}
	}

	port := 0
	var logical strings.Builder
	var heredocEnd string
	var heredocTrim bool
	for _, raw := range lines {
		if heredocEnd != "" {
			end := raw
			if heredocTrim {
				end = strings.TrimLeft(end, "\t")
			}
			if strings.TrimRight(end, " ") == heredocEnd {
				heredocEnd = ""
			}
			continue
		}
		t := strings.TrimSpace(raw)
		if logical.Len() == 0 && (t == "" || strings.HasPrefix(t, "#")) {
			continue
		}
		if strings.HasSuffix(t, escape) {
			logical.WriteString(strings.TrimSuffix(t, escape) + " ")
			continue
		}
		logical.WriteString(t)
		line := logical.String()
		logical.Reset()

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			port = 0
		case "EXPOSE":
			if port == 0 {
				port = firstTCPPort(fields[1:])
			}
		default:
			if m := heredocRe.FindStringSubmatch(line); m != nil {
				heredocTrim, heredocEnd = m[1] == "-", m[2]
			}
		}
	}
	return port
}

func firstTCPPort(args []string) int {
	for _, a := range args {
		m := portRe.FindStringSubmatch(strings.ToLower(a))
		if m == nil || (m[2] != "" && m[2] != "tcp") {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 && n <= 65535 {
			return n
		}
	}
	return 0
}

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ImageTag is the tag kx builds: the first 12 characters of the commit
// checked out in dir (git rev-parse HEAD), with -dirty-<UTC time> added
// outside CI when the folder has uncommitted changes, or local-<UTC time>
// outside a git repository. The commit a CI system names is never used:
// kx tags what it builds. In CI the folder is built as it is.
func ImageTag(ctx context.Context, env ci.Env, dir string, now time.Time) string {
	tag, _ := imageTag(ctx, env, dir, now)
	return tag
}

// imageTag returns ImageTag's tag and, when the tag names a commit as it
// is (no -dirty- part), that commit; sha is "" otherwise.
func imageTag(ctx context.Context, env ci.Env, dir string, now time.Time) (tag, sha string) {
	stamp := now.UTC().Format("20060102150405")
	sha = gitHead(ctx, dir)
	if sha == "" {
		return "local-" + stamp, ""
	}
	if !env.IsCI && dirty(ctx, dir) {
		return sha[:12] + "-dirty-" + stamp, ""
	}
	return sha[:12], sha
}

// gitHead returns the commit checked out in dir, or "" when git can't
// tell (not a repository, no commit yet).
func gitHead(ctx context.Context, dir string) string {
	cmd := procenv.Command(ctx, "git", "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	cmd.Dir = dir
	out, err := cmd.Output()
	sha := strings.ToLower(strings.TrimSpace(string(out)))
	if err != nil || !commitRe.MatchString(sha) {
		return ""
	}
	return sha
}

// dirty reports uncommitted changes; when git can't tell, it says yes, so
// a clean commit's tag is never reused for other content. The .kx.json
// kx writes doesn't count: it isn't part of the image's code.
func dirty(ctx context.Context, dir string) bool {
	cmd := procenv.Command(ctx, "git", "status", "--porcelain", "-z", "--untracked-files=all")
	cmd.Dir = dir
	out, err := cmd.Output()
	return err != nil || changedBesidesKxJSON(string(out))
}

// changedBesidesKxJSON reads git status --porcelain -z output: entries
// "XY path" separated by NUL, where a rename or copy (X is R or C) is
// followed by the original path as its own entry.
func changedBesidesKxJSON(porcelain string) bool {
	entries := strings.Split(porcelain, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		if e[0] == 'R' || e[0] == 'C' {
			i++ // skip the original path
		}
		if path.Base(strings.TrimSuffix(e[3:], "/")) != config.ProjectFile {
			return true
		}
	}
	return false
}
