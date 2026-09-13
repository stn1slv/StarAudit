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
