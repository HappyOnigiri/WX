package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

func (m *Manager) Heartbeat(ctx context.Context, id, token string) error {
	return m.store.Heartbeat(ctx, id, token)
}

func (m *Manager) RegisterAgentProcess(ctx context.Context, id, token string, pid int) error {
	return m.store.RegisterAgentProcess(ctx, id, token, pid)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ReleaseUnreceivedPathLease は、まだ path を渡せていない path 貸出を返却する。
// wx new の待機が中断されると client は token ごと消えるが、path 貸出は client_pid も heartbeat も持たず
// OrphanCandidates から外れるため、ここで返さないと lease.ttl まで slot を占める。path 以外は対象にしない。
func (m *Manager) ReleaseUnreceivedPathLease(ctx context.Context, id, token string) {
	session, err := m.store.Session(ctx, id, token)
	if err != nil {
		m.log.Warn("could not inspect a lease after its client disconnected", "session_id", id, "error", err)
		return
	}
	if session.LeaseKind != state.LeaseKindPath || !sessionInUse(session.State) {
		return
	}
	m.log.Info("releasing a path lease whose client disconnected before the workspace was ready", "session_id", id, "slot_id", session.SlotID)
	if err := m.Release(ctx, id, token, "lease-setup-failed"); err != nil {
		m.log.Error("lease release failed", "session_id", id, "error", err)
	}
}

func (m *Manager) WaitReady(ctx context.Context, id, token string) error {
	return m.waitReadiness(ctx, id, token, false)
}

// WaitEarlyReady は起動用ファイルまでの準備を待ち、hook が使う WaitReady とは独立に判定する。
func (m *Manager) WaitEarlyReady(ctx context.Context, id, token string) error {
	return m.waitReadiness(ctx, id, token, true)
}

// LeaseProgress は貸出の準備がいまどこにいるかである。表示専用で、準備の結果には関与しない。
type LeaseProgress struct {
	// State は slot の状態。
	State string `json:"state"`
	// Running は準備 job がこの slot で走っていることを示す。
	// Phase は区間の切れ目でも空になるため、job 待ち行列との区別はこちらで行う。
	Running bool `json:"running,omitempty"`
	// Phase は実行中の準備区間名。`wx bench` の区間名と同じ語彙である。
	Phase string `json:"phase,omitempty"`
	// Target は Phase が属する repository の、workspace root からの相対 path である。
	// 区間名は repository ごとに繰り返すため、これが無いと表示が何周目かを読めない。
	// workspace 全体に属する区間と単一 repository の workspace では空になる。
	Target string `json:"target,omitempty"`
	// TargetIndex は TargetTotal 件中の何件目かで、1 始まりである。対象を持たない区間では 0 になる。
	TargetIndex int `json:"target_index,omitempty"`
	TargetTotal int `json:"target_total,omitempty"`
	// PhaseElapsedMS は Phase が始まってからの経過である。
	PhaseElapsedMS int64 `json:"phase_elapsed_ms,omitempty"`
}

// LeaseProgress は準備中の slot の現在位置を返す。認証は WaitReady と同じく session ID と token で行う。
// slot ID は session ID と同じなので、貸出を持つ client だけが自分の準備を読める。
func (m *Manager) LeaseProgress(ctx context.Context, id, token string) (LeaseProgress, error) {
	if _, err := m.store.Session(ctx, id, token); err != nil {
		return LeaseProgress{}, err
	}
	slot, err := m.store.Slot(ctx, id)
	if err != nil {
		return LeaseProgress{}, err
	}
	active, running, ok := m.ActivePhase(id)
	progress := LeaseProgress{State: slot.State, Running: running}
	if ok {
		progress.Phase = active.Name
		progress.Target = active.Scope.Target
		progress.TargetIndex, progress.TargetTotal = active.Scope.Index, active.Scope.Total
		progress.PhaseElapsedMS = time.Since(active.Start).Milliseconds()
	}
	return progress, nil
}

func (m *Manager) waitReadiness(ctx context.Context, id, token string, early bool) error {
	if _, err := m.store.Session(ctx, id, token); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return err
		}
		switch slot.State {
		case "READY", "LEASED":
			if !early {
				return nil
			}
		case "FAILED", "QUARANTINED":
			failureID := slot.FailureCode
			if failureID == "" {
				failureID = "UNKNOWN"
			}
			for _, prefix := range []string{"PREPARE_FAILED:", "RESTORE_FAILED:"} {
				if strings.HasPrefix(failureID, prefix) {
					failureID = strings.TrimPrefix(failureID, prefix)
					break
				}
			}
			metadata := readPrepareDiagnostic(slot.FailureDetailPath)
			if metadata.FailureID != "" {
				failureID = metadata.FailureID
			}
			detailPath := slot.FailureDetailPath
			if detailPath == "" {
				detailPath = "unavailable"
			}
			exitCode := "unknown"
			if metadata.HasExitCode {
				exitCode = strconv.Itoa(metadata.ExitCode)
			}
			// 復元の失敗は marker で区別する。client は会話の再開を優先し、新しい worktree で作り直してよいか確認する。
			recovery := ""
			if recoveryUnavailable(slot.FailureCode) {
				recovery = " " + RecoveryUnavailableMarker
			}
			return fmt.Errorf("workspace readiness failed: state=%s failure_id=%s%s detail_path=%s exit_code=%s timed_out=%t canceled=%t; run `wx status` or `wx doctor` for details", slot.State, failureID, recovery, detailPath, exitCode, metadata.TimedOut, metadata.Canceled)
		}
		if early {
			session, sessionErr := m.store.Session(ctx, id, token)
			if sessionErr != nil {
				return sessionErr
			}
			switch session.State {
			case "STARTING", "ACTIVE":
			default:
				return fmt.Errorf("workspace readiness failed: session state=%s", session.State)
			}
			switch slot.State {
			case "READY", "LEASED":
				return nil
			case "PREPARING":
				if slot.EarlyReadyAt != "" {
					return nil
				}
			default:
				return fmt.Errorf("workspace readiness failed: state=%s", slot.State)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type prepareDiagnosticMetadata struct {
	FailureID   string
	ExitCode    int
	HasExitCode bool
	TimedOut    bool
	Canceled    bool
}

func readPrepareDiagnostic(path string) prepareDiagnosticMetadata {
	var metadata prepareDiagnosticMetadata
	if path == "" {
		return metadata
	}
	file, err := os.Open(path)
	if err != nil {
		return metadata
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 8<<10))
	if err != nil {
		return metadata
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "failure_id":
			metadata.FailureID = strings.TrimSpace(value)
		case "exit_code":
			if exitCode, parseErr := strconv.Atoi(strings.TrimSpace(value)); parseErr == nil {
				metadata.ExitCode, metadata.HasExitCode = exitCode, true
			}
		case "timed_out":
			metadata.TimedOut, _ = strconv.ParseBool(strings.TrimSpace(value))
		case "canceled":
			metadata.Canceled, _ = strconv.ParseBool(strings.TrimSpace(value))
		}
	}
	return metadata
}

func (m *Manager) BindAgentSession(ctx context.Context, id, token, agentID string, replaces ...string) error {
	if _, err := m.store.Session(ctx, id, token); err != nil {
		return err
	}
	return m.store.BindAgentSession(ctx, id, agentID, replaces...)
}
