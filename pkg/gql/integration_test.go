package gql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	staraudit_context "github.com/stn1slv/staraudit/pkg/context"
)

// stubAPI points the package at a test server for the duration of the test.
func stubAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	originalURL, originalInterval := githubGraphQLURL, retryInterval
	githubGraphQLURL = server.URL
	// Keep retries instant so the suite does not wait out real backoff.
	retryInterval = time.Millisecond

	t.Cleanup(func() {
		githubGraphQLURL = originalURL
		retryInterval = originalInterval
	})

	// The limiter is package level state shared across tests.
	limiter.update(0, 0)
}

// stargazerPage renders a stargazer list response for users [start, end).
func stargazerPage(start, end int) string {
	type node struct {
		Login string `json:"login"`
	}
	type edge struct {
		Cursor string `json:"cursor"`
	}

	var nodes []node
	var edges []edge

	for i := start; i < end; i++ {
		nodes = append(nodes, node{Login: fmt.Sprintf("user-%d", i)})
		edges = append(edges, edge{Cursor: fmt.Sprintf("cursor-%d", i)})
	}

	body, err := json.Marshal(map[string]any{
		"data": map[string]any{
			"ratelimit":  map[string]int{"limit": 5000, "remaining": 4999},
			"repository": map[string]any{"stargazers": map[string]any{"nodes": nodes, "edges": edges}},
		},
	})
	if err != nil {
		panic(err)
	}

	return string(body)
}

func integrationContext(t *testing.T) *staraudit_context.Context {
	t.Helper()

	return &staraudit_context.Context{
		RepoOwner:          "stn1slv",
		RepoName:           "staraudit",
		GithubToken:        "test-token",
		CacheDirectoryPath: t.TempDir(),
		Stars:              1000,
	}
}

func TestFetchStargazersPaginates(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		page := requests.Add(1)

		// Two full pages, then a short one to end the pagination.
		switch page {
		case 1:
			_, _ = io.WriteString(w, stargazerPage(0, listPagination))
		case 2:
			_, _ = io.WriteString(w, stargazerPage(listPagination, 2*listPagination))
		default:
			_, _ = io.WriteString(w, stargazerPage(2*listPagination, 2*listPagination+30))
		}
	})

	starauditCtx := integrationContext(t)

	cursors, totalUsers, err := FetchStargazers(context.Background(), starauditCtx)

	require.NoError(t, err)
	assert.Equal(t, uint(230), totalUsers)
	assert.Equal(t, int32(3), requests.Load(), "pagination must stop on a short page")
	assert.NotEmpty(t, cursors)

	seen := make(map[string]bool, len(cursors))
	for _, cursor := range cursors {
		assert.False(t, seen[cursor], "cursor %q was selected twice", cursor)
		seen[cursor] = true
	}
}

// TestFetchStargazersRetriesWithAFreshBody covers the retry path. Reusing the
// same *http.Request made every attempt after the first send an empty body,
// so a single transient failure was fatal.
func TestFetchStargazersRetriesWithAFreshBody(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			// This is what a retried, already drained request looks like.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"empty request body"}`)

			return
		}

		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"message":"server error"}`)

			return
		}

		_, _ = io.WriteString(w, stargazerPage(0, 30))
	})

	_, totalUsers, err := FetchStargazers(context.Background(), integrationContext(t))

	require.NoError(t, err, "a transient failure must be retried successfully")
	assert.Equal(t, uint(30), totalUsers)
	assert.GreaterOrEqual(t, requests.Load(), int32(2), "the request must have been retried")
}

func TestFetchStargazersWithRejectedCredentials(t *testing.T) {
	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	})

	_, _, err := FetchStargazers(context.Background(), integrationContext(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "GITHUB_TOKEN", "the cause must name the credentials")
}

// TestFetchStargazersWithRestrictedList covers a repository the token holder
// does not administer. Since 2026-06-30 GitHub answers with an empty list and
// no error, which used to surface much later as a scan of 0 users.
func TestFetchStargazersWithRestrictedList(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"data":{"repository":{"stargazerCount":239,"stargazers":{"edges":[],"nodes":[]}}}}`)
	})

	starauditCtx := integrationContext(t)

	_, _, err := FetchStargazers(context.Background(), starauditCtx)
	require.ErrorIs(t, err, ErrStargazersRestricted)

	// A later run, possibly with a token that is allowed to see the list,
	// must not be served the empty page from the cache.
	_, _, err = FetchStargazers(context.Background(), starauditCtx)
	require.ErrorIs(t, err, ErrStargazersRestricted)
	assert.Equal(t, int32(2), requests.Load(), "the restricted page must not be cached")
}

// TestFetchStargazersWithForbiddenToken covers a fine-grained token, for which
// GitHub reports the restricted list as a FORBIDDEN error. It used to be
// retried for about five minutes before failing with an unclear message.
func TestFetchStargazersWithForbiddenToken(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"data":{"repository":{"stargazerCount":239,"stargazers":null}},`+
			`"errors":[{"type":"FORBIDDEN","path":["repository","stargazers"],"message":"Resource not accessible by personal access token"}]}`)
	})

	_, _, err := FetchStargazers(context.Background(), integrationContext(t))

	require.ErrorIs(t, err, ErrStargazersRestricted)
	assert.Contains(t, err.Error(), "Resource not accessible by personal access token", "the GitHub message must be kept")
	assert.Equal(t, int32(1), requests.Load(), "a permission error must not be retried")
}

// TestFetchStargazersWithForbiddenErrorNotFirst covers a response where the
// FORBIDDEN error is not the first one, which used to be retried again.
func TestFetchStargazersWithForbiddenErrorNotFirst(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"data":{"repository":{"stargazerCount":239,"stargazers":null}},"errors":[`+
			`{"type":"INTERNAL","message":"something else went wrong"},`+
			`{"type":"FORBIDDEN","path":["repository","stargazers"],"message":"Resource not accessible by personal access token"}]}`)
	})

	_, _, err := FetchStargazers(context.Background(), integrationContext(t))

	require.ErrorIs(t, err, ErrStargazersRestricted)
	assert.Equal(t, int32(1), requests.Load(), "a permission error must not be retried")
}

// TestFetchStargazersWithOtherForbiddenError covers a FORBIDDEN error that is
// not about the stargazer list, such as SAML enforcement. It must not be
// retried, but it must not blame the stargazer restriction either.
func TestFetchStargazersWithOtherForbiddenError(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"data":{"repository":null},`+
			`"errors":[{"type":"FORBIDDEN","path":["repository"],"message":"Resource protected by organization SAML enforcement"}]}`)
	})

	_, _, err := FetchStargazers(context.Background(), integrationContext(t))

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrStargazersRestricted)
	assert.Contains(t, err.Error(), "Resource protected by organization SAML enforcement")
	assert.Equal(t, int32(1), requests.Load(), "a permission error must not be retried")
}

// TestFetchStargazersIgnoresCacheWithoutStargazerCount covers an empty first
// page cached by a version that did not ask for stargazerCount. It cannot be
// told apart from a restricted list, so it must be fetched again.
func TestFetchStargazersIgnoresCacheWithoutStargazerCount(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, stargazerPage(0, 30))
	})

	starauditCtx := integrationContext(t)

	filename := cacheEntryFilename(starauditCtx, listFilePagination(""))
	require.NoError(t, os.MkdirAll(filepath.Dir(filename), 0o750))
	require.NoError(t, os.WriteFile(filename, []byte(`{"data":{"repository":{"stargazers":{"edges":[],"nodes":[]}}}}`), 0o600))

	_, totalUsers, err := FetchStargazers(context.Background(), starauditCtx)

	require.NoError(t, err)
	assert.Equal(t, uint(30), totalUsers)
	assert.Equal(t, int32(1), requests.Load(), "the old cache entry must have been refetched")
}

func TestFetchStargazersWithoutStars(t *testing.T) {
	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"repository":{"stargazerCount":0,"stargazers":{"edges":[],"nodes":[]}}}}`)
	})

	_, totalUsers, err := FetchStargazers(context.Background(), integrationContext(t))

	require.NoError(t, err, "a repository without stars is not a restricted one")
	assert.Zero(t, totalUsers)
}

func TestFetchStargazersServesFromCache(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, stargazerPage(0, 30))
	})

	starauditCtx := integrationContext(t)

	_, _, err := FetchStargazers(context.Background(), starauditCtx)
	require.NoError(t, err)
	require.Equal(t, int32(1), requests.Load())

	_, totalUsers, err := FetchStargazers(context.Background(), starauditCtx)
	require.NoError(t, err)

	assert.Equal(t, uint(30), totalUsers)
	assert.Equal(t, int32(1), requests.Load(), "the second run must be served from the cache")
}

// TestFetchStargazersRecoversFromCorruptCache covers a truncated cache entry,
// which used to abort the whole scan with an error that never mentioned the
// cache and could only be resolved by deleting the file by hand.
func TestFetchStargazersRecoversFromCorruptCache(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, stargazerPage(0, 30))
	})

	starauditCtx := integrationContext(t)

	_, _, err := FetchStargazers(context.Background(), starauditCtx)
	require.NoError(t, err)
	require.Equal(t, int32(1), requests.Load())

	// Truncate the cache entry, as an interrupted run would.
	filename := cacheEntryFilename(starauditCtx, listFilePagination(""))
	require.NoError(t, os.WriteFile(filename, []byte(`{"data":{"repos`), 0o600))

	_, totalUsers, err := FetchStargazers(context.Background(), starauditCtx)

	require.NoError(t, err, "a corrupt cache entry must be refetched, not fatal")
	assert.Equal(t, uint(30), totalUsers)
	assert.Equal(t, int32(2), requests.Load(), "the corrupt entry must have been refetched")
}

// contributionsStub serves each cursor its own distinct block of users, the
// way the real API paginates: "firstpage" is users 0..19, "cursor-19" is
// users 20..39, and so on.
func contributionsStub(t *testing.T) {
	t.Helper()

	cursorPattern := regexp.MustCompile(`after:\\"cursor-(\d+)\\"`)

	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// Reporting the failure must not call FailNow off the test
			// goroutine, so this answers with an error the client will surface.
			t.Errorf("unable to read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		start := 0
		if match := cursorPattern.FindSubmatch(body); match != nil {
			boundary, err := strconv.Atoi(string(match[1]))
			if err != nil {
				t.Errorf("unparseable cursor %q: %v", match[1], err)
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			start = boundary + 1
		}

		_, _ = io.WriteString(w, stargazerPage(start, start+contribPagination))
	})
}

// TestFetchContributionsPreservesStargazerOrder is the regression test for the
// comparative report: users must come back in stargazer order, not in the
// order the concurrent page requests happened to complete. The trust report
// splits users by position to compare early adopters against the rest, so
// arrival order would make the score non-deterministic.
func TestFetchContributionsPreservesStargazerOrder(t *testing.T) {
	contributionsStub(t)

	starauditCtx := integrationContext(t)
	starauditCtx.ScanAll = true

	// Cursors are in ascending stargazer order, as getCursors returns them.
	cursors := []string{"cursor-19", "cursor-39", "cursor-59", "cursor-79", "cursor-99"}

	// Run repeatedly: an ordering bug here is a race and would show up
	// intermittently rather than every time.
	for attempt := range 5 {
		users, err := FetchContributions(context.Background(), starauditCtx, cursors, 2024)
		require.NoError(t, err)

		logins := make([]string, 0, len(users))
		for _, user := range users {
			logins = append(logins, user.Login)
			assert.NotEmpty(t, user.YearlyContributions, "user %q has no yearly data", user.Login)
		}

		expected := make([]string, 0, (len(cursors)+1)*contribPagination)
		for i := range (len(cursors) + 1) * contribPagination {
			expected = append(expected, fmt.Sprintf("user-%d", i))
		}

		require.Equal(t, expected, logins, "attempt %d returned users out of stargazer order", attempt)
	}
}

// TestFetchContributionsReturnsOnError covers the error path that used to
// leave the progress bar unfinished, blocking the deferred Wait forever.
func TestFetchContributionsReturnsOnError(t *testing.T) {
	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	})

	starauditCtx := integrationContext(t)
	starauditCtx.ScanAll = true

	done := make(chan error, 1)
	go func() {
		_, err := FetchContributions(context.Background(), starauditCtx, []string{"cursor-19"}, 2025)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("FetchContributions did not return: the progress bar is blocking shutdown")
	}
}

// TestFetchContributionsWithForbiddenToken covers a fine-grained token that
// loses access to the list between the list step, served from the cache of an
// earlier run, and the contributions step.
func TestFetchContributionsWithForbiddenToken(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"data":{"repository":{"stargazerCount":239,"stargazers":null}},`+
			`"errors":[{"type":"FORBIDDEN","path":["repository","stargazers"],"message":"Resource not accessible by personal access token"}]}`)
	})

	_, err := FetchContributions(context.Background(), integrationContext(t), nil, time.Now().UTC().Year())

	require.ErrorIs(t, err, ErrStargazersRestricted)
	assert.Equal(t, int32(1), requests.Load(), "a permission error must not be retried")
}

// TestFetchContributionsWithRestrictedList covers the same situation with a
// classic token, which gets empty pages instead of an error. It used to end
// in a report computed on 0 users, with the empty pages cached for good.
func TestFetchContributionsWithRestrictedList(t *testing.T) {
	var requests atomic.Int32

	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"data":{"repository":{"stargazerCount":239,"stargazers":{"edges":[],"nodes":[]}}}}`)
	})

	starauditCtx := integrationContext(t)
	year := time.Now().UTC().Year()

	_, err := FetchContributions(context.Background(), starauditCtx, nil, year)
	require.ErrorIs(t, err, ErrStargazersRestricted)

	_, err = FetchContributions(context.Background(), starauditCtx, nil, year)
	require.ErrorIs(t, err, ErrStargazersRestricted)
	assert.Equal(t, int32(2), requests.Load(), "the restricted page must not be cached")
}

// TestFetchContributionsCancels covers Ctrl-C during a scan.
func TestFetchContributionsCancels(t *testing.T) {
	stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, stargazerPage(0, contribPagination))
	})

	starauditCtx := integrationContext(t)
	starauditCtx.ScanAll = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = FetchContributions(ctx, starauditCtx, []string{"cursor-19"}, 2013)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("FetchContributions ignored a cancelled context")
	}
}
