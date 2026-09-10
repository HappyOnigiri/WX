package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// test2jsonのOutputに残るshuffle seedの表記を抽出する。
var shufflePattern = regexp.MustCompile(`(?i)(?:shuffle seed|test\.shuffle)[ :=]+([0-9]+)`)

func executeRun(ctx context.Context, cfg config, command []string, label, coverage string, output io.Writer) (testResult, error) {
	started := now()
	_ = coverage
	jsonPath := filepath.Join(cfg.ReportDir, label+".jsonl")
	stderrPath := filepath.Join(cfg.ReportDir, label+".stderr")
	logPath := filepath.Join(cfg.ReportDir, label+".log")
	result := testResult{Tests: make(map[string][]testEvent), ShuffleByPackage: make(map[string]string), StartedAt: started}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = cfg.RepoRoot
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result.Exit, result.Signal = exitDetails(err)
	if err != nil && result.Exit == 0 {
		result.Exit = 1
	}
	if err := writeFile(jsonPath, stdout.Bytes()); err != nil {
		return result, err
	}
	if err := writeFile(stderrPath, stderr.Bytes()); err != nil {
		return result, err
	}
	if err := parseJSONL(stdout.Bytes(), &result); err != nil {
		return result, fmt.Errorf("parse %s JSON: %w", label, err)
	}
	log := restoreLog(result.Events)
	if stderr.Len() > 0 {
		log += stderr.String()
	}
	result.LogExcerpt = tail(log, 4000)
	if err := writeFile(logPath, []byte(log)); err != nil {
		return result, err
	}
	if output != nil {
		_, _ = io.WriteString(output, log)
	}
	result.Shuffle = findShuffle(log)
	classifyResult(&result)
	result.FinishedAt = now()
	if err != nil && ctx.Err() != nil {
		result.Anomaly = "test process interrupted: " + ctx.Err().Error()
	}
	return result, nil
}

func parseJSONL(data []byte, result *testResult) error {
	if result.Tests == nil {
		result.Tests = make(map[string][]testEvent)
	}
	if result.ShuffleByPackage == nil {
		result.ShuffleByPackage = make(map[string]string)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// 生成されたdiffなどで出力行が長くなっても証拠を切り詰めない。
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event testEvent
		if err := json.Unmarshal(line, &event); err != nil {
			result.Malformed = true
			continue
		}
		result.Events = append(result.Events, event)
		if event.Package != "" {
			if result.Package == "" {
				result.Package = event.Package
			}
			if event.Test != "" {
				key := event.Package + "\x00" + event.Test
				result.Tests[key] = append(result.Tests[key], event)
			}
			if seed := findShuffle(event.Output); seed != "" {
				result.ShuffleByPackage[event.Package] = seed
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func restoreLog(events []testEvent) string {
	var builder strings.Builder
	for _, event := range events {
		if event.Output == "" {
			continue
		}
		_, _ = builder.WriteString(event.Output)
	}
	return builder.String()
}

func classifyResult(result *testResult) {
	namedFailures := failedTests(*result)
	packageFailures := make(map[string]bool)
	packageTerminals := make(map[string]bool)
	packageStarts := make(map[string]bool)
	for _, event := range result.Events {
		if event.Package != "" && event.Test == "" {
			switch event.Action {
			case "start":
				packageStarts[event.Package] = true
			case "pass", "fail", "skip":
				packageTerminals[event.Package] = true
				if event.Action == "fail" {
					packageFailures[event.Package] = true
				}
			}
		}
		if event.Action == "fail" && event.Test == "" && event.FailedBuild != "" {
			result.Anomaly = "build failure: " + event.FailedBuild
		}
		// 名前付きテストに紐づく出力は、recover済みpanicの記録やエラー文字列の検証でも
		// "panic:"を含み得る。異常として扱うのはテスト名の無い出力だけに限る。
		if event.Test == "" && strings.Contains(strings.ToLower(event.Output), "panic:") {
			result.Anomaly = "panic outside a named test"
		}
	}
	if result.Malformed {
		result.Anomaly = "test2json output contained malformed JSON"
	}
	for packageName := range packageFailures {
		if len(namedFailures[packageName]) == 0 && result.Anomaly == "" {
			result.Anomaly = "package failed without a named test: " + packageName
		}
	}
	for packageName := range packageStarts {
		if !packageTerminals[packageName] && result.Anomaly == "" {
			result.Anomaly = "package did not reach a terminal test2json event: " + packageName
		}
	}
	if result.Exit != 0 && len(namedFailures) == 0 && result.Anomaly == "" {
		result.Anomaly = "test process failed without a named test failure"
	}
	if result.Exit < 0 && result.Anomaly == "" {
		result.Anomaly = "test process terminated by " + result.Signal
	}
	result.Status = "passed"
	if result.Exit != 0 || len(failedTests(*result)) > 0 || result.Anomaly != "" {
		result.Status = "failed"
	}
}

func failedTests(result testResult) map[string][]string {
	failed := make(map[string][]string)
	for key, events := range result.Tests {
		if len(events) == 0 {
			continue
		}
		last := events[len(events)-1]
		if last.Action != "fail" {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		failed[parts[0]] = append(failed[parts[0]], parts[1])
	}
	for packageName := range failed {
		sortStrings(failed[packageName])
	}
	return failed
}

func exitDetails(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return 1, ""
	}
	if status, ok := exitError.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -1, status.Signal().String()
	}
	return exitError.ExitCode(), ""
}

func findShuffle(log string) string {
	match := shufflePattern.FindStringSubmatch(log)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func goVersion() string {
	command := exec.CommandContext(context.Background(), "go", "version")
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func durationMS(start, finish time.Time) int64 {
	return finish.Sub(start).Milliseconds()
}

func tail(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return "... (truncated) ...\n" + value[len(value)-limit:]
}

func sortStrings(values []string) {
	sort.Strings(values)
}
