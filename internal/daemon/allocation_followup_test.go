package daemon

import (
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func TestCreateSlotRootReturnsLeaseIdentityForSingleRepositoryLease(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(base, "worktrees")
	store, err := openTestStoreAtPath(t, filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)

	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "workspace", "slot")
	repositories := []state.SlotRepository{{RepositoryID: "repository", DirName: "repository"}}
	leasePathValue := leasePath(slotPath, "repository", repositories)
	if leasePathValue == slotPath {
		t.Fatal("single-repository lease path must differ from slot path")
	}

	slotIdentity, leaseIdentity, err := manager.createSlotRoot(slotPath, leasePathValue)
	if err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	if slotIdentity == "" || leaseIdentity == "" {
		t.Fatalf("slot identity=%q lease identity=%q", slotIdentity, leaseIdentity)
	}
	if slotIdentity == leaseIdentity {
		t.Fatalf("separate slot and lease directories share identity %q", slotIdentity)
	}

	gotSlotIdentity, err := pathIdentity(slotPath)
	if err != nil {
		t.Fatalf("slot identity: %v", err)
	}
	gotLeaseIdentity, err := pathIdentity(leasePathValue)
	if err != nil {
		t.Fatalf("lease identity: %v", err)
	}
	if slotIdentity != gotSlotIdentity {
		t.Fatalf("slot identity=%q want=%q", slotIdentity, gotSlotIdentity)
	}
	if leaseIdentity != gotLeaseIdentity {
		t.Fatalf("lease identity=%q want=%q", leaseIdentity, gotLeaseIdentity)
	}
}
