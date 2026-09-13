package gql

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Ullaakut/disgo"
	"github.com/Ullaakut/disgo/style"
	"github.com/cenkalti/backoff/v5"
	"github.com/stn1slv/staraudit/v2/pkg/context"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/sync/errgroup"
)

var (
	// githubGraphQLURL is the endpoint every request in this package targets.
	// It is a variable so that tests can point it at a stub server.
	githubGraphQLURL = "https://api.github.com/graphql"

	// retryInterval is the constant delay between two attempts.
	retryInterval = 15 * time.Second
)

const (
	defaultTimeout = 30 * time.Second

	// maxAttempts is how many times a request is tried before giving up.
	maxAttempts = 20

	// rateLimitThreshold is the amount of remaining API points below which
	// requests start being spaced out.
	rateLimitThreshold = 10

	// firstPageCursor stands for the first page of stargazers, which is
	// fetched without a cursor.
	firstPageCursor = "firstpage"
)

// ErrStargazersRestricted is returned when GitHub withholds the stargazer list
// of a repository. Since 2026-06-30, GitHub only shows it to the admins and
// collaborators of the repository, whatever the scopes of the token.
var ErrStargazersRestricted = errors.New("GitHub only shows the stargazers of a repository to its admins and collaborators, so GITHUB_TOKEN must belong to one of them")

// errForbidden marks a GraphQL error that retrying cannot fix.
var errForbidden = errors.New("the GitHub API denied access")

// errNoStargazerCount marks an empty first page without a star count, as
// cached by versions that did not query it. Such a page cannot be told apart
// from a restricted list.
var errNoStargazerCount = errors.New("empty first page without a star count")

// blacklistedUsers contains the list of users that can't be
// fetched from the GitHub API. When one of these users is found
// in a list request, he must be skipped when fetching user contributions
// or staraudit will be stuck due to constant API timeouts.
var blacklistedUsers = []string{
	// "jstrachan", // has been fixed.
}

// limiter is shared by the stargazer and contribution fetches so that what
// one phase learns about the remaining quota applies to the next.
var limiter rateLimiter

// rateLimiter derives a delay between requests from the rate limit
// information returned by the GitHub API.
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	nextSlot time.Time
}

// update recomputes the interval from a rate limit payload. It is cleared
// again as soon as the remaining quota recovers.
func (r *rateLimiter) update(limit, remaining int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if limit <= 0 || remaining > rateLimitThreshold {
		r.interval = 0
		r.nextSlot = time.Time{}

		return
	}

	disgo.Debugln("Rate limit reached, slowing down requests")

	r.interval = time.Hour / time.Duration(limit)
}

// reserve claims the next request slot and reports how long to wait for it.
// Slots are handed out one interval apart, so concurrent workers queue up
// behind each other instead of all sleeping and then firing at once.
func (r *rateLimiter) reserve(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.interval <= 0 {
		return 0
	}

	if r.nextSlot.Before(now) {
		r.nextSlot = now
	}

	wait := r.nextSlot.Sub(now)
	r.nextSlot = r.nextSlot.Add(r.interval)

	return wait
}

// wait blocks until this caller's slot comes up, or until ctx is cancelled.
func (r *rateLimiter) wait(ctx stdcontext.Context) error {
	wait := r.reserve(time.Now())
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// FetchStargazers fetches the list of cursors to iterate upon to
// fetch stargazer contributions.
func FetchStargazers(ctx stdcontext.Context, starauditCtx *context.Context) (cursors []string, totalUsers uint, err error) {
	var (
		stargazers []stargazers
		lastCursor string
	)

	// The star limit is only meaningful when scanning a subset of the
	// stargazers, so it is neither validated nor rounded under --all.
	if !starauditCtx.ScanAll {
		if starauditCtx.Stars < uint(contribPagination) {
			return nil, 0, fmt.Errorf("unable to compute less stars than the amount fetched per page. Please set stars to at least %d", contribPagination)
		}

		// Round amount of stars to get according to pagination.
		if starauditCtx.Stars%contribPagination != 0 {
			starauditCtx.Stars -= starauditCtx.Stars % contribPagination
			disgo.Infoln(style.Important("Rounding amount of stars to fetch to ", starauditCtx.Stars, " in order to match pagination"))
		}
	}

	// Inject constants in request body.
	requestBody := buildRequestBody(starauditCtx, fetchUsersRequest, listPagination)
	client := &http.Client{Timeout: defaultTimeout}

	disgo.StartStep("Pre-fetching all stargazers")

	defer disgo.EndStep()

	for {
		paginatedRequestBody := requestBody
		if lastCursor != "" {
			paginatedRequestBody = strings.Replace(
				paginatedRequestBody,
				fmt.Sprintf("stargazers(first:%d){", listPagination),
				fmt.Sprintf("stargazers(first:%d,after:\\\"%s\\\"){", listPagination, lastCursor),
				1)
		}

		response, err := fetchOrCache(ctx, starauditCtx, client, paginatedRequestBody, listFilePagination(lastCursor), lastCursor == "")
		if err != nil {
			return nil, 0, disgo.FailStepf("unable to fetch stargazers: %w", err)
		}

		stargazers = append(stargazers, response.Repository.Stargazers)

		totalUsers += uint(len(response.Repository.Stargazers.Users))

		if len(response.Repository.Stargazers.Users) < listPagination {
			break
		}

		nextCursor := response.Repository.Stargazers.Meta.cursor()
		if nextCursor == "" {
			// Without a cursor the next iteration would request the very
			// first page again, forever.
			break
		}

		lastCursor = nextCursor
	}

	cursors = getCursors(starauditCtx, stargazers, totalUsers)

	return cursors, totalUsers, nil
}

// FetchContributions fetches the contribution data of a list of stargazers.
// starauditCtx contains the scanned context of the staraudit command.
// untilYear is the year until which to scan for contributions.
func FetchContributions(ctx stdcontext.Context, starauditCtx *context.Context, cursors []string, untilYear int) ([]User, error) {
	currentYear := time.Now().UTC().Year()
	if untilYear > currentYear {
		return nil, fmt.Errorf("unable to scan until %d, which is in the future", untilYear)
	}

	// Every page is fetched once per year in the scanned range.
	yearsPerPage := currentYear - untilYear + 1

	requestBody := buildRequestBody(starauditCtx, fetchContributionsRequest, contribPagination)
	client := &http.Client{Timeout: defaultTimeout}

	// Every page pointed at by a cursor, plus the first page, which is
	// where the earliest stargazers live and which needs no cursor.
	totalPages := len(cursors) + 1

	progress, bar := setupProgressBar(totalPages * yearsPerPage)

	// Deferred calls run last-in-first-out, so the bar is aborted before
	// Wait is reached. Without that, an early return would leave the bar
	// unfinished and Wait would block forever.
	defer progress.Wait()
	defer bar.Abort(true)

	// Results are collected per page rather than appended as responses come
	// back. Pages complete in an arbitrary order, and the trust report splits
	// the users by position to compare early adopters against the rest, so
	// arrival order would make that split - and the score - non-deterministic.
	pageUsers := make([][]User, totalPages)

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(10)

	// Iterate on pages of user contributions, following the cursors generated
	// in fetchStargazers.
	for page := 1; page <= totalPages; page++ {
		currentCursor := getCursor(cursors, page)

		g.Go(func() error {
			var users []User

			// Get all user contributions for each year.
			for yearToFetch := currentYear; yearToFetch >= untilYear; yearToFetch-- {
				response, err := fetchYearlyContributions(gCtx, starauditCtx, client, requestBody, currentCursor, yearToFetch)
				if err != nil {
					return err
				}

				users = updateUsers(users, *response, yearToFetch)

				bar.IncrBy(1)
			}

			// Each goroutine owns its own index, so no locking is needed.
			pageUsers[page-1] = users

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Flatten in page order. A login can show up on two pages if the
	// repository gained or lost stars mid-scan, in which case the earliest
	// occurrence wins so that the same user is never counted twice.
	users := make([]User, 0, totalPages*contribPagination)
	seen := make(map[string]bool, totalPages*contribPagination)

	for _, page := range pageUsers {
		for _, user := range page {
			if seen[user.Login] {
				continue
			}

			seen[user.Login] = true
			users = append(users, user)
		}
	}

	return users, nil
}

func fetchYearlyContributions(
	ctx stdcontext.Context,
	starauditCtx *context.Context,
	client *http.Client,
	requestBody string,
	currentCursor string,
	currentYear int,
) (*listStargazersResponse, error) {
	// If this isn't the first page, inject the cursor value.
	paginatedRequestBody := requestBody
	if currentCursor != firstPageCursor {
		paginatedRequestBody = strings.Replace(
			paginatedRequestBody,
			fmt.Sprintf("stargazers(first:%d){", contribPagination),
			fmt.Sprintf("stargazers(first:%d,after:\\\"%s\\\"){", contribPagination, currentCursor),
			1,
		)
	}

	// Inject the dates corresponding to the year we're scanning, into the request body.
	// AddDate is used rather than a fixed 365 day offset so that leap years
	// do not lose their last day.
	from := time.Date(currentYear, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(1, 0, 0).Add(-time.Second)

	yearlyRequestBody := strings.Replace(paginatedRequestBody, "$dateFrom", from.Format(iso8601Format), 1)
	yearlyRequestBody = strings.Replace(yearlyRequestBody, "$dateTo", to.Format(iso8601Format), 1)

	response, err := fetchOrCache(ctx, starauditCtx, client, yearlyRequestBody, contribFilePagination(currentCursor, currentYear), currentCursor == firstPageCursor)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user contributions at cursor %s: %w", currentCursor, err)
	}

	return response, nil
}

func buildRequestBody(starauditCtx *context.Context, baseRequest string, pagination int) string {
	// Inject constant values into request body.
	requestBody := strings.Replace(baseRequest, "$repoOwner", starauditCtx.RepoOwner, 1)
	requestBody = strings.Replace(requestBody, "$repoName", starauditCtx.RepoName, 1)
	requestBody = strings.Replace(requestBody, "$pagination", fmt.Sprint(pagination), 1)

	// Remove all `\n` so that it's valid JSON. Remove all spaces.
	requestBody = strings.ReplaceAll(requestBody, "\t", "")
	requestBody = strings.ReplaceAll(requestBody, " ", "")
	requestBody = strings.ReplaceAll(requestBody, "\n", " ")

	return requestBody
}

// Return the appropriate cursors to be used by the fetchContributions function
// according to the value of ${contribPagination}. Also makes sure not to include
// any page of users containing blacklisted individuals.
func getCursors(starauditCtx *context.Context, sg []stargazers, totalUsers uint) []string {
	var (
		skip      bool
		iteration uint
		cursors   []string
	)

	if totalUsers == 0 {
		return nil
	}

	for _, stargazers := range sg {
		var currentPageUsers int

		for _, user := range stargazers.Users {
			if isBlacklisted(user.Login) {
				skip = true
			}

			// If this is the last user of the whole set, even if it's exactly at the
			// end of the current page, we don't need its cursor, because there is nothing
			// to get after his profile.
			if iteration == totalUsers-1 {
				break
			}

			// Iterate through list of stargazers, and add a cursor for every
			// ${contribPagination} users, unless one of the users within the current
			// page is blacklisted, in which case we skip the whole page.
			// The API can return fewer edges than nodes, so the cursor for
			// this user is not guaranteed to exist.
			if iteration%contribPagination == contribPagination-1 && currentPageUsers < len(stargazers.Meta) {
				if !skip {
					cursors = append(cursors, stargazers.Meta[currentPageUsers].Cursor)
				} else {
					skip = false
				}
			}

			iteration++
			currentPageUsers++
		}
	}

	// The GitHub API returns stargazers oldest first, so the earliest
	// adopters are at the *start* of the cursor list. The very first page of
	// users needs no cursor, hence one fewer cursor than pages.
	const beginCursorAmount = 200/contribPagination - 1

	// Anything that fits in the first block is scanned in full.
	if len(cursors) <= beginCursorAmount {
		disgo.Infof("All %d stargazers will be scanned\n", totalUsers)
		return cursors
	}

	beginCursors := cursors[:beginCursorAmount]
	remainingCursors := cursors[beginCursorAmount:]

	disgo.Infof("Selecting 200 first stargazers out of %d\n", totalUsers)

	selectedCursors := make([]string, 0, len(cursors))
	selectedCursors = append(selectedCursors, beginCursors...)

	if starauditCtx.ScanAll || totalUsers < starauditCtx.Stars {
		disgo.Infof("Selecting all %d remaining stargazers\n", totalUsers-200)
		return append(selectedCursors, remainingCursors...)
	}

	// totalCursorAmount is the total amount of pages to fetch, one of which
	// is the cursor-less first page. The subtraction is clamped because a
	// star limit below 200 asks for fewer pages than the first block covers.
	totalCursorAmount := int(starauditCtx.Stars / contribPagination)
	endCursorAmount := min(max(totalCursorAmount-beginCursorAmount-1, 0), len(remainingCursors))

	disgo.Infof("Selecting %d random stargazers out of %d\n", endCursorAmount*contribPagination, totalUsers)

	return append(selectedCursors, pickRandom(remainingCursors, endCursorAmount)...)
}

// pickRandom returns `amount` distinct elements picked at random from s, or
// all of them if fewer are available. The relative order of s is preserved,
// so callers can rely on the result still being in stargazer order.
func pickRandom[T any](s []T, amount int) []T {
	amount = min(amount, len(s))
	if amount <= 0 {
		return nil
	}

	indices := make([]int, len(s))
	for i := range indices {
		indices[i] = i
	}

	// Make the random non-deterministic.
	random := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()))) // #nosec G404
	random.Shuffle(len(indices), func(i, j int) {
		indices[i], indices[j] = indices[j], indices[i]
	})

	indices = indices[:amount]
	slices.Sort(indices)

	picked := make([]T, 0, amount)
	for _, idx := range indices {
		picked = append(picked, s[idx])
	}

	return picked
}

// isBlacklisted checks if a user is blacklisted.
func isBlacklisted(user string) bool {
	for _, blacklistedUser := range blacklistedUsers {
		if user == blacklistedUser {
			return true
		}
	}

	return false
}

// setupProgressBar sets the progress bar properly according to
// the expected amount of requests, each of which increments it by one.
func setupProgressBar(requests int) (*mpb.Progress, *mpb.Bar) {
	p := mpb.New(mpb.WithWidth(64))

	bar := p.AddBar(int64(requests),
		mpb.BarRemoveOnComplete(),
		mpb.AppendDecorators(
			decor.Name("ETA: "),
			decor.AverageETA(decor.ET_STYLE_GO),
			decor.Name(" Elapsed: "),
			decor.Elapsed(decor.ET_STYLE_GO),
			decor.Name(" Progress: "),
			decor.Percentage()),
	)

	return p, bar
}

// getCursor returns the current cursor for the given page, depending on the
// order the cursors are being read in.
// The selection always starts with the earliest stargazers, so page one is
// always the cursor-less first page and page N uses cursors[N-2].
func getCursor(cursors []string, page int) string {
	if page > 1 {
		return cursors[page-2]
	}

	return firstPageCursor
}

// fetchResult carries what a single successful API call produced.
type fetchResult struct {
	response *listStargazersResponse
	body     []byte
}

// doRequest performs a GraphQL query, retrying transient failures.
//
// The request is rebuilt on every attempt: http.Client.Do consumes the
// request body, so re-issuing the same *http.Request would send an empty
// payload and every retry would be rejected by the transport.
func doRequest(ctx stdcontext.Context, client *http.Client, token, body string) (*listStargazersResponse, []byte, error) {
	result, err := backoff.Retry(ctx, func() (fetchResult, error) {
		// Space out requests when the API quota is running low.
		if err := limiter.wait(ctx); err != nil {
			return fetchResult{}, backoff.Permanent(err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubGraphQLURL, bytes.NewBufferString(body))
		if err != nil {
			return fetchResult{}, backoff.Permanent(fmt.Errorf("unable to prepare request: %w", err))
		}

		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("User-Agent", "StarAudit")

		resp, err := client.Do(req) // nolint:bodyclose // parseResponse closes the body.
		if err != nil {
			return fetchResult{}, fmt.Errorf("unable to reach the GitHub API: %w", err)
		}

		response, responseBody, parseErr := parseResponse(resp)

		// Rejected credentials will not start working on the next attempt,
		// so there is no point in spending the whole retry budget on them.
		if resp.StatusCode == http.StatusUnauthorized {
			return fetchResult{}, backoff.Permanent(
				fmt.Errorf("the GitHub API rejected the credentials (status %s), check GITHUB_TOKEN", resp.Status))
		}

		// Missing permissions will not be granted by the next attempt either.
		if errors.Is(parseErr, errForbidden) {
			return fetchResult{}, backoff.Permanent(parseErr)
		}

		if parseErr != nil {
			return fetchResult{}, fmt.Errorf("unusable response from the GitHub API (status %s): %w", resp.Status, parseErr)
		}

		if response.ErrorMessage != "" {
			return fetchResult{}, fmt.Errorf("github API error (status %s): %s", resp.Status, response.ErrorMessage)
		}

		limiter.update(response.RateLimit.Limit, response.RateLimit.Remaining)

		return fetchResult{response: response, body: responseBody}, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(retryInterval)), backoff.WithMaxTries(maxAttempts))
	if err != nil {
		return nil, nil, err
	}

	return result.response, result.body, nil
}

// fetchOrCache returns the response to the given request body, reading it from
// the local cache when possible and querying the GitHub API otherwise.
//
// A cache entry that cannot be parsed is discarded and refetched rather than
// failing the whole scan, since an interrupted run can leave one behind.
//
// When firstPage is set, the response is also checked for a restricted
// stargazer list. Such a page is never cached, and a cached first page that
// fails the check is refetched.
func fetchOrCache(
	ctx stdcontext.Context,
	starauditCtx *context.Context,
	client *http.Client,
	requestBody string,
	cacheKey string,
	firstPage bool,
) (*listStargazersResponse, error) {
	cached, filename, err := getCache(starauditCtx, cacheKey) // nolint:bodyclose // parseResponse closes the body.
	if err != nil {
		return nil, err
	}

	if cached != nil {
		response, _, err := parseResponse(cached)
		if err == nil && firstPage {
			err = checkFirstPage(response)
		}

		if err == nil {
			return response, nil
		}

		disgo.Debugf("Discarding unusable cache entry %q: %v\n", filename, err)

		if err := os.Remove(filename); err != nil {
			return nil, fmt.Errorf("unable to remove corrupt cache entry %q: %w", filename, err)
		}
	}

	response, responseBody, err := doRequest(ctx, client, starauditCtx.GithubToken, requestBody)
	if err != nil {
		return nil, err
	}

	if firstPage {
		if err := checkFirstPage(response); err != nil {
			return nil, err
		}
	}

	if err := putCache(starauditCtx, cacheKey, responseBody); err != nil {
		return nil, err
	}

	return response, nil
}

// parseResponse parses a response from the GitHub API and converts it in the appropriate data model.
// It also returns the response body if it was read successfully.
func parseResponse(resp *http.Response) (*listStargazersResponse, []byte, error) {
	if resp == nil {
		return nil, nil, errors.New("unable to parse nil response")
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to read response body: %w", err)
	}

	var response listStargazersResponse
	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		disgo.Errorf("Unable to parse body: %s\n", responseBody)
		return nil, responseBody, fmt.Errorf("unable to unmarshal stargazers: %w", err)
	}

	if len(response.Errors) != 0 {
		for _, gqlErr := range response.Errors {
			if gqlErr.Type == "FORBIDDEN" {
				return nil, responseBody, forbiddenError(gqlErr)
			}
		}

		return nil, responseBody, fmt.Errorf("error while querying user data: %v [%s:%s]", response.Errors[0].Message, response.Errors[0].Extensions.ArgumentName, response.Errors[0].Extensions.Name)
	}

	return &response, responseBody, nil
}

// forbiddenError converts a FORBIDDEN GraphQL error into one that is not
// retried. Only a refusal of the stargazer list is blamed on the restriction,
// since GitHub also answers FORBIDDEN for SAML enforcement or IP allow lists.
func forbiddenError(gqlErr gqlError) error {
	err := fmt.Errorf("%w: %s", errForbidden, gqlErr.Message)

	if len(gqlErr.Path) >= 2 && gqlErr.Path[0] == "repository" && gqlErr.Path[1] == "stargazers" {
		return fmt.Errorf("%w (%w)", ErrStargazersRestricted, err)
	}

	return err
}

// checkFirstPage rejects a first page that does not hold the stargazers of the
// repository. Classic tokens get a restricted list as an empty page with no
// error, while the repository still reports its star count.
func checkFirstPage(response *listStargazersResponse) error {
	if len(response.Repository.Stargazers.Users) != 0 {
		return nil
	}

	count := response.Repository.StargazerCount

	switch {
	case count == nil:
		return errNoStargazerCount
	case *count > 0:
		return fmt.Errorf("%w (the repository has %d stars, but the list is empty)", ErrStargazersRestricted, *count)
	default:
		return nil
	}
}

// updateUsers updates a slice of user from the data in a list stargazer response.
// It also sets their yearly contributions accordingly.
func updateUsers(users []User, response listStargazersResponse, year int) []User {
	// Each incoming user is matched individually. Treating a single match as
	// proof that the whole page is already known would discard every other
	// user in it.
	knownUsers := make(map[string]int, len(users))
	for idx := range users {
		knownUsers[users[idx].Login] = idx
	}

	for _, u := range response.Repository.Stargazers.Users {
		idx, known := knownUsers[u.Login]
		if !known {
			u.YearlyContributions = map[int]int{
				year: u.Contributions.ContributionCalendar.TotalContributions + u.Contributions.PrivateContributions,
			}

			users = append(users, u)
			knownUsers[u.Login] = len(users) - 1

			continue
		}

		users[idx].YearlyContributions[year] = u.Contributions.ContributionCalendar.TotalContributions + u.Contributions.PrivateContributions

		users[idx].Contributions.PrivateContributions += u.Contributions.PrivateContributions
		users[idx].Contributions.TotalCommitContributions += u.Contributions.TotalCommitContributions
		users[idx].Contributions.TotalIssueContributions += u.Contributions.TotalIssueContributions
		users[idx].Contributions.TotalPullRequestContributions += u.Contributions.TotalPullRequestContributions
		users[idx].Contributions.TotalPullRequestReviewContributions += u.Contributions.TotalPullRequestReviewContributions
		users[idx].Contributions.TotalRepositoryContributions += u.Contributions.TotalRepositoryContributions
	}

	return users
}
