package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceIntervalUsesDefaultAtNonPositiveBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "negative", in: -time.Nanosecond, want: 10 * time.Minute},
		{name: "zero", in: 0, want: 10 * time.Minute},
		{name: "positive", in: time.Nanosecond, want: time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := maintenanceInterval(test.in); got != test.want {
				t.Fatalf("maintenance interval for %s=%s, want %s", test.in, got, test.want)
			}
		})
	}
}

func TestBackupDueIncludesExactDailyBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		last time.Time
		want bool
	}{
		{name: "never backed up", last: time.Time{}, want: true},
		{name: "just before boundary", last: now.Add(-24*time.Hour + time.Nanosecond), want: false},
		{name: "exact boundary", last: now.Add(-24 * time.Hour), want: true},
		{name: "past boundary", last: now.Add(-24*time.Hour - time.Nanosecond), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := backupDue(test.last, now); got != test.want {
				t.Fatalf("backupDue(%s, %s)=%v, want %v", test.last, now, got, test.want)
			}
		})
	}
}

func TestManagerHandlesUnavailableRootAndZeroLifecycleInterval(t *testing.T) {
	t.Parallel()
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		blockedRoot := filepath.Join(s.Root, "blocked-root")
		if err := os.WriteFile(blockedRoot, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		s.Config.Storage.WorktreeRoot = blockedRoot
		s.Config.Pool.PreparationConcurrency = 0
	})
	manager := f.Manager
	if len(manager.roots) != 0 {
		t.Fatalf("unavailable worktree root was registered: %v", manager.roots)
	}
	manager.Close()

	// 周期処理だけを手で回す Manager は、この検査が自分で作って自分で閉じる。
	lifecycle := testManager(t, f.Config, f.Store)
	lifecycle.cfg.Discovery.ReconcileInterval.Duration = 0
	lifecycle.cancel()
	lifecycle.maintainLifecycle()
	lifecycle.Close()
}
