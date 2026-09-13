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
