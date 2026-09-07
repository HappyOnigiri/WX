package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

// 共有型に移した後も CLI が送る payload の byte 列が旧 map 実装と一致することを、実 socket 越しの往復で確認する。
// 冪等キーと再送判定は Params の JSON 文字列を比較するため、キーの順序や欠落が変わると同じ要求が別物になる。

func TestLaunchSendsLegacyResolveAndLeasePayload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WX_TEST_LAUNCH_RECORD", filepath.Join(t.TempDir(), "launch-record"))
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "codex")
	prependPath(t, filepath.Dir(agent))
	workspace := filepath.Join(t.TempDir(), "slot")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Sessions.Paths.Codex.Sessions = []string{filepath.Join(home, "no-session-history")}
	handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: workspace, Ready: true}}
	client, stop := serveResumeLaunchRPCWithConfig(t, handler, cfg)
	defer stop()

	if exit := client.RunAgent(context.Background(), "codex", nil, nil, false); exit != 0 {
		t.Fatalf("RunAgent exit=%d", exit)
	}
	raw := handler.paramsFor("ResolveAndLease")
	var params rpc.ResolveAndLeaseParams
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatal(err)
	}
	if params.Agent != "codex" || params.ClientPID != os.Getpid() || params.CWD == "" || params.Branches != nil || params.ForceWorktree {
		t.Fatalf("ResolveAndLease params=%+v", params)
	}
	want, err := json.Marshal(map[string]any{"cwd": params.CWD, "branches": []string(nil), "agent": params.Agent, "client_pid": params.ClientPID, "force_worktree": params.ForceWorktree})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("ResolveAndLease payload=%s, want %s", raw, want)
	}
}

func TestLaunchSendsLegacyResumePayload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WX_TEST_LAUNCH_RECORD", filepath.Join(t.TempDir(), "launch-record"))
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "codex")
	prependPath(t, filepath.Dir(agent))
	workspace := filepath.Join(t.TempDir(), "resumed-slot")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := &resumeLaunchHandler{
		lease:  daemon.Lease{SessionID: "resumed", Token: "token", Path: workspace, Ready: true},
		status: resumeStatus{WXSessionID: "old-codex", Agent: "codex", AgentSessionID: "native-codex"},
	}
	client, stop := serveResumeLaunchRPC(t, handler)
	defer stop()

	if exit := client.RunResume(context.Background(), "old-codex", "codex", nil, []string{"main"}, true); exit != 0 {
		t.Fatalf("RunResume exit=%d", exit)
	}
	raw := handler.paramsFor("Resume")
	var params rpc.ResumeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatal(err)
	}
	if params.WXSessionID != "old-codex" || params.Agent != "codex" || params.AgentSessionID != "native-codex" || params.ClientPID != os.Getpid() || !params.Fresh || len(params.Branches) != 1 || params.Branches[0] != "main" {
		t.Fatalf("Resume params=%+v", params)
	}
	want, err := json.Marshal(map[string]any{"wx_session_id": params.WXSessionID, "agent": params.Agent, "client_pid": params.ClientPID, "agent_session_id": params.AgentSessionID, "fresh": params.Fresh, "branches": params.Branches})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("Resume payload=%s, want %s", raw, want)
	}
}
