package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// leaseAttrs は agent 起動以外への worktree 貸出（wx shell / wx run / wx new）の属性である。
// Kind が空なら従来の agent 起動で、期限も親も持たない。
// OwnerSessionID は wx new を呼んだ親 session で、親の終了で子貸出もまとめて返却する根拠になる。
type leaseAttrs struct {
	Kind           string
	OwnerSessionID string
	// Prepare はこの貸出で準備する slot にだけ効く設定の上書きで、`wx bench` の設定比較が使う。
	Prepare config.PrepareOverride
}

// withPrepareOverride は貸出要求の準備設定上書きを検証して attrs へ載せる。
// 値が不正なまま準備へ進めると、測定用の設定が黙って既定へ落ちた結果を比較表に並べてしまう。
func (a leaseAttrs) withPrepareOverride(copyMode string, cowMinSizeKiB *int) (leaseAttrs, error) {
	override := config.PrepareOverride{CopyMode: copyMode, COWMinSizeKiB: cowMinSizeKiB}
	if err := override.Validate(); err != nil {
		return leaseAttrs{}, fmt.Errorf("prepare override: %w", err)
	}
	a.Prepare = override
	return a, nil
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

// isLeaseKind は lease_kind が agent 起動以外への貸出かを返す。
func isLeaseKind(kind string) bool {
	return kind != "" && kind != state.LeaseKindAgent
}

// resumeAgentMatches は復元要求の agent が元の session と両立するかを返す。
// 貸出は種別をまたいで復元できる。厳密一致にすると wx new が出した貸出を wx shell --resume で開けない。
// agent 会話は ID の移譲と argv の作法が種別ごとに違うため厳密一致に留める。
func resumeAgentMatches(agent, originalAgent, leaseKind, originalLeaseKind string) bool {
	if agent == originalAgent {
		return true
	}
	return isLeaseKind(leaseKind) && isLeaseKind(originalLeaseKind)
}

// releaseLeaseWithoutToken は session token を持たない側からの返却を、通常の返却経路へ載せる。
// orphan 回収・期限掃引・親連動・wx release が共有し、保存の要否と slot の遷移は Store が決める。
// 書き込みの失敗は error で返す。再試行できる周期処理と wx release で扱いが違うためである。
func (m *Manager) releaseLeaseWithoutToken(ctx context.Context, candidate state.OrphanCandidate, reason string) error {
	_, err := m.releaseLeaseDiscarding(ctx, candidate, reason, false)
	return err
}

// releaseLeaseDiscarding は token を持たない返却を進め、discard が真なら保存を積まず削除を予約する。
// 保存を省くのは利用者が明示した --discard だけなので、周期処理からの返却は releaseLeaseWithoutToken を使う。
// 戻り値は削除を予約できたかで、偽なら slot は従来どおり保存経路に載っている（PREPARING などで予約が通らない場合を含む）。
func (m *Manager) releaseLeaseDiscarding(ctx context.Context, candidate state.OrphanCandidate, reason string, discard bool) (bool, error) {
	release := m.store.ReleaseWithOutcome
	if discard {
		release = m.store.ReleaseDiscardingWithOutcome
	}
	job, changed, quarantineExpired, err := release(ctx, candidate.ID, candidate.WorkspaceID, candidate.SlotID)
	if err != nil {
		return false, fmt.Errorf("release lease %s (%s): %w", candidate.ID, reason, err)
	}
	if quarantineExpired {
		m.log.Warn("session expired without a recovery snapshot: slot is quarantined", "session_id", candidate.ID, "slot_id", candidate.SlotID, "reason", reason)
	}
	if changed {
		m.schedule(job)
		return discard, nil
	}
	m.releaseLease(candidate.ID)
	return false, nil
}

// leaseCandidateRunning は貸出のプロセスがまだ生きているかを返す。
// 期限掃引と親連動は、実行中の wx shell / wx run を返却しないためこれで候補を見送る。
// 見送った候補は次の巡回で拾い直す。
func leaseCandidateRunning(candidate state.OrphanCandidate) bool {
	return processAlive(candidate.ClientPID) || processAlive(candidate.AgentPID)
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
		if leaseCandidateRunning(candidate) {
			continue
		}
		m.log.Info("releasing a lease that reached lease.ttl", "session_id", candidate.ID, "slot_id", candidate.SlotID)
		if err := m.releaseLeaseWithoutToken(ctx, candidate, "lease-expired"); err != nil {
			m.log.Error("lease release failed", "session_id", candidate.ID, "error", err)
		}
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
		if leaseCandidateRunning(candidate) {
			continue
		}
		m.log.Info("releasing a lease whose owner session ended", "session_id", candidate.ID, "slot_id", candidate.SlotID)
		if err := m.releaseLeaseWithoutToken(ctx, candidate, "lease-owner-ended"); err != nil {
			m.log.Error("lease release failed", "session_id", candidate.ID, "error", err)
		}
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
	scheduled := false
	if inUse {
		candidate := state.OrphanCandidate{ID: session.ID, WorkspaceID: session.WorkspaceID, SlotID: session.SlotID}
		var err error
		if scheduled, err = m.releaseLeaseDiscarding(ctx, candidate, reason, discard); err != nil {
			return nil, err
		}
		// 親を返却したので、この貸出が用意した子貸出も待たずに返す。
		m.releaseOrphanedChildLeases(ctx)
	}
	if !discard {
		return map[string]any{"released": true, "session_id": sessionID, "discarded": false}, nil
	}
	// 返却と同じ transaction で削除を積めた場合は、保存の完了を待つ必要がないのでそのまま返す。
	if scheduled {
		return map[string]any{"released": true, "session_id": sessionID, "discarded": true}, nil
	}
	discarded, pending, err := m.discardLeaseSlot(ctx, session.SlotID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"released": true, "session_id": sessionID, "discarded": discarded, "discard_pending": pending}, nil
}

// ReleaseLease の discard_pending が返す、削除を予約できなかった理由である。
// CLI が再実行の案内を出し分ける根拠なので、値は RPC の一部として扱う。
const (
	// DiscardPendingSaving は返却で積んだ保存がまだ走っていて、予約が通らない状態である。
	DiscardPendingSaving = "saving"
	// DiscardPendingRemoved は slot が既に保管済み・削除中で、再実行しても変わらない状態である。
	DiscardPendingRemoved = "already-removed"
)

// discardRemovalWait は保存ジョブの完了を待って削除を予約し直す上限である。
// 返却済みの貸出を --discard で追いかける経路では、先に走り出した SNAPSHOT の間は予約が通らないため、短い間だけ待ってから応答する。
// 待ち切れなかった場合は失敗にせず、保存が終わってからの再実行を CLI が案内する。
const discardRemovalWait = 3 * time.Second

// discardLeaseSlot は保存を要求せず slot の削除を予約する。
// 予約できたかと、できなかった理由（DiscardPending*）を返す。
func (m *Manager) discardLeaseSlot(ctx context.Context, slotID string) (bool, string, error) {
	deadline := time.Now().Add(discardRemovalWait)
	for {
		job, changed, err := m.store.ScheduleDiscardRemoval(ctx, slotID)
		if err != nil {
			return false, "", err
		}
		if changed {
			m.schedule(job)
			return true, "", nil
		}
		// 既に保管済み・削除中の slot は待っても予約が通らないので、待たずに理由を返す。
		slot, err := m.store.Slot(ctx, slotID)
		if err != nil {
			return false, "", err
		}
		if slot.State == "ARCHIVED" || slot.State == "REMOVING" {
			return false, DiscardPendingRemoved, nil
		}
		if time.Now().After(deadline) {
			return false, DiscardPendingSaving, nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
