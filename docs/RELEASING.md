# Releasing kx

kx is released by pushing a tag. The [release workflow](../.github/workflows/release.yml) builds
the binaries on GitHub, attests them, publishes them and checks what it published. Nothing is
built or uploaded from a laptop.

## Versions

kx follows [semantic versioning](https://semver.org). Its public interface is:

- command names, flags and arguments;
- exit codes and the two-line error format;
- the keys of every `--json` output, and the rule that a command's result (for `kx deploy`,
  the URL) is the only thing on standard output;
- environment variables (`KX_TOKEN`, `KX_ORG`, `KX_APP`, `KX_API_URL`), `.kx.json` and the
  settings file;
- the release file names below, which the install scripts and the
  [Kubernetix-AI/deploy](https://github.com/Kubernetix-AI/deploy) Action download.

| Change | Version |
|---|---|
| A fix that keeps the interface | patch: 1.0.0 → 1.0.1 |
| Something new that keeps every existing use working (a command, a flag, a `--json` key) | minor: 1.0.1 → 1.1.0 |
| Anything that can break a script or workflow that works today | major: 1.1.0 → 2.0.0 |

Wording of human-readable messages may change in any release; scripts should use `--json` and
exit codes.

### Release files

Every release has exactly these seven files. Their names never contain the version and never
change:

```text
kx_linux_amd64.tar.gz    kx_darwin_amd64.tar.gz    kx_windows_amd64.zip
kx_linux_arm64.tar.gz    kx_darwin_arm64.tar.gz    kx_windows_arm64.zip
checksums.txt            (sha256sum format)
```

Each archive holds only `kx` (`kx.exe` on Windows) at its root. A release's files are at
`https://github.com/Kubernetix-AI/kx/releases/download/v<version>/<file>`.
`scripts/check-release-assets.sh` checks a folder of release files against this list, and runs
in CI on every pull request (job "Release dry run") and twice during a release.

## Before the first release

Do these once, right after the `kx` and `deploy` repositories are made public, and before the
first tag:

1. **Require two-factor authentication** for the Kubernetix-AI organization (Organization
   settings → Authentication security).
2. **Apply the rulesets** to both repositories, as a repository admin: a dry run first, then
   `--apply` (see [Protecting tags](#protecting-tags)).

   ```sh
   scripts/apply-rulesets.sh
   scripts/apply-rulesets.sh --apply
   scripts/apply-rulesets.sh --repo Kubernetix-AI/deploy --dir ../deploy/.github/rulesets --apply
   ```

3. **Turn on immutable releases** for this repository (Settings → General → Releases →
   release immutability). Once a release is published, its files and tag can't be changed,
   so nobody with write access can swap the files behind checksums the Action has committed.
   The release workflow is already built for it: everything is uploaded to the draft and
   attested before the release is published.
4. **Turn on private vulnerability reporting** in both repositories (Settings → Code security →
   Private vulnerability reporting). `SECURITY.md` and the Action's error messages link to it.
5. **Rehearse with a release candidate:** tag `v1.0.0-rc.1` and follow the checklist below,
   including publishing the Action for it. Candidates never become "latest" and don't move the
   Action's `v1`.

## Release checklist

1. **CI is green on `main`.** The commit you release must be on `main` and its "CI" check must
   have passed. The release workflow refuses a tag that points anywhere else.
2. **Choose the version** using the table above. The version comes only from the tag; there is
   no version to edit in the code.
3. **Tag `main`** with an annotated tag (the workflow refuses lightweight tags):

   ```sh
   git switch main
   git pull --ff-only
   git log -1 --oneline        # the commit you mean to release
   git tag -a v1.2.3 -m "kx 1.2.3"
   git push origin v1.2.3
   ```

   Only repository admins can push `v*` tags (see [Protecting tags](#protecting-tags)). Push
   one release tag at a time and wait for its run: GitHub keeps only one release run waiting
   behind the running one, so a third tag pushed meanwhile cancels the waiting run. Re-run any
   release run that shows as cancelled.
4. **Watch the Release workflow** in the Actions tab. It runs three jobs:
   - *Check the tag and run the tests*: the tag is `vX.Y.Z` or `vX.Y.Z-rc.N`, annotated and on
     `main`; `go mod verify`, `go vet` and `go test -race` pass.
   - *Build, attest and publish*: goreleaser builds the six archives and `checksums.txt`
     reproducibly and uploads them to a draft release; the files are checked against the
     contract; every file gets a build provenance attestation; then the draft is published.
     The release is never public before its files are attested.
   - *Verify the published release*: downloads the files from the public URLs, checks
     `sha256sum -c`, the archive contents and `kx version`, verifies every attestation, and for
     a stable release checks that `releases/latest` now leads to it.
5. **Verify it yourself** on your own computer:

   ```sh
   mkdir /tmp/kx-1.2.3 && cd /tmp/kx-1.2.3
   gh release download v1.2.3 --repo Kubernetix-AI/kx
   sha256sum -c checksums.txt          # macOS: shasum -a 256 -c checksums.txt
   for f in kx_* checksums.txt; do
     gh attestation verify "$f" --repo Kubernetix-AI/kx \
       --signer-workflow Kubernetix-AI/kx/.github/workflows/release.yml \
       --source-ref refs/tags/v1.2.3 --deny-self-hosted-runners
   done
   curl -fsSL https://platform.kubernetix.ai/install.sh | sh && kx version
   ```

6. **Publish the Action.** In a clone of `Kubernetix-AI/deploy`, run
   `scripts/publish-action.sh 1.2.3` and read its dry run, then run it again with `--push`. It
   commits the release's checksums to the Action, makes the Action use this kx by default, tags
   the Action and moves its major tag. See that repository's `docs/RELEASING.md`.

## Release candidates

Tag `v1.0.0-rc.1`, `v1.0.0-rc.2` and so on, from `main`, exactly as above. The workflow marks
them as pre-releases, so they never become "latest": the install scripts only install one when
asked (`KX_VERSION=1.0.0-rc.1`). Publishing the Action for a candidate tags `v1.0.0-rc.1` in the
Action repository without moving its major tag. When a candidate is good, tag the same commit
`v1.0.0`.

## Hotfixes

Fix the problem on `main` with a pull request, then release the next patch version. Releases
are only made from `main`, so a fix for an older minor or major version also ships as the next
patch of the current one. If supporting an older line ever becomes necessary, change this
policy and the workflow's "on main" check together.

The workflow marks a release "latest" only when it is the highest stable version, so tagging
an older line could never take over what the install scripts download.

## Protecting tags

A `v*` tag is what customers' installers and workflows trust, so:

- **Rulesets** (as code in `.github/rulesets/`): only repository admins may create, update or
  delete `v*` tags; `main` needs a pull request with the "CI" check passing (reported by GitHub
  Actions, so no other app can fake it), and can't be force-pushed or deleted. Apply them with
  `scripts/apply-rulesets.sh` (a dry run) and then `scripts/apply-rulesets.sh --apply`, as a
  repository admin, once the repository is public. GitHub's free plan only enforces rulesets
  on public repositories. Run the script again after changing a JSON file; it updates the
  rulesets in place. The same script protects the Action's tags:
  `scripts/apply-rulesets.sh --repo Kubernetix-AI/deploy --dir ../deploy/.github/rulesets`.
- **Two-factor authentication** is required for every member of the Kubernetix-AI
  organization (Organization settings → Authentication security), so a stolen password alone
  can't push a tag. `apply-rulesets.sh` warns when it isn't on.
- **The workflow** refuses tags that aren't semantic versions, aren't annotated, or point at a
  commit that isn't on `main`, and it builds without any cache that another workflow could
  have written.
- **Immutable releases** (a repository setting) stop anyone from changing a published
  release's files or tag.
- **Attestations** let anyone check that a file was built by this repository's release
  workflow from a given tag, and the Action only installs kx versions whose checksums are
  committed in its own repository.

## Rollback

Never move, delete or reuse a published tag: people and workflows have already pinned it, and
checksums committed in the Action refer to its files.

If a release is bad, work in this order, so that nobody is left with a broken deploy:

1. **Stop the Action using it.** By the time a bad kx is noticed, the Action usually pins it
   and its `v1` points there. In the `deploy` repository, either move `v1` back to the previous
   good Action release (its `docs/RELEASING.md`, "Moving the major tag"), or publish the
   previous good kx under a new Action version:
   `scripts/publish-action.sh --push --action-version 1.2.4 1.2.2`.
   Workflows on `@v1` use it from their next run.
2. **Stop the install scripts installing it.** Mark the previous good release as "latest", and
   say in the bad release's notes that it is withdrawn and which version to use instead:

   ```sh
   gh release edit v1.2.2 --repo Kubernetix-AI/kx --latest
   gh release edit v1.2.3 --repo Kubernetix-AI/kx --notes "Withdrawn: use kx 1.2.2 or later. <why>"
   ```

   The release and its files stay, so Action releases that pin 1.2.3, and workflows pinned to
   them, keep working.
3. **Fix the problem on `main` and release the next patch version**, then publish the Action
   with it.

**Removing a release's files** is only for a build that is itself harmful (for example a
compromised build). It breaks every Action release that pins that kx: their workflows stop with
"kx 1.2.3 is no longer available". So first do step 1, and withdraw the version from the
Action (its `docs/RELEASING.md`, "Withdrawing a kx version"). Then delete the GitHub release,
keeping the tag: `gh release delete v1.2.3 --repo Kubernetix-AI/kx` (without `--cleanup-tag`).
With immutable releases on, GitHub doesn't let you change a published release's files; check
GitHub's documentation on immutable releases for how to remove one, or ask GitHub Support.

If the workflow fails before publishing, the draft release stays hidden. Fix the cause and
re-run the failed jobs; goreleaser replaces the old draft. If the *Verify the published release*
job fails, re-run it once first, because a download or the `releases/latest` redirect can lag
just after publishing. If it fails again, treat the release as bad and follow the steps above.

## Reproducing a release

The build is reproducible: the archives depend only on the commit, the Go toolchain (pinned
by the `toolchain` line in `go.mod`) and the goreleaser version (pinned in the release
workflow), not on the time or the machine. To rebuild a release and compare it with the
published `checksums.txt`:

```sh
go install github.com/goreleaser/goreleaser/v2@v2.20.1   # the version in release.yml
git checkout v1.2.3
goreleaser release --clean --skip=publish
diff dist/checksums.txt <(curl -fsSL https://github.com/Kubernetix-AI/kx/releases/download/v1.2.3/checksums.txt)
```
