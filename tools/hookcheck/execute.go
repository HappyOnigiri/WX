package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type commandResult struct {
	name    string
	stdout  string
	stderr  string
	err     error
	elapsed time.Duration
}

func execute(ctx context.Context, root string, plan selection, out io.Writer) error {
	started := time.Now()
	if plan.empty() {
		printTotal(out, "ok", started)
		return nil
	}
	checks := plan.sortedChecks()
	results := make([]commandResult, len(checks))
	var wait sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for index, check := range checks {
		wait.Add(1)
		go func(index int, check *checkSelection) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			results[index] = runCommand(ctx, root, []string{"make", check.name}, check.name)
		}(index, check)
	}
	wait.Wait()
	for _, result := range results {
		printResult(out, result)
		if result.err != nil {
			printTotal(out, "failed", started)
			return fmt.Errorf("hook check %s failed: %w", result.name, result.err)
		}
	}
	if plan.compile {
		result := runCommand(ctx, root, append([]string{goBinary(), "test", "-run", "^$"}, "./..."), "compile-all")
		printResult(out, result)
		if result.err != nil {
			printTotal(out, "failed", started)
			return fmt.Errorf("hook check %s failed: %w", result.name, result.err)
		}
	}
	for _, test := range plan.sortedTests() {
		args := []string{goBinary(), "test", "-short"}
		if test.countOne {
			args = append(args, "-count=1")
		}
		args = append(args, test.packages...)
		result := runCommand(ctx, root, args, "package-test "+strings.Join(test.packages, ", "))
		printResult(out, result)
		if result.err != nil {
			printTotal(out, "failed", started)
			return fmt.Errorf("hook check %s failed: %w", result.name, result.err)
		}
	}
	printTotal(out, "ok", started)
	return nil
}

func runCommand(ctx context.Context, root string, args []string, name string) commandResult {
	started := time.Now()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = root
	command.Env = cleanEnvironment()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return commandResult{
		name:    name,
		stdout:  stdout.String(),
		stderr:  stderr.String(),
		err:     err,
		elapsed: time.Since(started),
	}
}

func printResult(out io.Writer, result commandResult) {
	status := "ok"
	if result.err != nil {
		status = "failed"
	}
	_, _ = fmt.Fprintf(out, "[%s] %s (%s)\n", status, result.name, result.elapsed.Round(time.Millisecond))
	if result.stdout != "" {
		_, _ = fmt.Fprint(out, result.stdout)
	}
	if result.stderr != "" {
		_, _ = fmt.Fprint(out, result.stderr)
	}
}

func printTotal(out io.Writer, status string, started time.Time) {
	_, _ = fmt.Fprintf(out, "[total] %s (%s)\n", status, time.Since(started).Round(time.Millisecond))
}

func goBinary() string {
	if value := os.Getenv("GO"); value != "" {
		return value
	}
	return "go"
}

var repositoryGitEnvironment = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_GLOBAL":                true,
	"GIT_CONFIG_KEY_":                  true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_CONFIG_SYSTEM":                true,
	"GIT_CONFIG_VALUE_":                true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NAMESPACE":                    true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_WORK_TREE":                    true,
}

func cleanEnvironment() []string {
	environment := os.Environ()
	cleaned := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !isRepositoryGitEnvironment(name) {
			cleaned = append(cleaned, entry)
		}
	}
	return cleaned
}

func isRepositoryGitEnvironment(name string) bool {
	if repositoryGitEnvironment[name] {
		return true
	}
	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}
