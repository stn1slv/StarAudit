package history

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
)

var (
	// apiBaseURL is a variable so that tests can point it at a stub server.
	apiBaseURL = "https://api.github.com"

	// retryInitialInterval is the first backoff delay. Tests shorten it.
	retryInitialInterval = time.Second
)

const (
	// apiVersion is the REST API version that introduced the endpoint.
	apiVersion = "2026-03-10"

	// perPage is the largest page size the endpoint accepts.
	perPage = 30

	// maxPages is the page limit of the endpoint.
	maxPages = 100

	// maxAttempts is how many times a page is requested before giving up.
	maxAttempts = 5

	// maxRetryAfterSeconds is the longest Retry-After worth waiting for. A
	// longer wait would hold a CI job, so it is reported as a rate limit.
	maxRetryAfterSeconds = 60

	// maxRetryWait bounds the total time spent waiting for one page.
	maxRetryWait = 2 * time.Minute

	// maxErrorBody bounds how much of an error response is read.
	maxErrorBody = 64 << 10

	requestTimeout = 30 * time.Second

	day = 24 * time.Hour

	weekSeconds = int64(7 * day / time.Second)
)

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

// Fetch returns the daily star history of a repository. The token is
// optional: without it, the unauthenticated rate limit applies.
func Fetch(ctx context.Context, owner, repo, token string) (Series, error) {
	client := &http.Client{Timeout: requestTimeout}

	var weeks []week

	for page := 1; page <= maxPages; page++ {
		pageWeeks, err := fetchPage(ctx, client, owner, repo, token, page)
		if err != nil {
			return Series{}, err
		}

		if len(pageWeeks) == 0 {
			break
		}

		// Checked page by page, so that a broken response is reported at
		// once instead of after up to maxPages requests.
		if err := validateWeeks(pageWeeks); err != nil {
			return Series{}, err
		}

		weeks = append(weeks, pageWeeks...)
	}

	return parseWeeks(weeks, time.Now())
}

// fetchPage requests one page, retrying server errors and honouring
// Retry-After.
func fetchPage(ctx context.Context, client *http.Client, owner, repo, token string, page int) ([]week, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/stargazers/history?per_page=%d&page=%d",
		apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), perPage, page)

	backOff := backoff.NewExponentialBackOff()
	backOff.InitialInterval = retryInitialInterval

	weeks, err := backoff.Retry(ctx, func() ([]week, error) {
		return requestPage(ctx, client, endpoint, token)
	}, backoff.WithBackOff(backOff), backoff.WithMaxTries(maxAttempts), backoff.WithMaxElapsedTime(maxRetryWait))
	if err == nil {
		return weeks, nil
	}

	// The last attempt is returned as is, even when it was permanent.
	var permanent *backoff.PermanentError
	if errors.As(err, &permanent) {
		err = permanent.Unwrap()
	}

	var historyErr *Error
	if errors.As(err, &historyErr) || ctx.Err() != nil {
		return nil, err
	}

	// Only Retry-After waits that outlasted the attempts or maxRetryWait end
	// up here, which is a secondary rate limit.
	return nil, &Error{Code: CodeRateLimited, Message: "GitHub API secondary rate limit: it kept asking to retry later", Err: err}
}

// requestPage performs a single request for one page.
func requestPage(ctx context.Context, client *http.Client, endpoint, token string) ([]week, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, backoff.Permanent(&Error{Code: CodeAPIError, Message: "unable to prepare the request", Err: err})
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "StarAudit")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{Code: CodeAPIError, Message: "unable to reach the GitHub API", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	return readPage(resp, token != "")
}

// readPage turns a response into weeks, or into an error that tells the
// retry loop whether another attempt can help.
func readPage(resp *http.Response, authenticated bool) ([]week, error) {
	switch {
	case resp.StatusCode == http.StatusOK:
		var weeks []week
		if err := json.NewDecoder(resp.Body).Decode(&weeks); err != nil {
			return nil, backoff.Permanent(&Error{Code: CodeAPIError, Message: "unreadable star history from the GitHub API", Err: err})
		}

		return weeks, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, backoff.Permanent(&Error{Code: CodeNotFound, Message: "repository not found, or the token cannot see it"})
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, backoff.Permanent(&Error{Code: CodeUnauthorized, Message: "GitHub rejected the credentials, check GITHUB_TOKEN"})
	case isRateLimit(resp.StatusCode) && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return nil, backoff.Permanent(rateLimitError(resp.Header.Get("X-RateLimit-Reset"), authenticated))
	case isRateLimit(resp.StatusCode) && resp.Header.Get("Retry-After") != "":
		seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil || seconds < 0 {
			return nil, backoff.Permanent(&Error{
				Code:    CodeAPIError,
				Message: fmt.Sprintf("invalid Retry-After header %q", resp.Header.Get("Retry-After")),
			})
		}

		if seconds > maxRetryAfterSeconds {
			return nil, backoff.Permanent(&Error{
				Code:    CodeRateLimited,
				Message: fmt.Sprintf("GitHub asked to wait %d seconds before the next request (secondary rate limit)", seconds),
			})
		}

		return nil, backoff.RetryAfter(seconds)
	case isRateLimit(resp.StatusCode):
		return nil, backoff.Permanent(refusedError(resp))
	case resp.StatusCode == http.StatusAccepted:
		// GitHub's statistics endpoints answer 202 while they compute the
		// data, so the next attempt can find it ready.
		return nil, &Error{Code: CodeAPIError, Message: "GitHub is still computing the star history, try again later"}
	case resp.StatusCode >= http.StatusInternalServerError:
		return nil, &Error{Code: CodeAPIError, Message: fmt.Sprintf("GitHub API error (status %s)", resp.Status)}
	default:
		return nil, backoff.Permanent(&Error{
			Code:    CodeAPIError,
			Message: fmt.Sprintf("unexpected response from the GitHub API (status %s)", resp.Status),
		})
	}
}

// isRateLimit reports whether a status code can carry a rate limit.
func isRateLimit(status int) bool {
	return status == http.StatusForbidden || status == http.StatusTooManyRequests
}

// rateLimitError explains an exhausted primary rate limit. CI jobs should not
// sleep until the reset, so this is never retried.
func rateLimitError(reset string, authenticated bool) *Error {
	message := "GitHub API rate limit exceeded"

	if seconds, err := strconv.ParseInt(reset, 10, 64); err == nil {
		message += ", it resets at " + time.Unix(seconds, 0).UTC().Format("15:04 UTC")
	}

	if !authenticated {
		message += "; set GITHUB_TOKEN for a higher limit"
	}

	return &Error{Code: CodeRateLimited, Message: message}
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

// refusedError explains a 403 or 429 without rate limit headers. GitHub
// answers a secondary rate limit this way too, and says so in the message.
func refusedError(resp *http.Response) *Error {
	message := apiMessage(resp)

	if resp.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(message), "rate limit") {
		return &Error{Code: CodeRateLimited, Message: joinMessage("GitHub API secondary rate limit exceeded", message)}
	}

	return &Error{
		Code:    CodeAPIError,
		Message: joinMessage(fmt.Sprintf("the GitHub API refused the request (status %s)", resp.Status), message),
	}
}

// apiMessage returns the message of a GitHub error response. It only serves
// to explain the error, so a body that cannot be read gives an empty message.
func apiMessage(resp *http.Response) string {
	var body struct {
		Message string `json:"message"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body); err != nil {
		return ""
	}

	return body.Message
}

// joinMessage appends the message GitHub gave, when there is one.
func joinMessage(summary, detail string) string {
	if detail == "" {
		return summary
	}

	return summary + ": " + detail
}

// validateWeeks checks every week of a response.
func validateWeeks(weeks []week) error {
	for _, w := range weeks {
		if err := w.validate(); err != nil {
			return unexpectedHistory(err)
		}
	}

	return nil
}

// unexpectedHistory reports a response that does not match the documented
// format.
func unexpectedHistory(err error) *Error {
	return &Error{Code: CodeAPIError, Message: "unexpected star history from the GitHub API", Err: err}
}

// parseWeeks turns the weeks of every page into one daily series. Days after
// now are dropped, since the newest week also covers days to come.
func parseWeeks(weeks []week, now time.Time) (Series, error) {
	if err := validateWeeks(weeks); err != nil {
		return Series{}, err
	}

	if len(weeks) == 0 {
		return Series{}, nil
	}

	sorted := slices.Clone(weeks)
	slices.SortStableFunc(sorted, func(a, b week) int { return cmp.Compare(a.Week, b.Week) })

	// Pages are counted from the newest week, so a week that starts during
	// paging shifts them by one and the same week comes back twice. Such a
	// repeat is identical; one with different counts means broken data.
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Week == sorted[i-1].Week && !slices.Equal(sorted[i].Days, sorted[i-1].Days) {
			return Series{}, unexpectedHistory(fmt.Errorf("the week of %s came back twice with different counts",
				formatDay(time.Unix(sorted[i].Week, 0))))
		}
	}

	sorted = slices.CompactFunc(sorted, func(a, b week) bool { return a.Week == b.Week })

	// Days are dated by their position, so a missing week would shift every
	// later date.
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Week-sorted[i-1].Week != weekSeconds {
			return Series{}, unexpectedHistory(fmt.Errorf("weeks %d and %d are not 7 days apart", sorted[i-1].Week, sorted[i].Week))
		}
	}

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
