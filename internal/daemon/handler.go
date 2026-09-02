package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

type Handler struct{ Manager *Manager }

type DegradedHandler struct {
	DatabasePath string
	OpenError    error
}

func (h DegradedHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	message := fmt.Sprintf("SQLite state is unavailable: %v; restore a verified backup from %s.backups or preserve the database for wx doctor", h.OpenError, h.DatabasePath)
	switch method {
	case "Status":
		return map[string]any{"schema_version": state.SchemaVersion, "protocol_version": 1, "degraded": true, "database_path": h.DatabasePath, "error": message}, nil
	case "Doctor":
		return map[string]any{"schema_version": state.SchemaVersion, "degraded": true, "checks": map[string]any{"sqlite": message}}, nil
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

func (h Handler) Handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "ResolveAndLease":
		var p struct {
			CWD       string   `json:"cwd"`
			Branches  []string `json:"branches"`
			Agent     string   `json:"agent"`
			ClientPID int      `json:"client_pid"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return h.Manager.ResolveAndLease(ctx, p.CWD, p.Branches, p.Agent, p.ClientPID)
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
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
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
	case "Heartbeat":
		var p struct {
			SessionID string `json:"session_id"`
			Token     string `json:"token"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, h.Manager.Heartbeat(ctx, p.SessionID, p.Token)
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
		n, err := h.Manager.GC(ctx, p.DryRun)
		return map[string]int{"candidates": n}, err
	case "ReloadConfig":
		return map[string]bool{"reloaded": true}, h.Manager.ReloadConfig()
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
