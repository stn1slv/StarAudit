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
