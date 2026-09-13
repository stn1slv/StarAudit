package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stn1slv/staraudit/pkg/history"
)

type fetchFunc = func(ctx context.Context, owner, repo, token string) (history.Series, error)

// document holds the fields of the JSON output that the tests check.
type document struct {
	Repository string `json:"repository"`
	Verdict    string `json:"verdict"`
	Error      struct {
		Code string `json:"code"`
	} `json:"error"`
}

// result is what one run of the command produced.
type result struct {
	code   int
	stdout string
	stderr string
}

// runWith runs the command with the star history replaced by fetch.
func runWith(t *testing.T, fetch fetchFunc, args ...string) result {
	t.Helper()

	t.Setenv("GITHUB_TOKEN", "")

	original := fetchHistory
	fetchHistory = fetch

	t.Cleanup(func() { fetchHistory = original })

	var stdout, stderr bytes.Buffer

	code := run(args, &stdout, &stderr)

	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// notCalled fails the test if the star history is requested.
func notCalled(t *testing.T) fetchFunc {
	return func(context.Context, string, string, string) (history.Series, error) {
		t.Error("the star history must not be fetched")
		return history.Series{}, nil
	}
}

// returning answers every request with the same series and error.
func returning(series history.Series, err error) fetchFunc {
	return func(context.Context, string, string, string) (history.Series, error) {
		return series, err
	}
}

// decode parses stdout, which must hold exactly one JSON document.
func decode(t *testing.T, stdout string) document {
	t.Helper()

	var doc document
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc), "stdout: %q", stdout)

	return doc
}

// seriesOf builds a series from runs of days that got the same amount of
// stars, given as pairs of (days, stars).
func seriesOf(runs ...[2]int) history.Series {
	series := history.Series{Start: time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)}

	for _, r := range runs {
		for range r[0] {
			series.Days = append(series.Days, r[1])
		}
	}

	return series
}

func TestExitCode(t *testing.T) {
	assert.Equal(t, 0, exitCode(history.VerdictPass))
	assert.Equal(t, 2, exitCode(history.VerdictReview))
	assert.Equal(t, 3, exitCode(history.VerdictSuspicious))
}

func TestRunReportsTheVerdict(t *testing.T) {
	tests := map[string]struct {
		series          history.Series
		expectedCode    int
		expectedVerdict string
	}{
		"pass": {
			series:          seriesOf([2]int{365, 3}),
			expectedCode:    exitPass,
			expectedVerdict: "pass",
		},
		"review": {
			series:          seriesOf([2]int{8, 0}, [2]int{13, 10}, [2]int{1, 50}),
			expectedCode:    exitReview,
			expectedVerdict: "review",
		},
		"suspicious": {
			series:          seriesOf([2]int{30, 0}, [2]int{14, 20}, [2]int{60, 0}),
			expectedCode:    exitSuspicious,
			expectedVerdict: "suspicious",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			res := runWith(t, returning(test.series, nil), "--json", "owner/repo")

			assert.Equal(t, test.expectedCode, res.code)

			doc := decode(t, res.stdout)
			assert.Equal(t, "owner/repo", doc.Repository)
			assert.Equal(t, test.expectedVerdict, doc.Verdict)
		})
	}
}

// TestRunRejectsInvalidArguments guards the exit code contract: an argument
// error must exit with 1, never with a verdict code, and with --json it must
// still produce a JSON document.
func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := map[string][]string{
		"missing repository":             {"--json"},
		"repository without a slash":     {"--json", "owner"},
		"repository without an owner":    {"--json", "/repo"},
		"repository without a name":      {"--json", "owner/"},
		"repository with two slashes":    {"--json", "owner/repo/extra"},
		"more than one repository":       {"--json", "owner/repo", "other/repo"},
		"unknown flag":                   {"--json", "--no-such-flag", "0.5", "owner/repo"},
		"unknown flag before --json":     {"--no-such-flag", "--json", "owner/repo"},
		"unknown flag before --json=1":   {"--no-such-flag", "--json=true", "owner/repo"},
		"value that does not parse":      {"--json", "--burst-review=abc", "owner/repo"},
		"threshold that is not a number": {"--json", "--peak-review=NaN", "owner/repo"},
		"threshold out of range":         {"--json", "--burst-review=2", "owner/repo"},
		"json with the trust scan":       {"--json", "--trust", "owner/repo"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			res := runWith(t, notCalled(t), args...)

			assert.Equal(t, exitError, res.code)
			assert.Equal(t, codeInvalidArguments, decode(t, res.stdout).Error.Code)
		})
	}
}

// TestRunRejectsUnparsableEnvironmentValues covers variables that the viper
// getters used to turn into a zero value without an error, which switched
// rules off or selected the wrong mode.
func TestRunRejectsUnparsableEnvironmentValues(t *testing.T) {
	tests := map[string][2]string{
		"threshold":             {"STARAUDIT_TAIL_SUSPICIOUS", "abc"},
		"minimum with decimals": {"STARAUDIT_MIN_STARS", "50.9"},
		"trust switch":          {"STARAUDIT_TRUST", "yes"},
		"verbose switch":        {"STARAUDIT_VERBOSE", "maybe"},
		"star limit":            {"STARAUDIT_STARS", "abc"},
	}

	for name, variable := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(variable[0], variable[1])

			res := runWith(t, notCalled(t), "--json", "owner/repo")

			assert.Equal(t, exitError, res.code)
			assert.Equal(t, codeInvalidArguments, decode(t, res.stdout).Error.Code)
		})
	}
}

func TestRunRejectsUnparsableJSONSwitch(t *testing.T) {
	t.Setenv("STARAUDIT_JSON", "abc")

	res := runWith(t, notCalled(t), "owner/repo")

	assert.Equal(t, exitError, res.code)
	assert.Contains(t, res.stderr, "json")
}

func TestRunReadsSwitchesFromTheEnvironment(t *testing.T) {
	t.Setenv("STARAUDIT_JSON", "true")

	res := runWith(t, returning(seriesOf([2]int{365, 3}), nil), "owner/repo")

	assert.Equal(t, exitPass, res.code)
	assert.Equal(t, "pass", decode(t, res.stdout).Verdict)
}

func TestRunWithoutRepositoryInTextMode(t *testing.T) {
	res := runWith(t, notCalled(t))

	assert.Equal(t, exitError, res.code, "a missing repository must not look like a pass")
}

func TestRunHelp(t *testing.T) {
	res := runWith(t, notCalled(t), "--help")

	assert.Equal(t, exitPass, res.code)
}

func TestRunReportsFetchErrors(t *testing.T) {
	notFound := &history.Error{Code: history.CodeNotFound, Message: "repository not found"}

	res := runWith(t, returning(history.Series{}, notFound), "--json", "owner/repo")

	assert.Equal(t, exitError, res.code)
	assert.Equal(t, history.CodeNotFound, decode(t, res.stdout).Error.Code)
}

// TestRunTextReportWithoutTerminal covers a report redirected to a file or a
// CI log, which must not hold color escape codes.
func TestRunTextReportWithoutTerminal(t *testing.T) {
	res := runWith(t, returning(seriesOf([2]int{8, 0}, [2]int{13, 10}, [2]int{1, 50}), nil), "owner/repo")

	assert.Equal(t, exitReview, res.code)
	assert.Contains(t, res.stdout, "Verdict: REVIEW")
	assert.NotContains(t, res.stdout, "\x1b[")
}

func TestRunTrustScanWithoutToken(t *testing.T) {
	res := runWith(t, notCalled(t), "--trust", "owner/repo")

	assert.Equal(t, exitError, res.code)
	assert.Contains(t, res.stderr, "admin or collaborator")
}
