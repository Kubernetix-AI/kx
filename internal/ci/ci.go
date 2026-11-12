// Package ci recognises the CI system kx runs in and reads the facts kx
// needs from it: the commit, the run identity (for idempotency keys), the
// name for the User-Agent, and whether the pipeline is for a pull or merge
// request, which kx never deploys.
package ci

import (
	"context"
	"errors"
	"strings"

	"github.com/kubernetix-ai/kx/internal/procenv"
	"github.com/kubernetix-ai/kx/internal/uuid"
)

// Provider identifies a CI system.
type Provider int

// Known providers. Generic means CI=true with nothing more specific.
const (
	None Provider = iota
	GitHub
	GitLab
	Bitbucket
	Generic
)

// SkipMessage is what kx prints, before exiting 0, in a pipeline it does
// not deploy from.
const SkipMessage = "Pull request previews aren't available yet; this workflow only deploys on push."

// Env describes the CI run kx is part of. The zero value means "not CI".
type Env struct {
	Provider Provider
	// Name goes into the User-Agent: "GitHub Actions", "GitLab CI",
	// "Bitbucket Pipelines" or "CI". Empty outside CI.
	Name string
	IsCI bool
	// CommitSHA is the commit the CI system says it is building, if any.
	CommitSHA  string
	Repository string
	// RunID is the run, pipeline or build number; RunAttempt tells retries
	// of the same run apart (GitHub's run attempt, GitLab's job ID,
	// Bitbucket's step UUID, which IdempotencyKey doesn't rely on).
	RunID      string
	RunAttempt string
	// Event is the provider's trigger name (GitHub event, GitLab pipeline
	// source, or "push"/"pull_request" on Bitbucket).
	Event string
	// IsPullRequest is true for pull and merge request pipelines.
	IsPullRequest bool
}

// Detect reads the environment through getenv.
func Detect(getenv func(string) string) Env {
	switch {
	case getenv("GITHUB_ACTIONS") == "true":
		event := getenv("GITHUB_EVENT_NAME")
		return Env{
			Provider: GitHub, Name: "GitHub Actions", IsCI: true,
			CommitSHA:     getenv("GITHUB_SHA"),
			Repository:    getenv("GITHUB_REPOSITORY"),
			RunID:         getenv("GITHUB_RUN_ID"),
			RunAttempt:    getenv("GITHUB_RUN_ATTEMPT"),
			Event:         event,
			IsPullRequest: event == "pull_request" || event == "pull_request_target",
		}
	case getenv("GITLAB_CI") == "true":
		source := getenv("CI_PIPELINE_SOURCE")
		return Env{
			Provider: GitLab, Name: "GitLab CI", IsCI: true,
			CommitSHA:  getenv("CI_COMMIT_SHA"),
			Repository: getenv("CI_PROJECT_PATH"),
			RunID:      getenv("CI_PIPELINE_ID"),
			RunAttempt: getenv("CI_JOB_ID"),
			Event:      source,
			// Merge request pipelines, and pull requests from an external
			// repository (GitLab CI/CD for GitHub repositories).
			IsPullRequest: source == "merge_request_event" || source == "external_pull_request_event" ||
				getenv("CI_MERGE_REQUEST_IID") != "" || getenv("CI_EXTERNAL_PULL_REQUEST_IID") != "",
		}
	case getenv("BITBUCKET_BUILD_NUMBER") != "":
		pr := getenv("BITBUCKET_PR_ID") != ""
		event := "push"
		if pr {
			event = "pull_request"
		}
		return Env{
			Provider: Bitbucket, Name: "Bitbucket Pipelines", IsCI: true,
			CommitSHA:     getenv("BITBUCKET_COMMIT"),
			Repository:    getenv("BITBUCKET_REPO_FULL_NAME"),
			RunID:         getenv("BITBUCKET_BUILD_NUMBER"),
			RunAttempt:    getenv("BITBUCKET_STEP_UUID"),
			Event:         event,
			IsPullRequest: pr,
		}
	}
	switch strings.ToLower(getenv("CI")) {
	case "true", "1":
		return Env{Provider: Generic, Name: "CI", IsCI: true}
	}
	return Env{}
}

// SkipsDeploy reports whether kx must not deploy in this pipeline: any pull
// or merge request, and on GitHub every event other than push and
// workflow_dispatch, including a run that names no event (fail closed).
func (e Env) SkipsDeploy() bool {
	if e.IsPullRequest {
		return true
	}
	if e.Provider == GitHub && e.Event != "push" && e.Event != "workflow_dispatch" {
		return true
	}
	return false
}

// IdempotencyKey returns a UUIDv5 of repository, run ID, attempt and app,
// so retries inside one CI attempt reuse the same key and a re-run deploys
// again. Only GitHub Actions (run ID and run attempt) and GitLab CI (a new
// job ID for every retry) get one: elsewhere it isn't known that a re-run
// changes the attempt, and a reused key would hand back the earlier deploy
// instead of deploying again. It is empty then, and outside CI; callers
// make one random key per invocation.
func (e Env) IdempotencyKey(app string) string {
	if e.Provider != GitHub && e.Provider != GitLab {
		return ""
	}
	if !e.IsCI || e.RunID == "" || e.RunAttempt == "" {
		return ""
	}
	key, err := uuid.V5(uuid.NamespaceKx, strings.Join([]string{e.Repository, e.RunID, e.RunAttempt, app}, "\x00"))
	if err != nil {
		return ""
	}
	return key
}

// Commit returns the commit being built: the CI system's value when it
// gives one, otherwise `git rev-parse HEAD` in dir.
func (e Env) Commit(ctx context.Context, dir string) (string, error) {
	if e.CommitSHA != "" {
		return e.CommitSHA, nil
	}
	cmd := procenv.Command(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("this folder is not a git repository with a commit")
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("git did not report a commit")
	}
	return sha, nil
}
