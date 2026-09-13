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

// runWith runs the command with the star history replaced by fetch, and
// returns the exit code and what was written to stdout.
func runWith(t *testing.T, fetch fetchFunc, args ...string) (int, string) {
	t.Helper()

	t.Setenv("GITHUB_TOKEN", "")

	original := fetchHistory
	fetchHistory = fetch

	t.Cleanup(func() { fetchHistory = original })

	var stdout, stderr bytes.Buffer

	code := run(args, &stdout, &stderr)

	return code, stdout.String()
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
			code, stdout := runWith(t, returning(test.series, nil), "--json", "owner/repo")

			assert.Equal(t, test.expectedCode, code)

			doc := decode(t, stdout)
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
		"unknown flag":                   {"--json", "--no-such-flag", "0.5", "owner/repo"},
		"value that does not parse":      {"--json", "--burst-review=abc", "owner/repo"},
		"threshold that is not a number": {"--json", "--peak-review=NaN", "owner/repo"},
		"threshold out of range":         {"--json", "--burst-review=2", "owner/repo"},
		"json with the trust scan":       {"--json", "--trust", "owner/repo"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			code, stdout := runWith(t, notCalled(t), args...)

			assert.Equal(t, exitError, code)
			assert.Equal(t, codeInvalidArguments, decode(t, stdout).Error.Code)
		})
	}
}

// TestRunRejectsUnparsableEnvironmentValues covers a variable that used to
// turn into 0 without an error, which switched the suspicious rule off.
func TestRunRejectsUnparsableEnvironmentValues(t *testing.T) {
	t.Setenv("STARAUDIT_TAIL_SUSPICIOUS", "abc")

	code, stdout := runWith(t, notCalled(t), "--json", "owner/repo")

	assert.Equal(t, exitError, code)
	assert.Equal(t, codeInvalidArguments, decode(t, stdout).Error.Code)
}

func TestRunWithoutRepositoryInTextMode(t *testing.T) {
	code, _ := runWith(t, notCalled(t))

	assert.Equal(t, exitError, code, "a missing repository must not look like a pass")
}

func TestRunHelp(t *testing.T) {
	code, _ := runWith(t, notCalled(t), "--help")

	assert.Equal(t, exitPass, code)
}

func TestRunReportsFetchErrors(t *testing.T) {
	notFound := &history.Error{Code: history.CodeNotFound, Message: "repository not found"}

	code, stdout := runWith(t, returning(history.Series{}, notFound), "--json", "owner/repo")

	assert.Equal(t, exitError, code)
	assert.Equal(t, history.CodeNotFound, decode(t, stdout).Error.Code)
}
