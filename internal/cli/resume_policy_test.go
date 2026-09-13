package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	sessionsconfig "github.com/HappyOnigiri/WX/internal/sessions/config"
)

// resumePolicyClient は起動場所の policy を cfg で固定した client と、会話履歴の置き場所を用意する。
// 会話 ID を指定した再開は履歴の cwd 側で可否が決まるため、起動場所と会話の cwd を別のディレクトリにする。
func resumePolicyClient(t *testing.T, handler *resumeLaunchHandler, undefinedMode string) (Client, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	history := filepath.Join(home, "history")
	if err := os.MkdirAll(history, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Worktree.Undefined = undefinedMode
	cfg.Sessions.Paths.Claude = sessionsconfig.ToolPathsConfig{Sessions: []string{history}}
	client, _ := serveResumeLaunchRPCWithConfig(t, handler, cfg)
	return client, history
}

// writeClaudeHistory は claude の会話 1 件分の jsonl を置き、その ID を返す。
func writeClaudeHistory(t *testing.T, history, id, cwd string) {
	t.Helper()
	row, err := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": "resume"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(history, id+".jsonl")
	if err := os.WriteFile(path, append(row, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
}

func launchRecordFixture(t *testing.T) (record string, agentDirectory string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "launch-record")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "claude")
	prependPath(t, filepath.Dir(agent))
	return record, filepath.Dir(agent)
}

// 記録済み session の再開は当時の workspace を復元するため、起動場所の policy を見ないことを確かめる。
// off の workspace から再開しても、以前は現在地で起動して復元されなかった。
func TestResumeByIDRestoresManagedSessionRegardlessOfLaunchPolicy(t *testing.T) {
	workspace := t.TempDir()
	record, _ := launchRecordFixture(t)
	handler := &resumeLaunchHandler{
		lease:  daemon.Lease{SessionID: "new-session", Token: "new-token", Path: workspace, Ready: true},
		status: resumeStatus{WXSessionID: "old-session", Agent: "claude", AgentSessionID: "native-claude"},
	}
	for _, mode := range []string{"off", "ask"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := resumePolicyClient(t, handler, mode)
			args := []string{"--resume", "native-claude"}
			if exit := client.RunAgentWithPolicyFrom(context.Background(), t.TempDir(), "claude", args, nil, false, WorktreeOptions{}); exit != 0 {
				t.Fatalf("exit=%d", exit)
			}
			if got := handler.paramsFor("Resume"); len(got) == 0 {
				t.Fatalf("Resume was not requested; methods=%v", handler.methodsSnapshot())
			}
			launch := readLaunchRecord(t, record)
			physical, err := filepath.EvalSymlinks(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if launch["pwd"] != physical {
				t.Fatalf("agent cwd=%q, want the restored workspace %q", launch["pwd"], physical)
			}
		})
	}
}

// 管理外の会話は、会話に記録された cwd の policy で可否が決まることを確かめる。
// worktree を作らない方針なら、起動場所ではなく会話の cwd で agent を起動する。
func TestResumeByIDWithoutWorktreePolicyStartsInConversationCWD(t *testing.T) {
	conversation, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, _ := launchRecordFixture(t)
	handler := &resumeLaunchHandler{
		// 管理外の会話なので、ResumeStatus は該当なしを返す。
		resumeErr: errors.New("sql: no rows in result set"),
		policy:    daemon.WorktreePolicyReply{Root: conversation, Mode: "off", Resolved: true},
	}
	client, history := resumePolicyClient(t, handler, "hot")
	id := "11111111-1111-4111-8111-111111111111"
	writeClaudeHistory(t, history, id, conversation)

	if exit := client.RunAgentWithPolicyFrom(context.Background(), t.TempDir(), "claude", []string{"--resume", id}, nil, false, WorktreeOptions{}); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	for _, method := range handler.methodsSnapshot() {
		if method == "Resume" || method == "ResolveAndLease" {
			t.Fatalf("a worktree was requested for a workspace without a policy; methods=%v", handler.methodsSnapshot())
		}
	}
	launch := readLaunchRecord(t, record)
	if launch["pwd"] != conversation {
		t.Fatalf("agent cwd=%q, want the conversation cwd %q", launch["pwd"], conversation)
	}
	if launch["args"] != "--resume "+id {
		t.Fatalf("agent args=%q", launch["args"])
	}
}

// 会話の cwd も workspace も解決できないときは、失敗させず起動場所で会話だけ再開することを確かめる。
func TestResumeByIDFallsBackToLaunchDirectoryWhenWorkspaceUnresolved(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, _ := launchRecordFixture(t)
	handler := &resumeLaunchHandler{
		resumeErr: errors.New("sql: no rows in result set"),
		// workspace を解決できない応答。会話の cwd が消えている場合がこれにあたる。
		policy: daemon.WorktreePolicyReply{},
	}
	client, history := resumePolicyClient(t, handler, "hot")
	id := "22222222-2222-4222-8222-222222222222"
	writeClaudeHistory(t, history, id, filepath.Join(t.TempDir(), "removed"))

	if exit := client.RunAgentWithPolicyFrom(context.Background(), source, "claude", []string{"--resume", id}, nil, false, WorktreeOptions{}); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	launch := readLaunchRecord(t, record)
	if launch["pwd"] != source {
		t.Fatalf("agent cwd=%q, want the launch directory %q", launch["pwd"], source)
	}
}

// worktree の指定を明示した起動は従来どおり指定を優先し、会話側の判定へ入らないことを確かめる。
func TestResumeByIDKeepsExplicitWorktreeOverrides(t *testing.T) {
	record, _ := launchRecordFixture(t)
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := &resumeLaunchHandler{
		resumeErr: errors.New("sql: no rows in result set"),
		policy:    daemon.WorktreePolicyReply{Root: source, Mode: "cold", Resolved: true},
	}
	client, history := resumePolicyClient(t, handler, "hot")
	id := "33333333-3333-4333-8333-333333333333"
	writeClaudeHistory(t, history, id, t.TempDir())

	if exit := client.RunAgentWithPolicyFrom(context.Background(), source, "claude", []string{"--resume", id}, nil, false, WorktreeOptions{Disable: true}); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	for _, method := range handler.methodsSnapshot() {
		if method == "WorktreePolicy" {
			t.Fatalf("--no-worktree consulted the conversation policy; methods=%v", handler.methodsSnapshot())
		}
	}
	launch := readLaunchRecord(t, record)
	if launch["pwd"] != source {
		t.Fatalf("agent cwd=%q, want the launch directory %q", launch["pwd"], source)
	}
}

// wx も agent の履歴も引けない ID は、新しい会話として worktree を消費せずに agent へ渡すことを確かめる。
// 実在しない ID なら agent 自身が理由を示して終わり、取りこぼしなら再開できる。
func TestResumeByIDWithoutRecordStartsWithoutWorktree(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, _ := launchRecordFixture(t)
	handler := &resumeLaunchHandler{
		resumeErr: errors.New("sql: no rows in result set"),
		// 引けなかった ID では会話の cwd が無いため、policy の問い合わせ自体が起きない。
		policy: daemon.WorktreePolicyReply{Root: source, Mode: "hot", Resolved: true},
	}
	client, _ := resumePolicyClient(t, handler, "hot")
	id := "44444444-4444-4444-8444-444444444444"

	if exit := client.RunAgentWithPolicyFrom(context.Background(), source, "claude", []string{"--resume", id}, nil, false, WorktreeOptions{}); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	for _, method := range handler.methodsSnapshot() {
		if method == "Resume" || method == "ResolveAndLease" || method == "WorktreePolicy" {
			t.Fatalf("an unresolved conversation consumed a worktree; methods=%v", handler.methodsSnapshot())
		}
	}
	launch := readLaunchRecord(t, record)
	if launch["pwd"] != source {
		t.Fatalf("agent cwd=%q, want the launch directory %q", launch["pwd"], source)
	}
	if launch["args"] != "--resume "+id {
		t.Fatalf("agent args=%q", launch["args"])
	}
}

// codex の `resume <id>` も同じ経路を通り、引けない ID で worktree を作らないことを確かめる。
func TestResumeByIDWithoutRecordStartsWithoutWorktreeForCodex(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "launch-record")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "codex")
	prependPath(t, filepath.Dir(agent))
	handler := &resumeLaunchHandler{resumeErr: errors.New("sql: no rows in result set")}
	client, _ := resumePolicyClient(t, handler, "hot")
	id := "55555555-5555-4555-8555-555555555555"

	if exit := client.RunAgentWithPolicyFrom(context.Background(), source, "codex", []string{"resume", id}, nil, false, WorktreeOptions{}); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	for _, method := range handler.methodsSnapshot() {
		if method == "Resume" || method == "ResolveAndLease" {
			t.Fatalf("an unresolved conversation consumed a worktree; methods=%v", handler.methodsSnapshot())
		}
	}
	launch := readLaunchRecord(t, record)
	if launch["pwd"] != source {
		t.Fatalf("agent cwd=%q, want the launch directory %q", launch["pwd"], source)
	}
	if launch["args"] != "resume "+id {
		t.Fatalf("agent args=%q", launch["args"])
	}
}
