package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

// leaseAttrs は agent 起動以外への worktree 貸出（wx shell / wx run / wx new）の属性である。
// Kind が空なら従来の agent 起動で、期限も親も持たない。
// OwnerSessionID は wx new を呼んだ親 session で、親の終了で子貸出もまとめて返却する根拠になる。
type leaseAttrs struct {
	Kind           string
	OwnerSessionID string
}

// resolveLeaseAttrs は RPC 要求の貸出指定を検証し、親 session を既存の token 検証で確かめる。
// 親の指定が誤っていても貸出を進めると、親の終了で返却されない worktree が残るため fail closed にする。
func (m *Manager) resolveLeaseAttrs(ctx context.Context, kind, ownerSessionID, ownerToken string) (leaseAttrs, error) {
	switch kind {
	case "", state.LeaseKindAgent:
		if ownerSessionID != "" {
			return leaseAttrs{}, errors.New("lease owner is only accepted for a non-agent lease")
		}
		return leaseAttrs{}, nil
	case state.LeaseKindPath, state.LeaseKindShell, state.LeaseKindCommand:
	default:
		return leaseAttrs{}, fmt.Errorf("unknown lease kind %q", kind)
	}
	attrs := leaseAttrs{Kind: kind}
	if ownerSessionID == "" {
		return attrs, nil
	}
	owner, err := m.store.Session(ctx, ownerSessionID, ownerToken)
	if err != nil {
		return leaseAttrs{}, fmt.Errorf("authenticate lease owner session: %w", err)
	}
	if !sessionInUse(owner.State) {
		return leaseAttrs{}, fmt.Errorf("lease owner session %s is no longer in use", ownerSessionID)
	}
	attrs.OwnerSessionID = owner.ID
	return attrs, nil
}

// applyLeaseAttrs は貸出属性と lease.ttl による期限を session へ載せる。
// agent 起動には期限を付けない。lease.ttl が 0 の設定では期限を持たせず、返却は親の終了と wx release だけになる。
func (m *Manager) applyLeaseAttrs(session *state.Session, attrs leaseAttrs) {
	if attrs.Kind == "" || attrs.Kind == state.LeaseKindAgent {
		return
	}
	session.LeaseKind = attrs.Kind
	session.LeaseOwnerSessionID = attrs.OwnerSessionID
	if ttl := m.Config().Lease.TTL.Duration; ttl > 0 {
		session.LeaseExpiresAt = state.FormatTime(time.Now().Add(ttl))
	}
}

// releaseLeaseWithoutToken は session token を持たない側からの返却を、通常の返却経路へ載せる。
// orphan 回収・期限掃引・親連動・wx release が共有する。
// 保存の要否と slot の遷移は Store.ReleaseWithOutcome が決めるので、ここでは分岐を持たない。
func (m *Manager) releaseLeaseWithoutToken(ctx context.Context, candidate state.OrphanCandidate, reason string) {
	job, changed, quarantineExpired, err := m.store.ReleaseWithOutcome(ctx, candidate.ID, candidate.WorkspaceID, candidate.SlotID)
	if err != nil {
		m.log.Error("lease release failed", "session_id", candidate.ID, "reason", reason, "error", err)
		return
	}
	if quarantineExpired {
		m.log.Warn("session expired without a recovery snapshot: slot is quarantined", "session_id", candidate.ID, "slot_id", candidate.SlotID, "reason", reason)
	}
	if changed {
		m.schedule(job)
		return
	}
	m.releaseLease(candidate.ID)
}

// reconcileExpiredLeases は期限が来た貸出と、親が終了した子貸出を返却する。
// どちらも保存経路（session RELEASING → slot DRAINING → SNAPSHOT ジョブ）を通るので、
// 期限が来ても保存されてから返却され、実体は retention.ended_worktree の間残る。
func (m *Manager) reconcileExpiredLeases(ctx context.Context) {
	expired, err := m.store.ExpiredLeaseCandidates(ctx, state.FormatTime(time.Now()))
	if err != nil {
		m.log.Error("expired lease reconciliation failed", "error", err)
	}
	for _, candidate := range expired {
		m.log.Info("releasing a lease that reached lease.ttl", "session_id", candidate.ID, "slot_id", candidate.SlotID)
		m.releaseLeaseWithoutToken(ctx, candidate, "lease-expired")
	}
	m.releaseOrphanedChildLeases(ctx)
}

// releaseOrphanedChildLeases は親 session が使用中でなくなった子貸出を返却する。
// 正しさの根拠は周期処理であるこの一巡に置き、Manager.Release からの呼び出しは待ち時間の最適化に留める。
func (m *Manager) releaseOrphanedChildLeases(ctx context.Context) {
	children, err := m.store.OrphanedChildLeases(ctx)
	if err != nil {
		m.log.Error("child lease reconciliation failed", "error", err)
		return
	}
	for _, candidate := range children {
		m.log.Info("releasing a lease whose owner session ended", "session_id", candidate.ID, "slot_id", candidate.SlotID)
		m.releaseLeaseWithoutToken(ctx, candidate, "lease-owner-ended")
	}
}

// ReleaseLease は session token を持たない利用者からの明示的な返却である。
// 認可の根拠は socket が per-user であることと、wx clear が既に token 無しで返却を進めていることである。
// agent session は従来どおり client の token 経由でしか返却させず、生きたプロセスを持つ貸出は拒否する。
func (m *Manager) ReleaseLease(ctx context.Context, sessionID, reason string, discard bool) (map[string]any, error) {
	session, err := m.store.SessionByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.LeaseKind == "" || session.LeaseKind == state.LeaseKindAgent {
		return nil, fmt.Errorf("session %s was leased to an agent; it is released when that agent exits, or by wx clear --all", sessionID)
	}
	if processAlive(session.ClientPID) || processAlive(session.AgentPID) {
		return nil, fmt.Errorf("session %s is still running; exit that shell or command, or use wx clear --all", sessionID)
	}
	inUse := sessionInUse(session.State)
	// 返却済みの貸出へ --discard だけを追いかけさせる余地は残す。保存が先に走った後でも実体を消せるようにするためである。
	if !inUse && !discard {
		return nil, fmt.Errorf("session %s is no longer in use (state %s)", sessionID, session.State)
	}
	if inUse {
		m.releaseLeaseWithoutToken(ctx, state.OrphanCandidate{ID: session.ID, WorkspaceID: session.WorkspaceID, SlotID: session.SlotID}, reason)
		// 親を返却したので、この貸出が用意した子貸出も待たずに返す。
		m.releaseOrphanedChildLeases(ctx)
	}
	if !discard {
		return map[string]any{"released": true, "session_id": sessionID, "discarded": false}, nil
	}
	discarded, err := m.discardLeaseSlot(ctx, session.SlotID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"released": true, "session_id": sessionID, "discarded": discarded}, nil
}

// discardRemovalWait は保存ジョブの完了を待って削除を予約し直す上限である。
// 返却で登録した SNAPSHOT が既に走っている間は予約が通らないため、短い間だけ待ってから応答する。
// 待ち切れなかった場合は失敗にせず、保存が終わってからの再実行を CLI が案内する。
const discardRemovalWait = 3 * time.Second

// discardLeaseSlot は保存を要求せず slot の削除を予約する。予約できたかを返す。
func (m *Manager) discardLeaseSlot(ctx context.Context, slotID string) (bool, error) {
	deadline := time.Now().Add(discardRemovalWait)
	for {
		job, changed, err := m.store.ScheduleDiscardRemoval(ctx, slotID)
		if err != nil {
			return false, err
		}
		if changed {
			m.schedule(job)
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
