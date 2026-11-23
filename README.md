# kx

kx is the Kubernetix command line. Run `kx deploy` in any folder with a Dockerfile and the
last thing you see is your app's live URL.

```console
$ kx login
$ kx deploy
...
https://<your app's address>
```

- One command each to install, log in and deploy. No flags on the happy path.
- Works the same on your laptop and in CI (GitHub Actions, GitLab CI, Bitbucket Pipelines or
  any other).
- When something goes wrong you get two lines: what happened, then the one thing to do.

## Install

**macOS and Linux**

```sh
curl -fsSL https://platform.kubernetix.ai/install.sh | sh
```

No `curl`? Use `wget -qO- https://platform.kubernetix.ai/install.sh | sh`.

**Windows** (PowerShell)

```powershell
irm https://platform.kubernetix.ai/install.ps1 | iex
```

The installer downloads the release for your computer, checks its SHA-256 checksum and
installs `kx` to `/usr/local/bin` or `~/.local/bin` (Windows: `%LOCALAPPDATA%\Programs\kx`).

Installing can take a second step. On most Macs and Linux machines `/usr/local/bin` needs
administrator rights, so kx goes to `~/.local/bin`, which often isn't on your `PATH` yet.
The installer then says so and prints one line to run: it adds the folder for this terminal
and to your shell's startup file for new ones. Until you run it, start kx by its full path
(`~/.local/bin/kx login`), as the installer's last line shows. On Windows the installer adds
its folder to your `PATH` itself; other terminals see it once you open a new one.

To install a particular version, set `KX_VERSION`:

```sh
curl -fsSL https://platform.kubernetix.ai/install.sh | KX_VERSION=1.0.0 sh
```

```powershell
$env:KX_VERSION = "1.0.0"; irm https://platform.kubernetix.ai/install.ps1 | iex
```

**By hand.** Every [release](https://github.com/Kubernetix-AI/kx/releases) has
`kx_<os>_<arch>.tar.gz` (or `.zip` on Windows) for macOS, Linux and Windows on amd64 and
arm64, plus `checksums.txt`. Download the archive and `checksums.txt`, then check it:

```sh
sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c checksums.txt
```

**From source** (Go 1.27 or newer): `go install github.com/kubernetix-ai/kx@latest`.

## Quickstart

If the installer printed a line to add kx to your `PATH`, run that first (see Install).

```console
$ curl -fsSL https://platform.kubernetix.ai/install.sh | sh
kx is ready. Next: kx login

$ kx login
Opening platform.kubernetix.ai so you can create a token for this computer.
If your browser didn't open, go to https://platform.kubernetix.ai/settings?tab=tokens&new=cli
Click "Create token", then "Copy", and paste it here.
Token: ••••••••••••••••
Logged in to acme-corp as sam@acme.dev. This token can deploy every app in acme-corp until 11 Dec 2026.
Next: open your project folder and run kx deploy

$ cd ~/code/shop-api
$ kx deploy
Which app should this folder deploy to?
> (new app: shop-api)
  billing-worker
Saved in .kx.json. Next time, just run kx deploy.

✓ Checked   Dockerfile, port 8080
✓ Built     shop-api:3f9c2a1b7d4e                        1m 12s
✓ Pushed    registry.kubernetix.ai/acme-corp/shop-api    14s
✓ Created   app shop-api
✓ Live      v1  queued → starting → live                 48s
  4 of 5 deploys left today on the Free plan.
https://<your app's address>
```

The first choice is always a new app named after the folder (in lowercase, with other
characters turned into hyphens), so pressing Enter never deploys over an app you already
have; to deploy to an existing app, move to it with the arrow keys or type its number. The
next `kx deploy` in that folder asks nothing. Run `kx` on its own at any time to see these
steps again, and `kx <command> --help` for examples of any command.

**Ports and health checks.** A new app gets the port in your Dockerfile's first `EXPOSE` line
(8080 if there is none). Kubernetix sends traffic to that port, and a version is live once
your app answers HTTP requests on `/` there (any status below 400). If a new version's
container keeps crashing or doesn't answer, kx says so after about two minutes (longer if the
app's health checks allow more time), with the container's last log lines, and keeps
waiting, since a container can still get ready on a later start. The deploy fails (exit 8)
only if Kubernetix gives up on the version, and kx stops at `--timeout` (exit 9); either way
it names the likely cause. The version before it keeps serving.

## Commands

Every command takes `--json` (machine-readable output with stable keys) and `--org <org>`
(which organization to use).

### Getting started

| Command | What it does |
|---|---|
| `kx` | Prints the three-step quickstart. |
| `kx login` | Opens the dashboard so you can create a token for this computer, then asks you to paste it. |
| `kx deploy` | Builds the folder's Dockerfile, pushes the image, deploys it, waits until it's live and prints the URL. |
| `kx link [app]` | Saves which app this folder deploys to in `.kx.json`. Also called `kx init`. |

```sh
kx login                         # log in (the token lasts 30 days)
kx login --expires 1d            # a token that lasts one day
kx login --org acme-labs         # log in to a second organization too
kx login                         # again: replaces this computer's token and revokes the old one
echo "$TOKEN" | kx login --token-stdin

kx deploy                        # build, push, deploy, wait, print the URL
kx deploy --app web              # deploy this folder to another app
kx deploy --image registry.kubernetix.ai/acme-corp/shop-api:3f9c2a1b7d4e   # skip the build
kx deploy --build ./api --dockerfile ./api/Dockerfile.prod
kx deploy --no-wait              # start the deploy and return
kx deploy --timeout 20m          # wait longer than the default 18 minutes

kx link                          # pick an app, or a new one named after the folder
kx link shop-api                 # a new app if there's none by that name
```

`URL=$(kx deploy)` works: progress goes to standard error and the URL alone to standard
output.

### Your apps

| Command | What it does |
|---|---|
| `kx status` | Live version, URL, replicas and deploys left today. |
| `kx logs` | The last 100 lines your app printed in the past hour. |
| `kx open` | Opens the live URL in your browser (and prints it). |
| `kx rollback` | Redeploys the newest earlier version that went live with a different image. |
| `kx apps list`, `kx apps get <app>` | Lists your apps, or shows one. |

```sh
kx status
kx status --app web --json

kx logs
kx logs --follow                 # keep watching; Ctrl-C to stop
kx logs --since 2d               # 30m, 2h, 2d ...
kx logs --deployment v15         # why v15 didn't go live

kx open
kx open --dashboard              # the app's page in the dashboard

kx rollback                      # asks before rolling back
kx rollback --to v13 --yes

kx apps list
kx apps get shop-api
```

### Your account

| Command | What it does |
|---|---|
| `kx whoami` | Lists each organization this computer is logged in to and when each login ends. |
| `kx token list` | Lists tokens: what each can do, when it expires, when it was last used. |
| `kx token revoke <token>` | Revokes a token (by prefix, name or ID). |
| `kx logout` | Revokes this computer's token on Kubernetix, then forgets it. If tokens were made with it, asks before revoking them too. |
| `kx version` | Prints the version, commit, build date and platform. |

```sh
kx whoami

kx token list --app shop-api
kx token revoke kx_AbCdEfGh

kx logout
kx logout --org acme-labs
kx logout --yes                  # also revoke tokens made with this login, without asking

kx version
```

Tokens are created in the dashboard, by people signed in: a deploy token for CI on the app's
**Deploy** tab, any other token under Settings → API tokens. A token can't create tokens, so
kx doesn't either (`kx token create` points to the dashboard); `kx token` lists and revokes
them.

## Configuration

kx works out which organization, token, app and API to use from, highest first:

1. flags: `--org`, `--app`
2. environment: `KX_TOKEN`, `KX_ORG`, `KX_APP`, `KX_API_URL`
3. the folder's `.kx.json`
4. kx's settings file
5. defaults (`https://platform.kubernetix.ai/api/v1`)

The organization follows the same order (`--org`, `KX_ORG`, `.kx.json`, then the organization
you logged in to most recently), and kx uses the token saved for that organization, so a
linked folder never asks. `KX_TOKEN`, when set, is used instead of any saved login.

**`.kx.json`** holds `{"app", "app_id", "org"}` and no secret, so commit it: teammates and CI
then need no `--app`. kx looks for it in the current folder and its parents, up to the root
of the git repository. It cannot change where kx sends your token.

**The settings file** holds one token per organization with its expiry, and which
organization to use by default. kx writes it with permissions `0600` (on Windows, the file
inherits the permissions of your user profile folder):

| OS | Path |
|---|---|
| Linux | `~/.config/kx/config.json` (or `$XDG_CONFIG_HOME/kx/config.json`) |
| macOS | `~/Library/Application Support/kx/config.json` |
| Windows | `%AppData%\kx\config.json` |

Set `KX_CONFIG_DIR` to keep it somewhere else. In CI kx never reads or writes it; use
`KX_TOKEN`.

**Output.** Progress and prompts go to standard error, results to standard output. On a
terminal kx shows a spinner and colour; set `NO_COLOR=1` to turn colour off. Elsewhere,
including terminals with `TERM=dumb` and Windows consoles that can't show colour, it prints
plain progress lines, at least one every 10 seconds, and numbers the choices when it asks. With `--json`, a failure is printed on
standard output as:

```json
{"error":{"code":"not_logged_in","message":"You're not logged in to Kubernetix.","fix":"Run: kx login"}}
```

## When something goes wrong

kx prints what happened, then what to do, for example:

```text
Your login to acme-corp has ended.
Run: kx login --org acme-corp
```

The messages you are most likely to meet:

| What happened | What to do | Exit |
|---|---|---|
| You're not logged in to Kubernetix. | Run: kx login | 3 |
| Your login to acme-corp has ended. | Run: kx login --org acme-corp | 3 |
| The KX_TOKEN secret holds a token that expired on 9 Feb 2027. | Create a new deploy token on the app's Deploy tab and replace the KX_TOKEN secret. | 3 |
| This token stopped working because the person who created it is no longer a member of acme-corp. | Ask an admin of acme-corp for a new token: Settings → API tokens | 3 |
| This token can only deploy shop-api, and you asked to deploy web. | Log in with a token that can deploy web: kx login | 3 |
| This folder deploys to acme-labs, but this computer is only logged in to acme-corp. | Run: kx login --org acme-labs | 3 |
| This token can't upload images. | Create a token with Push images on the app's Deploy tab, or deploy an image that's already pushed: kx deploy --image &lt;image&gt; | 3 |
| The token in the KX_TOKEN secret doesn't have the Read permission, which kx deploy needs. | Create a deploy token on the app's Deploy tab (it has Deploy, Read and Push images) and replace the KX_TOKEN secret. | 3 |
| kx builds your app with Docker, and Docker isn't installed on this computer. | Install Docker Desktop from docker.com and run kx deploy again. | 10 |
| Docker is installed but not running. | Start Docker Desktop, then run kx deploy again. | 10 |
| There's no Dockerfile in /Users/sam/code/shop-api. | Run kx deploy in the folder that has your Dockerfile. | 2 |
| acme-corp has used all 5 deploys the Free plan includes today; more are available at 00:00 UTC (in 3 h 12 min). | Upgrade for more deploys a day: platform.kubernetix.ai/plans | 5 |
| This token has uploaded images too often in the last hour. | Wait 6 minutes, then run kx deploy again. | 5 |
| Payment for this organization is overdue. New deployments and new resources are paused until the balance is paid. Your running apps are not affected. | Pay the overdue balance on acme-corp's Billing page in the dashboard: platform.kubernetix.ai/settings?tab=billing | 5 |
| Your subscription has ended. Your apps keep running, but new deployments are paused until you subscribe again or change your usage to fit the Free plan. | Subscribe again on acme-corp's Billing page in the dashboard: platform.kubernetix.ai/settings?tab=billing | 5 |
| shop-api v15 didn't go live: your container is crashing on start. Version 14 is still serving. | See why: kx logs --deployment v15 | 8 |
| shop-api v15 didn't go live: its container kept crashing or didn't answer health checks on / at port 8080. Version 14 is still serving. | Make your app start and answer on / at port 8080 (the log lines above may say why it didn't), then run kx deploy again. | 8 |
| shop-api v15 didn't go live: Your plan's memory is full. The new version was not started; the previous one still runs. Pick a smaller size, remove an app or database, or upgrade at platform.kubernetix.ai/plans. | Free up room in your plan or upgrade at platform.kubernetix.ai/plans, then try again. | 5 (8 for now, see below) |

The last row needs Kubernetix to report that the plan had no room for the version, which it
doesn't do yet. Until it does, kx shows such a version like any other failed deploy and exits 8:
the platform's message (followed by "Version 14 is still serving." when an earlier version
serves), then "See why: kx logs --deployment v15". Once it does, kx exits 5 with the lines shown
above.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success (including a pull or merge request run that kx skips on purpose) |
| 1 | Something unexpected (network, server) |
| 2 | Usage: a flag, argument, `.kx.json` or missing Dockerfile |
| 3 | Login, token or permission |
| 4 | Not found |
| 5 | A plan limit or rate limit, or new deploys paused for billing (payment overdue or subscription ended); also a version your plan has no room for, once Kubernetix reports it as such (until then, 8) |
| 6 | The Docker build failed |
| 7 | The image upload failed |
| 8 | The deploy failed |
| 9 | kx stopped waiting; the deploy continues |
| 10 | Docker isn't installed or isn't running |

## Use in CI

Create the app first: run `kx deploy` once from your computer (it offers to create the app),
or create it in the Kubernetix dashboard. In CI, kx deploys to an app that already exists.

Create a deploy token on the app's Deploy tab in the dashboard, save it as a secret named
`KX_TOKEN`, and run `kx deploy`. On GitHub, the `Kubernetix-AI/deploy@v1` Action does this
for you. kx recognises GitHub Actions, GitLab CI and Bitbucket Pipelines, retries safely
without deploying twice, and skips pull and merge request runs (exit 0), so unmerged code
never reaches production. Every other run (a push, a release, a schedule, a manual run)
deploys the commit it checked out.

See [docs/ci.md](docs/ci.md) for copy-ready GitHub Actions, GitLab CI and Bitbucket
Pipelines setups.

## Security

- Tokens start with `kx_` and carry a checksum, so kx rejects a mistyped token before
  sending it anywhere. kx refuses the old `ct_` keys.
- kx only ever prints a token's first 11 characters (`kx_` and 8 more).
- The settings file is written with permissions `0600` (on Windows it inherits your user
  profile folder's permissions). `.kx.json` holds no secret.
- kx talks to the API over HTTPS only and never follows redirects, so your token is only
  sent to the API address you configured.
- In CI, keep `KX_TOKEN` in a secret that only your deploy branch can read (on GitHub, an
  environment limited to that branch). Rotate it by creating a new token and running
  `kx token revoke` on the old one; revocation takes effect at once.
- kx sends no telemetry. Each request carries a User-Agent with kx's version, your OS and
  architecture, and the name of the CI system, if any.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). Copyright 2026 Nandan, Inc.

The kx binaries include third-party Go modules; their licenses are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
