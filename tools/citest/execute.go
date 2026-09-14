package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HappyOnigiri/WX/tools/internal/gotest"
)

func executeRun(ctx context.Context, cfg config, command []string, label, coverage string, output io.Writer) (testResult, error) {
	started := now()
	_ = coverage
	jsonPath := filepath.Join(cfg.ReportDir, label+".jsonl")
	stderrPath := filepath.Join(cfg.ReportDir, label+".stderr")
	logPath := filepath.Join(cfg.ReportDir, label+".log")
	result := testResult{Tests: make(map[gotest.TestID][]testEvent), ShuffleByPackage: make(map[string]string), StartedAt: started}
	files, err := createRunFiles(jsonPath, stderrPath, logPath)
	if err != nil {
		return result, err
	}
	defer func() { _ = files.close() }()
	// ジョブがtimeoutで打ち切られても、どのテストで止まったかを残す必要がある。
	// 出力は終了後にまとめず、実行中に成果物とジョブログへ流す。
	sink := newSyncWriter(files.log, output)
	events := gotest.NewTextWriter(sink)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = cfg.RepoRoot
	cmd.Env = os.Environ()
	cmd.Stdout = io.MultiWriter(files.json, events)
	cmd.Stderr = io.MultiWriter(files.stderr, sink)
	runErr := cmd.Run()
	events.Flush()
	if err := files.close(); err != nil {
		return result, err
	}
	result.Exit, result.Signal = gotest.ExitDetails(runErr)
	if runErr != nil && result.Exit == 0 {
		result.Exit = 1
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return result, err
	}
	if err := gotest.ParseJSONL(data, &result); err != nil {
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
		result.Shuffle = gotest.FindShuffle(string(stderrData))
	}
	gotest.Classify(&result)
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
