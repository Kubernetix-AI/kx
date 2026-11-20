package deploy

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/procenv"
)

// versionRe matches a git tag that names a version: v1.2.3 or 1.2.3, with
// an optional pre-release such as -rc.1 (semantic versioning, without
// build metadata, which a registry tag can't hold).
var versionRe = regexp.MustCompile(`^v?((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)` +
	`(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?)$`)

// maxTagLen is the longest image tag a registry takes.
const maxTagLen = 128

// Tags are the tags kx pushes a build under, and the commit it is of.
type Tags struct {
	// Commit is the tag ImageTag gives.
	Commit string
	// Version is the highest version among the git tags of the commit
	// being built (and the tag the CI run is for), without its v: "0.0.11"
	// for v0.0.11; "" when there is none.
	Version string
	// Revision is the full commit when the build is of it as it is, else
	// "" (and Version is "" too).
	Revision string
}

// BuildTags returns the tags of a build of dir. Version and Revision are
// "" when the folder isn't a git repository, and outside CI when it has
// uncommitted changes, since the image then isn't the tagged commit's
// code. In CI the CI system's commit is taken as it is, as for the
// commit's own tag.
func BuildTags(ctx context.Context, env ci.Env, dir string, now time.Time) Tags {
	commit, sha := imageTag(ctx, env, dir, now)
	if sha == "" {
		return Tags{Commit: commit}
	}
	var tags []string
	if env.Tag != "" {
		tags = append(tags, env.Tag)
	}
	tags = append(tags, gitTagsAt(ctx, dir, sha)...)
	return Tags{Commit: commit, Version: highestVersion(tags), Revision: sha}
}

// gitTagsAt lists the tags of commit sha in dir's repository, annotated or
// not; none when git can't tell (no repository, a shallow clone without
// tags).
func gitTagsAt(ctx context.Context, dir, sha string) []string {
	cmd := procenv.Command(ctx, "git", "tag", "--list", "--points-at="+sha)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// versionOf returns the version a git tag names, without its v, or "".
func versionOf(tag string) string {
	m := versionRe.FindStringSubmatch(tag)
	if m == nil || len(m[1]) > maxTagLen {
		return ""
	}
	return m[1]
}

// highestVersion returns the highest version (in semantic versioning's
// order) among tags, without its v, or "" when none is a version.
func highestVersion(tags []string) string {
	best := ""
	for _, t := range tags {
		v := versionOf(t)
		if v != "" && (best == "" || compareVersions(v, best) > 0) {
			best = v
		}
	}
	return best
}

// compareVersions orders two versions versionOf returned: by major, minor
// and patch number, then a release after its pre-releases, and
// pre-releases part by part (numbers by value, before words, which sort
// as text; more parts sort later when the rest is equal).
func compareVersions(a, b string) int {
	aCore, aPre, _ := strings.Cut(a, "-")
	bCore, bPre, _ := strings.Cut(b, "-")
	an, bn := strings.Split(aCore, "."), strings.Split(bCore, ".")
	for i := range an {
		if c := compareNumbers(an[i], bn[i]); c != 0 {
			return c
		}
	}
	switch {
	case aPre == bPre:
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	ap, bp := strings.Split(aPre, "."), strings.Split(bPre, ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		aNum, bNum := isNumber(ap[i]), isNumber(bp[i])
		var c int
		switch {
		case aNum && bNum:
			c = compareNumbers(ap[i], bp[i])
		case aNum:
			c = -1
		case bNum:
			c = 1
		default:
			c = strings.Compare(ap[i], bp[i])
		}
		if c != 0 {
			return c
		}
	}
	return compareInts(len(ap), len(bp))
}

// compareNumbers compares two numbers written without leading zeros, of
// any length.
func compareNumbers(a, b string) int {
	if c := compareInts(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func isNumber(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
