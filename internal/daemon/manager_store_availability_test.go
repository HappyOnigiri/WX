package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestManagerFailsClosedWhenStateStoreBecomesUnavailable(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "missing", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m.recoverJobs(false)
	work, slot, ok := m.jobQueue.take()
	if !ok || work.id != job.ID {
		t.Fatalf("recovered job=%+v ok=%v", work, ok)
	}
	m.jobQueue.finish(work, slot)
	m.maybeBackup(ctx)
	m.maybeBackup(ctx)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(ctx); err == nil {
		t.Fatal("status succeeded after state store closure")
	}
	doctor := m.Doctor(ctx)
	checks := doctor["checks"].(map[string]any)
	if checks["sqlite"] == "ok" {
		t.Fatalf("doctor did not report SQLite failure: %v", checks)
	}
	if diagnostics := m.artifactDiagnostics(ctx); len(diagnostics["errors"].([]string)) == 0 {
		t.Fatalf("artifact diagnostics did not report state failure: %v", diagnostics)
	}
	m.reconcileArtifacts(ctx)
	m.reconcileRegistry(ctx)
	m.reconcileOrphans(ctx)
	m.recoverJobs(false)
	m.mu.Lock()
	m.lastBackup = time.Time{}
	m.mu.Unlock()
	m.maybeBackup(ctx)
	if err := m.enqueue("ENSURE_STANDBY", "", "", ""); err == nil {
		t.Fatal("job enqueue succeeded after state store closure")
	}
	for _, job := range []state.Job{{Kind: "PREPARE", WorkspaceID: "missing"}, {Kind: "ENSURE_STANDBY", WorkspaceID: "missing"}, {Kind: "SNAPSHOT", SessionID: "missing"}} {
		if err := m.runRecoveredJob(ctx, job); err == nil {
			t.Fatalf("%s job succeeded after state store closure", job.Kind)
		}
	}
	if _, err := m.GC(ctx, false); err == nil {
		t.Fatal("GC succeeded after state store closure")
	}
	m.cancel()
	m.schedule(state.Job{ID: "cancelled"})
	m.scheduleDelayed(state.Job{ID: "cancelled"}, time.Millisecond)
	m.Close()
	m.jobQueue.setInteractiveLimit(1)
	if m.jobQueue.add(queuedJob{id: "after-close", class: jobClassInteractive}) {
		t.Fatal("closed job queue accepted new work")
	}
}
