package daemon

import (
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
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
