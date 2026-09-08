package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type (
	retryableJobError      struct{ error }
	dependencyPendingError struct{ error }
)

const maxJobAttempts = 8

// jobClassOf は job の実行クラスを、その job row の事実だけから決める。
// SNAPSHOT は保存と将来の resume の前提なので、利用者が明示的に待っているかによらず利用者向けに置く。
// 待機用の準備・補充・自動削除は保守用とし、clean が完了を待つ削除だけを jobQueue.promote で昇格させる。
func jobClassOf(job state.Job) jobClass {
	switch job.Kind {
	case "PREPARE":
		if job.SessionID != "" {
			return jobClassInteractive
		}
		return jobClassMaintenance
	case "RESTORE", "SNAPSHOT":
		return jobClassInteractive
	default:
		return jobClassMaintenance
	}
}

// dispatchJobs はクラス別の実行枠が空くたびに未実行のジョブを 1 件配る。
// 枠を取ってから ClaimJob するため、キュー待ちのジョブは attempt も job lease も消費しない。
func (m *Manager) dispatchJobs() {
	for {
		// 変化の通知は take より先に受け取る。後で取ると、その間の登録や枠の返却を取りこぼして配送が止まる。
		changed := m.jobQueue.wait()
		work, slot, ok := m.jobQueue.take()
		if ok {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				m.executeJob(work, slot)
			}()
			continue
		}
		select {
		case <-m.ctx.Done():
			return
		case <-changed:
		}
	}
}

// executeJob は確保済みの実行枠で job を 1 回実行し、結果に応じて終了・retry・依存待ちへ落とす。
// 枠と重複判定の解放は実行の成否によらず行う。
func (m *Manager) executeJob(work queuedJob, slot jobExecutionSlot) {
	defer func() {
		m.jobQueue.finish(work, slot)
		m.notifyLifecycleCheckIfPending()
	}()
	owner := fmt.Sprintf("%d:%d", os.Getpid(), m.jobSeq.Add(1))
	waited := time.Since(work.queued)
	job, err := m.store.ClaimJob(context.Background(), work.id, owner)
	if err != nil {
		m.log.Debug("claim job skipped", "job_id", work.id, "owner", owner, "error", err)
		return
	}
	started := time.Now()
	m.mu.RLock()
	barrier := m.beforeJobRun
	m.mu.RUnlock()
	if barrier != nil {
		barrier(job)
	}
	jobCtx, cancel := context.WithCancel(m.ctx)
	// 実行枠をロック待ちの手放し先として渡す。
	// 同じリポジトリの Git 管理操作を待つだけのジョブが、無関係なリポジトリの枠を占有しなくなる。
	jobCtx = gitx.WithLockWaiter(jobCtx, slot)
	done := make(chan struct{})
	m.startBackground(func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := m.store.RenewJob(context.Background(), job.ID, owner); err != nil {
					m.log.Error("renew job lease failed", "job_id", job.ID, "error", err)
					cancel()
					return
				}
			case <-done:
				return
			}
		}
	})
	err = m.runRecoveredJob(jobCtx, job)
	close(done)
	cancel()
	// キュー待ちと実行の時間は原因が別なので分けて記録する。前者は枠の不足、後者は処理そのものの重さを表す。
	m.log.Debug("job attempt finished", "job_id", job.ID, "kind", job.Kind, "class", work.class.String(), "attempt", job.Attempt, "queue_wait", waited, "run_time", time.Since(started))
	m.settleJobAttempt(work, job, owner, err)
}

// settleJobAttempt は 1 回の実行結果を job の状態へ反映する。
// 依存待ちは retry budget を消費せず、retry 上限に達したものは終端させて slot の扱いを確定する。
func (m *Manager) settleJobAttempt(work queuedJob, job state.Job, owner string, err error) {
	var pending dependencyPendingError
	if errors.As(err, &pending) {
		delay := 5 * time.Second
		if deferErr := m.store.DeferJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); deferErr != nil {
			m.log.Error("defer dependency-bound job failed", "job_id", work.id, "error", deferErr)
			m.releaseLease(job.SessionID)
			return
		}
		m.scheduleDelayed(job, delay)
		return
	}
	var retryable retryableJobError
	if errors.As(err, &retryable) {
		m.log.Debug("job attempt failed and will be retried", "job_id", job.ID, "kind", job.Kind, "attempt", job.Attempt, "error", err)
		if job.Attempt >= maxJobAttempts {
			m.log.Error("job exhausted retry limit", "job_id", job.ID, "attempt", job.Attempt, "error", err)
			if job.Kind == "REMOVE" && job.SessionID != "" {
				// 保存の検証に失敗した正常終了 slot を隔離 GC の破棄対象に変えない。
				_ = m.store.SetSlotState(context.Background(), job.SlotID, []string{"REMOVING"}, "SNAPSHOTTED", "REMOVAL_RETRY_EXHAUSTED")
			} else {
				_ = m.store.SetSlotState(context.Background(), job.SlotID, []string{"PREPARING", "RESTORING", "FAILED", "REMOVING", "RETIRING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED")
			}
			if finishErr := m.finishJob(context.Background(), work.id, owner, err); finishErr != nil {
				m.log.Error("finish exhausted job failed", "job_id", work.id, "error", finishErr)
			}
			m.releaseLease(job.SessionID)
			return
		}
		delay := time.Duration(1<<min(job.Attempt, 6)) * time.Second
		if retryErr := m.store.RetryJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); retryErr != nil {
			m.log.Error("reschedule job failed", "job_id", work.id, "error", retryErr)
			m.releaseLease(job.SessionID)
			return
		}
		m.scheduleDelayed(job, delay)
		return
	}
	if finishErr := m.finishJob(context.Background(), work.id, owner, err); finishErr != nil {
		m.log.Error("finish job failed", "job_id", work.id, "error", finishErr)
	}
	m.releaseLease(job.SessionID)
}

func (m *Manager) enqueue(kind, workspaceID, slotID, sessionID string) error {
	job, err := m.store.CreateJob(context.Background(), kind, workspaceID, slotID, sessionID)
	if err != nil {
		return err
	}
	m.schedule(job)
	return nil
}

// schedule は job を実行待ちへ積む。DB を読まないので、RPC handler を実行枠待ちで塞がない。
// 積めなかった分は PENDING の durable job として残り、maintainJobs の定期回収が拾う。
func (m *Manager) schedule(job state.Job) {
	if m.ctx.Err() != nil {
		return
	}
	work := queuedJob{id: job.ID, slotID: job.SlotID, class: jobClassOf(job), queued: time.Now()}
	if !m.jobQueue.add(work) {
		m.log.Debug("job was left for durable recovery", "job_id", job.ID, "kind", job.Kind, "class", work.class.String())
	}
}

func (m *Manager) scheduleDelayed(job state.Job, delay time.Duration) {
	m.startBackground(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-m.ctx.Done():
		case <-timer.C:
			m.schedule(job)
		}
	})
}

func (m *Manager) recoverJobs(reclaimAll bool) {
	jobs, err := m.store.RecoverJobs(context.Background(), reclaimAll)
	if err != nil {
		m.log.Error("recover jobs failed", "error", err)
		return
	}
	for _, job := range jobs {
		m.schedule(job)
	}
}

func (m *Manager) finishJob(ctx context.Context, id, owner string, runErr error) error {
	var prepareErr *workspace.PrepareCommandError
	if errors.As(runErr, &prepareErr) {
		failureCode := "PREPARE_FAILED"
		if prepareErr.FailureID != "" {
			failureCode += ":" + prepareErr.FailureID
		}
		return m.store.FinishJobWithDetail(ctx, id, owner, runErr, failureCode, prepareErr.DetailPath)
	}
	return m.store.FinishJob(ctx, id, owner, runErr)
}

func (m *Manager) runRecoveredJob(ctx context.Context, job state.Job) error {
	switch job.Kind {
	case "PREPARE":
		if job.Attempt > 1 {
			if err := m.store.ResetPreparationForRetry(ctx, job.SlotID); err != nil {
				return retryableJobError{err}
			}
		}
		w, err := m.store.Workspace(ctx, job.WorkspaceID)
		if err != nil {
			return err
		}
		repos, err := m.store.SlotRepositories(ctx, job.SlotID)
		if err != nil {
			return err
		}
		resolved, err := m.resolvedFromStored(ctx, w, repos)
		if err != nil {
			return err
		}
		if err := m.prepareSlotWithJob(ctx, job.SlotID, w, resolved, repos, job); err != nil {
			m.suspendStandbyReplenishment(ctx, job)
			if errors.Is(err, state.ErrOwnership) {
				return err
			}
			var prepareErr *workspace.PrepareCommandError
			if errors.As(err, &prepareErr) {
				// prepare command は副作用を持つため、終了コードだけを根拠に再実行しない。
				return err
			}
			return retryableJobError{err}
		}
		return nil
	case "ENSURE_STANDBY":
		w, err := m.store.Workspace(ctx, job.WorkspaceID)
		if err != nil {
			return err
		}
		return m.ensureStandby(ctx, w)
	case "SNAPSHOT":
		s, err := m.store.SessionByID(ctx, job.SessionID)
		if err != nil {
			return err
		}
		return m.snapshotSession(ctx, s)
	case "RESTORE":
		return m.resumeRestoreJob(ctx, job.SessionID)
	case "REMOVE":
		if err := m.removeSlotJob(ctx, job); err != nil {
			if job.SessionID == "" && errors.Is(err, state.ErrOwnership) {
				return err
			}
			return retryableJobError{err}
		}
		return nil
	case "REMOVE_REPOSITORY":
		if err := m.removeColdRepositoryJob(ctx, job); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				return err
			}
			return retryableJobError{err}
		}
		return nil
	default:
		return fmt.Errorf("unknown persistent job kind %s", job.Kind)
	}
}
