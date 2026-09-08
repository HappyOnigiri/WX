package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

type Handler struct{ Manager *Manager }

type DegradedHandler struct {
	DatabasePath string
	OpenError    error
	// terminate は停止経路のテスト差し替え点。通常は nil のまま SIGTERM 実装を使う。
	terminate func() error
}

// 応答を書き終えるまで SIGTERM を遅らせる。listener の終了で RPC 接続が破棄されるためである。
const degradedStopDelay = 100 * time.Millisecond

func (h DegradedHandler) Handle(ctx context.Context, method string, _ json.RawMessage) (any, error) {
	previousLayout := errors.Is(h.OpenError, state.ErrPreviousWorktreeLayout)
	message := fmt.Sprintf("SQLite state is unavailable: %v", h.OpenError)
	if !previousLayout {
		message += fmt.Sprintf("; restore a verified backup from %s.backups or preserve the database for wx doctor", h.DatabasePath)
	}
	switch method {
	case "Ping":
		// degraded でも応答確認だけは成立させる。読み取り以上の制限は後続の method が従来どおり報告する。
		return map[string]any{"protocol_version": rpc.ProtocolVersion, "degraded": true, "pid": os.Getpid()}, nil
	case "Status":
		return map[string]any{"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "protocol_version": 1, "degraded": true, "database_path": h.DatabasePath, "error": message}, nil
	case "Doctor":
		return diag.Reply{
			SchemaVersion: state.JSONSchemaVersion, DBSchemaVersion: state.SchemaVersion, Degraded: true,
			Findings: diag.DegradedFindings(ctx, h.DatabasePath, h.OpenError, previousLayout),
		}, nil
	case "RequestStop":
		// Degraded mode は状態を変更せず予約もないため、manager のアイドルゲートを通さない。
		// ここを拒否すると、調査対象の DB を開いたデーモンを停止する手段が失われる。
		terminate := h.terminate
		if terminate == nil {
			terminate = signalSelfTerminate
		}
		time.AfterFunc(degradedStopDelay, func() { _ = terminate() })
		return map[string]any{"degraded": true, "stop_pending": true, "pid": os.Getpid()}, nil
	default:
		return nil, errors.New("wx daemon is read-only degraded: " + message)
	}
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// dispatch 全体を in-flight として数える。応答途中の kickstart を防ぐため、decode 失敗を含む全 method を囲む。
// 利用者操作として報告するかどうかは method を知るここで判定する。
func (h Handler) Handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	lifecycle := isLifecycleMethod(method)
	h.Manager.beginRequest(lifecycle)
	defer h.Manager.endRequest(lifecycle)
	return h.dispatch(ctx, method, raw)
}

// デーモン自身の実行状態だけを変更する method かを返す。未知の method はゲートを早く開けないよう対象外とする。
func isLifecycleMethod(method string) bool {
	switch method {
	case "RequestStop", "RequestRestart", "RequestStart":
		return true
	default:
		return false
	}
}

func (h Handler) dispatch(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if result, handled, err := h.dispatchClean(ctx, method, raw); handled {
		return result, err
	}
	if result, handled, err := h.dispatchLiveness(ctx, method, raw); handled {
		return result, err
	}
	if result, handled, err := h.dispatchLease(ctx, method, raw); handled {
		return result, err
	}
	switch method {
	case "Ping":
		// 状態を読まず何も変更しない応答確認。起動前の接続確認が Status の集計を待たないために置く。
		return map[string]any{"protocol_version": rpc.ProtocolVersion, "degraded": false, "pid": os.Getpid()}, nil
	case "WaitReady", "WaitEarlyReady":
		var p struct {
			SessionID string `json:"session_id"`
			Token     string `json:"token"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		if p.TimeoutMS > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(p.TimeoutMS)*time.Millisecond)
			defer cancel()
		}
		if method == "WaitEarlyReady" {
			return map[string]bool{"ready": true}, h.Manager.WaitEarlyReady(ctx, p.SessionID, p.Token)
		}
		return map[string]bool{"ready": true}, h.Manager.WaitReady(ctx, p.SessionID, p.Token)
	case "BindAgentSession":
		var p struct {
			SessionID              string `json:"session_id"`
			Token                  string `json:"token"`
			AgentSessionID         string `json:"agent_session_id"`
			ReplacesAgentSessionID string `json:"replaces_agent_session_id"`
			// hook の SessionStart payload と整合させるため Source は受け付けるが、通常 bind では使用しない。
			// strict decoder が hook の送信フィールドを拒否しないよう、ここでも decode する。
			Source string `json:"source"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		if err := h.Manager.BindAgentSession(ctx, p.SessionID, p.Token, p.AgentSessionID, p.ReplacesAgentSessionID); err != nil {
			return nil, err
		}
		previous, err := h.Manager.store.PreviousWorktree(ctx, p.SessionID)
		return map[string]string{"previous_worktree": previous}, err
	case "Release":
		var p struct {
			SessionID string `json:"session_id"`
			Token     string `json:"token"`
			Reason    string `json:"reason"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return map[string]bool{"released": true}, h.Manager.Release(ctx, p.SessionID, p.Token, p.Reason)
	case "ResumeStatus":
		return h.resumeStatusRPC(ctx, raw)
	case "WorkspaceScope":
		var p struct {
			CWD string `json:"cwd"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.WorkspaceScope(ctx, p.CWD)
	case "Status":
		return h.Manager.Status(ctx)
	case "Doctor":
		return h.Manager.Doctor(ctx), nil
	case "GC":
		var p struct {
			DryRun bool `json:"dry_run"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		result, err := h.Manager.GC(ctx, p.DryRun)
		if err != nil {
			// GC は途中まで予約・完了した対象を巻き戻さないため、RPC error だけにすると
			// 呼出元がその進捗と安全な保留理由を失う。件数と理由を含む結果を返し、CLI が終了コードを決める。
			if len(result.Reasons) == 0 {
				result.Reasons = []GCReason{{Target: "gc", Status: "failed", Reason: err.Error()}}
				result.Failed++
			}
			return result, nil
		}
		return result, nil
	case "Prune":
		var p struct {
			All    bool `json:"all"`
			DryRun bool `json:"dry_run"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.PruneRecoveryRefs(ctx, p.All, p.DryRun)
	case "ReloadConfig":
		return map[string]bool{"reloaded": true}, h.Manager.ReloadConfig()
	case "RequestRestart":
		return h.Manager.RequestRestart(ctx), nil
	case "RequestStop":
		return h.Manager.RequestStop(ctx), nil
	case "RequestStart":
		return h.Manager.RequestStart(ctx), nil
	case "Slots":
		var p struct {
			All bool `json:"all"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.Slots(ctx, p.All)
	case "Forget":
		var p struct {
			Path string `json:"path"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return map[string]bool{"forgotten": true}, h.Manager.Forget(ctx, p.Path)
	case "RetryStandby":
		var p struct {
			Path string `json:"path"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.RetryStandby(ctx, p.Path)
	default:
		return nil, errors.New("unknown RPC method")
	}
}

// dispatchClean は wx clear の method を分けて受け持つ。handled が false のときは他の method として扱う。
func (h Handler) dispatchClean(ctx context.Context, method string, raw json.RawMessage) (any, bool, error) {
	switch method {
	case "Clean":
		var p struct {
			All     bool `json:"all"`
			Standby bool `json:"standby"`
			DryRun  bool `json:"dry_run"`
			Discard bool `json:"discard"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		result, err := h.Manager.Clean(ctx, p.All, p.Standby, p.DryRun, p.Discard)
		return result, true, err
	case "CleanStatus":
		var p struct {
			RunID string `json:"run_id"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		result, err := h.Manager.CleanStatus(ctx, p.RunID)
		return result, true, err
	case "ConfirmTermination":
		var p struct {
			SessionID string `json:"session_id"`
			Token     string `json:"token"`
			RequestID string `json:"request_id"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		return map[string]bool{"confirmed": true}, true, h.Manager.ConfirmTermination(ctx, p.SessionID, p.Token, p.RequestID)
	default:
		return nil, false, nil
	}
}

// dispatchLease は worktree の貸出と復元、貸出の明示的な返却を受け持つ。
// handled が false のときは他の method として扱う。
func (h Handler) dispatchLease(ctx context.Context, method string, raw json.RawMessage) (any, bool, error) {
	switch method {
	case "ResolveAndLease":
		var p rpc.ResolveAndLeaseParams
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		attrs, err := h.Manager.resolveLeaseAttrs(ctx, p.LeaseKind, p.LeaseOwnerSessionID, p.LeaseOwnerToken)
		if err != nil {
			return nil, true, err
		}
		result, err := h.Manager.leaseWithPolicy(ctx, p.CWD, p.Branches, p.Agent, p.ClientPID, p.ForceWorktree, attrs)
		return result, true, err
	case "Resume":
		var p rpc.ResumeParams
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		attrs, err := h.Manager.resolveLeaseAttrs(ctx, p.LeaseKind, p.LeaseOwnerSessionID, p.LeaseOwnerToken)
		if err != nil {
			return nil, true, err
		}
		result, err := h.Manager.Resume(ctx, p.WXSessionID, p.Agent, p.ClientPID, p.Fresh, ResumeOptions{AgentSessionID: p.AgentSessionID, Branches: p.Branches, Lease: attrs})
		return result, true, err
	case "ReleaseLease":
		// 貸出の明示的な返却。session token を持たない wx release から呼ばれる。
		var p struct {
			SessionID string `json:"session_id"`
			Reason    string `json:"reason"`
			Discard   bool   `json:"discard"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		result, err := h.Manager.ReleaseLease(ctx, p.SessionID, p.Reason, p.Discard)
		return result, true, err
	default:
		return nil, false, nil
	}
}

// dispatchLiveness は session の生存報告を受け持つ。どちらの応答にも、届いている終了要求を載せる。
func (h Handler) dispatchLiveness(ctx context.Context, method string, raw json.RawMessage) (any, bool, error) {
	var p struct {
		SessionID string `json:"session_id"`
		Token     string `json:"token"`
		AgentPID  int    `json:"agent_pid"`
	}
	base := map[string]any{}
	switch method {
	case "Heartbeat":
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		if err := h.Manager.Heartbeat(ctx, p.SessionID, p.Token); err != nil {
			return nil, true, err
		}
		base["ok"] = true
	case "RegisterAgentProcess":
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		if err := h.Manager.RegisterAgentProcess(ctx, p.SessionID, p.Token, p.AgentPID); err != nil {
			return nil, true, err
		}
		base["registered"] = true
	default:
		return nil, false, nil
	}
	// 起動と終了要求が競合した場合、client は登録応答で要求を受け取り、起動直後のプロセスを終了させる。
	return h.Manager.terminationReply(ctx, p.SessionID, base), true, nil
}

func (h Handler) resumeStatusRPC(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		WXSessionID    string `json:"wx_session_id"`
		Agent          string `json:"agent"`
		AgentSessionID string `json:"agent_session_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if (p.WXSessionID != "" && (p.Agent != "" || p.AgentSessionID != "")) || (p.WXSessionID == "" && (p.Agent == "" || p.AgentSessionID == "")) {
		return nil, errors.New("specify wx_session_id or both agent and agent_session_id")
	}
	if p.WXSessionID == "" {
		se, err := h.Manager.store.FindByAgentSession(ctx, p.Agent, p.AgentSessionID)
		if err != nil {
			return nil, err
		}
		p.WXSessionID = se.ID
	}
	return h.Manager.ResumeStatus(ctx, p.WXSessionID)
}
