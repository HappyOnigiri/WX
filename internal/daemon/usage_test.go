package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type reportedRootUsage struct {
	Path           string `json:"path"`
	Bytes          int64  `json:"bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	Measurement    string `json:"measurement"`
	MeasuredAt     string `json:"measured_at"`
}

func statusRootUsage(t *testing.T, manager *Manager, root string) reportedRootUsage {
	t.Helper()
	status, err := manager.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status["worktree_roots"])
	if err != nil {
		t.Fatal(err)
	}
	var reported []reportedRootUsage
	if err := json.Unmarshal(encoded, &reported); err != nil {
		t.Fatal(err)
	}
	for _, item := range reported {
		if filepath.Clean(item.Path) == filepath.Clean(root) {
			return item
		}
	}
	t.Fatalf("root %s is missing from worktree_roots=%s", root, encoded)
	return reportedRootUsage{}
}

// Status は root 配下を walk せず lifecycle が測った値を返す。要求のたびに測ると総ファイル数に比例して遅くなるため、
// 未測定は pending として数値を出さず、測定後は cache の値と測定時刻を返すことを検査する。
func TestStatusReportsCachedRootUsageInsteadOfMeasuringPerRequest(t *testing.T) {
	t.Parallel()
	_, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotRoot := filepath.Join(root, string(workspaceRecord.ID), "slot")
	if _, _, err := manager.createSlotRoot(slotRoot, slotRoot); err != nil {
		t.Fatalf("create slot root: %v", err)
	}

	pending := statusRootUsage(t, manager, root)
	if pending.Measurement != rootUsagePendingMeasurement || pending.MeasuredAt != "" || pending.AllocatedBytes != 0 {
		t.Fatalf("unmeasured root usage=%+v", pending)
	}

	if err := os.WriteFile(filepath.Join(slotRoot, "payload"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.measureRootUsage(t.Context())
	measured := statusRootUsage(t, manager, root)
	if measured.Measurement != rootUsageMeasurement || measured.MeasuredAt == "" {
		t.Fatalf("measured root usage=%+v", measured)
	}
	if measured.Bytes < 8192 || measured.AllocatedBytes < 8192 {
		t.Fatalf("measured root usage did not account for the slot payload: %+v", measured)
	}

	// 追加分は次の測定まで反映されない。ここが変わるなら Status が要求経路で walk している。
	if err := os.WriteFile(filepath.Join(slotRoot, "second"), make([]byte, 65536), 0o600); err != nil {
		t.Fatal(err)
	}
	again := statusRootUsage(t, manager, root)
	if again != measured {
		t.Fatalf("Status re-measured root usage: before=%+v after=%+v", measured, again)
	}

	manager.measureRootUsage(t.Context())
	refreshed := statusRootUsage(t, manager, root)
	if refreshed.AllocatedBytes <= measured.AllocatedBytes {
		t.Fatalf("re-measurement did not pick up the new file: before=%+v after=%+v", measured, refreshed)
	}
}

// 測定を打ち切った回は cache を据え置き、部分的な値で既存の測定結果を壊さない。
func TestMeasureRootUsageKeepsPreviousSampleWhenCanceled(t *testing.T) {
	t.Parallel()
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	manager.measureRootUsage(t.Context())
	measured := statusRootUsage(t, manager, root)
	if measured.Measurement != rootUsageMeasurement {
		t.Fatalf("root usage was not measured: %+v", measured)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	manager.measureRootUsage(canceled)
	if after := statusRootUsage(t, manager, root); after != measured {
		t.Fatalf("canceled measurement replaced the cached sample: before=%+v after=%+v", measured, after)
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

func TestRootDirectoryUsageFailsWhenAnEntryIsUnreadable(t *testing.T) {
	t.Parallel()
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	blocked := filepath.Join(root, "blocked")
	if _, _, err := manager.createSlotRoot(blocked, blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	if _, _, err := manager.rootDirectoryUsage(t.Context(), root, nil, nil); err == nil {
		t.Fatal("root directory usage succeeded despite an unreadable entry")
	}
}
