package agent

import (
	"encoding/json"
	"strings"
)

const (
	subagentIsolationSystemMessage = `wx blocked Agent(isolation="worktree"); lease a workspace with wx new.`
	subagentIsolationReason        = `wx prepares the worktrees in a wx session. isolation="worktree" leaves a worktree that wx does not manage, ` +
		"and its worktree-agent-* branch, registered in the source repository, and it skips the .worktreelink symlinks (such as .tools), " +
		"so the subagent has to rebuild its environment. " +
		"Run `wx new` once per subagent, write the absolute path it prints into the prompt with an instruction to work in that directory, " +
		"and call Agent without isolation. Dropping isolation alone makes the subagent share this working tree, " +
		"and subagents running in parallel then collide on their edits."
)

// subagentTools は SubAgent を起動するツール名である。Task は Agent へ改称される前の名前。
var subagentTools = map[string]bool{"Agent": true, "Task": true}

// subagentIsolationPayload は Agent の呼び出しのうち判定に使う部分だけを持つ。
// 型の合わない値は json.Unmarshal が失敗させ、呼び出し側が fail-open で通す。
type subagentIsolationPayload struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Isolation string `json:"isolation"`
	} `json:"tool_input"`
}

// subagentIsolationHookOutput は wx session での Agent(isolation="worktree") を deny する。
// prompt に isolation や worktree の語が入るのは普通なので、判定は tool_input.isolation の値だけで行う。
// remote はローカルに worktree を作らないので通し、読めない payload も通す。
func subagentIsolationHookOutput(payload []byte) (preToolUseHookOutput, bool) {
	// 全ツール呼び出しで走るため、関係しない payload は JSON を解く前に落とす。
	if !strings.Contains(string(payload), `"isolation"`) {
		return preToolUseHookOutput{}, false
	}
	var decoded subagentIsolationPayload
	if json.Unmarshal(payload, &decoded) != nil {
		return preToolUseHookOutput{}, false
	}
	if !subagentTools[decoded.ToolName] || decoded.ToolInput.Isolation != "worktree" {
		return preToolUseHookOutput{}, false
	}
	return denyPreToolUse(subagentIsolationReason, subagentIsolationSystemMessage), true
}

// denyPreToolUse は updatedInput を持たない deny を組み立てる。
func denyPreToolUse(reason, systemMessage string) preToolUseHookOutput {
	return preToolUseHookOutput{
		HookSpecificOutput: preToolUseHookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: reason,
		},
		SystemMessage: systemMessage,
	}
}
