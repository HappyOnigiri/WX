package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	os.Exit(commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func commandMain(parent context.Context, args []string, output, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("citest", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	profile := flags.String("profile", "", "CI profile name")
	reportDir := flags.String("report-dir", "", "artifact report directory")
	coverage := flags.String("coverprofile", "", "coverage profile written by go test")
	repoRoot := flags.String("repo-root", ".", "repository root")
	goCommand := flags.String("go", "go", "Go command used for go list")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *profile == "" {
		_, _ = fmt.Fprintln(errorOutput, "citest: -profile is required")
		return 2
	}
	command := flags.Args()
	if len(command) == 0 || command[0] == "--" {
		_, _ = fmt.Fprintln(errorOutput, "citest: a test command is required after --")
		return 2
	}
	if *reportDir == "" {
		*reportDir = filepath.Join("artifacts", "ci-tests", *profile)
	}
	root, err := filepath.Abs(*repoRoot)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "citest: resolve repository root: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()
	code, err := run(ctx, config{
		Profile:         *profile,
		ReportDir:       *reportDir,
		CoverageProfile: *coverage,
		RepoRoot:        root,
		GoCommand:       *goCommand,
		Command:         command,
	}, output)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "citest: %v\n", err)
		return 1
	}
	return code
}

func run(ctx context.Context, cfg config, output io.Writer) (int, error) {
	if err := validateConfig(cfg); err != nil {
		return 1, err
	}
	if err := os.MkdirAll(cfg.ReportDir, 0o755); err != nil {
		return 1, fmt.Errorf("create report directory: %w", err)
	}
	started := now()
	initialCommand, err := commandWithJSON(cfg.Command)
	if err != nil {
		return 1, err
	}
	initialResult, err := executeRun(ctx, cfg, initialCommand, "initial", cfg.CoverageProfile, output)
	if err != nil {
		return 1, err
	}
	man := newManifest(cfg, runRecordFromResult(initialResult, cfg, initialCommand, "initial", cfg.CoverageProfile), started)
	initialCoverageError := false
	if cfg.CoverageProfile != "" {
		if err := preserveCoverage(cfg, cfg.CoverageProfile, "initial.out"); err != nil {
			initialCoverageError = true
			man.Diagnostics = append(man.Diagnostics, err.Error())
		} else {
			man.Initial.Coverage = "initial.out"
		}
	}
	failed := failedTests(initialResult)
	switch {
	case initialResult.Anomaly != "":
		man.Status = "failed"
		man.Diagnostics = append(man.Diagnostics, initialResult.Anomaly)
	case initialResult.Exit == 0 && len(failed) == 0:
		man.Status = "passed"
	case len(failed) == 0:
		man.Status = "failed"
		man.Diagnostics = append(man.Diagnostics, "test command did not complete as named test failures")
	default:
		declarations, diagnostics := resolveFailedDeclarations(ctx, cfg, failed)
		man.Diagnostics = append(man.Diagnostics, diagnostics...)
		if len(diagnostics) > 0 {
			man.Status = "failed"
		} else {
			man.Status = retryFailures(ctx, cfg, initialResult, failed, declarations, &man, output)
		}
	}
	if initialCoverageError {
		man.Status = "failed"
	}
	if cfg.CoverageProfile != "" && !initialCoverageError {
		if err := ensureCoverage(cfg.CoverageProfile); err != nil {
			man.Diagnostics = append(man.Diagnostics, err.Error())
			man.Status = "failed"
		} else if err := preserveCoverage(cfg, cfg.CoverageProfile, "coverage.out"); err != nil {
			man.Diagnostics = append(man.Diagnostics, err.Error())
			man.Status = "failed"
		}
	}
	man.Recovered = len(man.Recoveries) > 0
	if err := writeManifest(cfg.ReportDir, man); err != nil {
		return 1, err
	}
	if man.Status == "passed" {
		_, _ = fmt.Fprintf(output, "citest: %s passed; recovered=%d\n", cfg.Profile, len(man.Recoveries))
		return 0, nil
	}
	_, _ = fmt.Fprintf(output, "citest: %s failed; recovered=%d\n", cfg.Profile, len(man.Recoveries))
	return 1, nil
}

func validateConfig(cfg config) error {
	if cfg.Profile != "coverage" && cfg.Profile != "race-daemon" && cfg.Profile != "race-rest" {
		return fmt.Errorf("unsupported profile %q", cfg.Profile)
	}
	if cfg.ReportDir == "" || cfg.RepoRoot == "" {
		return errors.New("report directory and repository root are required")
	}
	if len(cfg.Command) < 2 || cfg.Command[0] == "" {
		return errors.New("test command must contain an executable and go test")
	}
	foundTest := false
	for _, arg := range cfg.Command[1:] {
		if arg == "test" {
			foundTest = true
			break
		}
	}
	if !foundTest {
		return errors.New("test command must invoke go test")
	}
	return nil
}

func commandWithJSON(command []string) ([]string, error) {
	result := append([]string(nil), command...)
	for _, arg := range result {
		if arg == "-json" || arg == "-test.json" {
			return result, nil
		}
	}
	for index, arg := range result {
		if index > 0 && arg == "test" {
			return append(result[:index+1], append([]string{"-json"}, result[index+1:]...)...), nil
		}
	}
	return nil, errors.New("test command must invoke go test")
}

func now() time.Time {
	return time.Now().UTC()
}

func newManifest(cfg config, initial runRecord, started time.Time) manifest {
	conditions := map[string]string{
		"profile": cfg.Profile,
		"command": strings.Join(cfg.Command, " "),
	}
	for _, key := range []string{"CI", "GOMAXPROCS", "GODEBUG", "RACE_SHARD"} {
		if value := os.Getenv(key); value != "" {
			conditions[key] = value
		}
	}
	return manifest{
		SchemaVersion: 1,
		Profile:       cfg.Profile,
		Repository:    os.Getenv("GITHUB_REPOSITORY"),
		Initial:       initial,
		RunID:         os.Getenv("GITHUB_RUN_ID"),
		RunAttempt:    os.Getenv("GITHUB_RUN_ATTEMPT"),
		Event:         os.Getenv("GITHUB_EVENT_NAME"),
		Ref:           os.Getenv("GITHUB_REF"),
		APISHA:        os.Getenv("GITHUB_SHA"),
		TestSHA:       testSHA(cfg.RepoRoot),
		GoVersion:     goVersion(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Conditions:    conditions,
		CreatedAt:     started,
	}
}
