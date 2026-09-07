package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// reloadFixture は設定ファイルを持つ manager を用意し、実効設定を daemon の現在値へ揃えた状態から始める。
// 最初の reload は正規化の差だけで変更ありと判定されるため、未変更の判定はその後の reload から観測する。
func reloadFixture(t *testing.T, body string) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	m.reloads = make(chan struct{}, 1)
	writeReloadConfig(t, cfg.Storage.WorktreeRoot, body)
	if err := m.reloadConfig(false); err != nil {
		t.Fatalf("normalizing reload: %v", err)
	}
	return m, cfg.Storage.WorktreeRoot
}

func writeReloadConfig(t *testing.T, worktreeRoot, body string) {
	t.Helper()
	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	document := "version: 1\nstorage:\n  worktree_root: " + worktreeRoot + "\n" + body
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

// countMaintenanceSweeps は保守一巡の開始回数を数える barrier を仕掛ける。
func countMaintenanceSweeps(m *Manager) *atomic.Int64 {
	var sweeps atomic.Int64
	m.mu.Lock()
	m.beforeMaintenanceSweep = func() { sweeps.Add(1) }
	m.mu.Unlock()
	return &sweeps
}

func TestReloadConfigSkipsRediscoveryAndGCWhenNothingChanged(t *testing.T) {
	m, _ := reloadFixture(t, "")
	sweeps := countMaintenanceSweeps(m)
	drainReloadNotice(m)
	before := m.lastReloadAt()
	for range 3 {
		if err := m.ReloadConfig(); err != nil {
			t.Fatalf("reload of an unchanged configuration: %v", err)
		}
	}
	m.backgroundWG.Wait()
	if got := sweeps.Load(); got != 0 {
		t.Fatalf("maintenance sweeps for unchanged reloads=%d, want 0", got)
	}
	if len(m.reloads) != 0 {
		t.Fatal("unchanged reload deferred the periodic maintenance timer")
	}
	if after := m.lastReloadAt(); !after.After(before) {
		t.Fatalf("lastReload=%v was not updated by the verification, before=%v", after, before)
	}
	m.mu.RLock()
	reloadError := m.reloadError
	m.mu.RUnlock()
	if reloadError != "" {
		t.Fatalf("unchanged reload recorded an error: %s", reloadError)
	}
}

func TestReloadConfigAppliesAndRequestsMaintenanceWhenConfigurationChanges(t *testing.T) {
	m, worktreeRoot := reloadFixture(t, "")
	sweeps := countMaintenanceSweeps(m)
	drainReloadNotice(m)
	writeReloadConfig(t, worktreeRoot, "worktree:\n  undefined: hot\npool:\n  preparation_concurrency: 3\n")
	if err := m.ReloadConfig(); err != nil {
		t.Fatalf("reload of a changed configuration: %v", err)
	}
	if got := m.Config().Worktree.Undefined; got != "hot" {
		t.Fatalf("worktree.undefined=%q, want hot", got)
	}
	if got := m.jobQueue.limit(jobClassInteractive); got != 3 {
		t.Fatalf("preparation concurrency=%d, want 3", got)
	}
	if len(m.reloads) != 1 {
		t.Fatal("changed reload did not restart the periodic maintenance timer")
	}
	m.backgroundWG.Wait()
	if got := sweeps.Load(); got != 1 {
		t.Fatalf("maintenance sweeps for a changed reload=%d, want 1", got)
	}
}

func TestReloadConfigStillRejectsASwappedRootAfterAnUnchangedReload(t *testing.T) {
	m, worktreeRoot := reloadFixture(t, "")
	if err := m.reloadConfig(false); err != nil {
		t.Fatalf("unchanged reload: %v", err)
	}
	if err := os.RemoveAll(worktreeRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.reloadConfig(false); err == nil || !strings.Contains(err.Error(), "inode changed") {
		t.Fatalf("swapped root behind an unchanged configuration error=%v", err)
	}
}

func TestMaintenanceRunsOneSweepAtATimeAndCoalescesRequests(t *testing.T) {
	m, _ := reloadFixture(t, "")
	var sweeps atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	m.mu.Lock()
	m.beforeMaintenanceSweep = func() {
		if sweeps.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); m.runMaintenance() }()
	<-entered
	// 実行中に届いた要求は dirty へ集約し、並行した一巡を増やさない。
	for range 3 {
		m.runMaintenance()
	}
	if got := sweeps.Load(); got != 1 {
		t.Fatalf("concurrent maintenance sweeps=%d, want 1", got)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance sweep did not finish")
	}
	if got := sweeps.Load(); got != 2 {
		t.Fatalf("maintenance sweeps for one in-flight run plus coalesced requests=%d, want 2", got)
	}
	m.maintenanceMu.Lock()
	running, dirty := m.maintenanceRunning, m.maintenanceDirty
	m.maintenanceMu.Unlock()
	if running || dirty {
		t.Fatalf("maintenance state after the final sweep running=%v dirty=%v", running, dirty)
	}
}

func (m *Manager) lastReloadAt() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastReload
}

func drainReloadNotice(m *Manager) {
	select {
	case <-m.reloads:
	default:
	}
}
