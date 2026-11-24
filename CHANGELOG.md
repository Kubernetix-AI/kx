# Changelog

## Unreleased

- Docs: with a committed `.kx.json` that has no app ID, deleting the app means the next CI run
  creates it again; the CI docs say how to stop that (remove `.kx.json` or the workflow, or
  commit the `.kx.json` that `kx link` writes once the app exists).

## 1.0.0

First release.

- Docs: the GitHub Actions example uses `actions/checkout@v7`, so runs show no Node 20
  deprecation notice, and the CI docs say how the first CI deploy gets its app: create it
  first, or let the first run create it from a committed `.kx.json`.
