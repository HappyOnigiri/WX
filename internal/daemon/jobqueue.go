package daemon

import (
	"context"
	"sync"
	"time"
)

// jobClass は実行枠のクラス。利用者が待つ処理を、待機枠の補充や自動削除が占有しないように分ける。
type jobClass int

const (
	// jobClassInteractive は利用者が完了を待つ準備・復元・保存。枠数は pool.preparation_concurrency。
	jobClassInteractive jobClass = iota
	// jobClassMaintenance は待機枠の補充と自動削除。利用者向けの枠は借りず、専用枠だけで動かす。
	jobClassMaintenance
	jobClassCount
)

func (c jobClass) String() string {
	if c == jobClassInteractive {
		return "interactive"
	}
	return "maintenance"
}

// maintenanceJobSlots は保守クラスの実行枠数。
// preparation_concurrency=1 でも保守と利用者処理が互いの完了を待たないよう、利用者向けとは別に常に確保する。
const maintenanceJobSlots = 1

// queuedJobCapacity はクラスごとの未実行キューの上限。
// 溢れた分は PENDING の durable job として残り、maintainJobs の定期回収が拾う。
const queuedJobCapacity = 256

// queuedJob は実行待ちのジョブ 1 件。
// class は永続化せず、積むたびにその時点の DB 上の事実から決め直す。
type queuedJob struct {
	id     string
	slotID string
	class  jobClass
	queued time.Time
}

// jobExecutionSlot は実行中のジョブが持つ実行枠である。
// 枠の保持をジョブの goroutine と分け、ロック待ちのような安全な区間で枠を返して取り直せるようにする。
type jobExecutionSlot interface {
	// Release は実行枠を返し、待っているジョブへ回す。枠を持たない状態での呼び出しは無視する。
	Release()
	// Acquire は返した枠を取り直す。context が終わったときだけ false を返す。
	Acquire(ctx context.Context) bool
}

// jobQueue はクラス別の待ち行列と実行枠を持つ。
// 到着順を基本とし、後から来たジョブが同じクラスの既存ジョブを追い越さない。
type jobQueue struct {
	mu       sync.Mutex
	pending  [jobClassCount][]queuedJob
	running  [jobClassCount]int
	limits   [jobClassCount]int
	inflight map[string]bool
	closed   bool
	// changed は枠の空きと待ち行列の変化を待つ側へ知らせる。変化のたびに閉じて作り直す。
	changed chan struct{}
}

func newJobQueue(interactive int) *jobQueue {
	q := &jobQueue{inflight: map[string]bool{}, changed: make(chan struct{})}
	q.limits[jobClassMaintenance] = maintenanceJobSlots
	q.setInteractiveLimit(interactive)
	return q
}

// setInteractiveLimit は利用者向けの実行枠数を変える。
// 縮小しても実行中のコピーや prepare command は中断せず、超過分が終わるまで待つ。
func (q *jobQueue) setInteractiveLimit(limit int) {
	if limit < 1 {
		limit = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.limits[jobClassInteractive] = limit
	q.notifyLocked()
}

// add は未実行のジョブをクラスの待ち行列末尾へ積み、積めたかを返す。
// 待機中・実行中の同じジョブは積み直さず、上限に達したクラスへは積まない。
func (q *jobQueue) add(work queuedJob) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.inflight[work.id] || len(q.pending[work.class]) >= queuedJobCapacity {
		return false
	}
	q.inflight[work.id] = true
	q.pending[work.class] = append(q.pending[work.class], work)
	q.notifyLocked()
	return true
}

// promote は slot の未実行ジョブを利用者向けの待ち行列末尾へ移し、移したかを返す。
// 利用者が完了を待つ clean の削除を、保守枠 1 本の順番待ちに閉じ込めないために使う。
func (q *jobQueue) promote(slotID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if slotID == "" {
		return false
	}
	var remaining []queuedJob
	promoted := false
	for _, work := range q.pending[jobClassMaintenance] {
		if work.slotID != slotID {
			remaining = append(remaining, work)
			continue
		}
		work.class = jobClassInteractive
		q.pending[jobClassInteractive] = append(q.pending[jobClassInteractive], work)
		promoted = true
	}
	if !promoted {
		return false
	}
	q.pending[jobClassMaintenance] = remaining
	q.notifyLocked()
	return true
}

// take は空きのあるクラスから先頭のジョブを 1 件取り出し、その実行枠を確保する。
// 枠を取れなければ何も取り出さないので、キュー待ちのジョブは attempt も job lease も消費しない。
func (q *jobQueue) take() (queuedJob, *jobSlot, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return queuedJob{}, nil, false
	}
	for class := jobClassInteractive; class < jobClassCount; class++ {
		if q.running[class] >= q.limits[class] || len(q.pending[class]) == 0 {
			continue
		}
		work := q.pending[class][0]
		q.pending[class] = q.pending[class][1:]
		q.running[class]++
		return work, &jobSlot{queue: q, class: class, held: true}, true
	}
	return queuedJob{}, nil, false
}

// wait は枠の空きか待ち行列の変化を知らせる channel を返す。取得後の変化を取りこぼさないため、take の直後に読む。
func (q *jobQueue) wait() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.changed
}

// finish は実行枠を返し、ジョブを未実行の重複判定から外す。
func (q *jobQueue) finish(work queuedJob, slot jobExecutionSlot) {
	slot.Release()
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, work.id)
	q.notifyLocked()
}

// close は新しいジョブの受け付けと配送を止める。実行中のジョブは中断せず、枠の返却は受け付ける。
// 配送を使わない部分初期化の Manager からも呼ばれるため、nil receiver を許す。
func (q *jobQueue) close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.notifyLocked()
}

// limit はクラスの実行枠数を返す。
func (q *jobQueue) limit(class jobClass) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.limits[class]
}

// counts はクラスの待機数と実行数を返す。
func (q *jobQueue) counts(class jobClass) (pending, running int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending[class]), q.running[class]
}

func (q *jobQueue) notifyLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// jobSlot は 1 ジョブが持つ実行枠である。保持は所有するジョブの goroutine からだけ操作する。
type jobSlot struct {
	queue *jobQueue
	class jobClass
	held  bool
}

func (s *jobSlot) Release() {
	if s == nil || !s.held {
		return
	}
	s.held = false
	s.queue.mu.Lock()
	defer s.queue.mu.Unlock()
	s.queue.running[s.class]--
	s.queue.notifyLocked()
}

func (s *jobSlot) Acquire(ctx context.Context) bool {
	if s == nil {
		return false
	}
	for {
		if s.held {
			return true
		}
		s.queue.mu.Lock()
		if s.queue.running[s.class] < s.queue.limits[s.class] {
			s.queue.running[s.class]++
			s.queue.mu.Unlock()
			s.held = true
			return true
		}
		changed := s.queue.changed
		s.queue.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}
