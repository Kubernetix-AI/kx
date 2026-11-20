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

// Provenance says which build of which commit an image is. kx records it
// as labels on the image it builds, and deploys an image whose labels
// match instead of building the same commit again.
type Provenance struct {
	Revision   string // the full commit
	Context    string // the build folder, relative to the repository's root ("." for the root), with /
	Dockerfile string // the Dockerfile, relative to the repository's root, with /
}

var labelValueRe = regexp.MustCompile(`^[ -~]{1,512}$`)

func (p *Provenance) valid() bool {
	return p != nil && commitRe.MatchString(p.Revision) && labelValueRe.MatchString(p.Context) && labelValueRe.MatchString(p.Dockerfile)
}

// labels are the labels an image built this way carries; none for nil.
func (p *Provenance) labels() map[string]string {
	if p == nil {
		return nil
	}
	return map[string]string{LabelRevision: p.Revision, LabelContext: p.Context, LabelDockerfile: p.Dockerfile}
}

// sameBuild reports whether an image with labels l was built this way:
// same commit, folder and Dockerfile.
func (p *Provenance) sameBuild(l map[string]string) bool {
	return p.sameCommit(l) && l[LabelContext] == p.Context && l[LabelDockerfile] == p.Dockerfile
}

// sameCommit reports whether an image with labels l was built from this
// commit, whichever way.
func (p *Provenance) sameCommit(l map[string]string) bool {
	return p != nil && l[LabelRevision] != "" && l[LabelRevision] == p.Revision
}

// ProvenanceOf describes a build of revision (the full commit, as
// BuildTags returns it) from df. It is nil when revision is "" (the folder
// isn't that commit as it is), or the build folder or Dockerfile isn't
// inside the repository; kx then neither labels nor reuses the image.
func ProvenanceOf(ctx context.Context, revision string, df *Dockerfile) *Provenance {
	if revision == "" || df == nil {
		return nil
	}
	cmd := procenv.Command(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = df.Dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	root := strings.TrimSpace(string(out))
	dir, ok := within(root, df.Dir)
	if !ok {
		return nil
	}
	file, ok := within(root, df.Path)
	if !ok {
		return nil
	}
	p := &Provenance{Revision: revision, Context: dir, Dockerfile: file}
	if !p.valid() {
		return nil
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
