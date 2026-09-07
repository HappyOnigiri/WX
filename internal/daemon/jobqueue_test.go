package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestJobQueueKeepsArrivalOrderPerClassAndDropsDuplicates(t *testing.T) {
	t.Parallel()
	q := newJobQueue(2)
	for _, work := range []queuedJob{
		{id: "standby", slotID: "standby-slot", class: jobClassMaintenance},
		{id: "prepare", slotID: "session-slot", class: jobClassInteractive},
		{id: "prepare", slotID: "session-slot", class: jobClassInteractive},
		{id: "snapshot", slotID: "session-slot", class: jobClassInteractive},
	} {
		q.add(work)
	}
	if pending, _ := q.counts(jobClassInteractive); pending != 2 {
		t.Fatalf("interactive queue length=%d, want 2 after the duplicate was dropped", pending)
	}
	first, firstSlot, ok := q.take()
	if !ok || first.id != "prepare" {
		t.Fatalf("first dispatch=%+v ok=%v, want the interactive job that arrived first", first, ok)
	}
	second, secondSlot, ok := q.take()
	if !ok || second.id != "snapshot" {
		t.Fatalf("second dispatch=%+v ok=%v", second, ok)
	}
	// 保守枠は利用者向けの枠が埋まっても独立して動く。
	maintenance, maintenanceSlot, ok := q.take()
	if !ok || maintenance.id != "standby" {
		t.Fatalf("maintenance dispatch=%+v ok=%v", maintenance, ok)
	}
	if _, _, ok := q.take(); ok {
		t.Fatal("dispatch exceeded the configured execution slots")
	}
	// 実行中のジョブIDも重複判定に含め、完了後は積み直せる。
	if q.add(queuedJob{id: "prepare", class: jobClassInteractive}) {
		t.Fatal("running job was queued again")
	}
	q.finish(first, firstSlot)
	q.finish(second, secondSlot)
	q.finish(maintenance, maintenanceSlot)
	if !q.add(queuedJob{id: "prepare", class: jobClassInteractive}) {
		t.Fatal("finished job could not be queued again")
	}
}

func TestJobQueueKeepsBothClassesRunnableAtConcurrencyOne(t *testing.T) {
	t.Parallel()
	q := newJobQueue(1)
	q.add(queuedJob{id: "restore", class: jobClassInteractive})
	q.add(queuedJob{id: "remove", class: jobClassMaintenance})
	for range 2 {
		if _, _, ok := q.take(); !ok {
			t.Fatal("a class had no execution slot at preparation_concurrency=1")
		}
	}
	if _, running := q.counts(jobClassInteractive); running != 1 {
		t.Fatalf("interactive running=%d, want 1", running)
	}
	if _, running := q.counts(jobClassMaintenance); running != maintenanceJobSlots {
		t.Fatalf("maintenance running=%d, want %d", running, maintenanceJobSlots)
	}
}

func TestJobQueueLeavesOverflowUnqueuedForDurableRecovery(t *testing.T) {
	t.Parallel()
	q := newJobQueue(1)
	for index := range queuedJobCapacity {
		if !q.add(queuedJob{id: fmt.Sprintf("queued-%d", index), class: jobClassMaintenance}) {
			t.Fatalf("job %d was refused below the queue capacity", index)
		}
	}
	if q.add(queuedJob{id: "overflow", class: jobClassMaintenance}) {
		t.Fatal("queue accepted work beyond its capacity")
	}
	if !q.add(queuedJob{id: "interactive", class: jobClassInteractive}) {
		t.Fatal("a full maintenance queue blocked interactive work")
	}
}

func TestJobQueuePromotesCleanAwaitedRemovalToTheInteractiveClass(t *testing.T) {
	t.Parallel()
	q := newJobQueue(1)
	q.add(queuedJob{id: "standby", slotID: "other-slot", class: jobClassMaintenance})
	q.add(queuedJob{id: "remove", slotID: "clean-slot", class: jobClassMaintenance})
	if q.promote("") || q.promote("unknown-slot") {
		t.Fatal("promotion moved work for a slot without queued jobs")
	}
	if !q.promote("clean-slot") {
		t.Fatal("clean-awaited removal was not promoted")
	}
	if pending, _ := q.counts(jobClassMaintenance); pending != 1 {
		t.Fatalf("maintenance queue length=%d, want the untouched job only", pending)
	}
	work, _, ok := q.take()
	if !ok || work.id != "remove" || work.class != jobClassInteractive {
		t.Fatalf("promoted dispatch=%+v ok=%v", work, ok)
	}
}

func TestJobSlotReleasesAndReacquiresItsExecutionSlot(t *testing.T) {
	t.Parallel()
	q := newJobQueue(1)
	q.add(queuedJob{id: "running", class: jobClassInteractive})
	q.add(queuedJob{id: "waiting", class: jobClassInteractive})
	running, slot, ok := q.take()
	if !ok {
		t.Fatal("the first interactive job was not dispatched")
	}
	if _, _, ok := q.take(); ok {
		t.Fatal("a second job ran while the only interactive slot was held")
	}
	// ロック待ちのような区間で枠を返すと、待っているジョブが先に進める。
	slot.Release()
	slot.Release()
	waiting, waitingSlot, ok := q.take()
	if !ok || waiting.id != "waiting" {
		t.Fatalf("dispatch after release=%+v ok=%v", waiting, ok)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reacquired := make(chan bool, 1)
	go func() { reacquired <- slot.Acquire(ctx) }()
	select {
	case got := <-reacquired:
		t.Fatalf("reacquire returned %v while the slot was still taken", got)
	case <-time.After(50 * time.Millisecond):
	}
	q.finish(waiting, waitingSlot)
	if !<-reacquired {
		t.Fatal("reacquire failed after the slot became free")
	}
	if _, running := q.counts(jobClassInteractive); running != 1 {
		t.Fatalf("interactive running=%d after reacquire, want 1", running)
	}
	cancel()
	// 枠が空かないまま context が終われば、取り直しは失敗して枠を持たないまま戻る。
	canceled, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if (&jobSlot{queue: q, class: jobClassInteractive}).Acquire(canceled) {
		t.Fatal("acquire succeeded without a free execution slot")
	}
	q.finish(running, slot)
}

// waitForRunning はクラスの実行数が want になるまで待つ。
func waitForRunning(t *testing.T, q *jobQueue, class jobClass, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, running := q.counts(class); running == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, running := q.counts(class)
	t.Fatalf("%s running=%d, want %d", class, running, want)
}

// TestJobSlotIsReturnedWhileWaitingForACommonDirectoryLock は、共通ロック待ちのジョブが実行枠を返し、
// 無関係なリポジトリのジョブがその枠で開始できることを確認する。
// preparation_concurrency=1 でも、枠の取り直しはロック取得後なので循環待ちにならない。
func TestJobSlotIsReturnedWhileWaitingForACommonDirectoryLock(t *testing.T) {
	t.Parallel()
	q := newJobQueue(1)
	q.add(queuedJob{id: "same-repository", class: jobClassInteractive})
	q.add(queuedJob{id: "other-repository", class: jobClassInteractive})
	blocked, blockedSlot, ok := q.take()
	if !ok {
		t.Fatal("the first interactive job was not dispatched")
	}
	runner := &gitx.Runner{}
	common := filepath.Join(t.TempDir(), ".git")
	_, releaseHolder, err := runner.AcquireCommonDirLock(context.Background(), common)
	if err != nil {
		t.Fatalf("hold the common-directory lock: %v", err)
	}
	waited := make(chan error, 1)
	go func() {
		ctx := gitx.WithLockWaiter(context.Background(), blockedSlot)
		waited <- runner.WithCommonDirLock(ctx, common, func(context.Context) error { return nil })
	}()
	waitForRunning(t, q, jobClassInteractive, 0)
	other, otherSlot, ok := q.take()
	if !ok || other.id != "other-repository" {
		t.Fatalf("dispatch while the lock wait released its slot=%+v ok=%v", other, ok)
	}
	releaseHolder()
	select {
	case err := <-waited:
		t.Fatalf("the lock wait finished without reacquiring an execution slot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	q.finish(other, otherSlot)
	if err := <-waited; err != nil {
		t.Fatalf("common-directory lock after waiting: %v", err)
	}
	if _, running := q.counts(jobClassInteractive); running != 1 {
		t.Fatalf("interactive running=%d after the lock wait returned, want 1", running)
	}
	q.finish(blocked, blockedSlot)
}
