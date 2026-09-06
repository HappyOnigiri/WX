package daemon

import (
	"runtime/debug"
	"testing"

	buildversion "github.com/HappyOnigiri/WX/internal/version"
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
