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
