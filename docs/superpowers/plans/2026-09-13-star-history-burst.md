# Star History Burst Analysis Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a star history burst analysis the default mode of StarAudit 2.0.0, with JSON output and an exit code per verdict, and keep the per-stargazer trust scan behind `--trust`.

**Architecture:** A new service package `pkg/history` fetches `GET /repos/{owner}/{repo}/stargazers/history` over REST, turns it into a daily series, scores it with pure threshold rules, and renders text or JSON. `main.go` stays the presentation layer: it parses flags, runs the analysis (or the unchanged trust scan with `--trust`) and maps the verdict to an exit code.

**Tech Stack:** Go 1.26, `net/http`, `github.com/cenkalti/backoff/v5`, `github.com/Ullaakut/disgo`, `github.com/spf13/pflag` and `viper`, `github.com/stretchr/testify`, `golangci-lint` v2.

**Spec:** `docs/superpowers/specs/2026-09-13-star-history-burst-design.md`

## Global Constraints

- Release: StarAudit 2.0.0. The module path stays `github.com/stn1slv/staraudit` (no `/v2`).
- Go version: `go 1.26.0` as pinned in `go.mod`. No new dependencies.
- Endpoint: `GET https://api.github.com/repos/{owner}/{repo}/stargazers/history?per_page=30&page=N` with headers `Accept: application/vnd.github+json` and `X-GitHub-Api-Version: 2026-03-10`; `Authorization: Bearer <token>` only when `GITHUB_TOKEN` is set.
- Windows: burst window 14 days, tail window 60 days (constants, not flags).
- Default thresholds: `min-stars` 50, `burst-review` 0.40, `burst-suspicious` 0.70, `tail-suspicious` 0.10, `peak-review` 0.15.
- Exit codes: 0 pass (and a successful `--trust` scan), 1 error, 2 review, 3 suspicious.
- Error codes: `not_found`, `unauthorized`, `rate_limited`, `api_error`, `invalid_arguments`.
- Reason codes, in this order: `no_tail`, `burst`, `short_history`, `peak_day`, `few_stars`.
- JSON: `schema_version` 1; shares and ratios rounded to 3 decimals; dates as `YYYY-MM-DD`.
- `pkg/gql`, `pkg/trust`, `pkg/signature` and `pkg/context` do not change.
- The calibration fixtures in `pkg/history/testdata/` are already committed (fetched 2026-09-13). Do not re-fetch them: the expected verdicts depend on that date.
- Tests live in the same package and use testify. The linter config enables `testifylint` with every rule: use `require` for error assertions, `assert.InDelta` for floats, `assert.JSONEq` for JSON, and never `require` inside an HTTP handler.
- Text style: no em dash character anywhere; Markdown paragraphs and list items are not hard-wrapped.
- Commits: Conventional Commits, subject under 72 characters, no AI attribution or session trailers.

## File Structure

| File | Responsibility |
|---|---|
| `pkg/history/errors.go` (create) | Package doc, typed `*Error` with stable codes. |
| `pkg/history/fetch.go` (create) | Raw `week` type, `Series`, week validation, `parseWeeks`, `Fetch` with paging and retries. |
| `pkg/history/analyze.go` (create) | `Thresholds`, `Verdict`, `Reason`, `Metrics`, `Result`, `Analyze` and its formatting helpers. |
| `pkg/history/render.go` (create) | `RenderText`, `RenderJSON`, `RenderErrorJSON`. |
| `pkg/history/*_test.go` (create) | Unit tests, stub server tests, calibration tests. |
| `main.go` (modify) | New flags, default mode, `--trust`, `--json`, exit codes. |
| `main_test.go` (create) | Verdict to exit code mapping. |
| `README.md`, `CLAUDE.md` (modify) | Documentation for 2.0.0. |

## Verified reference code

Every code block below was compiled and tested before this plan was written: `go test -race ./...` passed, `golangci-lint run ./...` reported 0 issues, and the smoke tests in Task 5 gave the documented results against the real API on 2026-09-13.

---

### Task 1: Parse and validate star history weeks

**Files:**
- Create: `pkg/history/errors.go`
- Create: `pkg/history/fetch.go`
- Test: `pkg/history/fetch_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Error struct{ Code, Message string; Err error }` with `Error()` and `Unwrap()`; constants `CodeNotFound`, `CodeUnauthorized`, `CodeRateLimited`, `CodeAPIError`; unexported `type week struct{ Week int64; Total int; Days []int }`; `type Series struct{ Start time.Time; Days []int }`; unexported `parseWeeks(weeks []week, now time.Time) (Series, error)`; unexported constant `day = 24 * time.Hour`. Test helpers `sunday` (2026-08-23 UTC) and `weekAt(start time.Time, days ...int) week` in `fetch_test.go`, reused by later tasks.

- [ ] **Step 1: Write the failing test**

Create `pkg/history/fetch_test.go`:

```go
package history

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sunday is the start of the first week in the test data.
var sunday = time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)

// weekAt builds a valid week starting at the given time.
func weekAt(start time.Time, days ...int) week {
	total := 0
	for _, count := range days {
		total += count
	}

	return week{Week: start.Unix(), Total: total, Days: days}
}

func TestParseWeeksSortsAndFlattens(t *testing.T) {
	weeks := []week{
		weekAt(sunday.AddDate(0, 0, 7), 8, 9, 10, 11, 12, 13, 14),
		weekAt(sunday, 1, 2, 3, 4, 5, 6, 7),
	}

	series, err := parseWeeks(weeks, sunday.AddDate(0, 0, 30))

	require.NoError(t, err)
	assert.Equal(t, sunday, series.Start)
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, series.Days)
}

func TestParseWeeksDropsFutureDays(t *testing.T) {
	weeks := []week{weekAt(sunday, 1, 2, 3, 4, 5, 6, 7)}

	// Tuesday noon: Sunday, Monday and Tuesday have started.
	series, err := parseWeeks(weeks, sunday.AddDate(0, 0, 2).Add(12*time.Hour))

	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, series.Days)
}

func TestParseWeeksWithoutWeeks(t *testing.T) {
	series, err := parseWeeks(nil, sunday)

	require.NoError(t, err)
	assert.Empty(t, series.Days)
}

func TestParseWeeksRejectsMalformedWeeks(t *testing.T) {
	tests := map[string]week{
		"missing start":  {Week: 0, Total: 0, Days: make([]int, 7)},
		"six days":       {Week: sunday.Unix(), Total: 0, Days: make([]int, 6)},
		"negative count": {Week: sunday.Unix(), Total: -1, Days: []int{-1, 0, 0, 0, 0, 0, 0}},
		"wrong total":    {Week: sunday.Unix(), Total: 5, Days: []int{1, 0, 0, 0, 0, 0, 0}},
	}

	for name, malformed := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseWeeks([]week{malformed}, sunday.AddDate(0, 0, 30))

			var historyErr *Error
			require.ErrorAs(t, err, &historyErr)
			assert.Equal(t, CodeAPIError, historyErr.Code)
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 ./pkg/history/`
Expected: FAIL with `undefined: week` and `undefined: parseWeeks`.

- [ ] **Step 3: Write the implementation**

Create `pkg/history/errors.go`:

```go
// Package history fetches the star history of a GitHub repository and
// detects star bursts in it.
package history

import "fmt"

// Error codes reported by this package. They are part of the JSON output, so
// they must stay stable.
const (
	CodeNotFound     = "not_found"
	CodeUnauthorized = "unauthorized"
	CodeRateLimited  = "rate_limited"
	CodeAPIError     = "api_error"
)

// Error is a failure to obtain the star history, with a stable code.
type Error struct {
	Code    string
	Message string
	Err     error
}

// Error returns the message, followed by the cause when there is one.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}

	return e.Message
}

// Unwrap returns the cause of the error, if any.
func (e *Error) Unwrap() error {
	return e.Err
}
```

Create `pkg/history/fetch.go`:

```go
package history

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

const day = 24 * time.Hour

// week is one element of the star history response.
type week struct {
	Week  int64 `json:"week"`
	Total int   `json:"total"`
	Days  []int `json:"days"`
}

// Series holds the daily star counts of a repository, oldest day first.
type Series struct {
	// Start is the date of the first element of Days, as GitHub reports it.
	Start time.Time
	Days  []int
}

// validate rejects a week that does not match the documented response.
func (w week) validate() error {
	if w.Week <= 0 {
		return fmt.Errorf("week %d has an invalid start", w.Week)
	}

	if len(w.Days) != 7 {
		return fmt.Errorf("week %d has %d days instead of 7", w.Week, len(w.Days))
	}

	sum := 0

	for _, count := range w.Days {
		if count < 0 {
			return fmt.Errorf("week %d has a negative count", w.Week)
		}

		sum += count
	}

	if sum != w.Total {
		return fmt.Errorf("week %d has a total of %d, but its days add up to %d", w.Week, w.Total, sum)
	}

	return nil
}

// parseWeeks turns the weeks of every page into one daily series. Days after
// now are dropped, since the newest week also covers days to come.
func parseWeeks(weeks []week, now time.Time) (Series, error) {
	for _, w := range weeks {
		if err := w.validate(); err != nil {
			return Series{}, &Error{Code: CodeAPIError, Message: "unexpected star history from the GitHub API", Err: err}
		}
	}

	if len(weeks) == 0 {
		return Series{}, nil
	}

	sorted := slices.Clone(weeks)
	slices.SortFunc(sorted, func(a, b week) int { return cmp.Compare(a.Week, b.Week) })

	series := Series{
		Start: time.Unix(sorted[0].Week, 0).UTC(),
		Days:  make([]int, 0, 7*len(sorted)),
	}

	for _, w := range sorted {
		for i, count := range w.Days {
			if time.Unix(w.Week, 0).Add(time.Duration(i) * day).After(now) {
				return series, nil
			}

			series.Days = append(series.Days, count)
		}
	}

	return series, nil
}
```

- [ ] **Step 4: Run the tests and the linter**

Run: `go test -race -count=1 ./pkg/history/ && golangci-lint run ./pkg/history/...`
Expected: `ok  github.com/stn1slv/staraudit/pkg/history` and `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add pkg/history/errors.go pkg/history/fetch.go pkg/history/fetch_test.go
git commit -m "feat(history): parse and validate star history weeks"
```

---

### Task 2: Score star bursts against thresholds

**Files:**
- Create: `pkg/history/analyze.go`
- Test: `pkg/history/analyze_test.go`
- Test: `pkg/history/calibration_test.go` (uses the committed `pkg/history/testdata/*.json`)

**Interfaces:**
- Consumes: `Series`, `parseWeeks`, `day`, test helper `sunday` (Task 1).
- Produces: `type Verdict string` with `VerdictPass`, `VerdictReview`, `VerdictSuspicious`; reason constants `ReasonNoTail`, `ReasonBurst`, `ReasonShortHistory`, `ReasonPeakDay`, `ReasonFewStars`; `type Thresholds struct{ MinStars int; BurstReview, BurstSuspicious, TailSuspicious, PeakReview float64 }` with JSON tags; `DefaultThresholds() Thresholds`; `(Thresholds) Validate() error`; `type Reason struct{ Code, Message string }`; `type Metrics struct{ TotalStars, HistoryDays int; BurstShare float64; BurstStart, BurstEnd time.Time; TailRatio float64; TailDaysObserved int; PeakShare float64; PeakDay time.Time }`; `type Result struct{ Verdict Verdict; Reasons []Reason; Metrics Metrics; Thresholds Thresholds }`; `Analyze(series Series, t Thresholds) Result`; unexported constants `burstWindowDays = 14`, `tailWindowDays = 60`; unexported helpers `percent(float64) string` and `formatDay(time.Time) string`. Test helper `reasonCodes(Result) []string` in `analyze_test.go`.

- [ ] **Step 1: Write the failing tests**

Create `pkg/history/analyze_test.go`:

```go
package history

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// run is a stretch of days that all received the same amount of stars.
type run struct {
	days  int
	stars int
}

// seriesOf builds a series starting on sunday from runs of equal days, so
// that seriesOf(run{10, 1}, run{14, 50}) is ten days with one star each,
// then fourteen days with fifty.
func seriesOf(runs ...run) Series {
	series := Series{Start: sunday}

	for _, r := range runs {
		for range r.days {
			series.Days = append(series.Days, r.stars)
		}
	}

	return series
}

func reasonCodes(result Result) []string {
	codes := make([]string, 0, len(result.Reasons))
	for _, reason := range result.Reasons {
		codes = append(codes, reason.Code)
	}

	return codes
}

func TestAnalyzeVerdicts(t *testing.T) {
	tests := map[string]struct {
		series     Series
		thresholds Thresholds

		expectedVerdict Verdict
		expectedReasons []string
	}{
		"fake burst followed by 60 days without stars": {
			series:          seriesOf(run{30, 0}, run{14, 20}, run{60, 0}),
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictSuspicious,
			expectedReasons: []string{ReasonNoTail, ReasonBurst},
		},
		"young repository with a spike": {
			series:          seriesOf(run{8, 0}, run{13, 10}, run{1, 50}),
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictReview,
			expectedReasons: []string{ReasonBurst, ReasonShortHistory, ReasonPeakDay},
		},
		"launch followed by a healthy tail": {
			series:          seriesOf(run{200, 1}, run{14, 30}, run{60, 5}, run{200, 1}),
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictPass,
			expectedReasons: []string{},
		},
		"steady growth": {
			series:          seriesOf(run{365, 3}),
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictPass,
			expectedReasons: []string{},
		},
		"fewer stars than the minimum": {
			series:          seriesOf(run{49, 1}),
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictReview,
			expectedReasons: []string{ReasonFewStars},
		},
		"no stars": {
			series:          seriesOf(run{30, 0}),
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictReview,
			expectedReasons: []string{ReasonFewStars},
		},
		"no days": {
			series:          Series{Start: sunday},
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictReview,
			expectedReasons: []string{ReasonFewStars},
		},
		"burst share exactly on the review threshold": {
			series:          seriesOf(run{8, 0}, run{14, 20}),
			thresholds:      Thresholds{BurstReview: 1, BurstSuspicious: 1, PeakReview: 1},
			expectedVerdict: VerdictReview,
			expectedReasons: []string{ReasonBurst, ReasonShortHistory},
		},
		"peak share exactly on the review threshold": {
			series:          seriesOf(run{4, 1}),
			thresholds:      Thresholds{BurstReview: 1, BurstSuspicious: 1, PeakReview: 0.25},
			expectedVerdict: VerdictReview,
			expectedReasons: []string{ReasonBurst, ReasonShortHistory, ReasonPeakDay},
		},
		"tail ratio exactly on the suspicious threshold": {
			// The tail is 14 stars for a burst of 140, a ratio of exactly 0.1.
			series:          seriesOf(run{14, 10}, run{14, 1}, run{46, 0}),
			thresholds:      DefaultThresholds(),
			expectedVerdict: VerdictReview,
			expectedReasons: []string{ReasonBurst},
		},
		"tail ratio just below the suspicious threshold": {
			series:          seriesOf(run{14, 10}, run{14, 1}, run{46, 0}),
			thresholds:      Thresholds{MinStars: 50, BurstReview: 0.4, BurstSuspicious: 0.7, TailSuspicious: 0.11, PeakReview: 0.15},
			expectedVerdict: VerdictSuspicious,
			expectedReasons: []string{ReasonNoTail, ReasonBurst},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := Analyze(test.series, test.thresholds)

			assert.Equal(t, test.expectedVerdict, result.Verdict)
			assert.Equal(t, test.expectedReasons, reasonCodes(result))
			assert.Equal(t, test.thresholds, result.Thresholds)
		})
	}
}

func TestAnalyzeMetrics(t *testing.T) {
	// 8 quiet days, 13 days with 10 stars, then one day with 50.
	result := Analyze(seriesOf(run{8, 0}, run{13, 10}, run{1, 50}), DefaultThresholds())
	m := result.Metrics

	assert.Equal(t, 180, m.TotalStars)
	assert.Equal(t, 22, m.HistoryDays)
	assert.InDelta(t, 1.0, m.BurstShare, 1e-9)
	assert.Equal(t, sunday.AddDate(0, 0, 8), m.BurstStart)
	assert.Equal(t, sunday.AddDate(0, 0, 21), m.BurstEnd)
	assert.Zero(t, m.TailDaysObserved)
	assert.InDelta(t, 0.0, m.TailRatio, 1e-9)
	assert.InDelta(t, 50.0/180, m.PeakShare, 1e-9)
	assert.Equal(t, sunday.AddDate(0, 0, 21), m.PeakDay)
}

func TestAnalyzeTailIsMeasuredAfterTheBurst(t *testing.T) {
	result := Analyze(seriesOf(run{14, 10}, run{60, 1}, run{10, 0}), DefaultThresholds())

	assert.Equal(t, 60, result.Metrics.TailDaysObserved)
	assert.InDelta(t, 60.0/140, result.Metrics.TailRatio, 1e-9)
}

func TestAnalyzePrefersTheEarliestOfEqualWindows(t *testing.T) {
	result := Analyze(seriesOf(run{14, 5}, run{30, 0}, run{14, 5}), DefaultThresholds())

	assert.Equal(t, sunday, result.Metrics.BurstStart)
	assert.Equal(t, sunday.AddDate(0, 0, 13), result.Metrics.BurstEnd)
}

func TestAnalyzeSeriesShorterThanTheWindow(t *testing.T) {
	result := Analyze(seriesOf(run{5, 20}), DefaultThresholds())

	assert.InDelta(t, 1.0, result.Metrics.BurstShare, 1e-9)
	assert.Equal(t, sunday, result.Metrics.BurstStart)
	assert.Equal(t, sunday.AddDate(0, 0, 4), result.Metrics.BurstEnd)
	assert.Equal(t, []string{ReasonBurst, ReasonShortHistory, ReasonPeakDay}, reasonCodes(result))
}

func TestAnalyzeWithoutStarsHasNoDates(t *testing.T) {
	m := Analyze(seriesOf(run{30, 0}), DefaultThresholds()).Metrics

	assert.True(t, m.BurstStart.IsZero())
	assert.True(t, m.PeakDay.IsZero())
}

func TestAnalyzeReasonMessages(t *testing.T) {
	result := Analyze(seriesOf(run{8, 0}, run{13, 10}, run{1, 50}), DefaultThresholds())

	require.Len(t, result.Reasons, 3)
	assert.Equal(t, "100% of stars arrived between 2026-08-31 and 2026-09-13", result.Reasons[0].Message)
	assert.Equal(t, "only 0 of 60 days after the burst exist yet", result.Reasons[1].Message)
	assert.Equal(t, "27.8% of stars arrived on 2026-09-13", result.Reasons[2].Message)
}

func TestThresholdsValidate(t *testing.T) {
	require.NoError(t, DefaultThresholds().Validate())

	tests := map[string]func(*Thresholds){
		"negative minimum":          func(t *Thresholds) { t.MinStars = -1 },
		"share above one":           func(t *Thresholds) { t.PeakReview = 1.5 },
		"negative share":            func(t *Thresholds) { t.TailSuspicious = -0.1 },
		"review above suspicious":   func(t *Thresholds) { t.BurstReview = 0.8 },
		"suspicious below review":   func(t *Thresholds) { t.BurstSuspicious = 0.3 },
		"burst review above one":    func(t *Thresholds) { t.BurstReview = 2 },
		"burst suspicious negative": func(t *Thresholds) { t.BurstSuspicious = -1 },
	}

	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			thresholds := DefaultThresholds()
			change(&thresholds)

			assert.Error(t, thresholds.Validate())
		})
	}
}

func TestPercent(t *testing.T) {
	assert.Equal(t, "100%", percent(1))
	assert.Equal(t, "18.8%", percent(45.0/239))
	assert.Equal(t, "0%", percent(0))
}

func TestFormatDayUsesUTC(t *testing.T) {
	assert.Equal(t, "2026-08-23", formatDay(sunday.In(time.FixedZone("UTC-5", -5*60*60))))
}
```

Create `pkg/history/calibration_test.go`:

```go
package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixturesFetchedAt is when the files in testdata were fetched.
var fixturesFetchedAt = time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC)

// loadFixture reads a star history saved from the real API.
func loadFixture(t *testing.T, name string) Series {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name+".json")) // #nosec G304 -- fixture names are constants in this file.
	require.NoError(t, err)

	var weeks []week
	require.NoError(t, json.Unmarshal(data, &weeks))

	series, err := parseWeeks(weeks, fixturesFetchedAt)
	require.NoError(t, err)

	return series
}

// TestAnalyzeCalibrationFixtures guards the default thresholds against the
// repositories they were calibrated on: OrcaReplay, the one confirmed fake,
// and six legitimate repositories, including launches with large bursts.
func TestAnalyzeCalibrationFixtures(t *testing.T) {
	tests := map[string]Verdict{
		"orcareplay":              VerdictReview,
		"openapi-style-validator": VerdictPass,
		"mcpjungle":               VerdictPass,
		"uv":                      VerdictPass,
		"vhs":                     VerdictPass,
		"ollama":                  VerdictPass,
		"zed":                     VerdictPass,
	}

	for name, expected := range tests {
		t.Run(name, func(t *testing.T) {
			result := Analyze(loadFixture(t, name), DefaultThresholds())

			assert.Equal(t, expected, result.Verdict, "reasons: %v", result.Reasons)
		})
	}
}

func TestAnalyzeOrcaReplayFixture(t *testing.T) {
	result := Analyze(loadFixture(t, "orcareplay"), DefaultThresholds())
	m := result.Metrics

	assert.Equal(t, []string{ReasonBurst, ReasonShortHistory, ReasonPeakDay}, reasonCodes(result))
	assert.Equal(t, 239, m.TotalStars)
	assert.InDelta(t, 1.0, m.BurstShare, 1e-9)
	assert.Equal(t, "2026-08-31", formatDay(m.BurstStart))
	assert.Equal(t, "2026-09-13", formatDay(m.BurstEnd))
	assert.Zero(t, m.TailDaysObserved)
	assert.Equal(t, "2026-09-02", formatDay(m.PeakDay))
	assert.InDelta(t, 45.0/239, m.PeakShare, 1e-9)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./pkg/history/`
Expected: FAIL with `undefined: Analyze`, `undefined: Thresholds` and similar.

- [ ] **Step 3: Write the implementation**

Create `pkg/history/analyze.go`:

```go
package history

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	// burstWindowDays is the length of the window in which a burst is measured.
	burstWindowDays = 14

	// tailWindowDays is how long after the burst window the tail is measured.
	tailWindowDays = 60
)

// Verdict is the outcome of the analysis.
type Verdict string

// Verdicts, from least to most severe.
const (
	VerdictPass       Verdict = "pass"
	VerdictReview     Verdict = "review"
	VerdictSuspicious Verdict = "suspicious"
)

// Reason codes. They are part of the JSON output, so they must stay stable.
const (
	ReasonNoTail       = "no_tail"
	ReasonBurst        = "burst"
	ReasonShortHistory = "short_history"
	ReasonPeakDay      = "peak_day"
	ReasonFewStars     = "few_stars"
)

// Thresholds configure the rules of the analysis.
type Thresholds struct {
	MinStars        int     `json:"min_stars"`
	BurstReview     float64 `json:"burst_review"`
	BurstSuspicious float64 `json:"burst_suspicious"`
	TailSuspicious  float64 `json:"tail_suspicious"`
	PeakReview      float64 `json:"peak_review"`
}

// DefaultThresholds returns the thresholds calibrated in the design.
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinStars:        50,
		BurstReview:     0.40,
		BurstSuspicious: 0.70,
		TailSuspicious:  0.10,
		PeakReview:      0.15,
	}
}

// Validate reports thresholds that the rules cannot work with. The names in
// the messages are the command line flags.
func (t Thresholds) Validate() error {
	if t.MinStars < 0 {
		return errors.New("min-stars must not be negative")
	}

	shares := []struct {
		name  string
		value float64
	}{
		{"burst-review", t.BurstReview},
		{"burst-suspicious", t.BurstSuspicious},
		{"tail-suspicious", t.TailSuspicious},
		{"peak-review", t.PeakReview},
	}

	for _, share := range shares {
		if share.value < 0 || share.value > 1 {
			return fmt.Errorf("%s must be between 0 and 1, got %v", share.name, share.value)
		}
	}

	if t.BurstReview > t.BurstSuspicious {
		return fmt.Errorf("burst-review (%v) must not be higher than burst-suspicious (%v)", t.BurstReview, t.BurstSuspicious)
	}

	return nil
}

// Reason explains why a rule matched.
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Metrics describe the shape of a star history. Dates are zero when the
// history has no stars.
type Metrics struct {
	TotalStars       int
	HistoryDays      int
	BurstShare       float64
	BurstStart       time.Time
	BurstEnd         time.Time
	TailRatio        float64
	TailDaysObserved int
	PeakShare        float64
	PeakDay          time.Time
}

// Result is the outcome of Analyze.
type Result struct {
	Verdict    Verdict
	Reasons    []Reason
	Metrics    Metrics
	Thresholds Thresholds
}

// Analyze computes the metrics of a series and applies the rules to them.
// Reasons are listed in the order of the rules, and the verdict is the most
// severe matching rule.
func Analyze(series Series, t Thresholds) Result {
	m := computeMetrics(series)
	result := Result{Verdict: VerdictPass, Reasons: []Reason{}, Metrics: m, Thresholds: t}

	add := func(verdict Verdict, code, message string) {
		result.Reasons = append(result.Reasons, Reason{Code: code, Message: message})
		if severity(verdict) > severity(result.Verdict) {
			result.Verdict = verdict
		}
	}

	hasStars := m.TotalStars > 0
	burst := hasStars && m.BurstShare >= t.BurstReview

	if hasStars && m.BurstShare >= t.BurstSuspicious && m.TailDaysObserved == tailWindowDays && m.TailRatio < t.TailSuspicious {
		add(VerdictSuspicious, ReasonNoTail,
			fmt.Sprintf("only %s more stars arrived in the %d days after the burst", percent(m.TailRatio), tailWindowDays))
	}

	if burst {
		add(VerdictReview, ReasonBurst,
			fmt.Sprintf("%s of stars arrived between %s and %s", percent(m.BurstShare), formatDay(m.BurstStart), formatDay(m.BurstEnd)))
	}

	if burst && m.TailDaysObserved < tailWindowDays {
		add(VerdictReview, ReasonShortHistory,
			fmt.Sprintf("only %d of %d days after the burst exist yet", m.TailDaysObserved, tailWindowDays))
	}

	if hasStars && m.PeakShare >= t.PeakReview {
		add(VerdictReview, ReasonPeakDay,
			fmt.Sprintf("%s of stars arrived on %s", percent(m.PeakShare), formatDay(m.PeakDay)))
	}

	if m.TotalStars < t.MinStars {
		add(VerdictReview, ReasonFewStars,
			fmt.Sprintf("only %d stars, fewer than the minimum of %d", m.TotalStars, t.MinStars))
	}

	return result
}

// computeMetrics measures the busiest window, the tail after it and the
// busiest day.
func computeMetrics(series Series) Metrics {
	days := series.Days
	m := Metrics{HistoryDays: len(days)}

	for _, count := range days {
		m.TotalStars += count
	}

	if m.TotalStars == 0 {
		return m
	}

	// A series shorter than the window is one window.
	window := min(burstWindowDays, len(days))

	sum := 0
	for _, count := range days[:window] {
		sum += count
	}

	// Only a strictly larger sum moves the window, so the earliest wins a tie.
	best, bestStart := sum, 0

	for i := window; i < len(days); i++ {
		sum += days[i] - days[i-window]
		if sum > best {
			best, bestStart = sum, i-window+1
		}
	}

	m.BurstShare = float64(best) / float64(m.TotalStars)
	m.BurstStart = series.date(bestStart)
	m.BurstEnd = series.date(bestStart + window - 1)

	tailStart := bestStart + window
	tailEnd := min(tailStart+tailWindowDays, len(days))

	tail := 0
	for _, count := range days[tailStart:tailEnd] {
		tail += count
	}

	m.TailDaysObserved = tailEnd - tailStart
	m.TailRatio = float64(tail) / float64(best)

	peak, peakIndex := days[0], 0

	for i, count := range days {
		if count > peak {
			peak, peakIndex = count, i
		}
	}

	m.PeakShare = float64(peak) / float64(m.TotalStars)
	m.PeakDay = series.date(peakIndex)

	return m
}

// date returns the date of the day at the given index.
func (s Series) date(index int) time.Time {
	return s.Start.Add(time.Duration(index) * day)
}

// severity orders verdicts so that the most severe one can be kept.
func severity(verdict Verdict) int {
	switch verdict {
	case VerdictSuspicious:
		return 2
	case VerdictReview:
		return 1
	default:
		return 0
	}
}

// percent formats a share as a percentage with at most one decimal.
func percent(share float64) string {
	return strconv.FormatFloat(math.Round(share*1000)/10, 'f', -1, 64) + "%"
}

// formatDay formats a date the way the reports show it.
func formatDay(t time.Time) string {
	return t.UTC().Format(time.DateOnly)
}
```

- [ ] **Step 4: Run the tests and the linter**

Run: `go test -race -count=1 ./pkg/history/ && golangci-lint run ./pkg/history/...`
Expected: `ok` and `0 issues.` In particular `TestAnalyzeCalibrationFixtures` passes for all seven fixtures.

- [ ] **Step 5: Commit**

```bash
git add pkg/history/analyze.go pkg/history/analyze_test.go pkg/history/calibration_test.go
git commit -m "feat(history): score star bursts against thresholds"
```

---

### Task 3: Fetch the star history from the GitHub REST API

**Files:**
- Modify: `pkg/history/fetch.go` (replace the whole file)
- Modify: `pkg/history/fetch_test.go` (replace the whole file)

**Interfaces:**
- Consumes: `week`, `Series`, `parseWeeks`, `Error`, error codes (Task 1).
- Produces: `Fetch(ctx context.Context, owner, repo, token string) (Series, error)`. Errors are `*Error` with `CodeNotFound`, `CodeUnauthorized`, `CodeRateLimited` or `CodeAPIError`, except a cancelled context, which is returned as the context error (`errors.Is(err, context.Canceled)`). Package variables `apiBaseURL` and `retryInitialInterval` for tests; unexported constant `maxAttempts = 5`.

- [ ] **Step 1: Write the failing tests**

Replace `pkg/history/fetch_test.go` with (the `parseWeeks` tests from Task 1 are kept unchanged at the top):

```go
package history

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sunday is the start of the first week in the test data.
var sunday = time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)

// weekAt builds a valid week starting at the given time.
func weekAt(start time.Time, days ...int) week {
	total := 0
	for _, count := range days {
		total += count
	}

	return week{Week: start.Unix(), Total: total, Days: days}
}

func TestParseWeeksSortsAndFlattens(t *testing.T) {
	weeks := []week{
		weekAt(sunday.AddDate(0, 0, 7), 8, 9, 10, 11, 12, 13, 14),
		weekAt(sunday, 1, 2, 3, 4, 5, 6, 7),
	}

	series, err := parseWeeks(weeks, sunday.AddDate(0, 0, 30))

	require.NoError(t, err)
	assert.Equal(t, sunday, series.Start)
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, series.Days)
}

func TestParseWeeksDropsFutureDays(t *testing.T) {
	weeks := []week{weekAt(sunday, 1, 2, 3, 4, 5, 6, 7)}

	// Tuesday noon: Sunday, Monday and Tuesday have started.
	series, err := parseWeeks(weeks, sunday.AddDate(0, 0, 2).Add(12*time.Hour))

	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, series.Days)
}

func TestParseWeeksWithoutWeeks(t *testing.T) {
	series, err := parseWeeks(nil, sunday)

	require.NoError(t, err)
	assert.Empty(t, series.Days)
}

func TestParseWeeksRejectsMalformedWeeks(t *testing.T) {
	tests := map[string]week{
		"missing start":  {Week: 0, Total: 0, Days: make([]int, 7)},
		"six days":       {Week: sunday.Unix(), Total: 0, Days: make([]int, 6)},
		"negative count": {Week: sunday.Unix(), Total: -1, Days: []int{-1, 0, 0, 0, 0, 0, 0}},
		"wrong total":    {Week: sunday.Unix(), Total: 5, Days: []int{1, 0, 0, 0, 0, 0, 0}},
	}

	for name, malformed := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseWeeks([]week{malformed}, sunday.AddDate(0, 0, 30))

			var historyErr *Error
			require.ErrorAs(t, err, &historyErr)
			assert.Equal(t, CodeAPIError, historyErr.Code)
		})
	}
}

// stubAPI points the package at a test server for the duration of the test.
func stubAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	originalURL, originalInterval := apiBaseURL, retryInitialInterval
	apiBaseURL = server.URL
	// Keep retries instant so the suite does not wait out real backoff.
	retryInitialInterval = time.Millisecond

	t.Cleanup(func() {
		apiBaseURL = originalURL
		retryInitialInterval = originalInterval
	})
}

// writeWeeks answers with the given weeks. No weeks is an empty page.
func writeWeeks(w http.ResponseWriter, weeks ...week) {
	if weeks == nil {
		weeks = []week{}
	}

	body, err := json.Marshal(weeks)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	_, _ = w.Write(body)
}

// twoPages serves one week on each of the first two pages, then an empty page.
func twoPages(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("page") {
	case "1":
		writeWeeks(w, weekAt(sunday.AddDate(0, 0, 7), 0, 0, 0, 0, 0, 0, 2))
	case "2":
		writeWeeks(w, weekAt(sunday, 1, 0, 0, 0, 0, 0, 0))
	default:
		writeWeeks(w)
	}
}

func TestFetchPagesUntilEmptyPage(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		twoPages(w, r)
	})

	series, err := Fetch(context.Background(), "owner", "repo", "")

	require.NoError(t, err)
	assert.Equal(t, sunday, series.Start)
	assert.Equal(t, []int{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}, series.Days)
	assert.Equal(t, int32(3), requests.Load(), "paging must stop at the first empty page")
}

func TestFetchSendsTheExpectedRequest(t *testing.T) {
	requests := make(chan *http.Request, 10)

	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		writeWeeks(w)
	})

	_, err := Fetch(context.Background(), "owner", "repo", "secret")
	require.NoError(t, err)

	request := <-requests
	assert.Equal(t, "/repos/owner/repo/stargazers/history", request.URL.Path)
	assert.Equal(t, "30", request.URL.Query().Get("per_page"))
	assert.Equal(t, "1", request.URL.Query().Get("page"))
	assert.Equal(t, "2026-03-10", request.Header.Get("X-GitHub-Api-Version"))
	assert.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
}

func TestFetchWithoutTokenSendsNoAuthorization(t *testing.T) {
	requests := make(chan *http.Request, 10)

	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		writeWeeks(w)
	})

	_, err := Fetch(context.Background(), "owner", "repo", "")
	require.NoError(t, err)

	assert.Empty(t, (<-requests).Header.Get("Authorization"))
}

func TestFetchErrors(t *testing.T) {
	tests := map[string]struct {
		handler          http.HandlerFunc
		expectedCode     string
		expectedRequests int32
	}{
		"not found": {
			handler:          func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			expectedCode:     CodeNotFound,
			expectedRequests: 1,
		},
		"rejected credentials": {
			handler:          func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			expectedCode:     CodeUnauthorized,
			expectedRequests: 1,
		},
		"rate limit used up": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", "1789300800")
				w.WriteHeader(http.StatusForbidden)
			},
			expectedCode:     CodeRateLimited,
			expectedRequests: 1,
		},
		"unexpected status": {
			handler:          func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) },
			expectedCode:     CodeAPIError,
			expectedRequests: 1,
		},
		"server error on every attempt": {
			handler:          func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
			expectedCode:     CodeAPIError,
			expectedRequests: maxAttempts,
		},
		"unreadable body": {
			handler:          func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{") },
			expectedCode:     CodeAPIError,
			expectedRequests: 1,
		},
		"malformed week": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "1" {
					writeWeeks(w, week{Week: sunday.Unix(), Total: 0, Days: make([]int, 6)})
					return
				}

				writeWeeks(w)
			},
			expectedCode:     CodeAPIError,
			expectedRequests: 2,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32

			stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				test.handler(w, r)
			})

			_, err := Fetch(context.Background(), "owner", "repo", "")

			var historyErr *Error
			require.ErrorAs(t, err, &historyErr)
			assert.Equal(t, test.expectedCode, historyErr.Code)
			assert.Equal(t, test.expectedRequests, requests.Load())
		})
	}
}

func TestFetchRateLimitSuggestsAToken(t *testing.T) {
	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := Fetch(context.Background(), "owner", "repo", "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "GITHUB_TOKEN")
}

func TestFetchRetriesServerErrors(t *testing.T) {
	var failures atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}

		twoPages(w, r)
	})

	series, err := Fetch(context.Background(), "owner", "repo", "")

	require.NoError(t, err, "a single server error must be retried")
	assert.Len(t, series.Days, 14)
}

func TestFetchHonoursRetryAfter(t *testing.T) {
	var attempts atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusForbidden)

			return
		}

		twoPages(w, r)
	})

	series, err := Fetch(context.Background(), "owner", "repo", "")

	require.NoError(t, err, "a secondary rate limit must be retried")
	assert.Len(t, series.Days, 14)
}

func TestFetchStopsOnCancelledContext(t *testing.T) {
	stubAPI(t, twoPages)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Fetch(ctx, "owner", "repo", "")

	require.ErrorIs(t, err, context.Canceled)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./pkg/history/`
Expected: FAIL with `undefined: apiBaseURL`, `undefined: Fetch` and `undefined: maxAttempts`.

- [ ] **Step 3: Write the implementation**

Replace `pkg/history/fetch.go` with:

```go
package history

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"
)

var (
	// apiBaseURL is a variable so that tests can point it at a stub server.
	apiBaseURL = "https://api.github.com"

	// retryInitialInterval is the first backoff delay. Tests shorten it.
	retryInitialInterval = time.Second
)

const (
	// apiVersion is the REST API version that introduced the endpoint.
	apiVersion = "2026-03-10"

	// perPage is the largest page size the endpoint accepts.
	perPage = 30

	// maxPages is the page limit of the endpoint.
	maxPages = 100

	// maxAttempts is how many times a page is requested before giving up.
	maxAttempts = 5

	requestTimeout = 30 * time.Second

	day = 24 * time.Hour
)

// week is one element of the star history response.
type week struct {
	Week  int64 `json:"week"`
	Total int   `json:"total"`
	Days  []int `json:"days"`
}

// Series holds the daily star counts of a repository, oldest day first.
type Series struct {
	// Start is the date of the first element of Days, as GitHub reports it.
	Start time.Time
	Days  []int
}

// Fetch returns the daily star history of a repository. The token is
// optional: without it, the unauthenticated rate limit applies.
func Fetch(ctx context.Context, owner, repo, token string) (Series, error) {
	client := &http.Client{Timeout: requestTimeout}

	var weeks []week

	for page := 1; page <= maxPages; page++ {
		pageWeeks, err := fetchPage(ctx, client, owner, repo, token, page)
		if err != nil {
			return Series{}, err
		}

		if len(pageWeeks) == 0 {
			break
		}

		weeks = append(weeks, pageWeeks...)
	}

	return parseWeeks(weeks, time.Now())
}

// fetchPage requests one page, retrying server errors and honouring
// Retry-After.
func fetchPage(ctx context.Context, client *http.Client, owner, repo, token string, page int) ([]week, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/stargazers/history?per_page=%d&page=%d",
		apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), perPage, page)

	backOff := backoff.NewExponentialBackOff()
	backOff.InitialInterval = retryInitialInterval

	weeks, err := backoff.Retry(ctx, func() ([]week, error) {
		return requestPage(ctx, client, endpoint, token)
	}, backoff.WithBackOff(backOff), backoff.WithMaxTries(maxAttempts))
	if err == nil {
		return weeks, nil
	}

	// The last attempt is returned as is, even when it was permanent.
	var permanent *backoff.PermanentError
	if errors.As(err, &permanent) {
		err = permanent.Unwrap()
	}

	var historyErr *Error
	if errors.As(err, &historyErr) || ctx.Err() != nil {
		return nil, err
	}

	// Only a Retry-After that outlasted every attempt ends up here.
	return nil, &Error{Code: CodeAPIError, Message: "the GitHub API kept asking to retry later", Err: err}
}

// requestPage performs a single request for one page.
func requestPage(ctx context.Context, client *http.Client, endpoint, token string) ([]week, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, backoff.Permanent(&Error{Code: CodeAPIError, Message: "unable to prepare the request", Err: err})
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "StarAudit")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{Code: CodeAPIError, Message: "unable to reach the GitHub API", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	return readPage(resp, token != "")
}

// readPage turns a response into weeks, or into an error that tells the
// retry loop whether another attempt can help.
func readPage(resp *http.Response, authenticated bool) ([]week, error) {
	switch {
	case resp.StatusCode == http.StatusOK:
		var weeks []week
		if err := json.NewDecoder(resp.Body).Decode(&weeks); err != nil {
			return nil, backoff.Permanent(&Error{Code: CodeAPIError, Message: "unreadable star history from the GitHub API", Err: err})
		}

		return weeks, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, backoff.Permanent(&Error{Code: CodeNotFound, Message: "repository not found, or the token cannot see it"})
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, backoff.Permanent(&Error{Code: CodeUnauthorized, Message: "GitHub rejected the credentials, check GITHUB_TOKEN"})
	case isRateLimit(resp.StatusCode) && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return nil, backoff.Permanent(rateLimitError(resp.Header.Get("X-RateLimit-Reset"), authenticated))
	case isRateLimit(resp.StatusCode) && resp.Header.Get("Retry-After") != "":
		seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil || seconds < 0 {
			return nil, backoff.Permanent(&Error{
				Code:    CodeAPIError,
				Message: fmt.Sprintf("invalid Retry-After header %q", resp.Header.Get("Retry-After")),
			})
		}

		return nil, backoff.RetryAfter(seconds)
	case resp.StatusCode >= http.StatusInternalServerError:
		return nil, &Error{Code: CodeAPIError, Message: fmt.Sprintf("GitHub API error (status %s)", resp.Status)}
	default:
		return nil, backoff.Permanent(&Error{
			Code:    CodeAPIError,
			Message: fmt.Sprintf("unexpected response from the GitHub API (status %s)", resp.Status),
		})
	}
}

// isRateLimit reports whether a status code can carry a rate limit.
func isRateLimit(status int) bool {
	return status == http.StatusForbidden || status == http.StatusTooManyRequests
}

// rateLimitError explains an exhausted primary rate limit. CI jobs should not
// sleep until the reset, so this is never retried.
func rateLimitError(reset string, authenticated bool) *Error {
	message := "GitHub API rate limit exceeded"

	if seconds, err := strconv.ParseInt(reset, 10, 64); err == nil {
		message += ", it resets at " + time.Unix(seconds, 0).UTC().Format("15:04 UTC")
	}

	if !authenticated {
		message += "; set GITHUB_TOKEN for a higher limit"
	}

	return &Error{Code: CodeRateLimited, Message: message}
}

// validate rejects a week that does not match the documented response.
func (w week) validate() error {
	if w.Week <= 0 {
		return fmt.Errorf("week %d has an invalid start", w.Week)
	}

	if len(w.Days) != 7 {
		return fmt.Errorf("week %d has %d days instead of 7", w.Week, len(w.Days))
	}

	sum := 0

	for _, count := range w.Days {
		if count < 0 {
			return fmt.Errorf("week %d has a negative count", w.Week)
		}

		sum += count
	}

	if sum != w.Total {
		return fmt.Errorf("week %d has a total of %d, but its days add up to %d", w.Week, w.Total, sum)
	}

	return nil
}

// parseWeeks turns the weeks of every page into one daily series. Days after
// now are dropped, since the newest week also covers days to come.
func parseWeeks(weeks []week, now time.Time) (Series, error) {
	for _, w := range weeks {
		if err := w.validate(); err != nil {
			return Series{}, &Error{Code: CodeAPIError, Message: "unexpected star history from the GitHub API", Err: err}
		}
	}

	if len(weeks) == 0 {
		return Series{}, nil
	}

	sorted := slices.Clone(weeks)
	slices.SortFunc(sorted, func(a, b week) int { return cmp.Compare(a.Week, b.Week) })

	series := Series{
		Start: time.Unix(sorted[0].Week, 0).UTC(),
		Days:  make([]int, 0, 7*len(sorted)),
	}

	for _, w := range sorted {
		for i, count := range w.Days {
			if time.Unix(w.Week, 0).Add(time.Duration(i) * day).After(now) {
				return series, nil
			}

			series.Days = append(series.Days, count)
		}
	}

	return series, nil
}
```

- [ ] **Step 4: Run the tests and the linter**

Run: `go test -race -count=1 ./pkg/history/ && golangci-lint run ./pkg/history/...`
Expected: `ok` and `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add pkg/history/fetch.go pkg/history/fetch_test.go
git commit -m "feat(history): fetch star history from the GitHub REST API"
```

---

### Task 4: Render the analysis as text and JSON

**Files:**
- Create: `pkg/history/render.go`
- Test: `pkg/history/render_test.go`

**Interfaces:**
- Consumes: `Result`, `Metrics`, `Reason`, `Thresholds`, `Verdict` constants, `percent`, `formatDay`, `burstWindowDays`, `tailWindowDays` (Task 2); error codes (Task 1).
- Produces: `RenderText(w io.Writer, repository string, result Result) error`; `RenderJSON(w io.Writer, repository string, analyzedAt time.Time, result Result) error`; `RenderErrorJSON(w io.Writer, repository, code, message string) error`.

- [ ] **Step 1: Write the failing tests**

Create `pkg/history/render_test.go`:

```go
package history

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orcaLikeResult is the result for a young repository whose stars all came
// in one burst.
func orcaLikeResult() Result {
	return Result{
		Verdict: VerdictReview,
		Reasons: []Reason{
			{Code: ReasonBurst, Message: "100% of stars arrived between 2026-08-31 and 2026-09-13"},
			{Code: ReasonShortHistory, Message: "only 0 of 60 days after the burst exist yet"},
		},
		Metrics: Metrics{
			TotalStars:       239,
			HistoryDays:      22,
			BurstShare:       1,
			BurstStart:       time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC),
			BurstEnd:         time.Date(2026, time.September, 13, 0, 0, 0, 0, time.UTC),
			TailRatio:        0,
			TailDaysObserved: 0,
			PeakShare:        45.0 / 239,
			PeakDay:          time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC),
		},
		Thresholds: DefaultThresholds(),
	}
}

func TestRenderJSON(t *testing.T) {
	var out bytes.Buffer

	err := RenderJSON(&out, "owner/repo", time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC), orcaLikeResult())

	require.NoError(t, err)
	assert.JSONEq(t, `{
		"schema_version": 1,
		"repository": "owner/repo",
		"analyzed_at": "2026-09-13T12:00:00Z",
		"verdict": "review",
		"reasons": [
			{"code": "burst", "message": "100% of stars arrived between 2026-08-31 and 2026-09-13"},
			{"code": "short_history", "message": "only 0 of 60 days after the burst exist yet"}
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
	}`, out.String())
}

func TestRenderJSONWithoutStars(t *testing.T) {
	var out bytes.Buffer

	result := Result{Verdict: VerdictReview, Metrics: Metrics{HistoryDays: 30}, Thresholds: DefaultThresholds()}

	require.NoError(t, RenderJSON(&out, "owner/repo", time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC), result))

	assert.JSONEq(t, `{
		"schema_version": 1,
		"repository": "owner/repo",
		"analyzed_at": "2026-09-13T12:00:00Z",
		"verdict": "review",
		"reasons": [],
		"metrics": {
			"total_stars": 0,
			"history_days": 30,
			"burst_share": 0,
			"tail_ratio": 0,
			"tail_days_observed": 0,
			"peak_share": 0
		},
		"thresholds": {
			"min_stars": 50,
			"burst_review": 0.4,
			"burst_suspicious": 0.7,
			"tail_suspicious": 0.1,
			"peak_review": 0.15
		}
	}`, out.String())
}

func TestRenderErrorJSON(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, RenderErrorJSON(&out, "owner/repo", CodeNotFound, "repository not found"))

	assert.JSONEq(t, `{
		"schema_version": 1,
		"repository": "owner/repo",
		"error": {"code": "not_found", "message": "repository not found"}
	}`, out.String())
}

func TestRenderText(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, RenderText(&out, "owner/repo", orcaLikeResult()))

	text := out.String()
	assert.Contains(t, text, "Star history of owner/repo: 239 stars over 22 days")
	assert.Contains(t, text, "Busiest 14 days:    100%   (2026-08-31 to 2026-09-13)")
	assert.Contains(t, text, "After the burst:    0% of the burst, 0 of 60 days observed")
	assert.Contains(t, text, "Busiest day:        18.8%  (2026-09-02)")
	assert.Contains(t, text, "Verdict: REVIEW")
	assert.Contains(t, text, "  - burst: 100% of stars arrived between 2026-08-31 and 2026-09-13")
	assert.Contains(t, text, "  - short_history: only 0 of 60 days after the burst exist yet")
}

func TestRenderTextVerdicts(t *testing.T) {
	tests := map[Verdict]string{
		VerdictPass:       "Verdict: PASS",
		VerdictReview:     "Verdict: REVIEW",
		VerdictSuspicious: "Verdict: SUSPICIOUS",
	}

	for verdict, expected := range tests {
		t.Run(string(verdict), func(t *testing.T) {
			var out bytes.Buffer

			require.NoError(t, RenderText(&out, "owner/repo", Result{Verdict: verdict}))

			assert.Contains(t, out.String(), expected)
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./pkg/history/`
Expected: FAIL with `undefined: RenderJSON`, `undefined: RenderErrorJSON` and `undefined: RenderText`.

- [ ] **Step 3: Write the implementation**

Create `pkg/history/render.go`:

```go
package history

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/Ullaakut/disgo/style"
)

// schemaVersion is increased only for incompatible changes to the JSON output.
const schemaVersion = 1

type jsonMetrics struct {
	TotalStars       int     `json:"total_stars"`
	HistoryDays      int     `json:"history_days"`
	BurstShare       float64 `json:"burst_share"`
	BurstStart       string  `json:"burst_start,omitempty"`
	BurstEnd         string  `json:"burst_end,omitempty"`
	TailRatio        float64 `json:"tail_ratio"`
	TailDaysObserved int     `json:"tail_days_observed"`
	PeakShare        float64 `json:"peak_share"`
	PeakDay          string  `json:"peak_day,omitempty"`
}

type jsonReport struct {
	SchemaVersion int         `json:"schema_version"`
	Repository    string      `json:"repository"`
	AnalyzedAt    string      `json:"analyzed_at"`
	Verdict       Verdict     `json:"verdict"`
	Reasons       []Reason    `json:"reasons"`
	Metrics       jsonMetrics `json:"metrics"`
	Thresholds    Thresholds  `json:"thresholds"`
}

type jsonError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type jsonErrorReport struct {
	SchemaVersion int       `json:"schema_version"`
	Repository    string    `json:"repository"`
	Error         jsonError `json:"error"`
}

// RenderText writes the human-readable report.
func RenderText(w io.Writer, repository string, result Result) error {
	m := result.Metrics

	var b strings.Builder

	fmt.Fprintf(&b, "Star history of %s: %d stars over %d days\n\n", repository, m.TotalStars, m.HistoryDays)

	if m.TotalStars > 0 {
		fmt.Fprintf(&b, "Busiest %d days:    %-7s(%s to %s)\n",
			burstWindowDays, percent(m.BurstShare), formatDay(m.BurstStart), formatDay(m.BurstEnd))
		fmt.Fprintf(&b, "After the burst:    %s of the burst, %d of %d days observed\n",
			percent(m.TailRatio), m.TailDaysObserved, tailWindowDays)
		fmt.Fprintf(&b, "Busiest day:        %-7s(%s)\n", percent(m.PeakShare), formatDay(m.PeakDay))
	}

	fmt.Fprintf(&b, "\n%s\n", verdictLine(result.Verdict))

	for _, reason := range result.Reasons {
		fmt.Fprintf(&b, "  - %s: %s\n", reason.Code, reason.Message)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("unable to write the report: %w", err)
	}

	return nil
}

// RenderJSON writes the result as the JSON document described in the README.
// Shares and ratios are rounded to three decimals.
func RenderJSON(w io.Writer, repository string, analyzedAt time.Time, result Result) error {
	m := result.Metrics

	reasons := result.Reasons
	if reasons == nil {
		reasons = []Reason{}
	}

	return encode(w, jsonReport{
		SchemaVersion: schemaVersion,
		Repository:    repository,
		AnalyzedAt:    analyzedAt.UTC().Format(time.RFC3339),
		Verdict:       result.Verdict,
		Reasons:       reasons,
		Metrics: jsonMetrics{
			TotalStars:       m.TotalStars,
			HistoryDays:      m.HistoryDays,
			BurstShare:       round3(m.BurstShare),
			BurstStart:       optionalDay(m.BurstStart),
			BurstEnd:         optionalDay(m.BurstEnd),
			TailRatio:        round3(m.TailRatio),
			TailDaysObserved: m.TailDaysObserved,
			PeakShare:        round3(m.PeakShare),
			PeakDay:          optionalDay(m.PeakDay),
		},
		Thresholds: result.Thresholds,
	})
}

// RenderErrorJSON writes a failure as JSON, so that callers using --json
// always receive a document.
func RenderErrorJSON(w io.Writer, repository, code, message string) error {
	return encode(w, jsonErrorReport{
		SchemaVersion: schemaVersion,
		Repository:    repository,
		Error:         jsonError{Code: code, Message: message},
	})
}

func encode(w io.Writer, document any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(document); err != nil {
		return fmt.Errorf("unable to write the JSON output: %w", err)
	}

	return nil
}

func verdictLine(verdict Verdict) string {
	switch verdict {
	case VerdictSuspicious:
		return style.Failure(style.SymbolCross, " Verdict: SUSPICIOUS")
	case VerdictReview:
		return style.Important("⚠ Verdict: REVIEW")
	default:
		return style.Success(style.SymbolCheck, " Verdict: PASS")
	}
}

func round3(value float64) float64 {
	return math.Round(value*1000) / 1000
}

func optionalDay(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return formatDay(t)
}
```

- [ ] **Step 4: Run the tests and the linter**

Run: `go test -race -count=1 ./pkg/history/ && golangci-lint run ./pkg/history/...`
Expected: `ok` and `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add pkg/history/render.go pkg/history/render_test.go
git commit -m "feat(history): render burst analysis as text and JSON"
```

---

### Task 5: Make the star history analysis the default mode

**Files:**
- Modify: `main.go` (replace the whole file; `detectFakeStars` is unchanged)
- Test: `main_test.go` (create)

**Interfaces:**
- Consumes: `history.Fetch`, `history.Analyze`, `history.DefaultThresholds`, `history.Thresholds` and `Validate`, `history.RenderText`, `history.RenderJSON`, `history.RenderErrorJSON`, `history.Error`, `history.CodeAPIError`, `history.Verdict*` constants (Tasks 1 to 4).
- Produces: the CLI contract of the spec: flags `--trust`, `--json`, `--min-stars`, `--burst-review`, `--burst-suspicious`, `--tail-suspicious`, `--peak-review`; exit codes 0, 1, 2, 3; `exitCode(history.Verdict) int`.

Decisions recorded in the spec under "Decisions added during planning": `--json` together with `--trust` is rejected with `invalid_arguments`; a cancelled run (Ctrl-C, SIGTERM) prints no JSON document.

- [ ] **Step 1: Write the failing test**

Create `main_test.go`:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stn1slv/staraudit/pkg/history"
)

func TestExitCode(t *testing.T) {
	assert.Equal(t, 0, exitCode(history.VerdictPass))
	assert.Equal(t, 2, exitCode(history.VerdictReview))
	assert.Equal(t, 3, exitCode(history.VerdictSuspicious))
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 .`
Expected: FAIL with `undefined: exitCode`.

- [ ] **Step 3: Write the implementation**

Replace `main.go` with:

```go
// Package main is the entry point for the staraudit CLI tool.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Ullaakut/disgo"
	"github.com/Ullaakut/disgo/style"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	staraudit_context "github.com/stn1slv/staraudit/pkg/context"
	"github.com/stn1slv/staraudit/pkg/gql"
	"github.com/stn1slv/staraudit/pkg/history"
	"github.com/stn1slv/staraudit/pkg/signature"
	"github.com/stn1slv/staraudit/pkg/trust"
)

// Exit codes. The verdict codes let CI jobs act on the result without
// parsing the output.
const (
	exitPass       = 0
	exitError      = 1
	exitReview     = 2
	exitSuspicious = 3
)

// codeInvalidArguments is the JSON error code for unusable command line input.
const codeInvalidArguments = "invalid_arguments"

func parseArguments() error {
	viper.SetEnvPrefix("staraudit")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	defaults := history.DefaultThresholds()

	pflag.BoolP("verbose", "v", false, "Show extra logs (including comparative reports)")
	pflag.Bool("trust", false, "Run the per-stargazer trust scan instead of the star history analysis (only works on repositories you administer)")
	pflag.Bool("json", false, "Print the star history analysis as JSON on stdout")
	pflag.Int("min-stars", defaults.MinStars, "Fewer stars than this gives a review verdict")
	pflag.Float64("burst-review", defaults.BurstReview, "Share of stars in the busiest 14 days that gives a review verdict")
	pflag.Float64("burst-suspicious", defaults.BurstSuspicious, "Share of stars in the busiest 14 days that can give a suspicious verdict")
	pflag.Float64("tail-suspicious", defaults.TailSuspicious, "Stars in the 60 days after the burst, as a share of the burst, below which the burst is suspicious")
	pflag.Float64("peak-review", defaults.PeakReview, "Share of stars on a single day that gives a review verdict")
	pflag.BoolP("all", "a", false, "Trust scan: scan every stargazer of the repository (overrides --stars)")
	pflag.UintP("stars", "s", 1000, "Trust scan: maximum amount of stars to scan")
	pflag.StringP("cachedir", "c", "./data", "Trust scan: directory in which to store cache data")

	viper.AutomaticEnv()

	pflag.Parse()

	err := viper.BindPFlags(pflag.CommandLine)
	if err != nil {
		return err
	}

	if len(pflag.Args()) == 0 {
		disgo.Infoln("Missing required repository argument")
		pflag.Usage()
		os.Exit(0)
	}

	return nil
}

func main() {
	code, err := run()
	if err != nil {
		disgo.Errorln(style.Failure(style.SymbolCross, " ", err))
	}

	os.Exit(code)
}

// run executes the command and returns its exit code. It is separated from
// main so that deferred cleanups run before the process exits.
func run() (int, error) {
	err := parseArguments()
	if err != nil {
		return exitError, err
	}

	jsonOutput := viper.GetBool("json")

	// With --json, stdout carries only the JSON document, so logs go to stderr.
	terminalOptions := []func(*disgo.Terminal){
		disgo.WithColors(!jsonOutput),
		disgo.WithDebug(viper.GetBool("verbose")),
	}
	if jsonOutput {
		terminalOptions = append(terminalOptions, disgo.WithDefaultOutput(os.Stderr))
	}

	disgo.SetTerminalOptions(terminalOptions...)

	// Handle OS signals for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repository := pflag.Arg(0)

	// Split repository into repo owner & repo name.
	repoInfo := strings.Split(repository, "/")
	if len(repoInfo) != 2 {
		return exitError, reportFailure(jsonOutput, repository, codeInvalidArguments,
			fmt.Errorf("invalid repository %q: should be of the form \"repoOwner/repoName\"", repository))
	}

	token := os.Getenv("GITHUB_TOKEN")

	if !viper.GetBool("trust") {
		return analyzeHistory(ctx, repository, repoInfo[0], repoInfo[1], token, jsonOutput)
	}

	if jsonOutput {
		return exitError, reportFailure(jsonOutput, repository, codeInvalidArguments,
			errors.New("--json is only available for the star history analysis, not with --trust"))
	}

	if token == "" {
		return exitError, errors.New("missing github access token. Please set one in your GITHUB_TOKEN environment variable, with \"repo\" rights")
	}

	starauditCtx := &staraudit_context.Context{
		RepoOwner:          repoInfo[0],
		RepoName:           repoInfo[1],
		GithubToken:        token,
		Stars:              viper.GetUint("stars"),
		CacheDirectoryPath: viper.GetString("cachedir"),
		ScanAll:            viper.GetBool("all"),
		Verbose:            viper.GetBool("verbose"),
	}

	if err := detectFakeStars(ctx, starauditCtx); err != nil {
		return exitError, err
	}

	return exitPass, nil
}

// analyzeHistory runs the star history analysis and maps its verdict to an
// exit code.
func analyzeHistory(ctx context.Context, repository, owner, name, token string, jsonOutput bool) (int, error) {
	thresholds := history.Thresholds{
		MinStars:        viper.GetInt("min-stars"),
		BurstReview:     viper.GetFloat64("burst-review"),
		BurstSuspicious: viper.GetFloat64("burst-suspicious"),
		TailSuspicious:  viper.GetFloat64("tail-suspicious"),
		PeakReview:      viper.GetFloat64("peak-review"),
	}

	if err := thresholds.Validate(); err != nil {
		return exitError, reportFailure(jsonOutput, repository, codeInvalidArguments, err)
	}

	if token == "" {
		disgo.Infoln(style.Important("GITHUB_TOKEN is not set, so GitHub allows only 60 requests per hour"))
	}

	series, err := history.Fetch(ctx, owner, name, token)
	if err != nil {
		// An interrupted run has nothing to report.
		if errors.Is(err, context.Canceled) {
			return exitError, err
		}

		code := history.CodeAPIError

		var historyErr *history.Error
		if errors.As(err, &historyErr) {
			code = historyErr.Code
		}

		return exitError, reportFailure(jsonOutput, repository, code, fmt.Errorf("unable to fetch the star history: %w", err))
	}

	result := history.Analyze(series, thresholds)

	if jsonOutput {
		err = history.RenderJSON(os.Stdout, repository, time.Now(), result)
	} else {
		err = history.RenderText(os.Stdout, repository, result)
	}

	if err != nil {
		return exitError, err
	}

	return exitCode(result.Verdict), nil
}

// reportFailure also writes the error as a JSON document on stdout when
// --json is set, and returns it for main to print on stderr.
func reportFailure(jsonOutput bool, repository, code string, err error) error {
	if !jsonOutput {
		return err
	}

	if renderErr := history.RenderErrorJSON(os.Stdout, repository, code, err.Error()); renderErr != nil {
		return errors.Join(err, renderErr)
	}

	return err
}

// exitCode maps a verdict to the exit code documented in the README.
func exitCode(verdict history.Verdict) int {
	switch verdict {
	case history.VerdictSuspicious:
		return exitSuspicious
	case history.VerdictReview:
		return exitReview
	default:
		return exitPass
	}
}

func detectFakeStars(ctx context.Context, starauditCtx *staraudit_context.Context) error {
	disgo.Infof("Beginning fetching process for repository %s/%s\n", starauditCtx.RepoOwner, starauditCtx.RepoName)

	cursors, totalUsers, err := gql.FetchStargazers(ctx, starauditCtx)
	if err != nil {
		return fmt.Errorf("failed to query stargazer data: %w", err)
	}

	if totalUsers < 1000 {
		disgo.Infoln(style.Important("This repository appears to have a low amount of stargazers. Trust calculations might not be accurate."))
	}

	// For now, we only fetch contributions since 2013. It will be configurable later on
	// once the algorithm is more accurate and more data has been fetched.
	if !starauditCtx.ScanAll && totalUsers > starauditCtx.Stars {
		disgo.Infof("Fetching contributions for %d users up to year %d\n", starauditCtx.Stars, 2013)
	} else {
		disgo.Infof("Fetching contributions for %d users up to year %d\n", totalUsers, 2013)
	}

	users, err := gql.FetchContributions(ctx, starauditCtx, cursors, 2013)
	if err != nil {
		return fmt.Errorf("failed to query stargazer data: %w", err)
	}

	report, err := trust.Compute(ctx, starauditCtx, users)
	if err != nil {
		return fmt.Errorf("unable to compute trust report: %w", err)
	}

	trust.Render(report, true)

	// Uploading the report is opt-in, and failing to upload it is not fatal:
	// the report has already been computed and rendered locally.
	if signature.Enabled() {
		err = signature.SendReport(ctx, starauditCtx, report)
		if err != nil {
			disgo.Errorln(style.Important("Unable to send trust report to the staraudit server: ", err))
		}
	} else {
		disgo.Debugln("No signing key configured, skipping report upload.")
	}

	disgo.Infof("\n%s Analysis successful. %d users computed.\n", style.Success(style.SymbolCheck), len(users))

	badgeEndpoint := url.QueryEscape(fmt.Sprintf("https://astronomer.ullaakut.eu/shields?owner=%s&name=%s", starauditCtx.RepoOwner, starauditCtx.RepoName))

	disgo.Infof("GitHub badge available at https://img.shields.io/endpoint.svg?url=%s\n", badgeEndpoint)

	return nil
}
```

- [ ] **Step 4: Run the full test suite and the linter**

Run: `make test && make lint`
Expected: every package `ok` (including `github.com/stn1slv/staraudit`), `0 issues.`

- [ ] **Step 5: Smoke test against the real API**

Run each command and compare with the expected result. The OrcaReplay verdict is `review` until about 2026-11-12, when 60 days after its burst exist; after that date it is expected to become `suspicious` with `no_tail`.

```bash
go build -o staraudit .
export GITHUB_TOKEN=$(gh auth token)

./staraudit Continuum-AI-Corp/OrcaReplay; echo "exit=$?"
# Expected: "Verdict: REVIEW" with burst, short_history and peak_day; exit=2

./staraudit --json Continuum-AI-Corp/OrcaReplay 2>/dev/null | jq -c '{verdict, codes: [.reasons[].code]}'
# Expected: {"verdict":"review","codes":["burst","short_history","peak_day"]}

./staraudit charmbracelet/vhs; echo "exit=$?"
# Expected: "Verdict: PASS"; exit=0

./staraudit --json --burst-review 2 cli/cli 2>/dev/null; echo "exit=$?"
# Expected: error.code "invalid_arguments"; exit=1

./staraudit --json stn1slv/does-not-exist-xyz 2>/dev/null; echo "exit=$?"
# Expected: error.code "not_found"; exit=1

./staraudit --json --trust cli/cli 2>/dev/null | jq -r .error.code
# Expected: invalid_arguments

env -u GITHUB_TOKEN ./staraudit zed-industries/zed; echo "exit=$?"
# Expected: a notice about the 60 requests per hour limit, "Verdict: PASS"; exit=0

./staraudit --trust stn1slv/staraudit; echo "exit=$?"
# Expected: the trust report of 1.x ("Analysis successful"); exit=0
```

`staraudit` is listed in `.gitignore`, so the binary is not committed.

- [ ] **Step 6: Commit**

```bash
git add main.go main_test.go
git commit -m "feat!: make star history analysis the default mode" -m "BREAKING CHANGE: the per-stargazer trust scan now needs --trust."
```

---

### Task 6: Document StarAudit 2.0.0

**Files:**
- Modify: `README.md` (replace the whole file)
- Modify: `CLAUDE.md` (three lines)

**Interfaces:**
- Consumes: the CLI contract from Task 5.
- Produces: user documentation only.

- [ ] **Step 1: Replace `README.md`**

Replace `README.md` with the content below. Keep every paragraph and list item on one line.

````markdown
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
````

- [ ] **Step 2: Update `CLAUDE.md`**

In the "Project Structure" list, replace the `main.go` line and add a `pkg/history/` line after it:

```markdown
- `main.go`: Entry point. Parses CLI arguments and runs the star history analysis, or the trust scan with `--trust`.
- `pkg/history/`: Star history analysis (the default mode): fetches `/stargazers/history` over REST, scores star bursts, and renders text and JSON.
```

In "Go (Golang)", replace the `**API**` line with:

```markdown
- **API**: GitHub REST star history endpoint via `pkg/history`; GitHub GraphQL API via `pkg/gql` (trust scan only).
```

In "Security", replace the `**GitHub Token**` line with:

```markdown
- **GitHub Token**: `GITHUB_TOKEN` is optional for the star history analysis and required for `--trust`, where it must belong to an admin or collaborator of the repository (GitHub restricts stargazer lists since 2026-06-30).
```

- [ ] **Step 3: Check the documents**

Run: `grep -n "$(printf '\342\200\224')" README.md CLAUDE.md; echo "em dashes: $?"`
Expected: no lines printed, and `em dashes: 1`.

Run: `make test && make lint`
Expected: every package `ok`, `0 issues.`

- [ ] **Step 4: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "docs: document star history analysis for 2.0.0"
```

---

## After the plan

- Pushing the branch, opening the PR and tagging `v2.0.0` are left to the user. The `fix/stargazer-restriction` PR must be merged first, because this branch is based on it.
- Known risk outside this plan: `.github/workflows/build.yml` and `release.yml` set `go-version: '1.25'`, while `go.mod` requires `go 1.26.0`. Check that the release job builds before tagging `v2.0.0`.
