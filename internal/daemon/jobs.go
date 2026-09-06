package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type jobWork struct {
	id string
}
type (
	retryableJobError      struct{ error }
	dependencyPendingError struct{ error }
)

const maxJobAttempts = 8

func (m *Manager) resizeWorkers(target int) {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	if m.closed {
		return
	}
	for len(m.workerStops) < target {
		stop := make(chan struct{})
		workerID := m.workerSeq
		m.workerSeq++
		m.workerStops = append(m.workerStops, stop)
		m.wg.Add(1)
		go m.runWorker(workerID, stop)
	}
	for len(m.workerStops) > target {
		last := len(m.workerStops) - 1
		close(m.workerStops[last])
		m.workerStops = m.workerStops[:last]
	}
}

func (m *Manager) runWorker(workerID int, stop <-chan struct{}) {
	defer m.wg.Done()
	owner := fmt.Sprintf("%d:%d", os.Getpid(), workerID)
	for {
		select {
		case <-stop:
			return
		default:
		}
		var work jobWork
		select {
		case <-m.ctx.Done():
			return
		case <-stop:
			return
		case work = <-m.jobs:
		}
		job, err := m.store.ClaimJob(context.Background(), work.id, owner)
		if err != nil {
			m.log.Debug("claim job skipped", "job_id", work.id, "worker", owner, "error", err)
			continue
		}
		jobCtx, cancel := context.WithCancel(m.ctx)
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
		var pending dependencyPendingError
		if errors.As(err, &pending) {
			delay := 5 * time.Second
			if deferErr := m.store.DeferJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); deferErr != nil {
				m.log.Error("defer dependency-bound job failed", "job_id", work.id, "error", deferErr)
				m.releaseLease(job.SessionID)
			} else {
				m.scheduleDelayed(job, delay)
			}
			continue
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
				continue
			}
			delay := time.Duration(1<<min(job.Attempt, 6)) * time.Second
			if retryErr := m.store.RetryJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); retryErr != nil {
				m.log.Error("reschedule job failed", "job_id", work.id, "error", retryErr)
				m.releaseLease(job.SessionID)
			} else {
				m.scheduleDelayed(job, delay)
			}
			continue
		}
		if finishErr := m.finishJob(context.Background(), work.id, owner, err); finishErr != nil {
			m.log.Error("finish job failed", "job_id", work.id, "error", finishErr)
		}
		m.releaseLease(job.SessionID)
	}
}

func (m *Manager) enqueue(kind, workspaceID, slotID, sessionID string) error {
	job, err := m.store.CreateJob(context.Background(), kind, workspaceID, slotID, sessionID)
	if err != nil {
		return err
	}
	m.schedule(job)
	return nil
}

func (m *Manager) schedule(job state.Job) {
	select {
	case <-m.ctx.Done():
	case m.jobs <- jobWork{id: job.ID}:
	default:
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
