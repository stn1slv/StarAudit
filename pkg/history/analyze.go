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
		// Written as "not inside", because NaN fails every comparison and
		// would slip through a "below 0 or above 1" check.
		if !(share.value >= 0 && share.value <= 1) {
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
