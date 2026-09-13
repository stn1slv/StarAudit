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
