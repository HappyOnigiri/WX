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
	"time"

	"github.com/HappyOnigiri/WX/tools/internal/gotest"
)

type declaration = gotest.Declaration

func main() {
	os.Exit(commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func commandMain(parent context.Context, args []string, output, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("huntreport", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	huntID := flags.String("id", "", "hunt container id")
	reportDir := flags.String("report-dir", "", "manifest directory")
	logDir := flags.String("log-dir", "hunt-logs", "directory keeping the logs of failed rounds")
	repoRoot := flags.String("repo-root", ".", "repository root")
	goCommand := flags.String("go", "go", "Go command used for go list")
	minutes := flags.Int("minutes", 0, "how long to keep repeating rounds")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *huntID == "" {
		_, _ = fmt.Fprintln(errorOutput, "huntreport: -id is required")
		return 2
	}
	if *minutes <= 0 {
		_, _ = fmt.Fprintln(errorOutput, "huntreport: -minutes must be positive")
		return 2
	}
	command := flags.Args()
	if len(command) == 0 || command[0] == "--" {
		_, _ = fmt.Fprintln(errorOutput, "huntreport: a test command is required after --")
		return 2
	}
	if *reportDir == "" {
		*reportDir = filepath.Join("artifacts", "flake-hunt", *huntID)
	}
	root, err := filepath.Abs(*repoRoot)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "huntreport: resolve repository root: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()
	code, err := run(ctx, config{
		HuntID:    *huntID,
		ReportDir: *reportDir,
		LogDir:    *logDir,
		RepoRoot:  root,
		GoCommand: *goCommand,
		Deadline:  time.Duration(*minutes) * time.Minute,
		Command:   command,
	}, output)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "huntreport: %v\n", err)
		return 1
	}
	return code
}

// run はラウンドを期限まで繰り返し、manifestとstep summaryを残す。
// 失敗ラウンドが1つでもあればコード1を返し、ハントのジョブは従来どおり赤くなる。
func run(ctx context.Context, cfg config, output io.Writer) (int, error) {
	if err := validateConfig(cfg); err != nil {
		return 1, err
	}
	for _, dir := range []string{cfg.ReportDir, cfg.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 1, fmt.Errorf("create directory: %w", err)
		}
	}
	hunt, err := repeatRounds(ctx, cfg, output)
	if err != nil {
		return 1, err
	}
	man := hunt.manifest(cfg)
	if err := writeManifest(cfg.ReportDir, man); err != nil {
		return 1, err
	}
	if err := writeSummary(man); err != nil {
		return 1, err
	}
	_, _ = fmt.Fprintf(output, "huntreport: %s rounds=%d failures=%d anomalies=%d tests=%d\n",
		cfg.HuntID, man.Rounds, man.FailedRounds, man.AnomalyRounds, len(man.Tests))
	if man.FailedRounds > 0 || man.AnomalyRounds > 0 {
		return 1, nil
	}
	return 0, nil
}

func validateConfig(cfg config) error {
	if cfg.ReportDir == "" || cfg.LogDir == "" || cfg.RepoRoot == "" {
		return errors.New("report directory, log directory and repository root are required")
	}
	if len(cfg.Command) < 2 || cfg.Command[0] == "" {
		return errors.New("test command must contain an executable and go test")
	}
	for _, arg := range cfg.Command[1:] {
		if arg == "test" {
			return nil
		}
	}
	return errors.New("test command must invoke go test")
}

// commandWithJSON は go test へ -json を足す。
// stdoutをJSONLにするため、呼び出し側はstdoutとstderrを混ぜてはならない。
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
