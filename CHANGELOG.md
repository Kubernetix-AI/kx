# Changelog

## 1.1.0 (unreleased)

### Fewer steps to a first deploy

- `kx deploy` logs in by itself when this computer has no login, or its login has ended, or
  Kubernetix says the saved token expired, was revoked or is unknown: it opens the token page,
  takes the pasted token and carries on. So do `kx status`, `kx logs`, `kx open`,
  `kx rollback`, `kx link` and `kx apps`. Only when standard input and standard error are a
  terminal, at most once per run, and never in CI, with `--json` or when `KX_TOKEN` is set.
  `kx login`, `kx logout`, `kx whoami`, `kx token` and `kx version` never log in by
  themselves.
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

### Questions before a deploy that can't work

- When kx is about to create an app and the Dockerfile has no `EXPOSE` line, it asks which
  port the app listens on (8080 by default). Without a terminal it assumes 8080, as before.
- When the Dockerfile exposes a port below 1024, kx says the app may not be able to listen
  there and asks before building (`Deploy anyway? [y/N]`). Without a terminal, or in CI, it
  prints the same lines and deploys. An app already live on that port isn't asked about it.

### Clearer messages

- A 401 that gives no reason (or only "Invalid or expired token") no longer says to log in
  again or make a new token, which could throw away a good token: kx says Kubernetix didn't
  accept the token and to try again in a few minutes. New problem code `token_not_accepted`,
  exit 3.
- `kx status` right after a deploy says "starting (counts can take a minute to appear)"
  instead of "none running" while replica counts catch up; `--json` adds `"starting": true`
  inside `"replicas"`.
- A failed deploy shows Kubernetix's own reason when Kubernetix names the kind of failure,
  instead of kx's guess from the replicas.
- When a deployment records no health check, kx names only the port it checks, not a path.
- When Kubernetix reports that an organization has started the most deploys it may start in a
  day, failed ones included, kx says so instead of using the daily deploy limit's wording,
  with the number and plan Kubernetix gives, if any. Problem code `deploy_limit_reached`,
  exit 5, as before.

## 1.0.0

First release.
