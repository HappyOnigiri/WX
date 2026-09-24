package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// agentPayload は Agent ツールの PreToolUse payload を組む。isolation が空なら指定なしの呼び出しになる。
func agentPayload(t *testing.T, toolName, isolation, prompt string, extra map[string]any) []byte {
	t.Helper()
	toolInput := map[string]any{"description": "Implement T-1", "subagent_type": "general-purpose", "prompt": prompt}
	if isolation != "" {
		toolInput["isolation"] = isolation
	}
	body := map[string]any{
		"session_id":      "test-session",
		"transcript_path": "/dev/null",
		"hook_event_name": "PreToolUse",
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"cwd":             "/tmp/wx-session/root",
	}
	for key, value := range extra {
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestSubagentIsolationHookOutputDeniesWorktreeIsolation(t *testing.T) {
	for name, payload := range map[string][]byte{
		"agent":        agentPayload(t, "Agent", "worktree", "Implement T-1.", nil),
		"former task":  agentPayload(t, "Task", "worktree", "Implement T-1.", nil),
		"extra fields": agentPayload(t, "Agent", "worktree", "Implement T-1.", map[string]any{"tool_use_id": "tool-1", "permission_mode": "bypassPermissions"}),
	} {
		t.Run(name, func(t *testing.T) {
			output, ok := subagentIsolationHookOutput(payload)
			if !ok {
				t.Fatal("worktree isolation was not denied")
			}
			specific := output.HookSpecificOutput
			if specific.PermissionDecision != "deny" || specific.HookEventName != "PreToolUse" || specific.UpdatedInput != nil {
				t.Fatalf("decision=%+v", specific)
			}
			// isolation を外すだけでは並列実行で衝突するため、代替手順まで理由に入れる。
			for _, want := range []string{"wx new", "absolute path", "prompt", "without isolation", "collide"} {
				if !strings.Contains(specific.PermissionDecisionReason, want) {
					t.Fatalf("reason %q does not mention %q", specific.PermissionDecisionReason, want)
				}
			}
			if output.SystemMessage != subagentIsolationSystemMessage {
				t.Fatalf("systemMessage=%q", output.SystemMessage)
			}
		})
	}
}

// 誤爆がこの判定の実害なので、通す呼び出しを厚く保つ。
func TestSubagentIsolationHookOutputPassesEverythingElse(t *testing.T) {
	passing := map[string][]byte{
		"no isolation": agentPayload(t, "Agent", "", "Implement T-1.", nil),
		// remote はローカルの worktree を作らない。
		"remote isolation": agentPayload(t, "Agent", "remote", "Implement T-1.", nil),
		// prompt 本文に isolation や worktree の語が入るのは普通なので、フィールドの値だけを見る。
		"prompt mentions isolation": agentPayload(t, "Agent", "", `Do not use isolation: "worktree"; run git worktree add --detach instead.`, nil),
		"bash":                      []byte(`{"tool_name":"Bash","tool_input":{"isolation":"worktree","command":"git status"}}`),
		"edit":                      []byte(`{"tool_name":"Edit","tool_input":{"isolation":"worktree"}}`),
		"write":                     []byte(`{"tool_name":"Write","tool_input":{"isolation":"worktree"}}`),
		"no isolation key":          []byte(`{"tool_name":"Bash","tool_input":{"command":"git status"}}`),
	}
	// 壊れた payload は fail-open にする。生の文字列での判定は正当な呼び出しを止め得る。
	for index, raw := range []string{
		"",
		"not json",
		"[]",
		`{"tool_name": "Agent", "tool_input": "isolation"}`,
		`{"tool_name": "Agent", "tool_input": {"isolation": null}}`,
		`{"tool_name": "Agent", "tool_input": {"isolation": ["worktree"]}}`,
		`{"tool_input": {"isolation": "worktree"}}`,
	} {
		passing["broken "+string(rune('a'+index))] = []byte(raw)
	}
	for name, payload := range passing {
		t.Run(name, func(t *testing.T) {
			if output, ok := subagentIsolationHookOutput(payload); ok {
				t.Fatalf("payload %s was decided: %+v", payload, output)
			}
		})
	}
}

// deny は管理下 session に限る。wx -n の直接起動と wx の外では何もしない。
func TestPreToolUseDecisionLimitsSubagentIsolationToManagedSessions(t *testing.T) {
	payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", nil)
	if _, ok := preToolUseDecision(context.Background(), payload, "/tmp/wx-session/root", true); !ok {
		t.Fatal("managed session did not deny worktree isolation")
	}
	if output, ok := preToolUseDecision(context.Background(), payload, "/tmp/wx-session/root", false); ok {
		t.Fatalf("a session outside wx management decided: %+v", output)
	}
}

func TestPreToolUseHookDeniesSubagentWorktreeIsolation(t *testing.T) {
	t.Run("managed session", func(t *testing.T) {
		clearHookEnvironment(t)
		handler := &recordingHandler{}
		ctx := startHookServer(t, handler)
		t.Setenv("WX_SESSION_ID", "wx-subagent")
		t.Setenv("WX_SESSION_TOKEN", "token")
		t.Setenv("WX_READINESS_TIMEOUT", "2s")
		t.Setenv("WX_WORKSPACE_ROOT", "/tmp/wx-session/root")
		payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", nil)
		output := captureHookStdout(t, func() {
			if err := RunHook(ctx, "pre-tool-use", strings.NewReader(string(payload))); err != nil {
				t.Fatal(err)
			}
		})
		// deny は daemon を必要としないので readiness を待たずに出す。
		if methods := handler.methodsSnapshot(); len(methods) != 0 {
			t.Fatalf("methods=%v, want no RPC before the deny", methods)
		}
		var decoded preToolUseHookOutput
		if err := json.Unmarshal([]byte(output), &decoded); err != nil {
			t.Fatalf("stdout %q is not a single JSON object: %v", output, err)
		}
		if decoded.HookSpecificOutput.PermissionDecision != "deny" || strings.Contains(output, "updatedInput") {
			t.Fatalf("output=%s", output)
		}
	})
	t.Run("direct launch", func(t *testing.T) {
		clearHookEnvironment(t)
		t.Setenv("WX_DIRECT_ROOT", "/tmp/wx-session/root")
		payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", nil)
		output := captureHookStdout(t, func() {
			if err := RunHook(context.Background(), "pre-tool-use", strings.NewReader(string(payload))); err != nil {
				t.Fatal(err)
			}
		})
		if output != "" {
			t.Fatalf("direct launch emitted %q", output)
		}
	})
}
