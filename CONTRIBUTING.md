# Contributing to kubeweave

Thanks for helping! Bug reports, ideas, docs fixes, plugins and code are all welcome.

## Before you start

- **Security problems:** report them privately, see [SECURITY.md](SECURITY.md).
- **Bigger changes:** open an issue first so we can agree on the approach before you spend time on it.
- **Plugins:** add them to [`plugins/index.yaml`](plugins/index.yaml). Pass values to `sh -c` as positional
  arguments (`"$1"`), never inside the script, and say which tool it needs (`requires:`).

## Development

You need the Go version in `go.mod`. A kind cluster is handy for testing against a real API server.

```bash
make build                      # → bin/kubeweave
make test                       # go vet + go test
go test -race ./...             # what CI runs on Linux and macOS
kind create cluster --name kubeweave-e2e && go test -tags e2e -v ./e2e/   # real-cluster tests
```

Run the UI against a **non-production** context, ideally with `--readonly` until you test a change on purpose.
`kubeweave serve --no-browser` serves the web UI; the files in `internal/web/static` are plain JavaScript and CSS
(no build step).

## Pull requests

- Keep each pull request to one change, with a test. Most packages have tests that run against fake clients
  (`k8s.io/client-go/kubernetes/fake`); the web API, sign-in, bot and installers have their own.
- `gofmt`, `go vet ./...` and `GOOS=windows go vet ./...` must pass.
- New features should be findable: the help screen, the command palette and, for row actions, the space menu.
- Changes that touch clusters must respect read-only mode (`guardWrite`), production-like contexts and the audit log.
- Never print or log Secret values, and never add personal data, cluster names or company details to code, tests
  or docs.
- Update the README when behaviour changes.

## Releases

Maintainers tag `vX.Y.Z` after CI is green; GoReleaser builds the binaries and packages and the Homebrew formula is
updated automatically.

By contributing you agree that your contribution is licensed under the [MIT licence](LICENSE).
