package deploy

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kubernetix-ai/kx/internal/procenv"
)

// The labels kx puts on the images it builds of a commit as it is.
const (
	LabelRevision   = "org.opencontainers.image.revision" // the full commit
	LabelContext    = "ai.kubernetix.kx.context"          // the build folder, from the repository's root
	LabelDockerfile = "ai.kubernetix.kx.dockerfile"       // the Dockerfile, from the repository's root
)

// Provenance says which commit an image was built from, and where the
// build sat in the repository. kx records it as labels on the image, for
// people and tools to read; the only thing kx itself reads back is the
// commit, to tell a version tag on an earlier build of the same commit
// from one on another commit. Nothing is deployed because of a label.
type Provenance struct {
	Revision   string // the full commit
	Context    string // the build folder, relative to the repository's root ("." for the root), with /; "" when unknown
	Dockerfile string // the Dockerfile, relative to the repository's root, with /; "" when unknown
}

var labelValueRe = regexp.MustCompile(`^[ -~]{1,512}$`)

func (p *Provenance) valid() bool {
	return p != nil && commitRe.MatchString(p.Revision) &&
		(p.Context == "" || labelValueRe.MatchString(p.Context)) &&
		(p.Dockerfile == "" || labelValueRe.MatchString(p.Dockerfile))
}

// labels are the labels an image built this way carries; none for nil.
func (p *Provenance) labels() map[string]string {
	if p == nil {
		return nil
	}
	l := map[string]string{LabelRevision: p.Revision}
	if p.Context != "" {
		l[LabelContext] = p.Context
	}
	if p.Dockerfile != "" {
		l[LabelDockerfile] = p.Dockerfile
	}
	return l
}

// sameCommit reports whether an image with labels l says it was built
// from this commit.
func (p *Provenance) sameCommit(l map[string]string) bool {
	return p != nil && l[LabelRevision] != "" && l[LabelRevision] == p.Revision
}

// ProvenanceOf describes a build of revision (the full commit, as
// BuildTags returns it) from df. It is nil when revision is "" (the folder
// isn't that commit as it is): such an image gets no labels. The build
// folder and Dockerfile are left out when they aren't inside the
// repository, or can't be written as a label.
func ProvenanceOf(ctx context.Context, revision string, df *Dockerfile) *Provenance {
	if !commitRe.MatchString(revision) || df == nil {
		return nil
	}
	p := &Provenance{Revision: revision}
	cmd := procenv.Command(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = df.Dir
	out, err := cmd.Output()
	if err != nil {
		return p
	}
	root := strings.TrimSpace(string(out))
	dir, okDir := within(root, df.Dir)
	file, okFile := within(root, df.Path)
	if okDir && okFile && labelValueRe.MatchString(dir) && labelValueRe.MatchString(file) {
		p.Context, p.Dockerfile = dir, file
	}
	return p
}

// within returns path relative to root, with /, when it is inside root
// (after resolving symbolic links on both).
func within(root, path string) (string, bool) {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(r, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
