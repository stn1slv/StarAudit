# Development Guidelines

Specific instructions and best practices for the StarAudit project.

## Core Principles

- **Security First**: This tool deals with signing reports and handling GitHub tokens. Never log, print, or commit secrets. Use environment variables for sensitive data.
- **Concurrency with Care**: We use `errgroup` with concurrency limits (currently 10) to fetch data. Always propagate contexts and handle rate limiting carefully to avoid hitting secondary GitHub API limits.
- **Caching is Key**: Large scans consume many points of the GitHub GraphQL quota. Always check the local cache before making a network request.

## Project Structure

- `main.go`: Entry point. Parses CLI arguments and runs the star history analysis, or the trust scan with `--trust`.
- `pkg/history/`: Star history analysis (the default mode): fetches `/stargazers/history` over REST, scores star bursts, and renders text and JSON.
- `pkg/gql/`: Core logic for interacting with GitHub GraphQL API, including pagination and caching.
- `pkg/trust/`: The trust algorithm logic, factors, and report rendering.
- `pkg/signature/`: Logic for signing reports and verifying signatures.
- `pkg/context/`: Shared context structure used across the application.

## Language & Framework Guidelines

### Go (Golang)

- **Version**: Go 1.25+ (pinned in `go.mod`).
- **Concurrency**: Use `golang.org/x/sync/errgroup` for parallel processing.
- **API**: GitHub REST star history endpoint via `pkg/history`; GitHub GraphQL API via `pkg/gql` (trust scan only).
- **UI**: `github.com/Ullaakut/disgo` for CLI output and `github.com/vbauerster/mpb/v4` for progress bars.

## Automation & Tooling

We use a `Makefile` as the single entry point for development tasks.

| Target | Purpose |
| :--- | :--- |
| `setup` | Download dependencies and tidy `go.mod`. |
| `test` | Run all unit tests. |
| `lint` | Run `golangci-lint` (if installed). |
| `format` | Run `go fmt` on the entire project. |
| `build` | Compile the `staraudit` binary. |
| `docker` | Build the Docker image. |
| `run` | Build and run the application locally (requires `REPO` env). |
| `upgrade-deps` | Upgrade all dependencies to their latest versions. |

## Testing

- Place tests in the same package as the code they test (`*_test.go`).
- Use `github.com/stretchr/testify` for assertions.
- Run `make test` before submitting any PR.

## Security

- **GitHub Token**: `GITHUB_TOKEN` is optional for the star history analysis and required for `--trust`, where it must belong to an admin or collaborator of the repository (GitHub restricts stargazer lists since 2026-06-30).
- **Private Key**: Report signing and upload are opt-in. Set `STARAUDIT_PRIVATE_KEY` to a PEM encoded PKCS#1 RSA key to enable them; when it is unset, the report is still computed and rendered locally, it is simply not uploaded. `STARAUDIT_PUBLIC_KEY` is the matching key used by `signature.Check`. No key is embedded in the binary.
