# Contributing to kx

Thanks for helping. kx is licensed under the Apache License 2.0 (see LICENSE). By sending a pull
request you agree that your contribution is licensed under the same terms. For a large change,
please open an issue first so we can discuss it.

## Build and test

You need Go 1.27 or newer. Docker is only needed to try `kx deploy` for real.

```sh
make build     # ./kx for this computer
make test      # go test -race ./...
make lint      # gofmt, go vet, golangci-lint (v2)
make vuln      # govulncheck
make cross     # all six release targets into dist/, with CGO_ENABLED=0
make fuzz      # each fuzz test for 30 s (FUZZTIME=2m make fuzz for longer)
```

Tests never touch the network or your real settings: the API is an `httptest` server, kx's
settings live in a temporary `KX_CONFIG_DIR`, and external programs are fakes on `PATH`.

Command output is checked against golden files in `cmd/testdata/`. After an intended change
to what a command prints, regenerate them and review the diff:

```sh
go test ./cmd -update
git diff cmd/testdata
```

## Layout

| Path | What it holds |
|---|---|
| `main.go` | Calls `cmd.Main`. |
| `cmd/` | One file per command. Each registers itself with `register(newXCmd)` in `init`. |
| `internal/api` | The API client, the local token check, and `Explain`, which turns any error into two lines and an exit code. |
| `internal/config` | Works out the token, organization, app and API address; reads and writes `.kx.json` and the settings file. |
| `internal/ui` | Output: progress on stderr, results on stdout, spinner, prompts, `--json`, token redaction. |
| `internal/ci` | Recognises GitHub Actions, GitLab CI, Bitbucket Pipelines and generic CI. |
| `internal/kxerr` | Errors kx raises itself. |
| `internal/version` | The build stamp set with `-ldflags`. |
| `internal/browser`, `internal/uuid` | Small helpers. |

## Conventions

- **Two-line errors.** Every failure ends up in `api.Explain` as one plain sentence (no HTTP
  codes, no stack traces), one fix and an exit code. A new error needs a case in
  `TestExplain_EveryCodeOneSentenceOneFix`.
- **Standard output is for results.** Progress, prompts and errors go to standard error, so
  `URL=$(kx deploy)` keeps working. Every command supports `--json` with stable keys.
- **Zero flags on the happy path.** Prompts have defaults; `--help` starts with examples.
- **Never print a token** beyond its `kx_` prefix, including in errors and test failures.
- **Tests first.** Write the failing test, watch it fail, then make it pass. Table-driven
  tests, `-race`, and a fuzz test for anything that parses untrusted input.
- **Few dependencies.** kx uses the standard library, `spf13/cobra` and `golang.org/x/term`.
  Ask in an issue before adding another.
- **Commits** use conventional subjects, for example `feat(kx): add kx rollback` or
  `fix(kx): keep the login when logout cannot reach the API`.

## Adding a command

Create `cmd/<name>.go`:

```go
func init() { register(newNameCmd) }

func newNameCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "name",
		Short:   "One line for --help",
		GroupID: groupApps,
		Example: "  kx name\n  kx name --json",
		Args:    usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.session(cmd, "")
			if err != nil {
				return err
			}
			// s.Client(), s.ResolveApp(ctx), s.P (the printer) ...
			return nil
		},
	}
}
```

Return errors rather than printing them; `App.Run` prints them through `api.Explain`.
