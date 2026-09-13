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
