package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestSlotViewReportsCopyModeFromTheMeasuredSharing(t *testing.T) {
	measuredAt := time.Unix(1700000000, 0).UTC()
	usage := map[string]slotUsageSample{
		"cow":  {usage: workspace.SlotUsage{Files: 3, Compared: 3, SharedFiles: 2, SharedBytes: 2048, AllocatedBytes: 4096}, measuredAt: measuredAt},
		"copy": {usage: workspace.SlotUsage{Files: 3, Compared: 3, AllocatedBytes: 4096}, measuredAt: measuredAt},
	}
	shared := slotView(state.SlotSummary{SlotID: "cow", State: "LEASED"}, usage)
	if !workspace.SharingSupported() {
		if shared.Measurement != slotSharingUnsupported || shared.CopyMode != "" {
			t.Fatalf("unsupported platform view=%+v", shared)
		}
		return
	}
	if shared.CopyMode != config.CopyModeCOW || shared.ExclusiveBytes != 2048 || shared.Measurement != slotSharingMeasurement {
		t.Fatalf("shared slot view=%+v", shared)
	}
	if shared.MeasuredAt != state.FormatTime(measuredAt) {
		t.Fatalf("measured at=%q", shared.MeasuredAt)
	}
	plain := slotView(state.SlotSummary{SlotID: "copy", State: "READY"}, usage)
	if plain.CopyMode != config.CopyModeCopy || plain.ExclusiveBytes != 4096 || plain.SharedBytes != 0 {
		t.Fatalf("copied slot view=%+v", plain)
	}
	// 未測定の slot は 0 バイトではなく pending として返し、測定前と空の worktree を取り違えないようにする。
	pending := slotView(state.SlotSummary{SlotID: "unmeasured", State: "READY"}, usage)
	if pending.Measurement != rootUsagePendingMeasurement || pending.CopyMode != "" || pending.AllocatedBytes != 0 {
		t.Fatalf("pending slot view=%+v", pending)
	}
	// slot を持たない session の行には方式も使用量も無い。
	detached := slotView(state.SlotSummary{SessionID: "archived", SessionState: "ARCHIVED"}, usage)
	if detached.Measurement != "" || detached.CopyMode != "" {
		t.Fatalf("detached session view=%+v", detached)
	}
}

func TestMeasureSlotUsageRecordsThePreparedSlotAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	defer manager.Close()
	ctx := context.Background()

	mainPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(mainPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainPath, "file"), []byte("main content"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{
		{ID: "repository", MainPath: discoveryPath(mainPath), CommonDir: discoveryPath(filepath.Join(mainPath, ".git")), DefaultBranch: "main"},
	}}
	registered, _, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(registered.ID)
	slot := testSlot(t, manager, workspaceID, "slot", 1, "LEASED")
	worktree := filepath.Join(slot.Path, "repo")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("main content"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 測定対象から外れる実体を root 直下に置き、slot 分だけを数えていることを確かめる。
	if err := os.WriteFile(filepath.Join(cfg.Storage.WorktreeRoot, "outside"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	repos := []state.SlotRepository{{RepositoryID: "repository", DirName: "repo", WorktreePath: worktree, State: "READY", BaseOID: "oid", Fingerprint: "fingerprint"}}
	session := state.Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("session")}
	if _, err := store.CreateSlotSession(ctx, slot, repos, session, ""); err != nil {
		t.Fatal(err)
	}

	manager.measureSlotUsage(ctx, "slot")
	sample, measured := manager.slotUsage["slot"]
	if !measured || sample.usage.Files != 1 || sample.measuredAt.IsZero() {
		t.Fatalf("slot sample=%+v measured=%v", sample, measured)
	}
	if !workspace.SharingSupported() {
		return
	}
	// 共有判定の cache は root 単位で持ち、次の root 全体の測定が再判定を省けるようにする。
	if len(manager.sharedFiles[filepath.Clean(cfg.Storage.WorktreeRoot)]) != 1 {
		t.Fatalf("shared cache=%+v", manager.sharedFiles)
	}
	// 準備直後に測れているので、周期測定を待たずに方式が決まる。
	if view := slotView(state.SlotSummary{SlotID: "slot", State: "LEASED"}, manager.slotUsage); view.CopyMode == "" {
		t.Fatalf("slot view=%+v", view)
	}
}

func TestMeasureRootUsageKeepsSlotsMeasuredAfterItsTargetSnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	defer manager.Close()

	// 対象一覧を撮った後に準備が終わった slot は今回の測定に入らないため、上書きすると pending へ戻る。
	manager.slotUsage["fresh"] = slotUsageSample{usage: workspace.SlotUsage{Files: 2}, measuredAt: time.Now().Add(time.Hour)}
	// 前回の測定で消えた slot は残さない。
	manager.slotUsage["gone"] = slotUsageSample{usage: workspace.SlotUsage{Files: 3}, measuredAt: time.Now().Add(-time.Hour)}
	manager.measureRootUsage(context.Background())

	if sample, kept := manager.slotUsage["fresh"]; !kept || sample.usage.Files != 2 {
		t.Fatalf("fresh sample=%+v kept=%v", sample, kept)
	}
	if _, kept := manager.slotUsage["gone"]; kept {
		t.Fatalf("stale sample survived: %+v", manager.slotUsage)
	}
}
