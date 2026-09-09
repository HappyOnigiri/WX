package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

// leaseFixture は貸出コマンドの client と、要求を記録する fake daemon を用意する。
// worktree root は貸出 path の直上に置き、descriptor 束縛の検査を production と同じ経路で通す。
func leaseFixture(t *testing.T) (Client, *launcherHandler, string, context.Context) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "wx-lease-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "worktrees")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, identity, err := domain.OpenOwnedDirectory(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(base, "wxd.sock")
	handler := &launcherHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: workspace, RootIdentity: identity, SourceWorkspace: base, Ready: true}}
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	waitForSocket(t, socket, done)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	// PolicyRoot は base 配下を Git repository として解決できないため、貸出前の方針検査は判定を daemon へ委ねる。
	return Client{RPC: rpc.Client{Socket: socket, Timeout: 5 * time.Second}, Config: cfg}, handler, base, ctx
}

// leaseRequest は記録された要求のうち、最初の ResolveAndLease の Params を返す。
func leaseRequest(t *testing.T, handler *launcherHandler) rpc.ResolveAndLeaseParams {
	t.Helper()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	var params rpc.ResolveAndLeaseParams
	if err := json.Unmarshal(handler.leaseParams, &params); err != nil {
		t.Fatalf("lease params=%s err=%v", handler.leaseParams, err)
	}
	return params
}

// wx shell は agent 起動と同じ経路で、lease 種別を載せた要求・descriptor 束縛された CWD・
// agent プロセスの登録・終了時の返却を通る。
func TestRunLeaseShellSharesTheAgentLaunchPath(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	shell := filepath.Join(base, "shell")
	result := filepath.Join(base, "shell-pwd")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\npwd -P > \""+result+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client.Config.Lease.Shell = shell
	if exit := client.RunLeaseShell(ctx, []string{"main"}, ""); exit != 0 {
		t.Fatalf("RunLeaseShell exit=%d", exit)
	}
	params := leaseRequest(t, handler)
	if params.LeaseKind != "shell" || params.Agent != leaseAgentKindShell {
		t.Fatalf("lease params=%+v, want a shell lease recorded as %s", params, leaseAgentKindShell)
	}
	if params.ClientPID != os.Getpid() || len(params.Branches) != 1 || params.Branches[0] != "main" {
		t.Fatalf("lease params=%+v, want this process and the requested branch", params)
	}
	// descriptor 束縛された lease directory で起動し、返却と agent 登録も agent 起動と同じ経路を通る。
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != handler.lease.Path {
		t.Fatalf("shell CWD=%q, want the lease path %q", got, handler.lease.Path)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	for _, required := range []string{"ResolveAndLease", "RegisterAgentProcess", "Release"} {
		if !strings.Contains(methods, required) {
			t.Fatalf("methods=%s missing %s", methods, required)
		}
	}
}

// wx run は argv をそのまま実行し、終了コードをそのまま返す。
func TestRunLeaseCommandPassesArgumentsAndExitStatus(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	command := filepath.Join(base, "command")
	result := filepath.Join(base, "command-args")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s' \"$*\" > \""+result+"\"\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if exit := client.RunLeaseCommand(ctx, []string{command, "build", "--verbose"}, nil, ""); exit != 3 {
		t.Fatalf("RunLeaseCommand exit=%d, want the command status 3", exit)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "build --verbose" {
		t.Fatalf("command arguments=%q", got)
	}
	if params := leaseRequest(t, handler); params.LeaseKind != "command" || params.Agent != leaseAgentKindCommand {
		t.Fatalf("lease params=%+v, want a command lease", params)
	}
	// 実行するコマンドが無い呼び出しは引数エラーで終える。
	if exit := client.RunLeaseCommand(ctx, nil, nil, ""); exit != 2 {
		t.Fatalf("empty command exit=%d, want 2", exit)
	}
}

// wx new は貸出してパスを出力するだけで、Release も heartbeat も張らない。
// 環境の WX_SESSION_ID / WX_SESSION_TOKEN は親 session として要求へ載る。
func TestRunLeaseNewPrintsThePathWithoutFollowingTheProcess(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	t.Setenv("WX_SESSION_ID", "owner")
	t.Setenv("WX_SESSION_TOKEN", "owner-token")
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunLeaseNew(ctx, nil, false); exit != 0 {
			t.Fatalf("RunLeaseNew exit=%d", exit)
		}
	})
	if strings.TrimSpace(stdout) != handler.lease.Path {
		t.Fatalf("stdout=%q, want the lease path %q", stdout, handler.lease.Path)
	}
	params := leaseRequest(t, handler)
	if params.LeaseKind != "path" || params.Agent != leaseAgentKindPath || params.ClientPID != 0 {
		t.Fatalf("lease params=%+v, want a detached path lease", params)
	}
	if params.LeaseOwnerSessionID != "owner" || params.LeaseOwnerToken != "owner-token" {
		t.Fatalf("lease params=%+v, want the parent session from the environment", params)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	for _, forbidden := range []string{"Release", "Heartbeat", "RegisterAgentProcess"} {
		if strings.Contains(methods, forbidden) {
			t.Fatalf("methods=%s, wx new must not call %s", methods, forbidden)
		}
	}
	// --json は session ID とパスだけを出し、session token は出さない。
	stdout = captureLeaseStdout(t, func() {
		if exit := client.RunLeaseNew(ctx, nil, true); exit != 0 {
			t.Fatalf("RunLeaseNew --json exit=%d", exit)
		}
	})
	var reply map[string]any
	if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
		t.Fatalf("json output=%q err=%v", stdout, err)
	}
	if reply["session_id"] != handler.lease.SessionID || reply["path"] != handler.lease.Path || len(reply) != 2 {
		t.Fatalf("json reply=%+v, want only the session id and path", reply)
	}
	if strings.Contains(stdout, handler.lease.Token) {
		t.Fatalf("json output leaks the session token: %q", stdout)
	}
}

// 準備待ちが失敗したら、取った貸出を返却してから終わる。
// path 貸出は heartbeat も orphan 回収も持たないので、返さないと lease.ttl まで slot が残り、
// session id を出さないまま終わるため利用者は wx release もできない。
func TestRunLeaseNewReturnsTheLeaseWhenPreparationFails(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.waitReadyErr = errors.New("injected preparation failure")
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunLeaseNew(ctx, nil, false); exit != 1 {
			t.Fatalf("RunLeaseNew exit=%d, want 1", exit)
		}
	})
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("stdout=%q, want no path on failure", stdout)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if !strings.Contains(methods, "Release") {
		t.Fatalf("methods=%s, want the lease returned after a failed preparation", methods)
	}
}

// 親 session は ID と token の両方が揃ったときだけ要求へ載せる。
func TestLeaseOwnerFromEnvironmentNeedsBothIdentityAndToken(t *testing.T) {
	for name, test := range map[string]struct{ id, token, wantID string }{
		"both set":      {id: "owner", token: "token", wantID: "owner"},
		"missing token": {id: "owner"},
		"missing id":    {token: "token"},
		"neither":       {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WX_SESSION_ID", test.id)
			t.Setenv("WX_SESSION_TOKEN", test.token)
			id, token := leaseOwnerFromEnvironment()
			if id != test.wantID || (test.wantID == "" && token != "") {
				t.Fatalf("owner id=%q token=%q, want id %q", id, token, test.wantID)
			}
		})
	}
}

// wx release は ReleaseLease を呼び、応答の discarded で案内を分ける。
func TestRunLeaseReleaseReportsWhatHappened(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.releaseLeaseReply = map[string]any{"released": true, "discarded": true}
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunLeaseRelease(ctx, "session", true); exit != 0 {
			t.Fatalf("RunLeaseRelease exit=%d", exit)
		}
	})
	if !strings.Contains(stdout, "without requiring a snapshot") {
		t.Fatalf("discard output=%q", stdout)
	}
	// 保存が先に走った場合は、保存された事実と再実行を案内する。
	handler.releaseLeaseReply = map[string]any{"released": true, "discarded": false}
	stdout = captureLeaseStdout(t, func() {
		if exit := client.RunLeaseRelease(ctx, "session", true); exit != 0 {
			t.Fatalf("RunLeaseRelease exit=%d", exit)
		}
	})
	if !strings.Contains(stdout, "again to remove it") {
		t.Fatalf("pending removal output=%q", stdout)
	}
	// 既に削除まで進んだ slot では、何度実行しても変わらない再実行を案内しない。
	handler.releaseLeaseReply = map[string]any{"released": true, "discarded": false, "discard_pending": daemon.DiscardPendingRemoved}
	stdout = captureLeaseStdout(t, func() {
		if exit := client.RunLeaseRelease(ctx, "session", true); exit != 0 {
			t.Fatalf("RunLeaseRelease exit=%d", exit)
		}
	})
	if strings.Contains(stdout, "again to remove it") || !strings.Contains(stdout, "already removed") {
		t.Fatalf("already removed output=%q", stdout)
	}
	stdout = captureLeaseStdout(t, func() {
		if exit := client.RunLeaseRelease(ctx, "session", false); exit != 0 {
			t.Fatalf("RunLeaseRelease exit=%d", exit)
		}
	})
	if strings.TrimSpace(stdout) != "released session" {
		t.Fatalf("release output=%q", stdout)
	}
}

// worktree を使わない設定の workspace は、失敗（1）ではなく引数エラー（2）で終える。
func TestLeaseCommandsRejectWorkspacesWithoutAWorktree(t *testing.T) {
	disabled := errors.New("workspace /repo is configured not to use a worktree " + daemon.WorktreeDisabledMarker)
	if got := reportLeaseError(disabled); got != 2 {
		t.Fatalf("disabled worktree exit=%d, want 2", got)
	}
	if got := reportLeaseError(context.DeadlineExceeded); got != 1 {
		t.Fatalf("other failure exit=%d, want 1", got)
	}
}

// wx shell が起動するシェルは lease.shell → $SHELL → /bin/sh の順で決まる。
func TestLeaseShellPrefersConfigurationThenEnvironment(t *testing.T) {
	client := Client{Config: config.Defaults()}
	t.Setenv("SHELL", "/bin/bash")
	if got := client.leaseShell(); got != "/bin/bash" {
		t.Fatalf("shell without configuration=%q, want $SHELL", got)
	}
	client.Config.Lease.Shell = "/bin/zsh"
	if got := client.leaseShell(); got != "/bin/zsh" {
		t.Fatalf("configured shell=%q", got)
	}
	client.Config.Lease.Shell = ""
	t.Setenv("SHELL", "")
	if got := client.leaseShell(); got != defaultLeaseShell {
		t.Fatalf("shell without $SHELL=%q, want %q", got, defaultLeaseShell)
	}
}

// wx resume は貸出 session を受け付けず、wx shell --resume を案内して引数エラーで終える。
func TestRunResumeRejectsLeaseSessions(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.resumeStatus = map[string]any{"wx_session_id": "session", "agent": leaseAgentKindShell, "expired": false}
	stderr := captureStderrForLease(t, func() {
		if exit := client.RunResume(ctx, "session", "", nil, nil, false); exit != 2 {
			t.Fatalf("RunResume exit=%d, want 2", exit)
		}
	})
	if !strings.Contains(stderr, "wx shell --resume session") {
		t.Fatalf("stderr=%q, want the wx shell guidance", stderr)
	}
}

// captureLeaseStdout は stdout を差し替えて fn を実行し、書かれた内容を返す。
func captureLeaseStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureLeaseStream(t, &os.Stdout, fn)
}

// captureStderrForLease は stderr を差し替えて fn を実行し、書かれた内容を返す。
func captureStderrForLease(t *testing.T, fn func()) string {
	t.Helper()
	return captureLeaseStream(t, &os.Stderr, fn)
}

func captureLeaseStream(t *testing.T, stream **os.File, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "captured")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := *stream
	*stream = file
	defer func() {
		*stream = original
		_ = file.Close()
	}()
	fn()
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
