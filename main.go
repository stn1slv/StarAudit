// Package main is the entry point for the staraudit CLI tool.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
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

// errMissingRepository is returned when no repository argument is given.
var errMissingRepository = errors.New("missing required repository argument")

// fetchHistory is a variable so that tests can replace the GitHub API.
var fetchHistory = history.Fetch

// options are the settings of one run, read strictly from the flags and the
// STARAUDIT_* variables before any mode is chosen.
type options struct {
	trust      bool
	all        bool
	stars      uint
	cacheDir   string
	thresholds history.Thresholds
}

// parseArguments defines and parses the command line. The flags and the
// settings are returned even when parsing fails, so that --json still
// selects the format of the error.
func parseArguments(args []string, stderr io.Writer) (*pflag.FlagSet, *viper.Viper, error) {
	defaults := history.DefaultThresholds()

	// Not the default ExitOnError: pflag would then exit with code 2, which
	// means "review".
	flags := pflag.NewFlagSet("staraudit", pflag.ContinueOnError)
	flags.SetOutput(stderr)

	// A new flag set has no usage function of its own.
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: staraudit [flags] owner/repo")
		flags.PrintDefaults()
	}

	flags.BoolP("verbose", "v", false, "Show extra logs (including comparative reports)")
	flags.Bool("trust", false, "Run the per-stargazer trust scan instead of the star history analysis (only works on repositories you administer)")
	flags.Bool("json", false, "Print the star history analysis as JSON on stdout")
	flags.Int("min-stars", defaults.MinStars, "Fewer stars than this gives a review verdict")
	flags.Float64("burst-review", defaults.BurstReview, "Share of stars in the busiest 14 days that gives a review verdict")
	flags.Float64("burst-suspicious", defaults.BurstSuspicious, "Share of stars in the busiest 14 days that can give a suspicious verdict")
	flags.Float64("tail-suspicious", defaults.TailSuspicious, "Stars in the 60 days after the burst, as a share of the burst, below which the burst is suspicious")
	flags.Float64("peak-review", defaults.PeakReview, "Share of stars on a single day that gives a review verdict")
	flags.BoolP("all", "a", false, "Trust scan: scan every stargazer of the repository (overrides --stars)")
	flags.UintP("stars", "s", 1000, "Trust scan: maximum amount of stars to scan")
	flags.StringP("cachedir", "c", "./data", "Trust scan: directory in which to store cache data")

	settings := viper.New()
	settings.SetEnvPrefix("staraudit")
	settings.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	settings.AutomaticEnv()

	parseErr := flags.Parse(args)

	if err := settings.BindPFlags(flags); err != nil {
		return flags, settings, err
	}

	if parseErr != nil {
		return flags, settings, parseErr
	}

	switch flags.NArg() {
	case 0:
		return flags, settings, errMissingRepository
	case 1:
		return flags, settings, nil
	default:
		return flags, settings, fmt.Errorf("expected one repository argument, got %d: %s", flags.NArg(), strings.Join(flags.Args(), " "))
	}
}

// jsonRequested looks for --json in arguments that failed to parse. pflag
// stops at the first bad flag, so a --json after it would be missed and the
// error would not be written as JSON.
func jsonRequested(args []string) bool {
	probe := pflag.NewFlagSet("json-probe", pflag.ContinueOnError)
	probe.ParseErrorsAllowlist.UnknownFlags = true
	probe.SetOutput(io.Discard)

	requested := probe.Bool("json", false, "")

	// The caller reports the real parse error; this pass only looks for --json.
	_ = probe.Parse(args)

	return *requested
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command and returns its exit code. It is separated from
// main so that deferred cleanups run before the process exits, and so that
// tests can call it.
func run(args []string, stdout, stderr io.Writer) int {
	code, err := runCommand(args, stdout, stderr)
	if err != nil {
		disgo.Errorln(style.Failure(style.SymbolCross, " ", err))
	}

	return code
}

func runCommand(args []string, stdout, stderr io.Writer) (int, error) {
	flags, settings, parseErr := parseArguments(args, stderr)
	if errors.Is(parseErr, pflag.ErrHelp) {
		return exitPass, nil
	}

	jsonOutput, jsonErr := setting(settings, "json", strconv.ParseBool)
	if parseErr != nil && !jsonOutput {
		jsonOutput = jsonRequested(args)
	}

	verbose, verboseErr := setting(settings, "verbose", strconv.ParseBool)

	configureOutput(stdout, stderr, jsonOutput, verbose)

	repository := flags.Arg(0)

	if err := cmp.Or(parseErr, jsonErr, verboseErr); err != nil {
		if errors.Is(err, errMissingRepository) {
			flags.Usage()
		}

		return exitError, reportFailure(stdout, jsonOutput, repository, codeInvalidArguments, err)
	}

	opts, err := readOptions(settings)
	if err != nil {
		return exitError, reportFailure(stdout, jsonOutput, repository, codeInvalidArguments, err)
	}

	owner, name, err := parseRepository(repository)
	if err != nil {
		return exitError, reportFailure(stdout, jsonOutput, repository, codeInvalidArguments, err)
	}

	if opts.trust && jsonOutput {
		return exitError, reportFailure(stdout, jsonOutput, repository, codeInvalidArguments,
			errors.New("--json is only available for the star history analysis, not with --trust"))
	}

	// Handle OS signals for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := os.Getenv("GITHUB_TOKEN")

	if !opts.trust {
		return analyzeHistory(ctx, stdout, opts.thresholds, repository, owner, name, token, jsonOutput)
	}

	if token == "" {
		return exitError, errors.New("missing GitHub access token: --trust needs a GITHUB_TOKEN that belongs to an admin or collaborator of the repository")
	}

	starauditCtx := &staraudit_context.Context{
		RepoOwner:          owner,
		RepoName:           name,
		GithubToken:        token,
		Stars:              opts.stars,
		CacheDirectoryPath: opts.cacheDir,
		ScanAll:            opts.all,
		Verbose:            verbose,
	}

	if err := detectFakeStars(ctx, starauditCtx); err != nil {
		return exitError, err
	}

	return exitPass, nil
}

// configureOutput routes the logs and chooses colors. With --json, stdout
// carries only the JSON document, so logs go to stderr. Colors are only
// used on a terminal, so that a redirected report holds no escape codes.
func configureOutput(stdout, stderr io.Writer, jsonOutput, verbose bool) {
	logs := stdout
	if jsonOutput {
		logs = stderr
	}

	disgo.SetTerminalOptions(
		disgo.WithColors(!jsonOutput && isTerminal(stdout)),
		disgo.WithDebug(verbose),
		disgo.WithDefaultOutput(logs),
		disgo.WithErrorOutput(stderr),
	)
}

// isTerminal reports whether w is an interactive terminal.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}

	info, err := file.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// parseRepository splits "owner/name" and rejects anything else, so that a
// malformed name fails here instead of as a not_found from GitHub.
func parseRepository(repository string) (string, string, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("invalid repository %q: should be of the form \"repoOwner/repoName\"", repository)
	}

	return owner, name, nil
}

// analyzeHistory runs the star history analysis and maps its verdict to an
// exit code.
func analyzeHistory(
	ctx context.Context,
	stdout io.Writer,
	thresholds history.Thresholds,
	repository, owner, name, token string,
	jsonOutput bool,
) (int, error) {
	if token == "" {
		disgo.Infoln(style.Important("GITHUB_TOKEN is not set, so GitHub allows only 60 requests per hour"))
	}

	series, err := fetchHistory(ctx, owner, name, token)
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

		return exitError, reportFailure(stdout, jsonOutput, repository, code, fmt.Errorf("unable to fetch the star history: %w", err))
	}

	result := history.Analyze(series, thresholds)

	if jsonOutput {
		err = history.RenderJSON(stdout, repository, time.Now(), result)
	} else {
		err = history.RenderText(stdout, repository, result)
	}

	if err != nil {
		return exitError, err
	}

	return exitCode(result.Verdict), nil
}

// readOptions reads every setting that the modes use, so that a value that
// does not parse is reported whichever mode runs.
func readOptions(settings *viper.Viper) (options, error) {
	var (
		opts options
		err  error
	)

	if opts.trust, err = setting(settings, "trust", strconv.ParseBool); err != nil {
		return options{}, err
	}

	if opts.all, err = setting(settings, "all", strconv.ParseBool); err != nil {
		return options{}, err
	}

	if opts.stars, err = setting(settings, "stars", parseUint); err != nil {
		return options{}, err
	}

	opts.cacheDir = settings.GetString("cachedir")

	if opts.thresholds, err = thresholdsFrom(settings); err != nil {
		return options{}, err
	}

	return opts, nil
}

// thresholdsFrom reads the thresholds and validates them.
func thresholdsFrom(settings *viper.Viper) (history.Thresholds, error) {
	minStars, err := setting(settings, "min-stars", strconv.Atoi)
	if err != nil {
		return history.Thresholds{}, err
	}

	thresholds := history.Thresholds{MinStars: minStars}

	shares := []struct {
		key   string
		value *float64
	}{
		{"burst-review", &thresholds.BurstReview},
		{"burst-suspicious", &thresholds.BurstSuspicious},
		{"tail-suspicious", &thresholds.TailSuspicious},
		{"peak-review", &thresholds.PeakReview},
	}

	for _, share := range shares {
		value, err := setting(settings, share.key, parseFloat)
		if err != nil {
			return history.Thresholds{}, err
		}

		*share.value = value
	}

	return thresholds, thresholds.Validate()
}

// setting reads one flag or STARAUDIT_* variable. A flag arrives typed or as
// its string form, and a variable always as a string. Strings are parsed
// strictly, because the viper getters silently turn a value that does not
// parse into a zero value.
func setting[T any](settings *viper.Viper, key string, parse func(string) (T, error)) (T, error) {
	var zero T

	switch value := settings.Get(key).(type) {
	case string:
		parsed, err := parse(strings.TrimSpace(value))
		if err != nil {
			return zero, fmt.Errorf("%s: invalid value %q", key, value)
		}

		return parsed, nil
	case T:
		return value, nil
	default:
		return zero, fmt.Errorf("%s: unexpected value %v", key, value)
	}
}

func parseFloat(value string) (float64, error) {
	return strconv.ParseFloat(value, 64)
}

func parseUint(value string) (uint, error) {
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize)

	return uint(parsed), err
}

// reportFailure also writes the error as a JSON document on stdout when
// --json is set, and returns it for run to print on stderr.
func reportFailure(stdout io.Writer, jsonOutput bool, repository, code string, err error) error {
	if !jsonOutput {
		return err
	}

	if renderErr := history.RenderErrorJSON(stdout, repository, code, err.Error()); renderErr != nil {
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
