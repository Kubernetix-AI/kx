package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProvenanceOf(t *testing.T) {
	ctx := context.Background()
	if p := ProvenanceOf(ctx, testRevision, &Dockerfile{Dir: t.TempDir(), Path: "Dockerfile"}); p != nil {
		t.Fatalf("outside a repository: %+v", p)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git(t, root, "init", "-q")
	api := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(api, "Dockerfile.prod"), "FROM scratch\n")
	elsewhere := filepath.Join(t.TempDir(), "Dockerfile")
	writeFile(t, elsewhere, "FROM scratch\n")

	tests := []struct {
		name     string
		revision string
		df       Dockerfile
		want     *Provenance
	}{
		{"the root", testRevision, Dockerfile{Dir: root, Path: filepath.Join(root, "Dockerfile")}, &Provenance{testRevision, ".", "Dockerfile"}},
		{"a subfolder", testRevision, Dockerfile{Dir: api, Path: filepath.Join(api, "Dockerfile.prod")}, &Provenance{testRevision, "services/api", "services/api/Dockerfile.prod"}},
		{"a Dockerfile outside the repository", testRevision, Dockerfile{Dir: root, Path: elsewhere}, nil},
		{"no commit as it is", "", Dockerfile{Dir: root, Path: filepath.Join(root, "Dockerfile")}, nil},
	}
	for _, tt := range tests {
		got := ProvenanceOf(ctx, tt.revision, &tt.df)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("%s: ProvenanceOf() = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestProvenance_Matching(t *testing.T) {
	p := testProvenance
	if !p.sameBuild(p.labels()) || !p.sameCommit(p.labels()) {
		t.Fatal("an image's own labels don't match it")
	}
	other := p
	other.Dockerfile = "Dockerfile.dev"
	if p.sameBuild(other.labels()) || !p.sameCommit(other.labels()) {
		t.Fatal("another Dockerfile of the same commit is the same commit but not the same build")
	}
	var none *Provenance
	if none.sameCommit(p.labels()) || none.sameBuild(p.labels()) || none.labels() != nil {
		t.Fatal("no provenance matches nothing")
	}
	if p.sameCommit(map[string]string{}) || p.sameCommit(nil) {
		t.Fatal("an unlabelled image matched")
	}
}
