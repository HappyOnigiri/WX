package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

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

func (h DegradedHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	message := fmt.Sprintf("SQLite state is unavailable: %v", h.OpenError)
	if !errors.Is(h.OpenError, state.ErrPreviousWorktreeLayout) {
		message += fmt.Sprintf("; restore a verified backup from %s.backups or preserve the database for wx doctor", h.DatabasePath)
	}
	switch method {
	case "Status":
		return map[string]any{"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "protocol_version": 1, "degraded": true, "database_path": h.DatabasePath, "error": message}, nil
	case "Doctor":
		return map[string]any{"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "degraded": true, "checks": map[string]any{"sqlite": message}}, nil
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
	switch method {
	case "ResolveAndLease":
		var p struct {
			ForceWorktree bool     `json:"force_worktree"`
			CWD           string   `json:"cwd"`
			Branches      []string `json:"branches"`
			Agent         string   `json:"agent"`
			ClientPID     int      `json:"client_pid"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.leaseWithPolicy(ctx, p.CWD, p.Branches, p.Agent, p.ClientPID, p.ForceWorktree)
	case "AllocateResumeSlot":
		var p struct {
			Agent     string `json:"agent"`
			ClientPID int    `json:"client_pid"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.AllocateResumeSlot(ctx, p.Agent, p.ClientPID)
	case "WaitReady":
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
		return map[string]bool{"ready": true}, h.Manager.WaitReady(ctx, p.SessionID, p.Token)
	case "BindAgentSession":
		var p struct {
			SessionID      string `json:"session_id"`
			Token          string `json:"token"`
			AgentSessionID string `json:"agent_session_id"`
			// hook の SessionStart payload と整合させるため Source は受け付けるが、通常 bind では使用しない。
			// strict decoder が hook の送信フィールドを拒否しないよう、ここでも decode する。
			Source string `json:"source"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return map[string]bool{"bound": true}, h.Manager.BindAgentSession(ctx, p.SessionID, p.Token, p.AgentSessionID)
	case "BindAndRestoreResume":
		var p struct {
			SessionID      string `json:"session_id"`
			Token          string `json:"token"`
			AgentSessionID string `json:"agent_session_id"`
			Source         string `json:"source"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		// hook 側の選択だけを信頼せず、サーバー側でも Source == "resume" を必須にする。
		// 欠落を許すと、有効な session token の caller が restore 経路へ到達できる。
		if p.Source != "resume" {
			return nil, fmt.Errorf("BindAndRestoreResume requires a resume source, got %q", p.Source)
		}
		return map[string]bool{"bound": true}, h.Manager.BindAndRestoreResume(ctx, p.SessionID, p.Token, p.AgentSessionID)
	case "ValidateFreshResume":
		var p struct {
			SessionID      string   `json:"session_id"`
			Token          string   `json:"token"`
			AgentSessionID string   `json:"agent_session_id"`
			CWD            string   `json:"cwd"`
			Branches       []string `json:"branches"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return map[string]bool{"allowed": true}, h.Manager.PrepareFreshResume(ctx, p.SessionID, p.Token, p.AgentSessionID, p.CWD, p.Branches)
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
	case "Resume":
		var p struct {
			WXSessionID string `json:"wx_session_id"`
			Agent       string `json:"agent"`
			ClientPID   int    `json:"client_pid"`
			AllowFresh  bool   `json:"allow_fresh"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.Resume(ctx, p.WXSessionID, p.Agent, p.ClientPID, p.AllowFresh)
	case "ResumeStatus":
		var p struct {
			WXSessionID string `json:"wx_session_id"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.ResumeStatus(ctx, p.WXSessionID)
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
	case "ReloadConfig":
		return map[string]bool{"reloaded": true}, h.Manager.ReloadConfig()
	case "RequestRestart":
		return h.Manager.RequestRestart(ctx), nil
	case "RequestStop":
		return h.Manager.RequestStop(ctx), nil
	case "RequestStart":
		return h.Manager.RequestStart(ctx), nil
	case "Sessions":
		var p struct {
			All bool `json:"all"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.Sessions(ctx, p.All)
	case "Forget":
		var p struct {
			Path string `json:"path"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return map[string]bool{"forgotten": true}, h.Manager.Forget(ctx, p.Path)
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
		}
		if err := decode(raw, &p); err != nil {
			return nil, true, err
		}
		result, err := h.Manager.Clean(ctx, p.All, p.Standby, p.DryRun)
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
