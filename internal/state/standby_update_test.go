package state

import (
	"context"
	"testing"
)

func TestStandbyUpdateReservationAndReleaseAreDurable(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	repository := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "old", Fingerprint: "old-fingerprint", CompatibilityFingerprint: "compatible"}
	prepareJob, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, []SlotRepository{repository})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplacePlacements(ctx, "slot", []Placement{{RepositoryID: "repository", RelativePath: "local.cfg", Kind: "copy", SourcePath: "/source/local.cfg", ContentSHA256: "old-hash"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, "slot"); err != nil {
		t.Fatal(err)
	}
	claimedPrepare, err := store.ClaimJob(ctx, prepareJob.ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimedPrepare.ID, "prepare", nil); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	target := SlotRepository{RepositoryID: "repository", RequestedRef: "feature", BaseOID: "new", Fingerprint: "new-fingerprint", CompatibilityFingerprint: "compatible", UpdateBaseOID: "old", UpdateFingerprint: "old-fingerprint"}
	newPlacement := Placement{RepositoryID: "repository", RelativePath: "local.cfg", Kind: "copy", SourcePath: "/source/local.cfg", ContentSHA256: "new-hash"}
	updateJob, err := store.ReserveStandbyUpdate(ctx, "slot", session, []SlotRepository{target}, []Placement{newPlacement}, "copy")
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.Slot(ctx, "slot")
	if err != nil || reserved.State != "PREPARING" || reserved.OwnerSessionID != "slot" || reserved.UpdateCopyMode != "copy" || reserved.EarlyReadyAt != "" {
		t.Fatalf("reserved slot=%+v err=%v", reserved, err)
	}
	if updateJob.Kind != "UPDATE" || updateJob.SessionID != "slot" {
		t.Fatalf("update job=%+v", updateJob)
	}
	if placements, err := store.UpdatePlacements(ctx, "slot"); err != nil || len(placements) != 1 || placements[0].ContentSHA256 != "new-hash" {
		t.Fatalf("update placements=%+v err=%v", placements, err)
	}
	if err := store.BeginStandbyUpdate(ctx, "slot"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRepositoryUpdateRunning(ctx, "slot", "repository"); err != nil {
		t.Fatal(err)
	}
	if _, scheduled, err := store.Release(ctx, "slot", "workspace", "slot"); err != nil || scheduled {
		t.Fatalf("release during update scheduled=%t err=%v", scheduled, err)
	}
	releaseJob, released, _, replenished, err := store.FinishStandbyUpdate(ctx, "slot")
	if err != nil || !released || replenished || releaseJob.Kind != "SNAPSHOT" {
		t.Fatalf("finish update release=%+v released=%t replenished=%t err=%v", releaseJob, released, replenished, err)
	}
	finished, err := store.SlotRepository(ctx, "slot", "repository")
	if err != nil || finished.BaseOID != "new" || finished.Fingerprint != "new-fingerprint" || finished.State != "READY" {
		t.Fatalf("finished repository=%+v err=%v", finished, err)
	}
	placements, err := store.Placements(ctx, "slot")
	if err != nil || len(placements) != 1 || placements[0].ContentSHA256 != "new-hash" {
		t.Fatalf("finished placements=%+v err=%v", placements, err)
	}
}
