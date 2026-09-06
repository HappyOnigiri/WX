package daemon

import (
	"os"
	"testing"
)

func TestStandbyRegistrationFailureQuarantinesCreatedSlot(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
	manager.mu.Lock()
	cfg := manager.cfg
	cfg.Pool.WarmPerWorkspace = 1
	manager.cfg = cfg
	manager.mu.Unlock()
	raw := openManagerCoverageDB(t, databasePath)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_standby_job BEFORE INSERT ON jobs WHEN NEW.kind='PREPARE' BEGIN SELECT RAISE(ABORT,'injected standby registration failure'); END`); err != nil {
		t.Fatal(err)
	}
	rootPath, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.createStandbySlot(ctx, rootPath, rootID, workspaceRecord, resolved, 1, nil); err == nil {
		t.Fatal("standby allocation succeeded despite an injected job registration failure")
	}
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].State != "QUARANTINED" {
		t.Fatalf("failed standby registration artifacts=%+v, want one QUARANTINED slot", artifacts)
	}
	if info, err := os.Stat(artifacts[0].Path); err != nil || !info.IsDir() {
		t.Fatalf("quarantined standby root was not preserved: info=%v err=%v", info, err)
	}
}
