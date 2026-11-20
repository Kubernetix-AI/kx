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

### Version tags and repositories

- `kx deploy` pushes the image under the commit's version too when the commit has a git tag
  that looks like one (`v1.4.0` or `1.4.0`, optionally with a pre-release such as `-rc.1`; the
  highest if there are several), without the `v`: `1.4.0`. In CI, the tag a tag push or
  release run was started for counts too. On a laptop, a folder with uncommitted changes gets
  no version tag. `--tag <tag>` pushes under that tag instead. kx looks the tag up first and never
  moves one that holds another image: it stops with exit 7 and deploys nothing, naming the
  digest the tag holds (new problem codes `tag_taken` and `tag_check_failed`). It looks again
  after pushing, so of two runs pushing the same new tag at once only the one whose image the
  tag holds deploys. It still deploys by digest.
- Deploys of the same commit are idempotent. kx labels the images it builds of a commit as it
  is (always in CI) with the commit (`org.opencontainers.image.revision`) and where the build
  sits in the repository, and before building looks at the commit's tag and the version tag:
  an image built the same way from this commit is deployed again without building
  (`✓ Reused`), and the missing tag is pointed at it (`✓ Tagged`). So a re-run, or two runs for
  one tagged push, deploy the same image. Unlabelled images are never reused. A version tag
  holding another commit's image stops kx before it builds; one holding another build of the
  same commit is left as it is. `--rebuild` builds anyway: it may move the commit's tag, never
  a version tag.
- The Pushed line shows the version tag, `--json` adds `"tags"`, `"version_tag"` and
  `"reused"`, and the GitHub job summary lists the image's tags.
- `kx deploy --repository <name>`, or `"repository"` in `.kx.json`, pushes to
  `registry.kubernetix.ai/<org>/<name>` instead of the repository named after the app. `kx link`
  keeps a `.kx.json`'s `"repository"`.

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
