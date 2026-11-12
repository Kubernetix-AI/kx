# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the **Security** tab of this
repository and choose **Report a vulnerability**. Don't open a public issue, pull request or
discussion for a security problem.

Tell us what you found, how to reproduce it, and what an attacker could do with it. We will
acknowledge your report, keep you updated while we fix it, and credit you when we publish the
fix unless you'd rather stay anonymous.

Never include a real token in a report. If you have shared one by mistake, revoke it with
`kx token revoke <prefix>` or in the dashboard under Settings → API tokens.

## Scope

In scope:

- the `kx` command line in this repository, including how it stores, sends and prints tokens
  and registry credentials;
- the release archives and `checksums.txt` published on this repository's releases page;
- the install scripts at `https://platform.kubernetix.ai/install.sh` and `install.ps1`;
- the `Kubernetix-AI/deploy` GitHub Action.

Problems with the Kubernetix platform itself (the dashboard or the API at
`https://platform.kubernetix.ai`) can be reported the same way.

Out of scope: denial of service through volume, findings that need a compromised computer or
CI runner, and reports produced only by automated scanners without a demonstrated impact.

## How kx handles secrets

- **API tokens** look like `kx_` + 46 characters. kx checks the format and checksum locally
  and refuses legacy `ct_` keys, so a mistyped or wrong secret is never sent.
- **Printing.** kx shows at most a token's first 11 characters (`kx_` + 8). Every line kx
  prints passes through a filter that shortens anything shaped like a whole token. The one
  exception is `kx token create`, which prints the new token once, alone on standard output.
- **Storage.** `kx login` saves one token per organization in kx's settings file
  (`os.UserConfigDir()/kx/config.json`), written atomically with permissions `0600`. On
  Windows those permissions don't apply; the file inherits the permissions of your user
  profile folder (normally you, SYSTEM and Administrators). In CI that file is never read
  or written; kx uses the `KX_TOKEN` environment variable instead.
- **`.kx.json`** contains only the app name, app ID and organization. It is meant to be
  committed and cannot change the API address kx sends tokens to.
- **Transport.** kx only talks to the API over HTTPS (plain HTTP is accepted for
  `localhost` alone, for development) and never follows redirects, so the token cannot be
  forwarded to another host.
- **Registry credentials** for pushing images are short-lived (one hour). kx hands them to
  Docker through a temporary configuration directory that is removed afterwards, never on
  a command line, and masks them in GitHub Actions logs before anything could print them.
  On a laptop kx keeps the current one in its cache folder (`os.UserCacheDir()/kx`, mode
  `0600`) until it expires, so back-to-back deploys don't need a new one; in CI it is
  never written to disk outside that temporary directory.
- **Logout** revokes the token on Kubernetix before deleting it locally. If kx cannot reach
  Kubernetix, it keeps the login so you can try again. Revoking a token also revokes the
  tokens made with it, so if there are any, kx lists them and asks first; without a
  terminal it needs `--yes`, and otherwise deletes the token locally only and says how to
  revoke it. If Kubernetix refuses the token because of the account that created it, kx
  can't revoke it and says to do so in the dashboard. Logging in again to the same
  organization revokes the token it replaces, unless tokens made with it depend on it.
- **Expiry.** Tokens made by `kx login` last 30 days (or 1 day with `--expires 1d`). Tokens
  made with `kx token create` default to 90 days and never outlive the login that makes
  them; dashboard tokens default to 90 days. Revoking a token revokes the tokens made with
  it, and takes effect immediately.

## Supported versions

Security fixes go into the latest release. Update with the install command in the README.
