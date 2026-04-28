# Repository Guidelines

## Project Structure & Module Organization

`cmd/server/main.go` is the single executable entry point. Core packages live under `internal/`: `config` handles flags and environment variables, `proxy` implements OpenAI-compatible request and response behavior, `transport` owns upstream HTTP access, and `tokenauth` plus `whitelist` handle access control. Package tests stay beside the package as `*_test.go`. Broader integration probes and scenario scripts live in `tests/` and `scripts/`. Design notes, audits, and compatibility reports belong in `docs/`. Build outputs go to `bin/` and should not be treated as source.

## Build, Test, and Development Commands

Use the `Makefile` targets as the default workflow:

- `make build`: compile `./cmd/server` into `bin/firew2oai`.
- `make run`: build, then start the local server.
- `make test`: run `go test -v -race ./...`.
- `make lint`: run `golangci-lint run ./...`.
- `make build-all`: cross-compile Linux, macOS, and Windows binaries.
- `make docker-up` / `make docker-down`: start or stop the Docker Compose service.

For a quick local check after building, run `./bin/firew2oai --help`.

## Coding Style & Naming Conventions

Follow standard Go formatting with `gofmt` and organized imports. Package names should be short, lowercase, and responsibility-oriented, such as `proxy` or `transport`. Exported identifiers use `CamelCase`; unexported identifiers use `camelCase`. Keep handlers and middleware focused on one concern, pass configuration explicitly, and preserve the existing `log/slog` structured logging style. Avoid hidden fallbacks or silent degradation in protocol paths.

## Testing Guidelines

Use Go's standard testing package. Name tests `TestXxx` and benchmarks `BenchmarkXxx`, with files named `*_test.go`. Cover both success and failure paths for authentication, proxy translation, upstream retries, tool protocol handling, and IP filtering. Before submitting behavior changes, run at least `make test`; for interface or protocol changes, also run `make lint` and add focused regression coverage.

## Commit & Pull Request Guidelines

Git history uses concise conventional-style subjects, for example `feat: harden codex response execution handling` and `test: improve realchain matrix scoring`. Keep each commit scoped to one clear change. Pull requests should describe user-visible impact, list validation commands and results, call out configuration or API behavior changes, and include request or response examples when protocol output changes.

## Security & Configuration Tips

Do not commit real API keys, token JSON files, or private local configuration. Prefer environment variables such as `API_KEY`, `PORT`, `CORS_ORIGINS`, and `IP_WHITELIST`. If testing requires relaxed CORS or whitelist settings, document that explicitly so reviewers can verify it is not shipped unintentionally.
