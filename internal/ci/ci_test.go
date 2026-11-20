package ci

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Env
	}{
		{
			name: "not CI",
			env:  map[string]string{"HOME": "/home/sam"},
			want: Env{},
		},
		{
			name: "GitHub push",
			env: map[string]string{
				"CI": "true", "GITHUB_ACTIONS": "true", "GITHUB_SHA": "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9a0b1c2",
				"GITHUB_REPOSITORY": "acme/shop-api", "GITHUB_RUN_ID": "12345", "GITHUB_RUN_ATTEMPT": "2",
				"GITHUB_EVENT_NAME": "push",
			},
			want: Env{
				Provider: GitHub, Name: "GitHub Actions", IsCI: true,
				CommitSHA: "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9a0b1c2", Repository: "acme/shop-api",
				RunID: "12345", RunAttempt: "2", Event: "push",
			},
		},
		{
			name: "GitHub pull request",
			env: map[string]string{
				"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_RUN_ID": "9", "GITHUB_RUN_ATTEMPT": "1",
			},
			want: Env{Provider: GitHub, Name: "GitHub Actions", IsCI: true, RunID: "9", RunAttempt: "1", Event: "pull_request", IsPullRequest: true},
		},
		{
			name: "GitHub pull_request_target",
			env:  map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request_target"},
			want: Env{Provider: GitHub, Name: "GitHub Actions", IsCI: true, Event: "pull_request_target", IsPullRequest: true},
		},
		{
			name: "GitLab merge request pipeline",
			env: map[string]string{
				"CI": "true", "GITLAB_CI": "true", "CI_COMMIT_SHA": "abc123", "CI_PROJECT_PATH": "acme/shop-api",
				"CI_PIPELINE_ID": "777", "CI_JOB_ID": "4242", "CI_PIPELINE_SOURCE": "merge_request_event",
			},
			want: Env{
				Provider: GitLab, Name: "GitLab CI", IsCI: true, CommitSHA: "abc123", Repository: "acme/shop-api",
				RunID: "777", RunAttempt: "4242", Event: "merge_request_event", IsPullRequest: true,
			},
		},
		{
			name: "GitLab push",
			env: map[string]string{
				"GITLAB_CI": "true", "CI_COMMIT_SHA": "abc123", "CI_PROJECT_PATH": "acme/shop-api",
				"CI_PIPELINE_ID": "777", "CI_JOB_ID": "4242", "CI_PIPELINE_SOURCE": "push",
			},
			want: Env{
				Provider: GitLab, Name: "GitLab CI", IsCI: true, CommitSHA: "abc123", Repository: "acme/shop-api",
				RunID: "777", RunAttempt: "4242", Event: "push",
			},
		},
		{
			name: "Bitbucket branch build",
			env: map[string]string{
				"CI": "true", "BITBUCKET_BUILD_NUMBER": "88", "BITBUCKET_COMMIT": "def456",
				"BITBUCKET_REPO_FULL_NAME": "acme/shop-api", "BITBUCKET_STEP_UUID": "{step-1}",
			},
			want: Env{
				Provider: Bitbucket, Name: "Bitbucket Pipelines", IsCI: true, CommitSHA: "def456",
				Repository: "acme/shop-api", RunID: "88", RunAttempt: "{step-1}", Event: "push",
			},
		},
		{
			name: "Bitbucket pull request",
			env: map[string]string{
				"BITBUCKET_BUILD_NUMBER": "89", "BITBUCKET_COMMIT": "def456", "BITBUCKET_PR_ID": "12",
				"BITBUCKET_REPO_FULL_NAME": "acme/shop-api",
			},
			want: Env{
				Provider: Bitbucket, Name: "Bitbucket Pipelines", IsCI: true, CommitSHA: "def456",
				Repository: "acme/shop-api", RunID: "89", Event: "pull_request", IsPullRequest: true,
			},
		},
		{
			name: "generic CI=true",
			env:  map[string]string{"CI": "true"},
			want: Env{Provider: Generic, Name: "CI", IsCI: true},
		},
		{
			name: "generic CI=1",
			env:  map[string]string{"CI": "1"},
			want: Env{Provider: Generic, Name: "CI", IsCI: true},
		},
		{
			name: "CI=false is not CI",
			env:  map[string]string{"CI": "false"},
			want: Env{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(envOf(tt.env))
			if got != tt.want {
				t.Fatalf("Detect() =\n %+v\nwant\n %+v", got, tt.want)
			}
		})
	}
}

func TestSkipsDeploy(t *testing.T) {
	tests := []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push"}, false},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch"}, false},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request"}, true},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request_target"}, true},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "merge_group"}, true},
		// Review events check out the pull request's merge commit too.
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request_review"}, true},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request_review_comment"}, true},
		// Every other event deploys the commit the workflow checked out.
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "schedule"}, false},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "release"}, false},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_run"}, false},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "repository_dispatch"}, false},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_call"}, false},
		{map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "merge_request_event"}, true},
		{map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "push"}, false},
		{map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "web"}, false},
		{map[string]string{"BITBUCKET_BUILD_NUMBER": "1", "BITBUCKET_PR_ID": "3"}, true},
		{map[string]string{"BITBUCKET_BUILD_NUMBER": "1"}, false},
		{map[string]string{"CI": "true"}, false},
		{map[string]string{}, false},
	}
	for _, tt := range tests {
		if got := Detect(envOf(tt.env)).SkipsDeploy(); got != tt.want {
			t.Errorf("SkipsDeploy(%v) = %v, want %v", tt.env, got, tt.want)
		}
	}
}

func TestIdempotencyKey(t *testing.T) {
	gh := Detect(envOf(map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/shop-api", "GITHUB_RUN_ID": "12345", "GITHUB_RUN_ATTEMPT": "2",
	}))
	if got, want := gh.IdempotencyKey("shop-api"), "f1498b64-ba27-5f9a-8704-8b6910db9ac2"; got != want {
		t.Fatalf("IdempotencyKey = %s, want %s", got, want)
	}
	if first, again := gh.IdempotencyKey("shop-api"), gh.IdempotencyKey("shop-api"); first != again {
		t.Fatal("IdempotencyKey is not stable")
	}
	if gh.IdempotencyKey("web") == gh.IdempotencyKey("shop-api") {
		t.Fatal("IdempotencyKey ignores the app")
	}
	retry := gh
	retry.RunAttempt = "3"
	if retry.IdempotencyKey("shop-api") == gh.IdempotencyKey("shop-api") {
		t.Fatal("IdempotencyKey ignores the attempt")
	}
	if got := (Env{}).IdempotencyKey("shop-api"); got != "" {
		t.Fatalf("IdempotencyKey outside CI = %q, want empty", got)
	}
	if got := Detect(envOf(map[string]string{"CI": "true"})).IdempotencyKey("shop-api"); got != "" {
		t.Fatalf("IdempotencyKey without a run ID = %q, want empty", got)
	}
}

// Only providers whose attempt or job ID is known to change on every
// retry get a deterministic key; elsewhere a re-run must deploy again, so
// kx makes a random key per invocation instead.
func TestIdempotencyKey_PerPlatform(t *testing.T) {
	tests := []struct {
		name          string
		env           map[string]string
		deterministic bool
	}{
		{"GitHub run and attempt", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/shop-api", "GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "1"}, true},
		{"GitHub without an attempt", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/shop-api", "GITHUB_RUN_ID": "1"}, false},
		{"GitLab pipeline and job", map[string]string{"GITLAB_CI": "true", "CI_PROJECT_PATH": "acme/shop-api", "CI_PIPELINE_ID": "55", "CI_JOB_ID": "77"}, true},
		{"GitLab without a job ID", map[string]string{"GITLAB_CI": "true", "CI_PROJECT_PATH": "acme/shop-api", "CI_PIPELINE_ID": "55"}, false},
		{"Bitbucket (step re-runs unverified)", map[string]string{"BITBUCKET_BUILD_NUMBER": "41", "BITBUCKET_REPO_FULL_NAME": "acme/shop-api", "BITBUCKET_STEP_UUID": "{9f1c}"}, false},
		{"generic CI", map[string]string{"CI": "true"}, false},
		{"laptop", map[string]string{}, false},
	}
	for _, tt := range tests {
		got := Detect(envOf(tt.env)).IdempotencyKey("shop-api")
		if (got != "") != tt.deterministic {
			t.Errorf("%s: IdempotencyKey = %q, deterministic want %v", tt.name, got, tt.deterministic)
		}
	}
	// GitLab: a retried job has a new job ID and so a new key.
	gl := Detect(envOf(map[string]string{"GITLAB_CI": "true", "CI_PROJECT_PATH": "acme/shop-api", "CI_PIPELINE_ID": "55", "CI_JOB_ID": "77"}))
	retry := gl
	retry.RunAttempt = "78"
	if gl.IdempotencyKey("shop-api") == retry.IdempotencyKey("shop-api") {
		t.Fatal("a retried GitLab job reused the key")
	}
}

// No pull or merge request may ever deploy. An Actions run that doesn't
// say why it runs isn't skipped (exit 0 would look like a deploy); kx
// refuses it instead (EventUnknown).
func TestSkipsDeploy_Edges(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		skip bool
	}{
		{"GitHub without an event name", map[string]string{"GITHUB_ACTIONS": "true"}, false},
		{"GitHub push", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push"}, false},
		{"GitLab external pull request", map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "external_pull_request_event"}, true},
		{"GitLab merge request IID", map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_MERGE_REQUEST_IID": "12"}, true},
		{"GitLab external pull request IID", map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_EXTERNAL_PULL_REQUEST_IID": "3"}, true},
		{"GitLab push", map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "push"}, false},
		{"Bitbucket pull request", map[string]string{"BITBUCKET_BUILD_NUMBER": "1", "BITBUCKET_PR_ID": "9"}, true},
	}
	for _, tt := range tests {
		if got := Detect(envOf(tt.env)).SkipsDeploy(); got != tt.skip {
			t.Errorf("%s: SkipsDeploy = %v, want %v", tt.name, got, tt.skip)
		}
	}
}

func TestEventUnknown(t *testing.T) {
	for _, tt := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"GITHUB_ACTIONS": "true"}, true},
		{map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push"}, false},
		{map[string]string{"GITLAB_CI": "true"}, false},
		{map[string]string{"CI": "true"}, false},
		{map[string]string{}, false},
	} {
		if got := Detect(envOf(tt.env)).EventUnknown(); got != tt.want {
			t.Errorf("EventUnknown(%v) = %v, want %v", tt.env, got, tt.want)
		}
	}
}

func TestCommit_FromEnv(t *testing.T) {
	e := Env{IsCI: true, CommitSHA: "3f9c2a1b7d4e5f60718293a4b5c6d7e8f9a0b1c2"}
	got, err := e.Commit(context.Background(), t.TempDir())
	if err != nil || got != e.CommitSHA {
		t.Fatalf("Commit() = %q, %v", got, err)
	}
}

func TestCommit_FromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=kx", "-c", "user.email=kx@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f")
	run("commit", "-q", "-m", "x")
	want := run("rev-parse", "HEAD")

	got, err := (Env{}).Commit(context.Background(), dir)
	if err != nil || got != want {
		t.Fatalf("Commit() = %q, %v; want %q", got, err, want)
	}
}

func TestCommit_NotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := (Env{}).Commit(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Commit() outside a repository returned no error")
	}
}

func TestDetect_Tag(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"GitHub tag push", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_REF_TYPE": "tag", "GITHUB_REF_NAME": "v1.4.0", "GITHUB_REF": "refs/tags/v1.4.0"}, "v1.4.0"},
		{"GitHub release", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "release", "GITHUB_REF": "refs/tags/1.4.0-rc.1"}, "1.4.0-rc.1"},
		{"GitHub branch push", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_REF_TYPE": "branch", "GITHUB_REF_NAME": "v1.4.0", "GITHUB_REF": "refs/heads/v1.4.0"}, ""},
		{"GitLab tag pipeline", map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_COMMIT_TAG": "v2.0.1"}, "v2.0.1"},
		{"GitLab branch pipeline", map[string]string{"GITLAB_CI": "true", "CI_PIPELINE_SOURCE": "push", "CI_COMMIT_BRANCH": "main"}, ""},
		{"Bitbucket tag build", map[string]string{"BITBUCKET_BUILD_NUMBER": "88", "BITBUCKET_TAG": "3.1.0"}, "3.1.0"},
		{"generic CI", map[string]string{"CI": "true", "CI_COMMIT_TAG": "v9.9.9"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(envOf(tt.env)).Tag; got != tt.want {
				t.Fatalf("Tag = %q, want %q", got, tt.want)
			}
		})
	}
}
