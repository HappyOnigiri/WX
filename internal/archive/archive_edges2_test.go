package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

// TestSnapshotWithPersistencePropagatesFailuresInEachLockedPhase exercises
// the two error branches unique to SnapshotWithPersistence itself (as
// opposed to Snapshot, which shares snapshotObjects/publishSnapshotRefs but
// wraps them differently): a Git failure while capturing the snapshot under
// the first lock, and a Git failure while publishing recovery refs under the
// second lock, after metadata has already been durably persisted.
func TestSnapshotWithPersistencePropagatesFailuresInEachLockedPhase(t *testing.T) {
	t.Run("snapshot capture failure", func(t *testing.T) {
		repository, repo, manager, _ := archiveFixture(t)
		installGitFault(t, " rev-parse HEAD ", 1)
		persistCalled := false
		if _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "capture-failure", time.Now().Add(time.Hour), func(state.Snapshot) error {
			persistCalled = true
			return nil
		}); err == nil {
			t.Fatal("snapshot capture succeeded despite an injected Git failure")
		}
		if persistCalled {
			t.Fatal("persistence callback ran despite a failed snapshot capture")
		}
	})

	t.Run("ref publication failure", func(t *testing.T) {
		repository, repo, manager, _ := archiveFixture(t)
		installGitFault(t, " update-ref --create-reflog ", 1)
		persistCalled := false
		if _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "publish-failure", time.Now().Add(time.Hour), func(state.Snapshot) error {
			persistCalled = true
			return nil
		}); err == nil {
			t.Fatal("snapshot succeeded despite an injected ref-publication Git failure")
		}
		if !persistCalled {
			t.Fatal("persistence callback did not run before the ref-publication attempt")
		}
		if refs := gitCommand(t, repository, "for-each-ref", "--format=%(refname)", "refs/wx/recovery"); refs != "" {
			t.Fatalf("recovery refs published despite an injected failure: %s", refs)
		}
	})
}

// TestSnapshotAndRestorePropagateTemporaryIndexCreationFailures exercises the
// os.CreateTemp failure branches in snapshotObjects and Restore: pointing
// TMPDIR at a path that does not exist makes both temporary-index files
// impossible to create.
func TestSnapshotAndRestorePropagateTemporaryIndexCreationFailures(t *testing.T) {
	t.Run("snapshot temporary index", func(t *testing.T) {
		repository, repo, manager, _ := archiveFixture(t)
		// A clean worktree skips the temporary index entirely (see
		// snapshotObjects's clean short-circuit); dirty it so this exercises
		// the os.CreateTemp failure it is meant to test.
		if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
		if _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "tmp-failure", time.Now().Add(time.Hour), nil); err == nil || !strings.Contains(err.Error(), "temporary snapshot index") {
			t.Fatalf("snapshot succeeded despite an unusable TMPDIR: %v", err)
		}
	})

	t.Run("restore temporary index", func(t *testing.T) {
		repository, repo, manager, worktreeRoot := archiveFixture(t)
		snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "source", time.Now().Add(time.Hour), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
		target := filepath.Join(worktreeRoot, "tmp-restore", "root")
		pointAtSlot(t, manager, worktreeRoot, target)
		if err := manager.Restore(context.Background(), repo, target, "tmp-restore", snapshot); err == nil || !strings.Contains(err.Error(), "temporary restore index") {
			t.Fatalf("restore succeeded despite an unusable TMPDIR: %v", err)
		}
	})
}

// TestRestorePropagatesRelockedRecoveryRefVerificationFailure exercises the
// second recovery-ref verification loop inside Restore's locked closure
// (distinct from the pre-lock check performed before PrepareForRestore):
// occurrence 4 of the verify pattern lands on the first ref check taken after
// the common-directory lock is acquired (head/worktree/index each verify once
// pre-lock, so the relocked loop starts at occurrence 4).
func TestRestorePropagatesRelockedRecoveryRefVerificationFailure(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "source", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	installGitFault(t, " rev-parse --verify refs/wx/recovery", 4)
	target := filepath.Join(worktreeRoot, "relock-fault", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "relock-fault", snapshot); err == nil || !strings.Contains(err.Error(), "changed during restore") {
		t.Fatalf("restore succeeded despite an injected relocked verification failure: %v", err)
	}
}
