# Star History Burst Analysis: Design

Date: 2026-09-13
Status: approved in design discussion, awaiting spec review

## Context

Since 2026-06-30, GitHub shows the stargazer list of a repository only to its admins and collaborators, in the REST API, the GraphQL API and the web UI. StarAudit's trust score is built from the contribution history of each stargazer, so it has no input data for any repository the token holder does not administer. Branch `fix/stargazer-restriction` makes that case fail fast with a clear message, but the tool still cannot judge third-party repositories.

On 2026-09-04 GitHub added `GET /repos/{owner}/{repo}/stargazers/history`. It returns star counts per week, with one count per day, and no stargazer identities. This design rebuilds the default mode of StarAudit around that endpoint: it detects star bursts instead of scoring users.

The main consumer is the awesome-integration PR workflow (`pr-spam-guard.yml`), which needs machine-readable output and an exit code that reflects the verdict.

## Goals

- Judge any public repository, with or without a token.
- Produce one of three verdicts: `pass`, `review`, `suspicious`.
- Offer JSON output on stdout and an exit code per verdict, for CI use.
- Explain every verdict with stable reason codes and readable messages.
- Keep the existing per-stargazer trust scan for repositories the token holder administers.

## Non-goals

- Policy checks such as minimum repository age or minimum star count for a list. These stay in `pr-spam-guard.yml`.
- A statistical anomaly score. Rejected during design because real launches look like extreme outliers and the result is hard to explain.
- Caching of star history. A full history costs one request per 30 weeks plus one (17 for a 9-year-old repository), and a cache would give CI stale results.
- Changes to the trust scan, the signed report upload or the badge.

## Command line

| Invocation | Behavior |
|---|---|
| `staraudit owner/repo` | Burst analysis (new default). |
| `staraudit --trust owner/repo` | Existing per-stargazer trust scan, unchanged. `--stars`, `--all` and `--cachedir` apply only to this mode. |
| `--json` | Print only a JSON document on stdout. All log lines go to stderr. |
| `--min-stars` (default `50`) | Below this total, the verdict is at least `review`. |
| `--burst-review` (default `0.40`) | Burst share at or above this value gives `review`. |
| `--burst-suspicious` (default `0.70`) | Burst share at or above this value can give `suspicious`. |
| `--tail-suspicious` (default `0.10`) | Tail ratio below this value, together with a suspicious burst, gives `suspicious`. |
| `--peak-review` (default `0.15`) | Peak day share at or above this value gives `review`. |

All flags can also be set through `STARAUDIT_*` environment variables, as the existing flags can (viper `AutomaticEnv`).

`GITHUB_TOKEN` is optional for the default mode. When set, it is sent as a bearer token (5000 requests per hour instead of 60). It stays required for `--trust`.

Flag validation happens before any request: every threshold must be in `[0, 1]` (so `NaN` and infinite values are rejected), `--burst-review` must not exceed `--burst-suspicious`, and `--min-stars` must not be negative. A missing repository argument, an unknown flag, and a flag or `STARAUDIT_*` value that does not parse are argument errors too. Every argument error exits with code 1 and error code `invalid_arguments`, never with a verdict code. `--help` exits with 0.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | `pass` (and success in `--trust` mode, as today) |
| 1 | Any error |
| 2 | `review` |
| 3 | `suspicious` |

The mapping lives in `main.go` only, as a small function with its own test.

## Architecture

The new package `pkg/history` is the service layer. `main.go` stays the presentation layer: it parses flags, calls the package, renders the result and chooses the exit code. The trust scan path (`pkg/gql`, `pkg/trust`, `pkg/signature`) is only reached through `--trust`.

### `pkg/history/fetch.go`

`Fetch(ctx context.Context, owner, repo, token string) (Series, error)`

- `Series` holds the date of the first day and the daily counts, oldest day first.
- Requests `GET https://api.github.com/repos/{owner}/{repo}/stargazers/history?per_page=30&page=N` with the headers `Accept: application/vnd.github+json` and `X-GitHub-Api-Version: 2026-03-10`, plus `Authorization: Bearer <token>` only when a token is set.
- Pages run newest to oldest. Paging stops at the first empty page. Weeks are then sorted by their `week` timestamp and flattened into one daily series.
- Days dated after the current time are removed, because the newest week contains days that have not happened yet.
- Turning the raw weeks into a `Series` is a separate function, `parseWeeks(weeks []week, now time.Time) (Series, error)`, so tests can pass a fixed time. `Fetch` calls it with `time.Now()`.
- The base URL is a package variable so tests can point it at a stub server, as in `pkg/gql`.
- Each week is validated at this boundary: `week` is positive, `days` has exactly 7 entries, no count is negative, and `total` equals the sum of `days`. Any violation is an `api_error`.

### `pkg/history/analyze.go`

`Analyze(series Series, t Thresholds) Result`

A pure function with no network access. `Result` holds the metrics, the verdict and the reasons. Details in "Scoring".

### `pkg/history/render.go`

Renders a `Result` as the text report (with `disgo`, like the trust report) or as JSON. Also renders an error as JSON when `--json` is set.

### Errors

`pkg/history` returns a typed error, `*Error{Code string; Message string; Err error}`, where `Code` is one of `not_found`, `unauthorized`, `rate_limited`, `api_error`. It implements `Unwrap`, so callers can use `errors.As`. `invalid_arguments` is produced in `main.go`.

## Scoring

### Constants

- Burst window: 14 days.
- Tail window: 60 days, starting the day after the burst window ends.

These are constants, not flags.

### Metrics

| Metric | Definition |
|---|---|
| `total_stars` | Sum of all daily counts. |
| `history_days` | Length of the series after removing future days. |
| `burst_share` | Largest sum over any 14 consecutive days, divided by `total_stars`. On a tie the earliest window wins. For a series shorter than 14 days, the window is the whole series. |
| `burst_start`, `burst_end` | Dates of the first and last day of that window. |
| `tail_ratio` | Sum of the up to 60 days after the burst window, divided by the burst sum. |
| `tail_days_observed` | How many of those 60 days exist in the series. |
| `peak_share` | Largest single-day count, divided by `total_stars`. |
| `peak_day` | Date of that day (earliest on a tie). |

When `total_stars` is 0, all shares and ratios are 0 and the dates are omitted.

### Rules

Every rule that matches adds a reason. The verdict is the most severe matching rule, or `pass` when none match.

| Verdict | Reason code | Condition |
|---|---|---|
| `suspicious` | `no_tail` | `burst_share >= burst_suspicious` and `tail_days_observed == 60` and `tail_ratio < tail_suspicious` |
| `review` | `burst` | `burst_share >= burst_review` |
| `review` | `short_history` | `burst_share >= burst_review` and `tail_days_observed < 60` |
| `review` | `peak_day` | `peak_share >= peak_review` |
| `review` | `few_stars` | `total_stars < min_stars` |

The `short_history` rule is how young repositories are handled: every young repository has a high burst share, so it cannot pass until 60 days after its busiest window have passed. There is no separate minimum age.

Example messages:

- `burst`: "100% of stars arrived between 2026-08-24 and 2026-09-06"
- `no_tail`: "only 2% more stars arrived in the 60 days after the burst"
- `short_history`: "only 6 of 60 days after the burst exist yet"
- `peak_day`: "18.8% of stars arrived on 2026-09-01"
- `few_stars`: "only 12 stars, fewer than the minimum of 50"

### Calibration data

Fetched on 2026-09-13. Peak is the busiest day as a share of all stars.

| Repository | Stars | Days | Burst share | Tail ratio (days observed) | Peak share | Expected verdict |
|---|---|---|---|---|---|---|
| Continuum-AI-Corp/OrcaReplay | 239 | 28 | 100% | 0% (6) | 18% | review |
| OpenAPITools/openapi-style-validator | 236 | 3318 | 2% | 71% (60) | 1% | pass |
| mcpjungle/MCPJungle | 1254 | 497 | 12% | 158% (60) | 4% | pass |
| astral-sh/uv | 89769 | 1085 | 8% | 49% (60) | 1% | pass |
| charmbracelet/vhs | 20881 | 1526 | 34% | 20% (60) | 9% | pass |
| ollama/ollama | 180783 | 1183 | 6% | 110% (60) | 0.7% | pass |
| zed-industries/zed | 90178 | 2044 | 19% | 54% (60) | 4% | pass |

OrcaReplay is the only confirmed fake example, so the defaults are an informed starting point, not validated values. A legitimate launch that reaches the Hacker News front page also produces a burst, which is why a burst alone gives `review`, not `suspicious`.

## Output

### Text (default)

```
Star history of Continuum-AI-Corp/OrcaReplay: 239 stars over 28 days

Busiest 14 days:    100%   (2026-08-24 to 2026-09-06)
After the burst:    0% of the burst, 6 of 60 days observed
Busiest day:        18.8%  (2026-09-01)

⚠ Verdict: REVIEW
  - burst: 100% of stars arrived between 2026-08-24 and 2026-09-06
  - short_history: only 6 of 60 days after the burst exist yet
  - peak_day: 18.8% of stars arrived on 2026-09-01
```

### JSON (`--json`)

```json
{
  "schema_version": 1,
  "repository": "Continuum-AI-Corp/OrcaReplay",
  "analyzed_at": "2026-09-13T12:00:00Z",
  "verdict": "review",
  "reasons": [
    {"code": "burst", "message": "100% of stars arrived between 2026-08-24 and 2026-09-06"},
    {"code": "short_history", "message": "only 6 of 60 days after the burst exist yet"},
    {"code": "peak_day", "message": "18.8% of stars arrived on 2026-09-01"}
  ],
  "metrics": {
    "total_stars": 239,
    "history_days": 28,
    "burst_share": 1.0,
    "burst_start": "2026-08-24",
    "burst_end": "2026-09-06",
    "tail_ratio": 0.0,
    "tail_days_observed": 6,
    "peak_share": 0.188,
    "peak_day": "2026-09-01"
  },
  "thresholds": {
    "min_stars": 50,
    "burst_review": 0.4,
    "burst_suspicious": 0.7,
    "tail_suspicious": 0.1,
    "peak_review": 0.15
  }
}
```

Reasons are listed in the order of the rules table. Shares and ratios are rounded to 3 decimal places; the verdict is computed before rounding. Dates use `YYYY-MM-DD` and are taken from the series as GitHub returns it; GitHub notes that its week and day boundaries are not guaranteed to align with UTC.

On error with `--json`, stdout holds `{"schema_version": 1, "repository": "owner/repo", "error": {"code": "not_found", "message": "..."}}` and the exit code is 1.

`schema_version` is increased only for incompatible changes. Adding a field is compatible.

## Error handling

| Case | Behavior | Code |
|---|---|---|
| HTTP 404 | "repository not found, or the token cannot see it". No retry. | `not_found` |
| HTTP 401 | "GitHub rejected the credentials, check GITHUB_TOKEN". No retry. | `unauthorized` |
| HTTP 403 or 429 with `X-RateLimit-Remaining: 0` | Stop at once and report the reset time from `X-RateLimit-Reset`. Without a token, suggest setting `GITHUB_TOKEN`. No waiting. | `rate_limited` |
| HTTP 403 or 429 with `Retry-After` of at most 60 seconds | Wait the given number of seconds, then retry. Counts toward the attempt limit. | retried |
| HTTP 403 or 429 with `Retry-After` above 60 seconds | Stop at once. A CI job should not wait that long. | `rate_limited` |
| HTTP 429 without either header, or 403 whose message mentions a rate limit | Stop at once (secondary rate limit). | `rate_limited` |
| HTTP 5xx or network error | Exponential backoff (`cenkalti/backoff/v5`), at most 5 attempts. | `api_error` after the last attempt |
| HTTP 422 or any other status | No retry. | `api_error` |
| Week fails validation, or the weeks are not exactly 7 days apart once duplicates are removed | No retry. Each page is checked as soon as it arrives. A week that comes back twice (the pages shift when a new week starts during paging) is kept once. | `api_error` |
| Context cancelled (Ctrl-C, SIGTERM) | Stop at once, exit 1. | none |

A repository with 0 stars is not an error: it gets `review` with `few_stars`.

## Testing

- `pkg/history/analyze_test.go`: table tests on exact series. Cases: fake burst with a full, empty tail (`suspicious`); young repository like OrcaReplay (`review` with `burst`, `short_history`, `peak_day`); launch with a healthy tail (`pass`); steady growth (`pass`); fewer than 50 stars and 0 stars (`review`, `few_stars`); values exactly on each threshold; tie between two windows (earliest wins); series shorter than 14 days.
- Calibration fixtures: the seven histories above, saved as the raw API pages under `pkg/history/testdata/`. A test runs `Analyze` with a fixed "now" and asserts `review` for OrcaReplay and `pass` for the others.
- `pkg/history/fetch_test.go`: stub server with `httptest`. Cases: paging until an empty page; version header sent; no `Authorization` header without a token; future days removed; 404, 401, primary rate limit, `Retry-After`, 5xx retry; malformed week rejected; cancelled context.
- `pkg/history/render_test.go`: JSON contains every documented field; error JSON; text output contains the verdict and each reason.
- `main.go`: verdict to exit code mapping. Existing tests for `--trust` stay unchanged.
- Manual smoke test, not in CI: OrcaReplay and one legitimate repository.

`make test` and `make lint` must pass.

## Documentation

- `README.md`: new default mode, `--trust`, all flags, exit codes and the JSON format. Also correct the statement that a token with `repo` rights is enough for the trust scan.
- `CLAUDE.md`: add `pkg/history/` to the project structure.

## Release

This change ships as StarAudit 2.0.0, because the default mode and the exit codes change. The Go module path stays `github.com/stn1slv/staraudit` (decided on 2026-09-13), so the release is the `v2.0.0` tag with its GitHub release binaries, and `go install github.com/stn1slv/staraudit@latest` keeps resolving to 1.x. The README gets an "Upgrading from 1.x" section that says so.

## Decisions added during planning

- `--json` together with `--trust` is rejected with `invalid_arguments`, because the trust scan has no JSON output.
- A cancelled run (Ctrl-C, SIGTERM) writes no JSON document, even with `--json`.
