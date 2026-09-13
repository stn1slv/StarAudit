// Package trust provides functions to compute the trust score of a repository.
package trust

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Ullaakut/disgo"
	"github.com/Ullaakut/disgo/style"
	"github.com/montanaflynn/stats"
	staraudit_context "github.com/stn1slv/staraudit/v2/pkg/context"
	"github.com/stn1slv/staraudit/v2/pkg/gql"
)

// Factor represents one of the trust factors used to compute
// the trust score for a repository.
type Factor struct {
	// The raw value of this factor.
	Value float64

	// The % of trust given to the value, compared
	// to global references.
	TrustPercent float64
}

// FactorName is a typed string representing a
// Factor's name.
type FactorName string

// Percentile is a typed string representing
// a percentile trust factor. It is a string
// instead of a float to allow the `encoding/json`
// package to marshal trust reports.
type Percentile string

// Report represents the result of the trust computation of a repository's
// stargazers. It contains every trust factor that has been computed.
type Report struct {
	Factors     map[FactorName]Factor
	Percentiles map[Percentile]Factor
}

const (
	// firstStargazersAmount is the size of the "early adopters" sample that
	// the comparative report weighs against the rest of the stargazers.
	firstStargazersAmount = 200

	// minPercentileSamples is the amount of values below which every fifth
	// percentile cannot be computed.
	minPercentileSamples = 20
)

// Compute computes all trust factors for the stargazers of a repository.
func Compute(_ context.Context, _ *staraudit_context.Context, users []gql.User) (*Report, error) {
	if len(users) == 0 {
		return nil, fmt.Errorf("unable to compute a trust report without any stargazer data")
	}

	trustData := make(map[FactorName][]float64)
	now := time.Now().UTC().Year()

	for idx := range users {
		var contributionScore float64
		for year, contributions := range users[idx].YearlyContributions {
			// How old these contributions are in years (starts at one)
			contributionAge := float64((now - year) + 1)

			// Consider contributions more trustworthy if they are older.
			contributionScore += float64(contributions) * (contributionAge * contributionAge)
		}

		// Gather all contribution data and account ages.
		trustData[PrivateContributionFactor] = append(trustData[PrivateContributionFactor], float64(users[idx].Contributions.PrivateContributions))
		trustData[IssueContributionFactor] = append(trustData[IssueContributionFactor], float64(users[idx].Contributions.TotalIssueContributions))
		trustData[CommitContributionFactor] = append(trustData[CommitContributionFactor], float64(users[idx].Contributions.TotalCommitContributions))
		trustData[RepoContributionFactor] = append(trustData[RepoContributionFactor], float64(users[idx].Contributions.TotalRepositoryContributions))
		trustData[PRContributionFactor] = append(trustData[PRContributionFactor], float64(users[idx].Contributions.TotalPullRequestContributions))
		trustData[PRReviewContributionFactor] = append(trustData[PRReviewContributionFactor], float64(users[idx].Contributions.TotalPullRequestReviewContributions))
		trustData[AccountAgeFactor] = append(trustData[AccountAgeFactor], users[idx].DaysOld())
		trustData[ContributionScoreFactor] = append(trustData[ContributionScoreFactor], contributionScore)
	}

	disgo.StartStepf("Building trust report")

	defer disgo.EndStep()

	// A comparative report is only meaningful when the stargazers left over
	// after the first block are still numerous enough to have percentiles.
	if len(users) > firstStargazersAmount+minPercentileSamples {
		return buildComparativeReport(trustData)
	}

	return buildReport(trustData)
}

func buildReport(trustData map[FactorName][]float64) (*Report, error) {
	report := &Report{
		Factors: make(map[FactorName]Factor),
	}

	for factor, data := range trustData {
		score, err := stats.Mean(data)
		if err != nil {
			return nil, disgo.FailStepf("unable to compute score for factor %q: %v", factor, err)
		}

		reference, ok := factorReferences[factor]
		if !ok {
			return nil, disgo.FailStepf("missing trust reference for factor %q", factor)
		}

		report.Factors[factor] = Factor{
			Value:        score,
			TrustPercent: computeTrustFromScore(score, reference),
		}
	}

	// Only compute percentiles if  there are enough stargazers to be
	// able to compute every fifth percentile.
	if len(trustData[ContributionScoreFactor]) > minPercentileSamples {
		report.Percentiles = make(map[Percentile]Factor)
		for _, percentile := range percentiles {
			// Error is ignored on purpose.
			pctl, _ := strconv.ParseFloat(string(percentile), 64)

			value, err := stats.Percentile(trustData[ContributionScoreFactor], pctl)
			if err != nil {
				return nil, fmt.Errorf("unable to compute score trust %sth percentile: %w", percentile, err)
			}

			reference, ok := percentileReferences[percentile]
			if !ok {
				return nil, disgo.FailStepf("missing trust reference for the %sth percentile", percentile)
			}

			report.Percentiles[percentile] = Factor{
				Value:        value,
				TrustPercent: computeTrustFromScore(value, reference),
			}
		}
	}

	allTrust, err := weightedTrust(report)
	if err != nil {
		return nil, err
	}

	// Take percentiles into consideration, if they were
	// computed.
	for _, percentileTrust := range report.Percentiles {
		allTrust = append(allTrust, percentileTrust.TrustPercent)
	}

	trust, err := stats.Mean(allTrust)
	if err != nil {
		return nil, disgo.FailStepf("unable to compute overall trust: %v", err)
	}

	report.Factors[Overall] = Factor{
		TrustPercent: trust,
	}

	return report, nil
}

// buildComparativeReport splits the trust data and percentiles between the first stargazers
// and current stargazers, and it then builds a report that contains the worst of both sets.
func buildComparativeReport(trustData map[FactorName][]float64) (*Report, error) {
	report := &Report{
		Factors:     make(map[FactorName]Factor),
		Percentiles: make(map[Percentile]Factor),
	}

	firstStarsTrust, currentStarsTrust := splitTrustData(trustData)

	// Compute one trust report for the early stargazers.
	firstStarsReport, err := buildReport(firstStarsTrust)
	if err != nil {
		return nil, err
	}

	disgo.Debugln(style.Important("First 200 stargazers"))

	Render(firstStarsReport, false)

	// Compute another trust report for the random stargazers.
	currentStarsReport, err := buildReport(currentStarsTrust)
	if err != nil {
		return nil, err
	}

	disgo.Debugln(style.Important(len(currentStarsTrust[ContributionScoreFactor]), " random stargazers"))

	Render(currentStarsReport, false)

	// Build comparative report using data from both sets.
	for _, factor := range factors {
		if firstStarsReport.Factors[factor].TrustPercent <= currentStarsReport.Factors[factor].TrustPercent {
			report.Factors[factor] = firstStarsReport.Factors[factor]
		} else {
			report.Factors[factor] = currentStarsReport.Factors[factor]
		}
	}

	// buildReport leaves Percentiles nil when a sample is too small to have
	// them. A trust percentage of exactly zero is a legitimate result -- and
	// the expected one for a repository with fake stars -- so it must not be
	// used to detect that percentiles are missing.
	if firstStarsReport.Percentiles == nil || currentStarsReport.Percentiles == nil {
		report.Percentiles = nil
	} else {
		for _, percentile := range percentiles {
			if firstStarsReport.Percentiles[percentile].TrustPercent <= currentStarsReport.Percentiles[percentile].TrustPercent {
				report.Percentiles[percentile] = firstStarsReport.Percentiles[percentile]
			} else {
				report.Percentiles[percentile] = currentStarsReport.Percentiles[percentile]
			}
		}
	}

	allTrust, err := weightedTrust(report)
	if err != nil {
		return nil, err
	}

	for _, percentileTrust := range report.Percentiles {
		allTrust = append(allTrust, percentileTrust.TrustPercent)
	}

	trust, err := stats.Mean(allTrust)
	if err != nil {
		return nil, disgo.FailStepf("unable to compute overall trust: %v", err)
	}

	report.Factors[Overall] = Factor{
		TrustPercent: trust,
	}

	return report, nil
}

// weightedTrust repeats each factor's trust percentage according to its
// weight, so that averaging the result yields the weighted overall trust.
func weightedTrust(report *Report) ([]float64, error) {
	var allTrust []float64

	for factorName, weight := range factorWeights {
		factor, ok := report.Factors[factorName]
		if !ok {
			// A missing factor would otherwise contribute a trust of zero,
			// which is indistinguishable from a genuinely computed zero.
			return nil, disgo.FailStepf("no value was computed for factor %q", factorName)
		}

		for i := 0; i < weight; i++ {
			allTrust = append(allTrust, factor.TrustPercent)
		}
	}

	return allTrust, nil
}

// splitTrustData split a trust data map between first and random stargazers.
func splitTrustData(trustData map[FactorName][]float64) (first, current map[FactorName][]float64) {
	first = make(map[FactorName][]float64)
	current = make(map[FactorName][]float64)

	for _, factor := range factors {
		values := trustData[factor]

		// Each factor is bounded by the length of its own slice: they are
		// not guaranteed to hold the same amount of samples.
		split := min(firstStargazersAmount, len(values))

		first[factor] = append(first[factor], values[:split]...)
		current[factor] = append(current[factor], values[split:]...)
	}

	return first, current
}

// computeTrustFromScore takes a score and a reference expected score,
// and computes a trust level depending on the difference between
// both. Trust will reach 0.99 if the score is over 1.5 times what
// is considered a good score.
func computeTrustFromScore(score, reference float64) float64 {
	// Dividing by a zero reference yields NaN for a zero score and +Inf
	// otherwise, and +Inf clamps to maximum trust. Neither is a meaningful
	// answer, so an unusable reference scores no trust at all.
	if reference <= 0 {
		return 0
	}

	return min(score/(1.5*reference), 0.99)
}
