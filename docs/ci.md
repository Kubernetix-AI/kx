# Deploy from CI

`kx deploy` works the same in CI as on a laptop: it builds the repository's Dockerfile, pushes
the image, deploys it, waits until it's live and prints the URL last. In CI it reads the
token from the `KX_TOKEN` environment variable, so all you need is a deploy token saved as a
secret and one `kx deploy` step.

- [GitHub Actions](#github-actions)
- [GitLab CI](#gitlab-ci)
- [Bitbucket Pipelines](#bitbucket-pipelines)
- [Any other CI](#any-other-ci)
- [What kx does differently in CI](#what-kx-does-differently-in-ci)
- [Troubleshooting](#troubleshooting)

## Get a deploy token

Open the app in the dashboard, go to its **Deploy** tab and click **Create deploy token**. The
token can deploy that one app, push its images and read its status, and it lasts a year. It is
shown once, so copy it straight into your CI's secret store as `KX_TOKEN`.

Keep the secret where only your deploy branch can read it. Anyone who can run a job that reads
the secret can deploy your app.

## GitHub Actions

### 1. Save the token in an environment

1. In your repository, open **Settings → Environments → New environment** and call it
   `production`.
2. Under **Deployment branches and tags**, choose **Selected branches and tags** and add your
   deploy branch, for example `main`.
3. Under **Environment secrets**, click **Add environment secret**, name it `KX_TOKEN` and
   paste the token.

A repository secret works too, but any workflow on any branch can read one, so anyone who can
push a branch could print it. An environment secret is only given to jobs that run on the
branches you allowed.

### 2. Add the workflow

Save this as `.github/workflows/kubernetix.yml` on your deploy branch, with your app's name:

```yaml
name: Deploy to Kubernetix
on:
  push: { branches: [main] }
concurrency: { group: kubernetix-shop-api }
permissions: { contents: read, statuses: write }
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: { name: production, url: "${{ steps.kx.outputs.url }}" }
    steps:
      - uses: actions/checkout@v4
      - id: kx
        uses: Kubernetix-AI/deploy@v1
        with: { app: shop-api, token: "${{ secrets.KX_TOKEN }}" }
```

Push it, and the run log shows the same Checked, Built, Pushed and Live lines as your laptop,
ending in the URL. GitHub then shows:

- the commit status **Kubernetix / production**: "Building", "Deploying v15", then "Live in
  3m 14s" linking to your app, or the first line of the error;
- a **production** deployment with **View deployment**, from the job's `environment:` line;
- a job summary with the version, URL, image digest, commit, how long it took and the
  rollback command;
- the step outputs `url`, `deployment-id`, `version` and `image`, for later steps.

`concurrency` makes deploys of one app run one at a time: a running deploy is never cancelled
and a newer push replaces the waiting run, so the latest commit wins. `statuses: write` lets
kx set the commit status; without it kx prints a warning and deploys anyway.

Use `runs-on: ubuntu-latest`. Windows runners can't build Linux images.

### Action inputs

| Input | Default | What it does |
|---|---|---|
| `token` | (required) | The deploy token, from a secret. |
| `app` | `.kx.json`, or the token's app | The app to deploy. |
| `path` | `.` | The folder to build. |
| `dockerfile` | the Dockerfile in `path` | The Dockerfile to build with, from the repository root or, if it isn't there, from `path`. |
| `image` | | Deploy an image that's already pushed instead of building one. |
| `environment` | `production` | The name in the commit status, "Kubernetix / production". Use your job's environment name. |
| `wait` | `true` | `false` stops once the deploy has started. |
| `timeout` | `15m` | How long to wait for the deploy to go live. |
| `github-token` | `${{ github.token }}` | The token kx sets the commit status with. |
| `kx-version` | pinned per Action release | The kx version to run. |

### Pin the Action to a commit

`@v1` follows the latest 1.x release. To run exactly the code you reviewed, pin the Action to
a full commit SHA and keep the version in a comment:

```yaml
        uses: Kubernetix-AI/deploy@<full 40-character commit SHA> # v1.0.0
```

Find the SHA for a release on the Action's releases page, or with
`git ls-remote https://github.com/Kubernetix-AI/deploy refs/tags/v1.0.0`. Dependabot can keep
pinned SHAs up to date.

### Which events deploy

kx never deploys a pull request. On `pull_request`, `pull_request_target`,
`pull_request_review`, `pull_request_review_comment` and `merge_group` it prints "Pull request
previews aren't available yet, so kx skips pull and merge request runs." and exits 0, so a
workflow that also runs on pull requests stays green and production never gets unmerged code.

Every other event deploys the commit the workflow checked out: `push`, `workflow_dispatch`,
`release`, `schedule`, `workflow_run`, `repository_dispatch` and the rest. Only add the events
you want to deploy from to the workflow's `on:`.

## GitLab CI

### 1. Save the token

In your project, open **Settings → CI/CD → Variables → Add variable**:

- Key: `KX_TOKEN`, value: the token.
- Turn on **Protect variable**, so only pipelines on protected branches (your deploy branch)
  get it, and **Mask variable**, so it never shows in job logs. Tokens contain only letters,
  digits and underscores, so GitLab can mask them.

### 2. Add the job

Add this to `.gitlab-ci.yml`:

```yaml
deploy:
  stage: deploy
  image: docker:27
  services:
    - docker:27-dind
  variables:
    DOCKER_HOST: tcp://docker:2376
    DOCKER_TLS_CERTDIR: "/certs"
    DOCKER_TLS_VERIFY: "1"
    DOCKER_CERT_PATH: "/certs/client"
  resource_group: kubernetix-shop-api
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH && $CI_PIPELINE_SOURCE == "push"
  before_script:
    - apk add --no-cache curl
    - curl -fsSL https://platform.kubernetix.ai/install.sh | KX_VERSION=1.0.0 sh
  script:
    - kx deploy --app shop-api
```

Notes:

- **Docker-in-Docker.** The `docker:27-dind` service runs the Docker engine; the variables
  point the Docker command at it over TLS, with the certificates GitLab shares through
  `DOCKER_TLS_CERTDIR`. kx leaves `DOCKER_HOST`, `DOCKER_TLS_VERIFY` and `DOCKER_CERT_PATH`
  exactly as they are. Docker-in-Docker needs a runner that allows privileged services; on
  GitLab.com's shared runners it works as shown.
- **curl.** The `docker` image is Alpine-based and its built-in `wget` can't check HTTPS
  certificates, so the installer needs `curl`.
- **One deploy at a time.** `resource_group` makes deploys of the app wait for each other.
- **Merge requests.** kx skips merge request pipelines and external pull request pipelines
  (`CI_PIPELINE_SOURCE` is `merge_request_event` or `external_pull_request_event`, or a merge
  request ID is set) with exit 0, even if your rules let the job run in one.
- Pin `KX_VERSION` so a new kx release never changes a deploy you didn't touch.

## Bitbucket Pipelines

### 1. Save the token

Open **Repository settings → Deployments**, pick the **Production** environment and add a
variable named `KX_TOKEN` with the token. Tick **Secured**, so Bitbucket masks it and nobody
can read it back. Deployment variables are only given to steps that deploy to that
environment. On Premium plans, also limit the environment to your deploy branch under its
deployment permissions.

### 2. Add the pipeline

Add this to `bitbucket-pipelines.yml`:

```yaml
image: atlassian/default-image:4

pipelines:
  branches:
    main:
      - step:
          name: Deploy to Kubernetix
          deployment: production
          services:
            - docker
          script:
            - curl -fsSL https://platform.kubernetix.ai/install.sh | KX_VERSION=1.0.0 sh
            - kx deploy --app shop-api

definitions:
  services:
    docker:
      memory: 2048
```

Notes:

- **Docker.** `services: [docker]` gives the step a Docker engine and the `docker` command;
  Bitbucket sets `DOCKER_HOST` for it, and kx leaves it as it is.
- **Memory.** The Docker service gets 1 GB by default, which large builds run out of. The
  `definitions` block above raises it to 2 GB; for more, give the step `size: 2x` and raise
  `memory` further.
- **Pull requests.** kx skips pull request pipelines (when `BITBUCKET_PR_ID` is set) with
  exit 0.
- `deployment: production` is what gives the step the deployment variable.

## Any other CI

Any system that can run a shell script with Docker available works:

```sh
curl -fsSL https://platform.kubernetix.ai/install.sh | KX_VERSION=1.0.0 sh
export KX_TOKEN="$YOUR_SECRET"   # from your CI's secret store; never write it in the script
kx deploy --app shop-api
```

- The installer uses `curl`, or GNU `wget` when there is no `curl`. It installs to
  `/usr/local/bin` when it can write there, otherwise to `~/.local/bin`; set `KX_INSTALL_DIR`
  to choose.
- kx takes the commit from `git rev-parse HEAD`, so run it in a git checkout.
- If your CI sets `CI=true`, kx knows it's in CI and never reads or writes a settings file. With
  `KX_TOKEN` kx never caches a registry credential, whether it recognises the CI or not.
- Capture the URL with `URL=$(kx deploy --app shop-api)`: progress goes to standard error and
  the URL alone to standard output. Or use `kx deploy --json`.
- The exit code says what went wrong (see the table in the [README](../README.md#exit-codes));
  9 means kx stopped waiting but the deploy continues.

If the app is in a `.kx.json` committed to the repository (run `kx link` on your laptop and
commit the file), you can leave out `--app`. A token made for one app needs no `--app` either.

## What kx does differently in CI

kx recognises GitHub Actions, GitLab CI, Bitbucket Pipelines, and any CI that sets `CI=true`.

- **It never asks.** Prompts would hang a pipeline, so kx needs the app from `--app`,
  `KX_APP`, a committed `.kx.json`, or a token made for one app.
- **Pull and merge requests are skipped** with exit 0 (GitHub: `pull_request`,
  `pull_request_target`, the two pull request review events and `merge_group`; GitLab: merge
  request and external pull request pipelines; Bitbucket: `BITBUCKET_PR_ID` set). Every other
  run deploys the commit it checked out.
- **Images are tagged with the commit**: the first 12 characters of the commit the CI system
  is building.
- **Retries never deploy twice.** Every deploy request carries an idempotency key, and when
  kx retries a request after a network or server hiccup it sends the same key, so Kubernetix
  returns the deploy it already started. On GitHub Actions and GitLab CI the key is made from
  the repository, the run, the attempt (GitHub's run attempt, GitLab's job ID) and the app,
  so even a step that runs kx twice within one attempt deploys once, while re-running a
  workflow or retrying a job is a new attempt and deploys again. On Bitbucket Pipelines and
  other CI systems each kx run gets a fresh key, so a re-run always deploys again.
- **Credentials stay in the job.** kx uses `KX_TOKEN`, never a settings file, and mints a new
  one-hour registry credential for each deploy. Docker gets it through a temporary
  configuration folder that is removed afterwards; it never appears on a command line or in
  the log. On GitHub, kx also masks it before anything could print it.
- **Docker settings are respected.** An existing `DOCKER_HOST`, `DOCKER_TLS_VERIFY` and
  `DOCKER_CERT_PATH` are used as they are. Without `DOCKER_HOST`, kx uses the active Docker
  context's address and TLS certificates. A buildx builder that keeps images in its own cache
  (such as the `docker-container` builder `docker/setup-buildx-action` creates) gets `--load`,
  so the image reaches the engine that pushes it.
- **The User-Agent names the CI system**, for example `kx/1.0.0 (linux/amd64; GitLab CI)`,
  so the token's "last used" line in the dashboard shows where it was used.

Run one deploy of an app at a time. The GitHub and GitLab setups above serialise deploys, and
kx waits for an earlier deploy of the same app to finish before it starts, but a deploy
started from the dashboard or another computer in the same moment can still race it.

## Troubleshooting

- **"This commit was already pushed; uploaded it as 3f9c2a1b7d4e-20261114081530."** The
  registry keeps image tags it has seen (for example on a re-run of the same commit), so kx
  uploaded the image under a new tag. It deploys exactly the image it just uploaded.
- **Exit code 9.** kx stopped waiting after `--timeout`, but the deploy continues. Run
  `kx status` to see whether it went live, or raise the Action's `timeout`.
- **"kx couldn't confirm the rollback…"** Kubernetix didn't answer the rollback request. Run
  `kx status` before trying again, because the rollback may already be running.
- **Workflow command lines in your GitHub log.** kx prints `::add-mask::`, `::warning::` and `::error::` on standard error so standard output carries only the URL; GitHub Actions reads both, though this release hasn't yet been checked on a live run, so tell us if such a line ever shows up as plain text.
