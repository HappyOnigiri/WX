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
	"github.com/HappyOnigiri/WX/internal/domain"
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

func TestMergeSlotSharedFileCacheReplacesOnlyMeasuredSlot(t *testing.T) {
	t.Parallel()
	previous := workspace.SharedFileCache{
		"workspace/slot/repo/file":  {Shared: true},
		"workspace/slot2/repo/file": {Shared: true},
	}
	measured := workspace.SharedFileCache{"workspace/slot/repo/new": {Shared: false}}
	merged := mergeSlotSharedFileCache(previous, measured, "workspace/slot")
	if _, stale := merged["workspace/slot/repo/file"]; stale {
		t.Fatalf("stale measured-slot entry survived: %+v", merged)
	}
	if got, kept := merged["workspace/slot/repo/new"]; !kept || got.Shared {
		t.Fatalf("measured entry missing or changed: %+v", merged)
	}
	if _, kept := merged["workspace/slot2/repo/file"]; !kept {
		t.Fatalf("other-slot entry was removed: %+v", merged)
	}
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
	_, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotRoot := filepath.Join(root, string(workspaceRecord.ID), "slot")
	if _, _, err := manager.createSlotRoot(slotRoot, slotRoot); err != nil {
		t.Fatalf("create slot root: %v", err)
	}

	if _, err := store.CreateStandby(t.Context(), slotAtPath(t, manager, string(workspaceRecord.ID), "slot", slotRoot, 1, "READY"), nil); err != nil {
		t.Fatal(err)
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
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
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
	cacheName := filepath.ToSlash(filepath.Join(slot.RelPath, "repo", "file"))
	// 共有判定の cache は root 単位で持ち、次の root 全体の測定が再判定を省けるようにする。
	if workspace.SharingSupported() && len(manager.sharedFiles[filepath.Clean(cfg.Storage.WorktreeRoot)]) != 1 {
		t.Fatalf("shared cache=%+v", manager.sharedFiles)
	}
	// 共有元を削除した測定では、対象 slot の古い cache entry も破棄する。
	if err := os.Remove(filepath.Join(mainPath, "file")); err != nil {
		t.Fatal(err)
	}
	manager.measureSlotUsage(ctx, "slot")
	if _, kept := manager.sharedFiles[filepath.Clean(cfg.Storage.WorktreeRoot)][cacheName]; kept {
		t.Fatalf("unverifiable cache entry survived: %+v", manager.sharedFiles)
	}
	if !workspace.SharingSupported() {
		return
	}
	// 準備直後に測れているので、周期測定を待たずに方式が決まる。
	if view := slotView(state.SlotSummary{SlotID: "slot", State: "LEASED"}, manager.slotUsage); view.CopyMode == "" {
		t.Fatalf("slot view=%+v", view)
	}
}

func TestMeasureRootUsageKeepsSlotsMeasuredAfterItsTargetSnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
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

// 削除の完了した slot は、次の周期測定を待たずに Status の Disk から消えることを検査する。
// `wx clear` の直後に消えた分が残ると、利用者は空いたはずの容量を確認できない。
func TestRemovedSlotLeavesTheRootTotalWithoutWaitingForTheNextMeasurement(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotID := domain.StableID("remove-slot", "usage")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "REMOVING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot.Path, "payload"), make([]byte, 65536), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.measureRootUsage(ctx)
	measured := statusRootUsage(t, manager, root)
	if measured.AllocatedBytes < 65536 {
		t.Fatalf("root usage did not account for the slot payload: %+v", measured)
	}

	if err := manager.removeSlotJob(ctx, state.Job{SlotID: slotID}); err != nil {
		t.Fatal(err)
	}
	if _, kept := manager.slotUsage[slotID]; kept {
		t.Fatalf("removed slot sample survived: %+v", manager.slotUsage)
	}
	after := statusRootUsage(t, manager, root)
	if after.AllocatedBytes > measured.AllocatedBytes-65536 {
		t.Fatalf("root usage kept the removed slot: before=%+v after=%+v", measured, after)
	}
	// 引いただけで測り直してはいないので、root の他の部分の鮮度を表す測定時刻は動かない。
	if after.MeasuredAt != measured.MeasuredAt {
		t.Fatalf("measured_at changed without a new measurement: before=%q after=%q", measured.MeasuredAt, after.MeasuredAt)
	}
}

// 測定後に増えた slot を引いても root 合計を負にしない。負の使用量は Status で意味を持たない。
func TestForgetSlotUsageStopsTheSubtractionAtZero(t *testing.T) {
	t.Parallel()
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := filepath.Clean(manager.Config().Storage.WorktreeRoot)
	manager.rootUsage = map[string]rootUsageSample{root: {bytes: 100, allocated: 200, shared: 50, measuredAt: time.Now()}}
	manager.slotUsage["slot"] = slotUsageSample{usage: workspace.SlotUsage{LogicalBytes: 400, AllocatedBytes: 800, SharedBytes: 200}}

	manager.forgetSlotUsage("slot", root)
	if sample := manager.rootUsage[root]; sample.bytes != 0 || sample.allocated != 0 || sample.shared != 0 {
		t.Fatalf("root sample went negative: %+v", sample)
	}
}

// 使用量の測り直しは1本に保ち、走っている間に届いた要求は畳んで追加の1巡にする。
// 一巡ごとに root 全体を歩き直すため、削除や準備が連続した回に walk を要求数だけ重ねない。
func TestRootUsageMeasurementRequestsCoalesceIntoOneSweep(t *testing.T) {
	t.Parallel()
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	if !manager.claimRootUsageMeasurement() {
		t.Fatal("the first request did not take the sweep")
	}
	if manager.claimRootUsageMeasurement() || manager.claimRootUsageMeasurement() {
		t.Fatal("a concurrent request started a second sweep")
	}
	if !manager.nextRootUsageMeasurement() {
		t.Fatal("the folded requests were dropped")
	}
	if manager.nextRootUsageMeasurement() {
		t.Fatal("the sweep repeated without a pending request")
	}
	if !manager.claimRootUsageMeasurement() {
		t.Fatal("the released sweep could not be retaken")
	}
}

// 準備の終わった slot は、周期測定を待たずに root 合計へ現れる。
// 容量が変わってから Disk が追いつくまでの目標は 10 秒で、待ち時間の上限でそれを検査する。
func TestPreparedSlotEntersTheRootTotalWithoutWaitingForTheNextMeasurement(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	manager.measureRootUsage(ctx)
	before := statusRootUsage(t, manager, root)

	slotID := domain.StableID("prepared-slot", "usage")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	const payload = 65536
	if err := os.WriteFile(filepath.Join(slot.Path, "payload"), make([]byte, payload), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.scheduleSlotUsageMeasurement(slotID)

	deadline := time.Now().Add(10 * time.Second)
	for {
		after := statusRootUsage(t, manager, root)
		if after.AllocatedBytes >= before.AllocatedBytes+payload {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("root total did not pick up the prepared slot: before=%+v after=%+v", before, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 準備中の slot は書き込みの途中を歩くので、その途中経過をその slot の使用量として公開しない。
// 実体は root 合計に数えたままにし、登録外の容量へ移して警告に見せない。
func TestMeasureRootUsageSkipsSlotsThatAreStillBeingPrepared(t *testing.T) {
	t.Parallel()
	_, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotRoot := filepath.Join(root, string(workspaceRecord.ID), "slot")
	if _, _, err := manager.createSlotRoot(slotRoot, slotRoot); err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	if _, err := store.CreateStandby(t.Context(), slotAtPath(t, manager, string(workspaceRecord.ID), "slot", slotRoot, 1, "PREPARING"), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotRoot, "payload"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}

	manager.measureRootUsage(t.Context())
	manager.mu.RLock()
	_, published := manager.slotUsage["slot"]
	manager.mu.RUnlock()
	if published {
		t.Fatal("published the usage of a slot that is still being prepared")
	}
	measured := statusRootUsage(t, manager, root)
	if measured.AllocatedBytes < 8192 {
		t.Fatalf("root usage lost the bytes of the preparing slot: %+v", measured)
	}

	// 準備が終われば次の測定で載る。準備中に落としたまま忘れないことを検査する。
	if err := store.SetSlotState(t.Context(), "slot", []string{"PREPARING"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	manager.measureRootUsage(t.Context())
	manager.mu.RLock()
	sample, measuredSlot := manager.slotUsage["slot"]
	manager.mu.RUnlock()
	if !measuredSlot || sample.usage.AllocatedBytes < 8192 {
		t.Fatalf("slot usage after preparation=%+v measured=%v", sample, measuredSlot)
	}
}
