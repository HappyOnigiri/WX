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
	"sync"
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
	files, err := createRunFiles(jsonPath, stderrPath, logPath)
	if err != nil {
		return result, err
	}
	defer func() { _ = files.close() }()
	// ジョブがtimeoutで打ち切られても、どのテストで止まったかを残す必要がある。
	// 出力は終了後にまとめず、実行中に成果物とジョブログへ流す。
	sink := newSyncWriter(files.log, output)
	events := &eventText{out: sink}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = cfg.RepoRoot
	cmd.Env = os.Environ()
	cmd.Stdout = io.MultiWriter(files.json, events)
	cmd.Stderr = io.MultiWriter(files.stderr, sink)
	runErr := cmd.Run()
	events.flush()
	if err := files.close(); err != nil {
		return result, err
	}
	result.Exit, result.Signal = exitDetails(runErr)
	if runErr != nil && result.Exit == 0 {
		result.Exit = 1
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return result, err
	}
	if err := parseJSONL(data, &result); err != nil {
		return result, fmt.Errorf("parse %s JSON: %w", label, err)
	}
	excerpt, err := tailFile(logPath, 4000)
	if err != nil {
		return result, err
	}
	result.LogExcerpt = excerpt
	if result.Shuffle == "" {
		stderrData, err := os.ReadFile(stderrPath)
		if err != nil {
			return result, err
		}
		result.Shuffle = findShuffle(string(stderrData))
	}
	classifyResult(&result)
	result.FinishedAt = now()
	if runErr != nil && ctx.Err() != nil {
		result.Anomaly = "test process interrupted: " + ctx.Err().Error()
	}
	return result, nil
}

// runFilesはひとつの実行が書き出す成果物をまとめ、二重closeを無害にする。
type runFiles struct {
	json   *os.File
	stderr *os.File
	log    *os.File
	closed bool
}

func createRunFiles(jsonPath, stderrPath, logPath string) (*runFiles, error) {
	files := &runFiles{}
	for _, target := range []struct {
		path string
		file **os.File
	}{{jsonPath, &files.json}, {stderrPath, &files.stderr}, {logPath, &files.log}} {
		file, err := createFile(target.path)
		if err != nil {
			_ = files.close()
			return nil, err
		}
		*target.file = file
	}
	return files, nil
}

func (f *runFiles) close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	var err error
	for _, file := range []*os.File{f.json, f.stderr, f.log} {
		if file == nil {
			continue
		}
		err = errors.Join(err, file.Close())
	}
	return err
}

func createFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

// syncWriterは共有する出力先への書き込みを直列化する。
// go testのstdoutとstderrは別goroutineから届くため、排他しないと競合する。
// ログ側の書き込み失敗で実行を止めないよう、エラーは伝えない。
type syncWriter struct {
	mu      sync.Mutex
	writers []io.Writer
}

func newSyncWriter(writers ...io.Writer) *syncWriter {
	result := &syncWriter{}
	for _, writer := range writers {
		if writer != nil {
			result.writers = append(result.writers, writer)
		}
	}
	return result
}

func (w *syncWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, writer := range w.writers {
		_, _ = writer.Write(data)
	}
	return len(data), nil
}

// eventTextはtest2jsonの行からOutputだけを取り出して流す。
// JSONとして読めない行はそのまま流し、枠外に出たビルドエラーなども失わない。
type eventText struct {
	out  io.Writer
	line []byte
}

func (w *eventText) Write(data []byte) (int, error) {
	w.line = append(w.line, data...)
	for {
		index := bytes.IndexByte(w.line, '\n')
		if index < 0 {
			break
		}
		w.emit(w.line[:index+1])
		w.line = append(w.line[:0], w.line[index+1:]...)
	}
	// 改行の来ない長大な行でメモリを持ち続けないよう、parseJSONLと同じ上限で吐き出す。
	if len(w.line) > 16*1024*1024 {
		w.flush()
	}
	return len(data), nil
}

func (w *eventText) flush() {
	if len(w.line) == 0 {
		return
	}
	w.emit(w.line)
	w.line = w.line[:0]
}

func (w *eventText) emit(line []byte) {
	var event testEvent
	if err := json.Unmarshal(line, &event); err != nil {
		_, _ = w.out.Write(line)
		return
	}
	if event.Output == "" {
		return
	}
	_, _ = io.WriteString(w.out, event.Output)
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
		if seed := findShuffle(event.Output); seed != "" {
			if result.Shuffle == "" {
				result.Shuffle = seed
			}
			if event.Package != "" {
				result.ShuffleByPackage[event.Package] = seed
			}
		}
		if event.Package != "" {
			if result.Package == "" {
				result.Package = event.Package
			}
			if event.Test != "" {
				key := event.Package + "\x00" + event.Test
				result.Tests[key] = append(result.Tests[key], event)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
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

// tailFileはlogの末尾limitバイトを返す。全体をメモリへ載せずに証拠だけを取り出す。
func tailFile(path string, limit int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() <= int64(limit) {
		data, err := io.ReadAll(file)
		return string(data), err
	}
	buffer := make([]byte, limit)
	if _, err := file.ReadAt(buffer, info.Size()-int64(limit)); err != nil {
		return "", err
	}
	return "... (truncated) ...\n" + string(buffer), nil
}

func sortStrings(values []string) {
	sort.Strings(values)
}
