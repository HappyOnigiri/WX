package daemon

import (
	"runtime/debug"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
	buildversion "github.com/HappyOnigiri/WX/internal/version"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestDaemonVersionKeepsVCSAndEmbeddedFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		info     *debug.BuildInfo
		ok       bool
		embedded string
		want     string
	}{
		{
			name:     "main module version",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260905214154-46e8d6f209e2"}},
			ok:       true,
			embedded: "v-test-dev",
			want:     "v0.0.0-20260905214154-46e8d6f209e2",
		},
		{
			name: "VCS revision",
			info: &debug.BuildInfo{
				Main:     debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			},
			ok:       true,
			embedded: "v-test-dev",
			want:     "abc123",
		},
		{
			name:     "embedded version without VCS",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			ok:       true,
			embedded: "v-test-dev",
			want:     "v-test-dev",
		},
		{
			name: "devel without embedded version",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			ok:   true,
			want: "devel",
		},
		{
			name: "unknown without build info",
			ok:   false,
			want: "unknown",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := daemonVersionForBuildInfo(test.info, test.ok, test.embedded); got != test.want {
				t.Fatalf("daemonVersionForBuildInfo()=%q, want %q", got, test.want)
			}
		})
	}
}

func TestManagerStatusReportsDaemonVersion(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := status["daemon_version"], daemonVersion(); got != want {
		t.Fatalf("status daemon_version=%v, want %v", got, want)
	}
}

func TestManagerStatusReportsDaemonUptimeSeconds(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	manager.started = time.Now().Add(-37 * time.Second)

	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	uptime, ok := status["uptime_seconds"].(int)
	if !ok {
		t.Fatalf("status uptime_seconds=%T %v, want int", status["uptime_seconds"], status["uptime_seconds"])
	}
	if uptime < 37 || uptime > 38 {
		t.Fatalf("status uptime_seconds=%d, want roughly 37", uptime)
	}
}

func TestDaemonVersionUsesEmbeddedBuildWithoutVCS(t *testing.T) {
	embedded, configured := buildversion.EmbeddedString()
	if !configured {
		t.Skip("no linker-embedded version")
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatalf("ReadBuildInfo returned no information")
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		t.Skip("the test binary has a VCS-derived module version")
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			t.Skip("the test binary has VCS metadata")
		}
	}
	if got := daemonVersion(); got != embedded {
		t.Fatalf("daemonVersion()=%q, want embedded version %q", got, embedded)
	}
}

func TestManagerStatusUsesEmbeddedVersionWithoutVCS(t *testing.T) {
	embedded, configured := buildversion.EmbeddedString()
	if !configured {
		t.Skip("no linker-embedded version")
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatalf("ReadBuildInfo returned no information")
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		t.Skip("the test binary has a VCS-derived module version")
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			t.Skip("the test binary has VCS metadata")
		}
	}

	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := status["daemon_version"]; got != embedded {
		t.Fatalf("status daemon_version=%v, want embedded version %q", got, embedded)
	}
}

func TestDoctorReportsGitAndSQLiteFailures(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	t.Setenv("PATH", t.TempDir())
	reply := manager.Doctor(ctx)
	git := doctorProblem(t, reply, diag.CheckGit)
	if git.Cause == "" || git.Action == "" {
		t.Fatalf("git failure lacks a cause or an action: %+v", git)
	}
}

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

// policy は設定側の方針で DB には無いため、Status が workspace ごとに埋めることを確かめる。
func TestManagerStatusReportsTheConfiguredWorktreePolicy(t *testing.T) {
	ctx, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
	cfg := manager.Config()
	cfg.Workspaces = map[string]config.Workspace{string(workspaceRecord.Root): {Worktree: "cold"}}
	manager.cfg = cfg

	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	details, ok := status["workspace_details"].([]state.WorkspaceDiagnostic)
	if !ok || len(details) != 1 {
		t.Fatalf("status workspace_details=%v", status["workspace_details"])
	}
	if got := details[0].Policy; got != "cold" {
		t.Fatalf("workspace policy=%q, want %q", got, "cold")
	}
}

// 設定に無い workspace は全体の既定方針を返し、policy を空のままにしない。
func TestManagerStatusFallsBackToTheDefaultWorktreePolicy(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	details, ok := status["workspace_details"].([]state.WorkspaceDiagnostic)
	if !ok || len(details) != 1 {
		t.Fatalf("status workspace_details=%v", status["workspace_details"])
	}
	if got, want := details[0].Policy, manager.Config().Worktree.Undefined; got != want {
		t.Fatalf("workspace policy=%q, want %q", got, want)
	}
}
