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

// excerptLimit は1つのテストについて残す失敗ログの上限バイト数。
// citest の失敗抜粋と同じ大きさにして、issue本文が切り詰められにくい量に抑える。
const excerptLimit = 4000

// emptyRoundLimit はテストイベントが1つも出ないラウンドを許す連続回数。
// ビルド失敗のまま20コンテナ×期限いっぱい空回りさせないための打ち切りである。
const emptyRoundLimit = 2

// huntState はラウンドをまたいだ集計を持つ。
// Tallies は root のテスト関数だけを数える。サブテストの結果は root の終端イベントに現れるため、
// 両方を数えると1ラウンドの成否を二重に数えることになる。
type huntState struct {
	rounds        []roundRecord
	tallies       map[gotest.TestID]gotest.Tally
	failingNames  map[gotest.TestID][]string
	excerpts      map[gotest.TestID]string
	declarations  map[string]map[string]declaration
	diagnostics   []string
	startedAt     time.Time
	finishedAt    time.Time
	resolver      *gotest.Resolver
	failedRounds  int
	anomalyRounds int
}

func newHuntState(cfg config) *huntState {
	return &huntState{
		tallies:      make(map[gotest.TestID]gotest.Tally),
		failingNames: make(map[gotest.TestID][]string),
		excerpts:     make(map[gotest.TestID]string),
		startedAt:    now(),
		resolver:     &gotest.Resolver{GoCommand: cfg.GoCommand, RepoRoot: cfg.RepoRoot},
	}
}

// repeatRounds は期限までラウンドを繰り返す。
// 期限の判定はラウンドの開始前に行うので、最後のラウンドは期限を越えて走る。
// 直近で最も長かったラウンドが残り時間に収まらないときは、job timeoutで結果を失わないよう打ち切る。
func repeatRounds(ctx context.Context, cfg config, output io.Writer) (*huntState, error) {
	command, err := commandWithJSON(cfg.Command)
	if err != nil {
		return nil, err
	}
	state := newHuntState(cfg)
	var longest time.Duration
	empty := 0
	for elapsed := time.Duration(0); elapsed < cfg.Deadline; elapsed = time.Since(state.startedAt) {
		round := len(state.rounds) + 1
		_, _ = fmt.Fprintf(output, "=== round %d starts at %s\n", round, time.Since(state.startedAt).Round(time.Second))
		began := now()
		result, logPath, err := executeRound(ctx, cfg, command, round, output)
		if err != nil {
			return nil, err
		}
		duration := time.Since(began)
		if duration > longest {
			longest = duration
		}
		state.record(result, round, logPath, duration, output)
		if len(result.Tests) == 0 {
			empty++
			if empty >= emptyRoundLimit {
				state.diagnostics = append(state.diagnostics,
					fmt.Sprintf("stopped after %d rounds without any test event; the packages likely fail to build", empty))
				_, _ = fmt.Fprintln(output, "=== stopping: rounds produced no test events")
				break
			}
		} else {
			empty = 0
		}
		if ctx.Err() != nil {
			state.diagnostics = append(state.diagnostics, "hunt interrupted: "+ctx.Err().Error())
			break
		}
		if remaining := cfg.Deadline - time.Since(state.startedAt); remaining < longest {
			_, _ = fmt.Fprintf(output, "=== stopping: the remaining time is shorter than the longest round (%s)\n", longest.Round(time.Second))
			break
		}
	}
	state.finishedAt = now()
	if err := state.resolveDeclarations(ctx); err != nil {
		return nil, err
	}
	return state, nil
}

// executeRound は1ラウンドを走らせ、stdoutのJSONLとstderrを別ファイルへ書く。
// 2>&1 でまとめるとJSONLが壊れるため、人間可読のログだけを両方から組み立てる。
func executeRound(ctx context.Context, cfg config, command []string, round int, output io.Writer) (gotest.Result, string, error) {
	result := gotest.Result{Tests: make(map[gotest.TestID][]gotest.Event), ShuffleByPackage: make(map[string]string)}
	jsonPath := filepath.Join(cfg.ReportDir, fmt.Sprintf("round-%d.jsonl", round))
	stderrPath := filepath.Join(cfg.ReportDir, fmt.Sprintf("round-%d.stderr", round))
	logPath := filepath.Join(cfg.LogDir, fmt.Sprintf("round-%d.log", round))
	files, err := createRunFiles(jsonPath, stderrPath, logPath)
	if err != nil {
		return result, logPath, err
	}
	defer func() { _ = files.close() }()
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
		return result, logPath, err
	}
	result.Exit, result.Signal = gotest.ExitDetails(runErr)
	if runErr != nil && result.Exit == 0 {
		result.Exit = 1
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return result, logPath, err
	}
	// 途中で切れたJSONLでもそこまでのイベントを集計する。ParseJSONLはMalformedを立てて読み進める。
	if err := gotest.ParseJSONL(data, &result); err != nil {
		return result, logPath, fmt.Errorf("parse round %d JSON: %w", round, err)
	}
	if result.Shuffle == "" {
		stderrData, err := os.ReadFile(stderrPath)
		if err != nil {
			return result, logPath, err
		}
		result.Shuffle = gotest.FindShuffle(string(stderrData))
	}
	gotest.Classify(&result)
	// JSONLとstderrはartifactに載せない。証拠は人間可読のログとmanifestに残す。
	_ = os.Remove(jsonPath)
	_ = os.Remove(stderrPath)
	return result, logPath, nil
}

// record は1ラウンドの結果を集計へ畳み込み、成功したラウンドのログを捨てる。
func (s *huntState) record(result gotest.Result, round int, logPath string, duration time.Duration, output io.Writer) {
	failed := result.Status != "passed"
	entry := roundRecord{
		Round:      round,
		Status:     result.Status,
		Exit:       result.Exit,
		Signal:     result.Signal,
		Anomaly:    result.Anomaly,
		Shuffle:    result.Shuffle,
		DurationMS: duration.Milliseconds(),
		TestEvents: len(result.Tests),
	}
	if failed {
		s.failedRounds++
		entry.Log = filepath.Base(logPath)
		_, _ = fmt.Fprintf(output, "=== failure in round %d\n", round)
	} else if err := os.Remove(logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.diagnostics = append(s.diagnostics, fmt.Sprintf("round %d: remove log: %v", round, err))
	}
	if result.Anomaly != "" {
		s.anomalyRounds++
	}
	s.rounds = append(s.rounds, entry)
	s.fold(result)
}

// fold はラウンドの終端イベントを root のテスト関数ごとに数え、失敗した名前と抜粋を拾う。
func (s *huntState) fold(result gotest.Result) {
	for id, tally := range gotest.Counts(result) {
		root := gotest.TestID{Package: id.Package, Test: gotest.RootTestName(id.Test)}
		if id.Test == root.Test {
			current := s.tallies[root]
			current.Pass += tally.Pass
			current.Fail += tally.Fail
			current.Skip += tally.Skip
			s.tallies[root] = current
		}
		if tally.Fail == 0 {
			continue
		}
		s.failingNames[root] = appendUnique(s.failingNames[root], id.Test)
		if _, exists := s.excerpts[root]; !exists {
			s.excerpts[root] = testExcerpt(result, root)
		}
	}
}

// testExcerpt は root のテスト関数とそのサブテストの出力を、届いた順に集める。
// root 自身の終端イベントには失敗の中身が付かないため、サブテストの出力まで含める。
func testExcerpt(result gotest.Result, root gotest.TestID) string {
	var builder strings.Builder
	for _, event := range result.Events {
		if event.Output == "" || event.Package != root.Package || event.Test == "" {
			continue
		}
		if gotest.RootTestName(event.Test) != root.Test {
			continue
		}
		builder.WriteString(event.Output)
	}
	text := builder.String()
	if len(text) <= excerptLimit {
		return text
	}
	return "... (truncated) ...\n" + text[len(text)-excerptLimit:]
}

// deterministicFailures は1度も成功しなかったテストを "package.Test" の形で並べる。
// 宣言を解決できずmanifestから落ちたテストも拾えるよう、集計そのものから数える。
func (s *huntState) deterministicFailures() []string {
	var names []string
	for id := range s.failingNames {
		if tally := s.tallies[id]; tally.Fail > 0 && tally.Pass == 0 {
			names = append(names, id.Package+"."+id.Test)
		}
	}
	sort.Strings(names)
	return names
}

// resolveDeclarations は失敗したテストの宣言をまとめて引く。
// 解決できないパッケージは診断へ落とし、他のテストの報告を止めない。
func (s *huntState) resolveDeclarations(ctx context.Context) error {
	packages := make([]string, 0, len(s.failingNames))
	seen := make(map[string]bool)
	for id := range s.failingNames {
		if !seen[id.Package] {
			seen[id.Package] = true
			packages = append(packages, id.Package)
		}
	}
	if len(packages) == 0 {
		return nil
	}
	sort.Strings(packages)
	found, err := s.resolver.Declarations(ctx, packages...)
	if err != nil {
		s.diagnostics = append(s.diagnostics, fmt.Sprintf("resolve test declarations: %v", err))
		return nil
	}
	s.declarations = found
	return nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// runFiles はひとつのラウンドが書き出すファイルをまとめ、二重closeを無害にする。
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

// syncWriter は共有する出力先への書き込みを直列化する。
// go testのstdoutとstderrは別goroutineから届くため、排他しないと競合する。
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
