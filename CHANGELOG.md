# Changelog

## 1.1.0 (unreleased)

### Fewer steps to a first deploy

- `kx deploy` logs in by itself when this computer has no login, or its login has ended, or
  Kubernetix says the saved token expired, was revoked or is unknown: it opens the token page,
  takes the pasted token and carries on. So do `kx status`, `kx logs`, `kx open`,
  `kx rollback`, `kx link` and `kx apps`. Only when standard input and standard error are a
  terminal, at most once per run, and never in CI, with `--json` or when `KX_TOKEN` is set.
  `kx login`, `kx logout`, `kx whoami`, `kx token` and `kx version` never log in by
  themselves. A pasted token for another organization than the command's is saved, as
  `kx login` does, but the command stops instead of carrying on there.
- `kx login` (and the automatic login) says, before the token prompt, what an account that is
  still awaiting approval sees.
- In an organization with no apps, the first `kx deploy` in a folder creates an app named
  after the folder without asking, with or without a terminal (not in CI). A folder whose name
  says nothing about the app (`src`, `app`, `code`, `Desktop`, the home folder and the like)
  asks for a name, suggesting the git repository's. When the organization has apps, kx asks
  which one, as before.
- `kx deploy --app web` asks whether to create `web` when there's no app by that name (on a
  terminal, and only a `y` creates it; without one it says how; in CI it fails as before).
  It saves the app in `.kx.json` when the folder has none and kx builds the folder's own
  Dockerfile (not in CI, not with `--image`, `--build` elsewhere or `--dockerfile`).
- `kx deploy` offers to write a Dockerfile for Node, Python and Go apps and static sites when
  the folder has none, shows what it wrote, and carries on.
- `kx status`, `kx logs` and `kx open` in a folder that isn't linked use the organization's
  only app, and say so. With none or several, they say how to go on and list up to five apps.
- `kx --help` shows the commands most people need under "Get started:" and the rest under
  "More commands:". `kx` on its own shows two steps: install, then `kx deploy`.

### Environment variables and replicas

- `kx env` (or `kx env list`) lists the names of the app's environment variables; Kubernetix
  hides the values from tokens, and kx says so. `--json` gives `"names"` and
  `"values_hidden": true`.
- `kx env set NAME=VALUE...` and `kx env unset NAME...` change the app's environment
  variables, keeping the others, and `kx scale <replicas>` sets how many replicas it runs. Each
  then deploys the app as configured and waits like `kx deploy` (the same lines, URL and exit
  codes); `--no-deploy` only saves the change. Every check kx can make (the token's
  permissions, a multi-region app, an earlier deploy still running, today's deploys) comes
  before anything is saved.
- When the deploy after a saved change doesn't go live, kx says what became of the change: still
  saved for the next deploy (refused, or kx can't tell whether the deploy started), or undone
  ("Kubernetix put shop-api back to v14, so this change was undone."; kx checks the app when
  it can, and says "most likely undone" for a new value of an existing variable, since it never
  sees values). `--json` results and errors carry `"saved"` and `"deployed"` (live; false with
  `--no-wait`), and errors `"change"`, `"restored"`, `"restored_to"`, `"deployment_id"` and
  `"version"`. What kx doesn't know is `null` with the reason (`"saved_unknown"`,
  `"restored_unknown"`), never a guess: an undo it couldn't confirm, or a save request that got
  no answer kx could use (the line then says how to check).
- `kx env set NAME --stdin` reads a value from standard input, or asks for it without showing
  it, and is the way to set a secret. `kx env set NAME` takes the value from kx's own
  environment, so a CI secret stays off the command line. `--from-file .env` reads `NAME=VALUE`
  lines like the common dotenv tools (a UTF-8 BOM dropped, `export` allowed, an unquoted value
  ending at ` #`, no `${...}` expansion). Names use letters, digits and `_`; an empty value
  needs `--allow-empty`, and a value made only of `=` is refused as a secret typed without a
  name.
- kx never prints a value: not in its output, errors (they name the argument's position or
  the line of the file, never its text) or `--json`. In GitHub Actions it masks each value it
  sets before printing anything.
- These commands need the Configure permission, and Deploy unless `--no-deploy`. Without them
  kx says so before changing anything, with a fix for where the token came from (log in again
  with Configure ticked, or in CI an API token with Configure; for Deploy, also `--no-deploy`);
  the platform's own refusal gets the same fix.
- Changes that change nothing don't deploy: `kx env unset` of names the app doesn't have, `kx
  scale` to the count the app already runs. `kx scale` on an app that autoscales is refused
  unless `--no-deploy`, which saves the count for when autoscaling is off.

### Fixes from the first real runs

- `kx status` during a rollout says what the replicas are doing instead of "1 of 2 ready":
  "v4 ready (1 of 1); v3 stopping" right after a version goes live, "v5 starting; v4 serving
  (2 of 3 ready)" while a new one starts, comparing with the count the live version runs (not
  one saved for the next deploy). `--json` adds `"desired"`, and `"stopping"`,
  `"stopping_version"` or `"starting_version"` when they apply.
- `kx status --json` always has `"latest"`, the newest version, also when it is the live one.
- `kx apps list` and `kx apps get` show each app's live version (the VERSION column was
  empty for most apps); `--json`'s `"version"` is that version. kx looks up 4 apps at a time
  and waits out rate limits; a version it couldn't find out shows `?` (`"version_unknown"`).
- `kx whoami` asks Kubernetix about each saved login that hasn't ended, so a login revoked or
  ended in the dashboard shows as "revoked" or "ended" at once, with how to log in again;
  `--json` adds `"status"`. A login kx couldn't check says "not checked".
- When a CI run creates the app because a committed `.kx.json` names it (it has no `app_id`),
  kx says so, and that committing the `app_id` `kx link` writes stops CI from creating it again
  (for example after the app was deleted in the dashboard).

### Kubernetix's new checks

- Kubernetix checks an image before it deploys it or creates an app with it. kx shows its
  refusal with the platform's sentence and hint: `image_not_found` (exit 4), and `image_refused`
  for an image that may not be used, needs a registry credential, has no linux/amd64 build or
  isn't a valid name (exit 2).
- A deploy refused while a database the app uses isn't running says to wait for it
  (`database_not_ready`); one refused because a variable of the app collides with one a database
  injects names the variable to remove (`env_prefix_conflict`). Other refusals use the platform's
  hint as the fix when it sends one.
- When Kubernetix fails a version at once because its pods can't run (out of memory, an image it
  can't pull, a container it can't configure or create, a crash loop), kx keeps the platform's
  cause and advice instead of its own guess from the replicas.
- A failed version's class (`error_code`, or until Kubernetix sends it, the class at the start of
  its own message) picks kx's words, fix and exit code: `crash_loop`, `oom_killed`,
  `image_pull_failed`, `container_config_error` and `container_create_error` exit 8 under their
  own code; `image_not_found` exits 4; the other image refusals exit 2 (`image_refused`);
  `quota_blocked` exits 5. kx never prints the platform's own message.
- After a failed deploy kx shows the failed version's own log lines, never the still-serving
  version's, without health probes' and metrics scrapers' requests; a version that printed
  nothing says "No output from v15's container."
- After a failed deploy, Kubernetix may record putting the serving version back as a new version
  (an automatic rollback). kx names the version that serves again ("Version 14 is serving
  again, recorded as v17."; kx env and kx scale say the change was put back to v14), and
  `kx status` shows the record ("v17 (v14 put back after v16 failed)", `"live_rollback"` in
  `--json`).
- `kx env set` explains Kubernetix's refusal of a variable the app already holds
  (`invalid_env_var`: remove it first with `kx env unset`), and refuses a value over 32768 bytes
  before sending anything.
- `kx rollback` with a token shows why Kubernetix refused a version that differs in settings the
  token can't change, instead of "This token isn't allowed to do that."

### Version tags and repositories

- `kx deploy` always builds the folder as it is and tags it with the commit checked out there
  (`git rev-parse HEAD`), in CI too: the commit a CI system names only goes into the commit
  status and job summary, and when it isn't the one checked out kx says so in one line.
- `kx deploy` also gives the image the commit's version when the commit has a git tag that looks
  like one (`v1.4.0` or `1.4.0`, optionally with a pre-release such as `-rc.1`; the highest if
  there are several), without the `v`: `1.4.0`. In CI, the tag a tag push or release run was
  started for counts too, when the run is for the commit checked out. On a laptop, a folder with
  uncommitted changes gets no version tag. `--tag <tag>` uses that tag instead, for names that
  never move.
- kx never moves a version tag. It looks the tag up before building and again after pushing: a
  free tag is given to the new image (in the registry, without a second upload), one on an
  earlier build of the same commit stays there with a warning while the new build deploys, and
  one on another commit's image (or an image without the commit label) stops kx with exit 7
  (problem code `tag_taken`; `tag_check_failed` when kx can't tell, with a fix that depends on
  why), before it builds when it could look first and right after pushing otherwise. It still deploys by digest, so a tag someone else took can only stop a
  deploy, never change what it ships.
- kx labels the images it builds with the commit (`org.opencontainers.image.revision`) and, when
  it can, where the build sits in the repository (`ai.kubernetix.kx.context`,
  `ai.kubernetix.kx.dockerfile`).
- The Pushed line shows the version tag, `--json` adds `"tags"` and `"version_tag"`, and the
  GitHub job summary lists the image's tags.
- `kx deploy --repository <name>`, or `"repository"` in `.kx.json`, pushes to
  `registry.kubernetix.ai/<org>/<name>` instead of the repository named after the app. `kx link`
  keeps a `.kx.json`'s `"repository"`. A name with a part called `tags` is refused (exit 2):
  Kubernetix's image routes would read it as a tag. So is an app called `tags` deploying to the
  repository named after it, before anything is built.

### Logging in

- A login lasts one day by default: kx asks the token page for a 1-day token (`expires=1d`),
  and `kx login --expires 30d` asks for 30 days. The token can deploy, push images and read; it
  can also configure (change environment variables and replicas, whose values stay hidden)
  when the dashboard creates it with Configure. The line after logging in says which: "This
  token can deploy and configure every app in acme-corp until 13 Nov 10:00." An end less than
  two days away is shown with the local time, after logging in and in `kx whoami` and
  `kx token list`.

### Questions before a deploy that can't work

- When kx is about to create an app and the Dockerfile has no `EXPOSE` line, it asks which
  port the app listens on (8080 by default). Without a terminal it assumes 8080, as before.
- When the Dockerfile exposes a port below 1024, or that is the port typed at the question, kx
  says the app may not be able to listen there and asks before building
  (`Deploy anyway? [y/N]`). Without a terminal, or in CI, it prints the same lines and
  deploys. An app already live on that port isn't asked about it.

### Clearer messages

- A 401 that gives no reason (or only "Invalid or expired token") no longer says to log in
  again or make a new token, which could throw away a good token: kx says Kubernetix didn't
  accept the token and to try again in a few minutes. New problem code `token_not_accepted`,
  exit 3.
- `kx status` right after a deploy says "starting (counts can take a minute to appear)"
  instead of "none running" while replica counts catch up; `--json` adds `"starting": true`
  inside `"replicas"`.
- When Kubernetix reports a specific cause for a failed deploy (more than that it never got
  ready), kx shows Kubernetix's own reason instead of its guess from the replicas.
- When a deployment records no health check, kx names only the port it checks, not a path.
- When Kubernetix reports that an organization has started the most deploys it may start in a
  day, failed ones included, kx says so instead of using the daily deploy limit's wording,
  with the number Kubernetix gives, if any. Problem code `deploy_limit_reached`,
  exit 5, as before.

## 1.0.0

First release.

- Docs: the GitHub Actions example uses `actions/checkout@v7`, so runs show no Node 20
  deprecation notice, and the CI docs say how the first CI deploy gets its app: create it
  first, or let the first run create it from a committed `.kx.json`.
