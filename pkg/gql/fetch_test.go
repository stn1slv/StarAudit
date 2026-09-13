package gql

import (
	"fmt"
	"testing"

	"github.com/stn1slv/staraudit/pkg/context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRequestBody(t *testing.T) {
	tests := map[string]struct {
		baseRequest string
		repoOwner   string
		repoName    string
		pagination  int

		expectedBody string
	}{
		"fetch users request": {
			baseRequest: fetchUsersRequest,
			repoOwner:   "ullaakut",
			repoName:    "cameradar",
			pagination:  42,

			expectedBody: `{"query":"{ rateLimit{ limit remaining } repository(owner:\"ullaakut\",name:\"cameradar\"){ stargazerCount stargazers(first:42){ edges{ cursor } nodes{ login } } } }"}`,
		},
		"fetch contributions request": {
			baseRequest: fetchContributionsRequest,
			repoOwner:   "ullaakut",
			repoName:    "camerattack",
			pagination:  84,

			expectedBody: `{"query":"{ rateLimit{ limit remaining } repository(owner:\"ullaakut\",name:\"camerattack\"){ stargazerCount stargazers(first:84){ edges{ cursor } nodes{ login createdAt contributionsCollection(from:\"$dateFrom\",to:\"$dateTo\"){ restrictedContributionsCount totalIssueContributions totalCommitContributions totalRepositoryContributions totalPullRequestContributions totalPullRequestReviewContributions contributionCalendar{ totalContributions } } } } } }"}`,
		},
	}

	for description, test := range tests {
		t.Run(description, func(t *testing.T) {
			t.Parallel()
			starauditCtx := &context.Context{
				RepoOwner: test.repoOwner,
				RepoName:  test.repoName,
			}

			requestBody := buildRequestBody(starauditCtx, test.baseRequest, test.pagination)

			assert.Equal(t, test.expectedBody, requestBody)
		})
	}
}

func TestGetCursors(t *testing.T) {
	sg := stargazers{
		Users: []User{{Login: "titi"}, {Login: "toto"}, {Login: "tete"}, {Login: "tata"}, {Login: "tutu"}},
		Meta:  metaData{{Cursor: "titi"}, {Cursor: "toto"}, {Cursor: "tete"}, {Cursor: "tata"}, {Cursor: "tutu"}},
	}

	// blacklistedStargazers := stargazers{
	// 	Users: []User{{Login: "jstrachan"}, {Login: "toto"}, {Login: "tete"}, {Login: "tata"}, {Login: "tutu"}},
	// 	Meta:  metaData{{Cursor: "jstrachan"}, {Cursor: "toto"}, {Cursor: "tete"}, {Cursor: "tata"}, {Cursor: "tutu"}},
	// }

	tests := map[string]struct {
		stargazers []stargazers
		totalUsers uint
		starLimit  uint
		scanAll    bool

		expectedCursors []string
	}{
		"less users than pagination": {
			stargazers: []stargazers{
				sg,
			},
			totalUsers: 5,
			starLimit:  100,

			expectedCursors: nil,
		},
		"exactly as many users as pagination": {
			stargazers: []stargazers{
				sg, sg, sg, sg,
			},
			totalUsers: 20,
			starLimit:  100,

			expectedCursors: nil,
		},
		"more users than pagination": {
			stargazers: []stargazers{
				sg, sg, sg, sg,
				sg, sg, sg,
			},
			totalUsers: 35,
			starLimit:  100,

			expectedCursors: []string{"tutu"},
		},
		"way more users than pagination": {
			stargazers: []stargazers{
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
			},
			totalUsers: 160,
			starLimit:  200,

			expectedCursors: []string{"tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu"},
		},
		"scan all stars should return all stars": {
			stargazers: []stargazers{
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
				sg, sg, sg, sg, sg, sg, sg, sg,
			},
			totalUsers: 400,
			scanAll:    true,

			expectedCursors: []string{"tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu", "tutu"},
		},
		// "blacklisted stargazers should dcause page skips": {
		// 	stargazers: []stargazers{
		// 		sg, sg, sg, sg, sg, sg, blacklistedStargazers, sg,
		// 		sg, sg, sg, sg, sg, sg, sg, sg,
		// 		sg, sg, sg, sg, sg, sg, sg, sg,
		// 		sg, sg, sg, sg, sg, sg, sg, sg,
		// 	},
		// 	totalUsers: 160,
		// 	starLimit:  200,

		// 	expectedCursors: []string{"tutu", "tutu", "tutu", "tutu", "tutu", "tutu"},
		// },
	}

	for description, test := range tests {
		t.Run(description, func(t *testing.T) {
			t.Parallel()
			starauditCtx := &context.Context{
				ScanAll: test.scanAll,
				Stars:   test.starLimit,
			}

			cursors := getCursors(starauditCtx, test.stargazers, test.totalUsers)

			assert.Equal(t, test.expectedCursors, cursors)
		})
	}
}

func TestUpdateUsers(t *testing.T) {
	tests := map[string]struct {
		users    []User
		response listStargazersResponse
		year     int

		expectedUsers []User
	}{
		"nil users": {
			users: nil,
			response: listStargazersResponse{
				response: response{
					Repository: repository{
						Stargazers: stargazers{
							Users: []User{
								{Login: "titi", Contributions: contributions{
									PrivateContributions: 84,
									ContributionCalendar: contributionCalendar{
										TotalContributions: 42,
									},
								}},
								{Login: "toto", Contributions: contributions{
									PrivateContributions: 21,
									ContributionCalendar: contributionCalendar{
										TotalContributions: 67,
									},
								}},
							},
						},
					},
				},
			},
			year: 2019,

			expectedUsers: []User{
				{Login: "titi", Contributions: contributions{
					PrivateContributions: 84,
					ContributionCalendar: contributionCalendar{
						TotalContributions: 42,
					},
				}, YearlyContributions: map[int]int{
					2019: 126,
				}},
				{Login: "toto", Contributions: contributions{
					PrivateContributions: 21,
					ContributionCalendar: contributionCalendar{
						TotalContributions: 67,
					},
				}, YearlyContributions: map[int]int{
					2019: 88,
				}},
			},
		},
		"update already existing users": {
			users: []User{
				{Login: "titi", Contributions: contributions{
					PrivateContributions: 84,
					ContributionCalendar: contributionCalendar{
						TotalContributions: 42,
					},
				}, YearlyContributions: map[int]int{
					2019: 126,
				}},
				{Login: "toto", Contributions: contributions{
					PrivateContributions: 21,
					ContributionCalendar: contributionCalendar{
						TotalContributions: 67,
					},
				}, YearlyContributions: map[int]int{
					2019: 88,
				}},
			},
			response: listStargazersResponse{
				response: response{
					Repository: repository{
						Stargazers: stargazers{
							Users: []User{
								{Login: "titi", Contributions: contributions{
									PrivateContributions: 84,
									ContributionCalendar: contributionCalendar{
										TotalContributions: 42,
									},
								}},
								{Login: "toto", Contributions: contributions{
									PrivateContributions: 21,
									ContributionCalendar: contributionCalendar{
										TotalContributions: 67,
									},
								}},
							},
						},
					},
				},
			},
			year: 2018,

			expectedUsers: []User{
				{Login: "titi", Contributions: contributions{
					PrivateContributions: 168,
					ContributionCalendar: contributionCalendar{
						TotalContributions: 42,
					},
				}, YearlyContributions: map[int]int{
					2019: 126,
					2018: 126,
				}},
				{Login: "toto", Contributions: contributions{
					PrivateContributions: 42,
					ContributionCalendar: contributionCalendar{
						TotalContributions: 67,
					},
				}, YearlyContributions: map[int]int{
					2019: 88,
					2018: 88,
				}},
			},
		},
	}

	for description, test := range tests {
		t.Run(description, func(t *testing.T) {
			t.Parallel()
			users := updateUsers(test.users, test.response, test.year)

			assert.Equal(t, test.expectedUsers, users)
		})
	}
}

// makeStargazers builds `total` stargazers spread over pages of listPagination
// users, each with a unique login and cursor.
func makeStargazers(total int) []stargazers {
	var pages []stargazers

	for start := 0; start < total; start += listPagination {
		var page stargazers

		for i := start; i < min(start+listPagination, total); i++ {
			page.Users = append(page.Users, User{Login: fmt.Sprintf("user-%d", i)})
			page.Meta = append(page.Meta, meta{Cursor: fmt.Sprintf("cursor-%d", i)})
		}

		pages = append(pages, page)
	}

	return pages
}

// expectedCursors mirrors the cursor generation rule: one cursor per full page
// of contribPagination users, except for the very last user of the set.
func expectedCursors(total int) []string {
	var cursors []string

	for i := 0; i < total-1; i++ {
		if i%contribPagination == contribPagination-1 {
			cursors = append(cursors, fmt.Sprintf("cursor-%d", i))
		}
	}

	return cursors
}

// TestGetCursorsSelectsEachPageOnce guards against the selection ranges
// overlapping, which made one page of users be fetched and counted twice,
// while another page was never fetched at all.
func TestGetCursorsSelectsEachPageOnce(t *testing.T) {
	tests := map[string]struct {
		totalUsers int
		starLimit  uint
		scanAll    bool
	}{
		"just below the comparative threshold": {totalUsers: 219, starLimit: 1000},
		"at the comparative threshold":         {totalUsers: 220, starLimit: 1000},
		"just above the comparative threshold": {totalUsers: 221, starLimit: 1000},
		"scan all":                             {totalUsers: 1000, starLimit: 1000, scanAll: true},
		"scan all, large repository":           {totalUsers: 5000, starLimit: 1000, scanAll: true},
		"random sample":                        {totalUsers: 5000, starLimit: 1000},
	}

	for description, test := range tests {
		t.Run(description, func(t *testing.T) {
			t.Parallel()

			starauditCtx := &context.Context{ScanAll: test.scanAll, Stars: test.starLimit}

			selected := getCursors(starauditCtx, makeStargazers(test.totalUsers), uint(test.totalUsers))

			seen := make(map[string]bool, len(selected))
			for _, cursor := range selected {
				assert.False(t, seen[cursor], "cursor %q was selected twice", cursor)
				seen[cursor] = true
			}

			assert.Subset(t, expectedCursors(test.totalUsers), selected,
				"every selected cursor must be one of the generated ones")

			if test.scanAll {
				assert.ElementsMatch(t, expectedCursors(test.totalUsers), selected,
					"--all must scan every page exactly once")
			}
		})
	}
}

// TestGetCursorsWithStarLimitBelowFirstBlock guards against the unsigned
// underflow that asked for more than 18 quintillion random cursors.
func TestGetCursorsWithStarLimitBelowFirstBlock(t *testing.T) {
	for _, starLimit := range []uint{20, 40, 100, 180} {
		t.Run(fmt.Sprint(starLimit), func(t *testing.T) {
			t.Parallel()

			starauditCtx := &context.Context{Stars: starLimit}

			selected := getCursors(starauditCtx, makeStargazers(1000), 1000)

			assert.LessOrEqual(t, len(selected), 200/contribPagination,
				"a star limit below the first block must not select extra pages")
		})
	}
}

func TestGetCursorsWithoutUsers(t *testing.T) {
	assert.Nil(t, getCursors(&context.Context{Stars: 1000}, nil, 0))
}

// TestGetCursorsWithFewerEdgesThanNodes covers a response where the API
// filtered out some edges, which used to index past the end of the slice.
func TestGetCursorsWithFewerEdgesThanNodes(t *testing.T) {
	pages := makeStargazers(220)
	for idx := range pages {
		pages[idx].Meta = pages[idx].Meta[:5]
	}

	assert.NotPanics(t, func() {
		getCursors(&context.Context{Stars: 1000}, pages, 220)
	})
}

func TestPickRandom(t *testing.T) {
	source := []string{"a", "b", "c", "d", "e"}

	tests := map[string]struct {
		amount         int
		expectedAmount int
	}{
		"fewer than available": {amount: 3, expectedAmount: 3},
		"exactly available":    {amount: 5, expectedAmount: 5},
		"more than available":  {amount: 12, expectedAmount: 5},
		"none":                 {amount: 0, expectedAmount: 0},
		"negative":             {amount: -4, expectedAmount: 0},
	}

	for description, test := range tests {
		t.Run(description, func(t *testing.T) {
			t.Parallel()

			picked := pickRandom(source, test.amount)

			require.Len(t, picked, test.expectedAmount)

			seen := make(map[string]bool, len(picked))
			for _, value := range picked {
				assert.False(t, seen[value], "%q was picked twice", value)
				assert.Contains(t, source, value)
				seen[value] = true
			}

			assert.Equal(t, []string{"a", "b", "c", "d", "e"}, source, "the source slice must not be modified")
		})
	}
}

func TestPickRandomFromEmptySlice(t *testing.T) {
	assert.NotPanics(t, func() {
		assert.Empty(t, pickRandom([]string{}, 3))
	})
}

// TestUpdateUsersPartialOverlap guards against a single already known user
// causing every other user of the same page to be discarded.
func TestUpdateUsersPartialOverlap(t *testing.T) {
	users := []User{
		{Login: "titi", YearlyContributions: map[int]int{2019: 10}},
	}

	response := listStargazersResponse{
		response: response{
			Repository: repository{
				Stargazers: stargazers{
					Users: []User{
						{Login: "titi", Contributions: contributions{ContributionCalendar: contributionCalendar{TotalContributions: 3}}},
						{Login: "toto", Contributions: contributions{ContributionCalendar: contributionCalendar{TotalContributions: 5}}},
						{Login: "tata", Contributions: contributions{ContributionCalendar: contributionCalendar{TotalContributions: 7}}},
					},
				},
			},
		},
	}

	updated := updateUsers(users, response, 2018)

	require.Len(t, updated, 3, "users that are new to this page must be kept")

	logins := make([]string, 0, len(updated))
	for _, user := range updated {
		logins = append(logins, user.Login)
	}
	assert.ElementsMatch(t, []string{"titi", "toto", "tata"}, logins)

	assert.Equal(t, 3, updated[0].YearlyContributions[2018], "the known user must still be updated")
	assert.Equal(t, 10, updated[0].YearlyContributions[2019], "previous years must be preserved")
}

// TestGetCursorsSelectsTheEarliestStargazers is the regression test for the
// early adopter sample. The GitHub API returns stargazers oldest first, so the
// first 200 of them are covered by the cursor-less first page plus the *first*
// cursors. Taking the end of the list sampled the most recent stargazers while
// labelling them "200 first stargazers" in the comparative report.
func TestGetCursorsSelectsTheEarliestStargazers(t *testing.T) {
	const beginCursorAmount = 200/contribPagination - 1

	tests := map[string]struct {
		totalUsers int
		scanAll    bool
	}{
		"sampled":  {totalUsers: 5000},
		"scan all": {totalUsers: 5000, scanAll: true},
	}

	for description, test := range tests {
		t.Run(description, func(t *testing.T) {
			t.Parallel()

			starauditCtx := &context.Context{Stars: 1000, ScanAll: test.scanAll}

			selected := getCursors(starauditCtx, makeStargazers(test.totalUsers), uint(test.totalUsers))
			require.Greater(t, len(selected), beginCursorAmount)

			assert.Equal(t, expectedCursors(test.totalUsers)[:beginCursorAmount], selected[:beginCursorAmount],
				"the sample must begin with the earliest cursors, not the most recent ones")

			// The first page plus these cursors is exactly the first 200 users.
			assert.Equal(t, "cursor-179", selected[beginCursorAmount-1])
		})
	}
}

// TestGetCursorsHonoursTheStarLimit checks that the selection plus the
// cursor-less first page adds up to the requested amount of stargazers.
func TestGetCursorsHonoursTheStarLimit(t *testing.T) {
	for _, starLimit := range []uint{200, 400, 1000, 2000} {
		t.Run(fmt.Sprint(starLimit), func(t *testing.T) {
			t.Parallel()

			starauditCtx := &context.Context{Stars: starLimit}

			selected := getCursors(starauditCtx, makeStargazers(100000), 100000)

			// +1 for the first page, which needs no cursor.
			assert.Equal(t, int(starLimit)/contribPagination, len(selected)+1)
		})
	}
}
