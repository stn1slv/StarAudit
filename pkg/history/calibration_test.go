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
