// Package main is the entry point for the staraudit CLI tool.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Ullaakut/disgo"
	"github.com/Ullaakut/disgo/style"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	staraudit_context "github.com/stn1slv/staraudit/pkg/context"
	"github.com/stn1slv/staraudit/pkg/gql"
	"github.com/stn1slv/staraudit/pkg/history"
	"github.com/stn1slv/staraudit/pkg/signature"
	"github.com/stn1slv/staraudit/pkg/trust"
)

// Exit codes. The verdict codes let CI jobs act on the result without
// parsing the output.
const (
	exitPass       = 0
	exitError      = 1
	exitReview     = 2
	exitSuspicious = 3
)

// codeInvalidArguments is the JSON error code for unusable command line input.
const codeInvalidArguments = "invalid_arguments"

func parseArguments() error {
	viper.SetEnvPrefix("staraudit")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	defaults := history.DefaultThresholds()

	pflag.BoolP("verbose", "v", false, "Show extra logs (including comparative reports)")
	pflag.Bool("trust", false, "Run the per-stargazer trust scan instead of the star history analysis (only works on repositories you administer)")
	pflag.Bool("json", false, "Print the star history analysis as JSON on stdout")
	pflag.Int("min-stars", defaults.MinStars, "Fewer stars than this gives a review verdict")
	pflag.Float64("burst-review", defaults.BurstReview, "Share of stars in the busiest 14 days that gives a review verdict")
	pflag.Float64("burst-suspicious", defaults.BurstSuspicious, "Share of stars in the busiest 14 days that can give a suspicious verdict")
	pflag.Float64("tail-suspicious", defaults.TailSuspicious, "Stars in the 60 days after the burst, as a share of the burst, below which the burst is suspicious")
	pflag.Float64("peak-review", defaults.PeakReview, "Share of stars on a single day that gives a review verdict")
	pflag.BoolP("all", "a", false, "Trust scan: scan every stargazer of the repository (overrides --stars)")
	pflag.UintP("stars", "s", 1000, "Trust scan: maximum amount of stars to scan")
	pflag.StringP("cachedir", "c", "./data", "Trust scan: directory in which to store cache data")

	viper.AutomaticEnv()

	pflag.Parse()

	err := viper.BindPFlags(pflag.CommandLine)
	if err != nil {
		return err
	}

	if len(pflag.Args()) == 0 {
		disgo.Infoln("Missing required repository argument")
		pflag.Usage()
		os.Exit(0)
	}

	return nil
}

func main() {
	code, err := run()
	if err != nil {
		disgo.Errorln(style.Failure(style.SymbolCross, " ", err))
	}

	os.Exit(code)
}

// run executes the command and returns its exit code. It is separated from
// main so that deferred cleanups run before the process exits.
func run() (int, error) {
	err := parseArguments()
	if err != nil {
		return exitError, err
	}

	jsonOutput := viper.GetBool("json")

	// With --json, stdout carries only the JSON document, so logs go to stderr.
	terminalOptions := []func(*disgo.Terminal){
		disgo.WithColors(!jsonOutput),
		disgo.WithDebug(viper.GetBool("verbose")),
	}
	if jsonOutput {
		terminalOptions = append(terminalOptions, disgo.WithDefaultOutput(os.Stderr))
	}

	disgo.SetTerminalOptions(terminalOptions...)

	// Handle OS signals for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repository := pflag.Arg(0)

	// Split repository into repo owner & repo name.
	repoInfo := strings.Split(repository, "/")
	if len(repoInfo) != 2 {
		return exitError, reportFailure(jsonOutput, repository, codeInvalidArguments,
			fmt.Errorf("invalid repository %q: should be of the form \"repoOwner/repoName\"", repository))
	}

	token := os.Getenv("GITHUB_TOKEN")

	if !viper.GetBool("trust") {
		return analyzeHistory(ctx, repository, repoInfo[0], repoInfo[1], token, jsonOutput)
	}

	if jsonOutput {
		return exitError, reportFailure(jsonOutput, repository, codeInvalidArguments,
			errors.New("--json is only available for the star history analysis, not with --trust"))
	}

	if token == "" {
		return exitError, errors.New("missing github access token. Please set one in your GITHUB_TOKEN environment variable, with \"repo\" rights")
	}

	starauditCtx := &staraudit_context.Context{
		RepoOwner:          repoInfo[0],
		RepoName:           repoInfo[1],
		GithubToken:        token,
		Stars:              viper.GetUint("stars"),
		CacheDirectoryPath: viper.GetString("cachedir"),
		ScanAll:            viper.GetBool("all"),
		Verbose:            viper.GetBool("verbose"),
	}

	if err := detectFakeStars(ctx, starauditCtx); err != nil {
		return exitError, err
	}

	return exitPass, nil
}

// analyzeHistory runs the star history analysis and maps its verdict to an
// exit code.
func analyzeHistory(ctx context.Context, repository, owner, name, token string, jsonOutput bool) (int, error) {
	thresholds := history.Thresholds{
		MinStars:        viper.GetInt("min-stars"),
		BurstReview:     viper.GetFloat64("burst-review"),
		BurstSuspicious: viper.GetFloat64("burst-suspicious"),
		TailSuspicious:  viper.GetFloat64("tail-suspicious"),
		PeakReview:      viper.GetFloat64("peak-review"),
	}

	if err := thresholds.Validate(); err != nil {
		return exitError, reportFailure(jsonOutput, repository, codeInvalidArguments, err)
	}

	if token == "" {
		disgo.Infoln(style.Important("GITHUB_TOKEN is not set, so GitHub allows only 60 requests per hour"))
	}

	series, err := history.Fetch(ctx, owner, name, token)
	if err != nil {
		// An interrupted run has nothing to report.
		if errors.Is(err, context.Canceled) {
			return exitError, err
		}

		code := history.CodeAPIError

		var historyErr *history.Error
		if errors.As(err, &historyErr) {
			code = historyErr.Code
		}

		return exitError, reportFailure(jsonOutput, repository, code, fmt.Errorf("unable to fetch the star history: %w", err))
	}

	result := history.Analyze(series, thresholds)

	if jsonOutput {
		err = history.RenderJSON(os.Stdout, repository, time.Now(), result)
	} else {
		err = history.RenderText(os.Stdout, repository, result)
	}

	if err != nil {
		return exitError, err
	}

	return exitCode(result.Verdict), nil
}

// reportFailure also writes the error as a JSON document on stdout when
// --json is set, and returns it for main to print on stderr.
func reportFailure(jsonOutput bool, repository, code string, err error) error {
	if !jsonOutput {
		return err
	}

	if renderErr := history.RenderErrorJSON(os.Stdout, repository, code, err.Error()); renderErr != nil {
		return errors.Join(err, renderErr)
	}

	return err
}

// exitCode maps a verdict to the exit code documented in the README.
func exitCode(verdict history.Verdict) int {
	switch verdict {
	case history.VerdictSuspicious:
		return exitSuspicious
	case history.VerdictReview:
		return exitReview
	default:
		return exitPass
	}
}

func detectFakeStars(ctx context.Context, starauditCtx *staraudit_context.Context) error {
	disgo.Infof("Beginning fetching process for repository %s/%s\n", starauditCtx.RepoOwner, starauditCtx.RepoName)

	cursors, totalUsers, err := gql.FetchStargazers(ctx, starauditCtx)
	if err != nil {
		return fmt.Errorf("failed to query stargazer data: %w", err)
	}

	if totalUsers < 1000 {
		disgo.Infoln(style.Important("This repository appears to have a low amount of stargazers. Trust calculations might not be accurate."))
	}

	// For now, we only fetch contributions since 2013. It will be configurable later on
	// once the algorithm is more accurate and more data has been fetched.
	if !starauditCtx.ScanAll && totalUsers > starauditCtx.Stars {
		disgo.Infof("Fetching contributions for %d users up to year %d\n", starauditCtx.Stars, 2013)
	} else {
		disgo.Infof("Fetching contributions for %d users up to year %d\n", totalUsers, 2013)
	}

	users, err := gql.FetchContributions(ctx, starauditCtx, cursors, 2013)
	if err != nil {
		return fmt.Errorf("failed to query stargazer data: %w", err)
	}

	report, err := trust.Compute(ctx, starauditCtx, users)
	if err != nil {
		return fmt.Errorf("unable to compute trust report: %w", err)
	}

	trust.Render(report, true)

	// Uploading the report is opt-in, and failing to upload it is not fatal:
	// the report has already been computed and rendered locally.
	if signature.Enabled() {
		err = signature.SendReport(ctx, starauditCtx, report)
		if err != nil {
			disgo.Errorln(style.Important("Unable to send trust report to the staraudit server: ", err))
		}
	} else {
		disgo.Debugln("No signing key configured, skipping report upload.")
	}

	disgo.Infof("\n%s Analysis successful. %d users computed.\n", style.Success(style.SymbolCheck), len(users))

	badgeEndpoint := url.QueryEscape(fmt.Sprintf("https://astronomer.ullaakut.eu/shields?owner=%s&name=%s", starauditCtx.RepoOwner, starauditCtx.RepoName))

	disgo.Infof("GitHub badge available at https://img.shields.io/endpoint.svg?url=%s\n", badgeEndpoint)

	return nil
}
