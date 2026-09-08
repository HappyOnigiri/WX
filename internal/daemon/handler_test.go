package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestHandlerRejectsUnknownFieldsForEveryParameterizedMethod(t *testing.T) {
	t.Parallel()
	handler := Handler{}
	methods := []string{
		"ResolveAndLease", "WaitReady", "BindAgentSession",
		"Release", "ReleaseLease", "Heartbeat", "RegisterAgentProcess", "Resume", "ResumeStatus", "WorkspaceScope", "GC", "Sessions", "Forget", "RetryStandby",
	}
	for _, method := range methods {
		if _, err := handler.Handle(context.Background(), method, json.RawMessage(`{"unexpected":true}`)); err == nil {
			t.Errorf("%s accepted unknown field", method)
		}
	}
	if _, err := handler.Handle(context.Background(), "Unknown", nil); err == nil {
		t.Fatal("unknown RPC method succeeded")
	}
	var value struct {
		Known bool `json:"known"`
	}
	if err := decode(nil, &value); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerRoutesResumeAndFreshOperationsToManager(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	handler := Handler{Manager: manager}
	requests := map[string]string{
		"Resume":       `{"wx_session_id":"missing","agent":"codex","client_pid":1,"fresh":false}`,
		"ResumeStatus": `{"wx_session_id":"missing"}`,
		"Forget":       `{"path":"/missing"}`,
	}
	for method, raw := range requests {
		if _, err := handler.Handle(context.Background(), method, json.RawMessage(raw)); err == nil {
			t.Errorf("%s unexpectedly succeeded", method)
		}
	}
}

func TestDegradedHandlerAllowsOnlyReadOnlyDiagnostics(t *testing.T) {
	t.Parallel()
	handler := DegradedHandler{DatabasePath: "/state.db", OpenError: errors.New("corrupt")}
	for _, method := range []string{"Status", "Doctor"} {
		if result, err := handler.Handle(context.Background(), method, nil); err != nil || result == nil {
			t.Fatalf("%s result=%v err=%v", method, result, err)
		}
	}
	if _, err := handler.Handle(context.Background(), "GC", nil); err == nil {
		t.Fatal("degraded mutating method succeeded")
	}
}

func TestDegradedHandlerGuidanceDependsOnOpenError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		openError  error
		want       []string
		wantAbsent []string
	}{
		{
			name: "previous worktree layout",
			openError: fmt.Errorf(
				"%w: /state.db was created by a wx release that used the previous worktree layout and cannot be migrated; stop the daemon, remove that file, and remove the old worktree root once no session needs it",
				state.ErrPreviousWorktreeLayout,
			),
			want:       []string{"previous worktree layout", "stop the daemon", "remove that file", "remove the old worktree root"},
			wantAbsent: []string{"state.db.backups", "wx doctor"},
		},
		{
			name:      "other open failure",
			openError: errors.New("corrupt"),
			// 復旧案内は「保全してから検証済みバックアップへ戻す」ことを、Status の文面と doctor の finding の両方で保つ。
			want: []string{"corrupt", "/state.db.backups", "preserve"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := DegradedHandler{DatabasePath: "/state.db", OpenError: test.openError}
			for _, method := range []string{"Status", "Doctor"} {
				result, err := handler.Handle(context.Background(), method, nil)
				if err != nil {
					t.Fatalf("%s err=%v", method, err)
				}
				message := degradedDiagnosticMessage(t, method, result)
				assertGuidance(t, method, message, test.want, test.wantAbsent)
			}
			_, err := handler.Handle(context.Background(), "GC", nil)
			if err == nil {
				t.Fatal("default degraded method unexpectedly succeeded")
			}
			assertGuidance(t, "default", err.Error(), test.want, test.wantAbsent)
		})
	}
}

func degradedDiagnosticMessage(t *testing.T, method string, result any) string {
	t.Helper()
	if method == "Doctor" {
		return degradedDoctorMessage(t, result)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("%s result=%T %v, want object", method, result, result)
	}
	if method == "Status" {
		message, ok := payload["error"].(string)
		if !ok {
			t.Fatalf("status error=%T %v, want string", payload["error"], payload["error"])
		}
		return message
	}
	t.Fatalf("unexpected degraded method %s", method)
	return ""
}

// degradedDoctorMessage は degraded 応答の SQLite 問題から、原因と対処を 1 つの文字列として返す。
func degradedDoctorMessage(t *testing.T, result any) string {
	t.Helper()
	reply, ok := result.(diag.Reply)
	if !ok {
		t.Fatalf("doctor result=%T %v, want a diagnostic reply", result, result)
	}
	if !reply.Degraded {
		t.Fatalf("doctor reply is not marked degraded: %+v", reply)
	}
	for _, finding := range reply.Findings {
		if finding.Check == diag.CheckSQLite && finding.Severity == diag.SeverityProblem {
			return finding.Cause + " " + finding.Action
		}
	}
	t.Fatalf("doctor reply has no SQLite problem: %+v", reply.Findings)
	return ""
}

func assertGuidance(t *testing.T, method, message string, want, wantAbsent []string) {
	t.Helper()
	for _, fragment := range want {
		if !strings.Contains(message, fragment) {
			t.Errorf("%s message=%q, want %q", method, message, fragment)
		}
	}
	for _, fragment := range wantAbsent {
		if strings.Contains(message, fragment) {
			t.Errorf("%s message=%q, must not contain %q", method, message, fragment)
		}
	}
}

func TestDegradedHandlerStillHonoursAStop(t *testing.T) {
	t.Parallel()
	signalled := make(chan struct{})
	handler := DegradedHandler{
		DatabasePath: "/state.db",
		OpenError:    errors.New("corrupt"),
		terminate:    func() error { close(signalled); return nil },
	}
	result, err := handler.Handle(context.Background(), "RequestStop", nil)
	if err != nil {
		t.Fatalf("degraded stop err=%v", err)
	}
	reply, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("degraded stop result=%v", result)
	}
	if pending, _ := reply["stop_pending"].(bool); !pending {
		t.Fatalf("degraded stop did not report the pending stop: %v", reply)
	}
	select {
	case <-signalled:
	case <-time.After(2 * time.Second):
		t.Fatal("the degraded stop never reached the signal")
	}
}

func TestHandlerRoutesAgentRegistrationAndConfigReload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if _, err := store.CreateSlotSession(context.Background(), testSlotRow(t, manager, "", "slot", 0, "LEASED"), nil, state.Session{ID: "session", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	handler := Handler{Manager: manager}
	registered, err := handler.Handle(context.Background(), "RegisterAgentProcess", json.RawMessage(`{"session_id":"session","token":"token","agent_pid":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := registered.(map[string]any); !ok || result["registered"] != true {
		t.Fatalf("registration result=%v", registered)
	}
	session, err := store.SessionByID(context.Background(), "session")
	if err != nil || session.AgentPID != 1 {
		t.Fatalf("registered agent PID=%d err=%v", session.AgentPID, err)
	}
	reloaded, err := handler.Handle(context.Background(), "ReloadConfig", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := reloaded.(map[string]bool); !ok || !result["reloaded"] {
		t.Fatalf("reload result=%v", reloaded)
	}
	if got, want := manager.Config().Storage.WorktreeRoot, filepath.Join(root, "wx"); got != want {
		t.Fatalf("reloaded worktree root=%q, want %q", got, want)
	}
}

// 貸出と復元の要求型を internal/rpc へ移したため、旧 CLI が送っていた payload をそのまま decode できることを固定する。
// 未知 field 拒否は TestHandlerRejectsUnknownFieldsForEveryParameterizedMethod が見るので、ここでは受理側の値を中心に確認する。
func TestHandlerDecodesLegacyLeaseAndResumeParams(t *testing.T) {
	t.Parallel()
	var lease rpc.ResolveAndLeaseParams
	if err := decode(json.RawMessage(`{"force_worktree":true,"cwd":"/repo","branches":["main"],"agent":"codex","client_pid":11}`), &lease); err != nil {
		t.Fatal(err)
	}
	if !lease.ForceWorktree || lease.CWD != "/repo" || lease.Agent != "codex" || lease.ClientPID != 11 || len(lease.Branches) != 1 || lease.Branches[0] != "main" {
		t.Fatalf("decoded ResolveAndLeaseParams=%+v", lease)
	}
	var resume rpc.ResumeParams
	if err := decode(json.RawMessage(`{"wx_session_id":"wx-1","agent":"claude","client_pid":12,"agent_session_id":"native","fresh":true,"branches":null}`), &resume); err != nil {
		t.Fatal(err)
	}
	if resume.WXSessionID != "wx-1" || resume.Agent != "claude" || resume.ClientPID != 12 || resume.AgentSessionID != "native" || !resume.Fresh || resume.Branches != nil {
		t.Fatalf("decoded ResumeParams=%+v", resume)
	}
	// 省略された Params・空 object・null はゼロ値のまま handler へ渡す。
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`null`)} {
		var empty rpc.ResolveAndLeaseParams
		if err := decode(raw, &empty); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if empty.Agent != "" || empty.CWD != "" || empty.ClientPID != 0 || empty.ForceWorktree || empty.Branches != nil {
			t.Fatalf("decode %q produced %+v, want zero value", raw, empty)
		}
	}
	if err := decode(json.RawMessage(`{"cwd":"/repo","unexpected":true}`), &rpc.ResolveAndLeaseParams{}); err == nil {
		t.Fatal("decode accepted an unknown field")
	}
	// 貸出コマンドの 3 フィールドは省略も許し、省略時は agent 起動として読む。
	var leaseKinds rpc.ResolveAndLeaseParams
	if err := decode(json.RawMessage(`{"cwd":"/repo","lease_kind":"shell","lease_owner_session_id":"owner","lease_owner_token":"token"}`), &leaseKinds); err != nil {
		t.Fatal(err)
	}
	if leaseKinds.LeaseKind != "shell" || leaseKinds.LeaseOwnerSessionID != "owner" || leaseKinds.LeaseOwnerToken != "token" {
		t.Fatalf("decoded lease fields=%+v", leaseKinds)
	}
	var resumeLease rpc.ResumeParams
	if err := decode(json.RawMessage(`{"wx_session_id":"wx-1","lease_kind":"command"}`), &resumeLease); err != nil {
		t.Fatal(err)
	}
	if resumeLease.LeaseKind != "command" || resumeLease.LeaseOwnerSessionID != "" {
		t.Fatalf("decoded resume lease fields=%+v", resumeLease)
	}
}

// ReleaseLease は未知のフィールドを拒否し、agent session の返却も断る。
// 認可は socket が per-user であることに依るため、session token は要求しない。
func TestReleaseLeaseRPCRejectsUnknownFieldsAndAgentSessions(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	handler := Handler{Manager: f.Manager}
	ctx := context.Background()
	if _, err := handler.dispatch(ctx, "ReleaseLease", json.RawMessage(`{"session_id":"x","token":"secret"}`)); err == nil {
		t.Fatal("ReleaseLease accepted an unknown field")
	}
	if _, err := handler.dispatch(ctx, "ReleaseLease", json.RawMessage(`{"session_id":"missing","reason":"wx-release","discard":false}`)); err == nil {
		t.Fatal("ReleaseLease accepted an unknown session")
	}
	agent := state.Session{ID: "agent", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := f.Store.CreateSlotSession(ctx, testSlotRow(t, f.Manager, "", "slot", 0, "LEASED"), nil, agent, ""); err != nil {
		t.Fatal(err)
	}
	_, err := handler.dispatch(ctx, "ReleaseLease", json.RawMessage(`{"session_id":"agent","reason":"wx-release","discard":false}`))
	if err == nil || !strings.Contains(err.Error(), "leased to an agent") {
		t.Fatalf("agent session ReleaseLease error=%v", err)
	}
}

func TestPingReportsProtocolVersionWithoutTouchingState(t *testing.T) {
	t.Parallel()
	// Manager を持たない Handler でも応答するのが、状態を読まない契約の証明である。
	result, err := Handler{}.dispatch(context.Background(), "Ping", nil)
	if err != nil {
		t.Fatalf("Ping err=%v", err)
	}
	reply, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("Ping result type=%T", result)
	}
	if reply["protocol_version"] != rpc.ProtocolVersion {
		t.Fatalf("Ping protocol_version=%v, want %d", reply["protocol_version"], rpc.ProtocolVersion)
	}
	if reply["degraded"] != false {
		t.Fatalf("Ping degraded=%v, want false", reply["degraded"])
	}
	if pid, ok := reply["pid"].(int); !ok || pid != os.Getpid() {
		t.Fatalf("Ping pid=%v, want %d", reply["pid"], os.Getpid())
	}
	if len(reply) != 3 {
		t.Fatalf("Ping reply=%v, want protocol_version, degraded, and pid", reply)
	}
}

func TestDegradedPingAnswersWithoutLiftingTheReadOnlyLimits(t *testing.T) {
	t.Parallel()
	handler := DegradedHandler{DatabasePath: "/state.db", OpenError: errors.New("corrupt")}
	result, err := handler.Handle(context.Background(), "Ping", nil)
	if err != nil {
		t.Fatalf("degraded Ping err=%v", err)
	}
	reply, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("degraded Ping result type=%T", result)
	}
	if reply["degraded"] != true {
		t.Fatalf("degraded Ping degraded=%v, want true", reply["degraded"])
	}
	if reply["protocol_version"] != rpc.ProtocolVersion {
		t.Fatalf("degraded Ping protocol_version=%v, want %d", reply["protocol_version"], rpc.ProtocolVersion)
	}
	if pid, ok := reply["pid"].(int); !ok || pid != os.Getpid() {
		t.Fatalf("degraded Ping pid=%v, want %d", reply["pid"], os.Getpid())
	}
	if _, err := handler.Handle(context.Background(), "ResolveAndLease", nil); err == nil {
		t.Fatal("degraded lease succeeded after a successful Ping")
	}
}
