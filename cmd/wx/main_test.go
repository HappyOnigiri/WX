package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/testsupport"
)

func TestRunConfigRejectsUnnormalizablePathBeforeSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	loop := filepath.Join(home, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}

	if code := runConfig(context.Background(), []string{"storage.worktree_root", loop}); code != 1 {
		t.Fatalf("runConfig exit=%d want=1", code)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid config was persisted: %v", err)
	}
}

type multiKeyStatusHandler struct{}

func (multiKeyStatusHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	return map[string]any{"zeta": 1, "alpha": 2, "mu": 3}, nil
}

func TestRunRPCDisplaySortsHumanReadableOutputByKey(t *testing.T) {
	// /tmp 配下の短い HOME で Unix socket path を sun_path の長さ上限内に収める。
	// t.TempDir() の深い path では収まらない。
	home, err := os.MkdirTemp("/tmp", "wx-status-sort-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{Socket: socket, Handler: multiKeyStatusHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	testsupport.WaitForSocket(t, socket, done)
	stdout := captureStdout(t, func() {
		if code := runRPCDisplay(ctx, "Status", nil); code != 0 {
			t.Fatalf("runRPCDisplay exit=%d", code)
		}
	})
	wantOrder := []string{"alpha", "mu", "zeta"}
	lastIndex := -1
	for _, key := range wantOrder {
		index := strings.Index(stdout, key)
		if index < 0 {
			t.Fatalf("output missing key %q: %q", key, stdout)
		}
		if index < lastIndex {
			t.Fatalf("keys were not printed in sorted order: %q", stdout)
		}
		lastIndex = index
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunRPCDisplayExplainsWhenDaemonIsNotReady(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stderr := captureStderr(t, func() {
		if code := runRPCDisplay(context.Background(), "Status", nil); code != 1 {
			t.Fatalf("runRPCDisplay exit=%d, want 1 when daemon is unavailable", code)
		}
	})
	if want := "wx daemon is not running or still starting; try again shortly"; !strings.Contains(stderr, want) {
		t.Fatalf("daemon connection guidance=%q, want substring %q", stderr, want)
	}
	if strings.Contains(stderr, "dial unix") {
		t.Fatalf("daemon connection guidance leaked socket implementation detail: %q", stderr)
	}
}

// daemon へ RPC するコマンドは、未待受のとき socket path や dial の syscall error を見せず status と同じ案内で終わる。
func TestDaemonUnavailableGuidanceIsSharedAcrossCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "status", args: []string{"status"}},
		{name: "slots", args: []string{"slots"}},
		{name: "gc", args: []string{"gc", "--dry-run"}},
		{name: "prune", args: []string{"prune", "--dry-run"}},
		{name: "clear", args: []string{"clear", "--dry-run"}},
		{name: "forget", args: []string{"forget", "/nonexistent"}},
		{name: "retry-standby", args: []string{"retry-standby", "/nonexistent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			stderr := captureStderr(t, func() {
				if code := run(context.Background(), tc.args); code != 1 {
					t.Fatalf("wx %s exit=%d, want 1 when daemon is unavailable", tc.name, code)
				}
			})
			if !strings.Contains(stderr, daemonUnavailableMessage) {
				t.Fatalf("stderr=%q, want substring %q", stderr, daemonUnavailableMessage)
			}
			if strings.Contains(stderr, "dial unix") {
				t.Fatalf("stderr leaked socket implementation detail: %q", stderr)
			}
		})
	}
}

func TestRunDoctorFallsBackToLocalFindingsWhenDaemonCannotConnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stdout := captureStdout(t, func() {
		if code := runDoctor(context.Background(), []string{"--json"}); code != 1 {
			t.Fatalf("runDoctor exit=%d, want 1 when daemon is unavailable", code)
		}
	})
	var reply diag.Reply
	if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
		t.Fatalf("local doctor output is not JSON: %v\n%s", err, stdout)
	}
	if reply.SchemaVersion != state.JSONSchemaVersion {
		t.Fatalf("schema_version=%d, want %d", reply.SchemaVersion, state.JSONSchemaVersion)
	}
	byCheck := map[string]diag.Finding{}
	for _, finding := range reply.Findings {
		byCheck[finding.Check] = finding
	}
	// --json は -v に左右されず全検査を返す。
	for _, check := range append([]string{
		diag.CheckConfig, diag.CheckGit, diag.CheckSocket, diag.CheckStateDatabase, diag.CheckLaunchAgent,
		diag.CheckWorktreeRoot, diag.CheckReadinessHooks, diag.CheckDaemon, diag.CheckSQLite,
	}, diag.StoreDependentChecks()...) {
		if _, ok := byCheck[check]; !ok {
			t.Fatalf("local doctor findings missing %q: %+v", check, reply.Findings)
		}
	}
	if got := byCheck[diag.CheckSQLite]; got.Severity != diag.SeverityUnchecked {
		t.Fatalf("local sqlite finding=%+v, want it reported as unchecked", got)
	}
	daemon := byCheck[diag.CheckDaemon]
	if daemon.Severity != diag.SeverityProblem || !strings.Contains(daemon.Cause, "connect") || daemon.Action == "" {
		t.Fatalf("local daemon finding=%+v", daemon)
	}
}

func TestRunDoctorReportsAnOlderDaemonThatCannotReturnFindings(t *testing.T) {
	findings := staleDaemonFindings(diag.Reply{SchemaVersion: diag.FindingsSchemaVersion - 1})
	if len(findings) != 1 || findings[0].Severity != diag.SeverityProblem || !strings.Contains(findings[0].Action, "wx daemon restart") {
		t.Fatalf("stale daemon findings=%+v", findings)
	}
	if got := staleDaemonFindings(diag.Reply{Findings: []diag.Finding{{Check: diag.CheckDaemon}}}); got != nil {
		t.Fatalf("stale daemon findings for a current reply=%+v", got)
	}
	// 読める schema で結果が空なら原因は schema 版ではないため、再起動を促さない。
	empty := staleDaemonFindings(diag.Reply{SchemaVersion: diag.FindingsSchemaVersion})
	if len(empty) != 1 || empty[0].Severity != diag.SeverityProblem {
		t.Fatalf("empty reply findings=%+v", empty)
	}
	if strings.Contains(empty[0].Action, "wx daemon restart") || strings.Contains(empty[0].Cause, "or newer") {
		t.Fatalf("empty reply finding blamed the schema version=%+v", empty[0])
	}
}

func TestRunGCReturnsNonZeroForPendingReport(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wx-gc-result-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{
		Socket: socket,
		Handler: commandHandler{gcResult: &daemon.GCResult{
			Candidates: 2,
			Pending:    1,
			Reasons:    []daemon.GCReason{{Target: "worktree slot", Status: "pending", Reason: "ownership could not be proven"}},
		}},
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	testsupport.WaitForSocket(t, socket, done)
	var code int
	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() { code = runGC(ctx, nil) })
		if !strings.Contains(stderr, "ownership could not be proven") {
			t.Fatalf("GC reason missing from stderr: %q", stderr)
		}
	})
	if code != 1 {
		t.Fatalf("runGC exit=%d, want 1 for pending report", code)
	}
	for _, field := range []string{"candidates: 2", "scheduled: 0", "pending: 1", "failed: 0"} {
		if !strings.Contains(stdout, field) {
			t.Fatalf("GC output=%q missing %q", stdout, field)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunSlotsListsLendableAndAll(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wx-slots-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := serveUntilCanceled(t, socket, commandHandler{
		slots: []map[string]any{
			{"slot_id": "leased", "state": "LEASED", "session_id": "active", "agent": "codex", "repositories": []any{"/src/api"}, "copy_mode": "cow", "measurement": "log2phys_first_last", "measured_at": "2026-01-01T00:00:00Z", "allocated_bytes": float64(4 << 30), "exclusive_bytes": float64(1258291200), "path": "/wx/leased"},
			{"slot_id": "tiny", "state": "SNAPSHOTTED", "repositories": []any{"/src/api", "/src/web"}, "copy_mode": "copy", "measurement": "log2phys_first_last", "measured_at": "2026-01-01T00:00:00Z", "allocated_bytes": float64(1), "exclusive_bytes": float64(1), "path": "/wx/tiny"},
			{"slot_id": "warm", "state": "READY", "measurement": "pending", "path": "/wx/warm"},
		},
		allSlots: []map[string]any{
			{"slot_id": "leased", "state": "LEASED", "session_id": "active", "agent": "codex"},
			{"session_id": "archived", "session_state": "ARCHIVED", "agent": "codex"},
		},
	})
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("RPC server stopped with error: %v", err)
		}
	})

	stdout := captureStdout(t, func() {
		if code := runSlots(context.Background(), nil); code != 0 {
			t.Fatalf("runSlots exit=%d", code)
		}
	})
	row := func(path string) []string {
		t.Helper()
		for _, line := range strings.Split(stdout, "\n") {
			if strings.HasSuffix(line, path) {
				return strings.Fields(line)
			}
		}
		t.Fatalf("slots output=%q missing row for %q", stdout, path)
		return nil
	}
	// 列の意味は見出しでしか分からないため、見出しの有無と並びを表の契約として固定する。
	header := strings.Fields(strings.Split(stdout, "\n")[0])
	if !slices.Equal(header, []string{"SLOT", "STATE", "REPO", "SESSION", "AGENT", "COPY", "SIZE(MB)", "PATH"}) {
		t.Fatalf("slots header=%q", header)
	}
	// 容量は共有ぶんを除いた占有量を MB で 1 列だけ出し、3 桁ごとに区切る。
	if got := row("/wx/leased"); !slices.Equal(got, []string{"leased", "LEASED", "api", "active", "codex", "cow", "1,200", "/wx/leased"}) {
		t.Fatalf("leased row=%q", got)
	}
	// 1MB 未満は切り上げるため、実体のある slot が 0 と表示されることはない。
	// 既定でも SNAPSHOTTED は並び、multi-repo の slot は REPO 列に basename をカンマ区切りで出す。
	if got := row("/wx/tiny"); !slices.Equal(got, []string{"tiny", "SNAPSHOTTED", "api,web", "-", "-", "copy", "1", "/wx/tiny"}) {
		t.Fatalf("tiny row=%q", got)
	}
	// 測定前の slot は方式の欄に pending を出し、使用量が未知であることと 0 バイトを取り違えないようにする。
	if got := row("/wx/warm"); !slices.Equal(got, []string{"warm", "READY", "-", "-", "-", "pending", "-", "/wx/warm"}) {
		t.Fatalf("warm row=%q", got)
	}
	if strings.Contains(stdout, "archived") {
		t.Fatalf("slots output=%q", stdout)
	}

	stdout = captureStdout(t, func() {
		if code := runSlots(context.Background(), []string{"--all"}); code != 0 {
			t.Fatalf("runSlots --all exit=%d", code)
		}
	})
	if !strings.Contains(stdout, "archived") {
		t.Fatalf("--all slots output=%q", stdout)
	}
}

// captureStdout は fn 中の os.Stdout を差し替え、書込み内容を返す。
// この package の test は並列実行しないため、process 全体の差し替えでも安全である。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	// t.Fatal/t.Fatalf は runtime.Goexit で後続処理を飛ばすため、defer で os.Stdout を復元する。
	// 閉じた pipe へのリダイレクトを test binary の残りへ残さない。
	defer func() { os.Stdout = original }()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(read); err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRunHookHelpPrintsUsageAndExitsTwo(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		stderr := captureStderr(t, func() {
			if code := runHook(context.Background(), args); code != 2 {
				t.Fatalf("runHook(%v) exit=%d want=2", args, code)
			}
		})
		if !strings.Contains(stderr, "Usage: wx hook") {
			t.Fatalf("runHook(%v) stderr=%q missing usage", args, stderr)
		}
	}
}

func TestRunHookRejectsUnknownFlag(t *testing.T) {
	captureStderr(t, func() {
		if code := runHook(context.Background(), []string{"--not-a-flag", "session-start"}); code != 2 {
			t.Fatalf("runHook exit=%d want=2", code)
		}
	})
}

// TestEverySubcommandHasAUniformPflagContract は全 subcommand の --help と未知 flag の終了コード・出力先を確認する。
// 入力は command 固有の処理より前に pflag が拒否するため、daemon や filesystem には触れない。
func TestEverySubcommandHasAUniformPflagContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	type contract struct {
		name         string
		run          func(ctx context.Context, args []string) int
		helpExit     int
		helpOnStdout bool
	}
	contracts := []contract{
		{name: "status", run: func(ctx context.Context, args []string) int { return runRPCDisplay(ctx, "Status", args) }, helpExit: 0, helpOnStdout: true},
		{name: "doctor", run: func(ctx context.Context, args []string) int { return runRPCDisplay(ctx, "Doctor", args) }, helpExit: 0, helpOnStdout: true},
		{name: "gc", run: runGC, helpExit: 0, helpOnStdout: true},
		{name: "slots", run: runSlots, helpExit: 0, helpOnStdout: true},
		{name: "config", run: runConfig, helpExit: 0, helpOnStdout: true},
		{name: "resume", run: runResume, helpExit: 0, helpOnStdout: true},
		{name: "daemon", run: runDaemon, helpExit: 0, helpOnStdout: true},
		{name: "forget", run: runForget, helpExit: 0, helpOnStdout: true},
		{name: "hook", run: runHook, helpExit: 2, helpOnStdout: false},
	}
	for _, c := range contracts {
		t.Run(c.name, func(t *testing.T) {
			for _, helpFlag := range []string{"--help", "-h"} {
				var code int
				var output string
				if c.helpOnStdout {
					output = captureStdout(t, func() { code = c.run(context.Background(), []string{helpFlag}) })
				} else {
					output = captureStderr(t, func() { code = c.run(context.Background(), []string{helpFlag}) })
				}
				if code != c.helpExit {
					t.Fatalf("%s %s exit=%d want=%d", c.name, helpFlag, code, c.helpExit)
				}
				if want := "Usage: wx " + c.name; !strings.Contains(output, want) {
					t.Fatalf("%s %s output=%q missing %q", c.name, helpFlag, output, want)
				}
			}
			var code int
			stderr := captureStderr(t, func() {
				code = c.run(context.Background(), []string{"--this-flag-does-not-exist"})
			})
			if code == 0 {
				t.Fatalf("%s accepted an unrecognized flag", c.name)
			}
			if want := "Usage: wx " + c.name; !strings.Contains(stderr, want) {
				t.Fatalf("%s unrecognized-flag stderr=%q missing %q", c.name, stderr, want)
			}
			// ContinueOnError の pflag はエラーを書かず、usage だけでは拒否した引数が分からない。
			// 診断には typo と未対応 option を区別できる flag 名を含める。
			if !strings.Contains(stderr, "--this-flag-does-not-exist") {
				t.Fatalf("%s unrecognized-flag stderr=%q does not name the rejected flag", c.name, stderr)
			}
		})
	}
}

// captureStderr は fn 中の os.Stderr を差し替え、書込み内容を返す。
// この package の test は並列実行しないため、process 全体の差し替えでも安全である。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = original }()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(read); err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
