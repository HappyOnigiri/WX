package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// prepareMeasurementHistory は保持する準備計測の件数である。
// 計測は診断専用で永続化しないため、`wx bench --runs` の1回分を後から引ける長さだけを持つ。
const prepareMeasurementHistory = 16

// PreparePhase は準備の1区間の名前・回数・所要時間である。
// 名前に "." を含む区間は並列 worker の合計なので、上位区間の実時間を超えることがある。
type PreparePhase struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	MS    int64  `json:"ms"`
}

// PrepareNotice は準備が成功したまま出力を残した区間 1 件である。
// Output は先頭だけを載せ、全文は DetailPath のログが持つ。書き出せなかった回は DetailPath が空になる。
type PrepareNotice struct {
	Target     string `json:"target"`
	Phase      string `json:"phase"`
	Output     string `json:"output,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	DetailPath string `json:"detail_path,omitempty"`
}

// PrepareMeasurement は1回の準備の節目と区間内訳である。daemon のメモリにだけ残り、再起動で消える。
type PrepareMeasurement struct {
	SlotID       string          `json:"slot_id"`
	WorkspaceID  string          `json:"workspace_id"`
	SessionID    string          `json:"session_id,omitempty"`
	StartedAt    string          `json:"started_at"`
	EarlyReadyMS int64           `json:"early_ready_ms"`
	TotalMS      int64           `json:"total_ms"`
	Failed       bool            `json:"failed"`
	Error        string          `json:"error,omitempty"`
	Phases       []PreparePhase  `json:"phases"`
	Notices      []PrepareNotice `json:"notices,omitempty"`
}

// prepareTimer は1回の準備の節目を測り、終了時に計測を Manager へ渡す。
type prepareTimer struct {
	manager     *Manager
	timings     *workspace.PhaseTimings
	notices     *workspace.PrepareNotices
	slotID      string
	workspaceID string
	sessionID   string
	started     time.Time
	early       time.Time
}

// newPrepareTimer は計測を開始し、Preparer へ区間集計と notice の器を差す。
func (m *Manager) newPrepareTimer(slot state.Slot, preparer *workspace.Preparer) *prepareTimer {
	timings := &workspace.PhaseTimings{}
	notices := &workspace.PrepareNotices{}
	preparer.Phases, preparer.Notices = timings, notices
	timer := &prepareTimer{
		manager: m, timings: timings, notices: notices, slotID: slot.ID, workspaceID: slot.WorkspaceID,
		sessionID: slot.OwnerSessionID, started: time.Now(),
	}
	m.mu.Lock()
	if m.activePrepares == nil {
		m.activePrepares = map[string]*prepareTimer{}
	}
	m.activePrepares[timer.slotID] = timer
	m.mu.Unlock()
	return timer
}

// ActivePhase は slot で実行中の準備区間を返す。
// running は準備 job がその slot で走っていることを示し、区間の切れ目でも true のままである。
// 区間の切れ目と job 待ち行列を区別できるよう、ok とは別に返す。
func (m *Manager) ActivePhase(slotID string) (active workspace.ActivePhase, running, ok bool) {
	m.mu.RLock()
	timer := m.activePrepares[slotID]
	m.mu.RUnlock()
	if timer == nil {
		return workspace.ActivePhase{}, false, false
	}
	active, ok = timer.timings.Active()
	return active, true, ok
}

// markEarly は Early Ready の到達時刻を記録する。二段階準備が一度だけ呼ぶ。
func (t *prepareTimer) markEarly() {
	if t == nil || !t.early.IsZero() {
		return
	}
	t.early = time.Now()
}

// finish は計測を確定して Manager の履歴へ積む。失敗した回も、どの区間で止まったかを残すため記録する。
func (t *prepareTimer) finish(prepareErr error) {
	if t == nil {
		return
	}
	t.manager.mu.Lock()
	if t.manager.activePrepares[t.slotID] == t {
		delete(t.manager.activePrepares, t.slotID)
	}
	t.manager.mu.Unlock()
	measurement := PrepareMeasurement{
		SlotID: t.slotID, WorkspaceID: t.workspaceID, SessionID: t.sessionID,
		StartedAt: state.FormatTime(t.started.UTC()),
		TotalMS:   time.Since(t.started).Milliseconds(),
		Failed:    prepareErr != nil,
	}
	if !t.early.IsZero() {
		measurement.EarlyReadyMS = t.early.Sub(t.started).Milliseconds()
	}
	if prepareErr != nil {
		measurement.Error = prepareErr.Error()
	}
	for _, phase := range t.timings.Phases() {
		measurement.Phases = append(measurement.Phases, PreparePhase{Name: phase.Name, Count: phase.Count, MS: phase.Total.Milliseconds()})
	}
	measurement.Phases = orderPreparePhases(measurement.Phases)
	measurement.Notices = t.manager.recordPrepareNotices(t.notices.Notices())
	t.manager.recordPrepareMeasurement(measurement)
}

// prepareNoticeSummary は notice の本文を計測へ載せる長さの上限である。
// これを超える出力は詳細ログへ回し、daemon のメモリと `wx doctor --probe` の応答を hook の出力量に引きずられなくする。
const prepareNoticeSummary = 4 << 10

// recordPrepareNotices は準備が残した出力を daemon log へ warn で出し、長い本文を詳細ログへ退避した notice を返す。
// exit 0 の出力は準備の失敗ではないので、記録の失敗も含めて準備結果は変えない。
func (m *Manager) recordPrepareNotices(notices []workspace.PrepareNotice) []PrepareNotice {
	if len(notices) == 0 {
		return nil
	}
	out := make([]PrepareNotice, 0, len(notices))
	for _, notice := range notices {
		output := notice.Stdout + notice.Stderr
		item := PrepareNotice{Target: notice.Target, Phase: notice.Phase, Output: output}
		if len(output) > prepareNoticeSummary {
			item.Output, item.Truncated = output[:prepareNoticeSummary], true
		}
		detail := fmt.Sprintf("phase: %s\ntarget: %s\nstdout:\n%s\nstderr:\n%s", notice.Phase, notice.Target, notice.Stdout, notice.Stderr)
		item.DetailPath = gitx.WriteDetail(m.prepareDetailDir, detail)
		m.log.Warn("prepare produced output without failing", "phase", notice.Phase, "target", notice.Target, "detail_path", item.DetailPath, "output", item.Output)
		out = append(out, item)
	}
	return out
}

// orderPreparePhases は下位区間（`cow.compare` のようにドットを含む名前）を親区間の直後へ並べ替える。
// 下位区間は親の計測が終わる前に記録されるため、記録順のままでは親より先に現れる。
// 親を持たない下位区間は、出処を隠さないよう元の順で末尾に残す。
func orderPreparePhases(phases []PreparePhase) []PreparePhase {
	children := map[string][]PreparePhase{}
	parents := map[string]bool{}
	for _, phase := range phases {
		if name, _, nested := strings.Cut(phase.Name, "."); nested {
			children[name] = append(children[name], phase)
			continue
		}
		parents[phase.Name] = true
	}
	ordered := make([]PreparePhase, 0, len(phases))
	for _, phase := range phases {
		if strings.Contains(phase.Name, ".") {
			continue
		}
		ordered = append(ordered, phase)
		ordered = append(ordered, children[phase.Name]...)
	}
	for _, phase := range phases {
		if name, _, nested := strings.Cut(phase.Name, "."); nested && !parents[name] {
			ordered = append(ordered, phase)
		}
	}
	return ordered
}

func (m *Manager) recordPrepareMeasurement(measurement PrepareMeasurement) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepareMeasurements = append(m.prepareMeasurements, measurement)
	if len(m.prepareMeasurements) > prepareMeasurementHistory {
		m.prepareMeasurements = m.prepareMeasurements[len(m.prepareMeasurements)-prepareMeasurementHistory:]
	}
}

// PrepareMeasurements は保持している準備計測を新しい順で返す。
// slotID・sessionID を渡すとその slot または貸出先 session の計測に絞る。
// 該当が無い場合は空を返し、失敗にはしない。計測は永続化しないので、daemon 再起動前の回は残らない。
func (m *Manager) PrepareMeasurements(slotID, sessionID string) []PrepareMeasurement {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PrepareMeasurement, 0, len(m.prepareMeasurements))
	for index := len(m.prepareMeasurements) - 1; index >= 0; index-- {
		measurement := m.prepareMeasurements[index]
		if slotID != "" && measurement.SlotID != slotID {
			continue
		}
		if sessionID != "" && measurement.SessionID != sessionID && measurement.SlotID != sessionID {
			continue
		}
		out = append(out, measurement)
	}
	return out
}

// RetireStandby は workspace の貸出されていない READY slot を STALE にし、次の貸出を cold start にする。
// 実体は通常の GC が回収し、補充が standby を作り直す。貸出中の slot と隔離済みの slot には触れない。
func (m *Manager) RetireStandby(ctx context.Context, root string) (map[string]any, error) {
	canonical, err := domain.Canonicalize(root)
	if err != nil {
		return nil, err
	}
	w, err := m.store.WorkspaceByRoot(ctx, string(canonical))
	if errors.Is(err, sql.ErrNoRows) {
		// 未登録の workspace には待機中の slot が無い。cold start の測定は成立するので失敗にしない。
		return map[string]any{"root": string(canonical), "registered": false, "retired": []string{}, "kept": 0}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find registered workspace %s: %w", canonical, err)
	}
	slots, err := m.store.ListSlots(ctx, false)
	if err != nil {
		return nil, err
	}
	retired, kept := []string{}, 0
	for _, slot := range slots {
		if slot.WorkspaceID != string(w.ID) || slot.State != "READY" {
			continue
		}
		if slot.SessionID != "" {
			kept++
			continue
		}
		if err := m.store.SetSlotState(ctx, slot.SlotID, []string{"READY"}, "STALE", "BENCH_COLD_START"); err != nil {
			// 直前に貸出された slot は CAS が失敗するだけで、cold start の妨げにはならない。
			if errors.Is(err, state.ErrOwnership) {
				return nil, err
			}
			kept++
			continue
		}
		retired = append(retired, slot.SlotID)
	}
	return map[string]any{"workspace_id": w.ID, "root": w.Root, "retired": retired, "kept": kept}, nil
}
