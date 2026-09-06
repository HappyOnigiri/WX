package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
