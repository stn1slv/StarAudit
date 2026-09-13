# StarAudit

<p align="center">
    <img width="300" src="img/logo.png"/>
</p>

> [!NOTE]
> This project is a continuation of [Ullaakut/astronomer](https://github.com/Ullaakut/astronomer), which was archived by the owner on Oct 12, 2020. This fork aims to maintain and modernize the tool for continued use.

StarAudit detects illegitimate GitHub stars, which are often used to artificially inflate the perceived popularity of open-source projects. By default, it analyzes the star history of a repository and looks for star bursts. For repositories you administer, it can also scan each stargazer and compute the likelihood that they are real humans.

<p align="center">
    <img width="75%" src="img/astronomer.gif">
</p>

## Key Features

*   **Star History Analysis (default)**: Detects star bursts from the GitHub star history endpoint. Works on any public repository, needs 1 to 13 API requests, and does not require a token.
*   **CI Friendly**: JSON output and an exit code for each verdict, so workflows can act on the result without parsing text.
*   **Weighted Trust Algorithm** (`--trust`): Computes trust based on contribution age, private activity, and diversity of interactions (commits, issues, PRs, reviews).
*   **Comparative Reporting** (`--trust`): Compares the "early adopters" of a repository against random samples to detect inorganic growth patterns.
*   **Concurrent Analysis** (`--trust`): Uses `errgroup` to fetch contribution data across multiple years and users simultaneously.
*   **Local Caching** (`--trust`): Caches GitHub GraphQL responses to minimize API usage and respect rate limits.
*   **Signed Reports** (`--trust`): Generates RSA-signed reports to ensure data integrity when transmitted to Astrolab.

## Star history analysis

Since 2026-06-30, GitHub shows the list of stargazers only to the admins and collaborators of a repository. The star history endpoint, added on 2026-09-04, still shows how many stars arrived each day, without the identities. StarAudit measures three things from it:

*   **Burst share**: the share of all stars that arrived in the busiest 14 days.
*   **Tail**: the stars that arrived in the 60 days after that window, compared with the burst. Real launches keep attracting stars after the spike, while bought stars usually stop.
*   **Peak day**: the busiest single day, as a share of all stars.

| Verdict | Exit code | When |
|---|---|---|
| `suspicious` | 3 | At least 70% of stars arrived in the busiest 14 days, and less than 10% more arrived in the 60 days after them. |
| `review` | 2 | At least 40% of stars arrived in the busiest 14 days, at least 15% arrived on one day, fewer than 60 days of history exist after a burst, or the repository has fewer than 50 stars. |
| `pass` | 0 | None of the above. |

A burst alone gives `review`, not `suspicious`: a real launch that reaches the front page of Hacker News also produces one. Every threshold can be changed with a flag.

## Trust scan (`--trust`)

The trust scan needs the list of stargazers, so it only works when `GITHUB_TOKEN` belongs to an admin or collaborator of the repository. On any other repository, it stops with a message that explains the GitHub restriction.

Trust is computed based on several factors:

*   **Weighted Contributions**: Older contributions are weighted more heavily, as they are harder to "fake" in bulk.
*   **Activity Diversity**: Analysis of commits, issues, pull requests, and code reviews.
*   **Private Activity**: Recognition of private contributions (restricted contribution counts).
*   **Account Maturity**: Average account age; older accounts are statistically more trustworthy.
*   **Statistical Percentiles**: Evaluation of the distribution of contribution scores from the 5th to the 95th percentile.

## Getting Started

### Prerequisites

*   **Go 1.26 or later**, to build from source.
*   A **GitHub personal access token**: optional for the star history analysis (without it, GitHub allows 60 requests per hour), required for `--trust`. [Generate one here](https://github.com/settings/tokens).

### Installation

```bash
git clone https://github.com/stn1slv/staraudit.git
cd staraudit
make build
```

Prebuilt binaries for Linux, macOS and Windows are attached to each [GitHub release](https://github.com/stn1slv/staraudit/releases).

### Usage

Analyze the star history of any public repository:

```bash
export GITHUB_TOKEN=your_token_here   # optional, raises the rate limit
./staraudit ullaakut/astronomer
```

Get the result as JSON, for example in a CI job:

```bash
./staraudit --json ullaakut/astronomer | jq .verdict
```

Run the per-stargazer trust scan on a repository you administer:

```bash
./staraudit --trust your-org/your-repo
```

## Arguments and Options

*   **`repositoryOwner/repositoryName`**: (Required) The repository to analyze.
*   **`--json`**: Print the star history analysis as JSON on stdout. Log lines go to stderr. Not available with `--trust`.
*   **`--min-stars` (int)**: Fewer stars than this gives `review` (default: `50`).
*   **`--burst-review` (float)**: Burst share that gives `review` (default: `0.4`).
*   **`--burst-suspicious` (float)**: Burst share that can give `suspicious` (default: `0.7`).
*   **`--tail-suspicious` (float)**: Tail, as a share of the burst, below which a burst is `suspicious` (default: `0.1`).
*   **`--peak-review` (float)**: Peak day share that gives `review` (default: `0.15`).
*   **`--trust`**: Run the per-stargazer trust scan instead of the star history analysis.
*   **`-c, --cachedir` (string)**: Trust scan only. Directory for cached data (default: `./data`).
*   **`-s, --stars` (uint)**: Trust scan only. Maximum stars to scan in fast mode (default: `1000`). Rounded down to a multiple of 20 to match pagination.
*   **`-a, --all`**: Trust scan only. Scan all stargazers. Overrides `--stars`. Use with caution on large repositories.
*   **`-v, --verbose`**: Enable detailed logs and comparative analysis reports.

Every flag can also be set as an environment variable with the `STARAUDIT_` prefix, for example `STARAUDIT_BURST_REVIEW=0.5`.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | `pass`, or a successful `--trust` scan |
| 1 | Error |
| 2 | `review` |
| 3 | `suspicious` |

### JSON output

```json
{
  "schema_version": 1,
  "repository": "Continuum-AI-Corp/OrcaReplay",
  "analyzed_at": "2026-09-13T12:00:00Z",
  "verdict": "review",
  "reasons": [
    {"code": "burst", "message": "100% of stars arrived between 2026-08-31 and 2026-09-13"},
    {"code": "short_history", "message": "only 0 of 60 days after the burst exist yet"},
    {"code": "peak_day", "message": "18.8% of stars arrived on 2026-09-02"}
  ],
  "metrics": {
    "total_stars": 239,
    "history_days": 22,
    "burst_share": 1,
    "burst_start": "2026-08-31",
    "burst_end": "2026-09-13",
    "tail_ratio": 0,
    "tail_days_observed": 0,
    "peak_share": 0.188,
    "peak_day": "2026-09-02"
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

Reason codes are `no_tail`, `burst`, `short_history`, `peak_day` and `few_stars`. `schema_version` changes only for incompatible changes. On error, the document holds an `error` object with a `code` (`not_found`, `unauthorized`, `rate_limited`, `api_error` or `invalid_arguments`) and a `message`, and the exit code is 1.

### Environment variables

*   **`GITHUB_TOKEN`**: (Optional for the star history analysis, required for `--trust`) A GitHub personal access token. For `--trust`, it must belong to an admin or collaborator of the repository.
*   **`STARAUDIT_PRIVATE_KEY`**: (Optional, `--trust` only) PEM encoded PKCS#1 RSA key. Report signing and upload to Astrolab are opt-in: without this key the report is still computed and rendered locally, it is simply not uploaded.

## Upgrading from 1.x

StarAudit 2.0.0 changes the default mode:

*   `staraudit owner/repo` now runs the star history analysis. Add `--trust` to run the per-stargazer scan of 1.x.
*   `--stars`, `--all` and `--cachedir` only apply with `--trust`.
*   A successful run can now exit with 2 (`review`) or 3 (`suspicious`). Scripts that treat every non-zero exit code as a failure need to be updated.
*   The Go module path did not change, so `go install github.com/stn1slv/staraudit@latest` still installs 1.x. Use the release binaries or build from source.

## Development

The project includes a `Makefile` to simplify common tasks:

*   `make setup`: Bootstrap the project and download dependencies.
*   `make build`: Compile the `staraudit` binary.
*   `make test`: Run the full test suite.
*   `make lint`: Run static analysis (requires `golangci-lint`).
*   `make format`: Auto-format source code.
*   `make upgrade-deps`: Upgrade all Go dependencies to their latest versions.

## Examples

Trust scan reports:

![Traefik](img/traefik.png)
![Suspicious_repo](img/suspicious_repo.png)
![envoy](img/envoy.png)

## Questions & Answers

> _Why would fake stars be an issue?_

Repositories with high star counts often appear in GitHub Trending and newsletters, attracting real users and even influencing technology choices in startups. Bot-driven stars create a false sense of security and community backing.

> _How accurate is the star history analysis?_

The default thresholds were calibrated on one confirmed fake repository and six legitimate ones, including launches with large bursts. Treat `review` as a prompt for a human look, not as proof.

> _How accurate is the trust algorithm?_

StarAudit provides an estimate. A low score might indicate a community of casual users or low precision due to a small sample size. It is meant as a diagnostic tool rather than an absolute verdict.

> _Why do trust scan results vary slightly between scans?_

In fast mode, the trust scan checks the first 200 users and then takes random slices of the remaining stargazers. These random samples can lead to slight variations (1-3%) in the final score. Use the `--all` flag for a deterministic, comprehensive report.

## Thanks

Inspired by [spencerkimball/stargazers](https://github.com/spencerkimball/stargazers).
The original Go gopher was designed by [Renee French](http://reneefrench.blogspot.com).
