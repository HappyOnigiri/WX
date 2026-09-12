package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
)

func (m *Manager) ReloadConfig() error {
	return m.reloadConfig(true)
}

func (m *Manager) reloadConfig(runGC bool) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	cfg, err := config.Load()
	if err != nil {
		m.mu.Lock()
		m.lastReload = time.Now()
		m.reloadError = err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.RLock()
	configuredRoot := m.cfg.Storage.WorktreeRoot
	m.mu.RUnlock()
	oldConfiguredRoot, oldRootErr := config.ExpandHome(configuredRoot)
	if oldRootErr != nil {
		oldConfiguredRoot = configuredRoot
	}
	oldConfiguredRoot = filepath.Clean(oldConfiguredRoot)
	newRoot, newHandle, err := ensureWorktreeRootDescriptor(cfg.Storage.WorktreeRoot)
	if err != nil {
		m.mu.Lock()
		m.lastReload = time.Now()
		m.reloadError = err.Error()
		m.mu.Unlock()
		return fmt.Errorf("validate worktree root: %w", err)
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	if m.rootClosing {
		m.mu.Unlock()
		_ = newHandle.Close()
		return errManagerClosed
	}
	oldRoot := oldConfiguredRoot
	if oldRoot != newRoot {
		// 旧rootのslotは移動・STALE化せず寿命まで使い、新規slotだけを新rootに置く。
		for existingPath := range m.roots {
			if existingPath == newRoot || !rootPathsOverlap(existingPath, newRoot) || !m.rootHasReferencesLocked(existingPath) {
				continue
			}
			reloadErr := fmt.Errorf("cannot reload overlapping worktree root %s while it has in-flight references", existingPath)
			m.lastReload = time.Now()
			m.reloadError = reloadErr.Error()
			m.mu.Unlock()
			_ = newHandle.Close()
			return fmt.Errorf("validate worktree root: %w", reloadErr)
		}
	}
	var existingHandle *os.Root
	if existing := m.rootRefs[newRoot]; existing != nil && !existing.closed && !existing.retired {
		existingHandle = existing.root
	}
	newIdentity, newIdentityErr := descriptorIdentity(newHandle)
	if expected := m.rootIdentities[newRoot]; expected != "" && (newIdentityErr != nil || expected != newIdentity) {
		reloadErr := fmt.Errorf("worktree root inode changed (expected %s, got %s)", expected, newIdentity)
		if newIdentityErr != nil {
			reloadErr = fmt.Errorf("inspect reloaded worktree root: %w", newIdentityErr)
		}
		m.lastReload = time.Now()
		m.reloadError = reloadErr.Error()
		m.mu.Unlock()
		_ = newHandle.Close()
		return fmt.Errorf("validate worktree root: %w", reloadErr)
	}
	if existingHandle != nil {
		existingInfo, existingErr := existingHandle.Lstat(".")
		newInfo, newInfoErr := newHandle.Lstat(".")
		if existingErr != nil || newInfoErr != nil || !os.SameFile(existingInfo, newInfo) {
			reloadErr := errors.New("configured worktree root inode changed")
			if existingErr != nil {
				reloadErr = fmt.Errorf("inspect current worktree root: %w", existingErr)
			} else if newInfoErr != nil {
				reloadErr = fmt.Errorf("inspect reloaded worktree root: %w", newInfoErr)
			}
			m.lastReload = time.Now()
			m.reloadError = reloadErr.Error()
			m.mu.Unlock()
			_ = newHandle.Close()
			return fmt.Errorf("validate worktree root: %w", reloadErr)
		}
	}
	if existing := m.rootRefs[newRoot]; existing != nil && !existing.closed && !existing.retired {
		_ = newHandle.Close()
		newHandle = nil
	}
	// root の physical path と inode を検査した後に、実効設定が同じかを判定する。
	// 設定ファイルの mtime では root の置き換えを見逃すため、検査を省いてここへ来ることはない。
	if oldRoot == newRoot && newHandle == nil && m.cfg.EffectiveEqual(cfg) {
		m.roots[newRoot] = true
		m.lastReload = time.Now()
		m.reloadError = ""
		m.mu.Unlock()
		return nil
	}
	if oldRoot != newRoot {
		m.roots[oldRoot] = false
		m.roots[newRoot] = true
		m.retireRootLocked(oldRoot)
	}
	if newHandle != nil {
		m.rootIdentities[newRoot] = newIdentity
		m.rootRefs[newRoot] = &managedRoot{root: newHandle, identity: newIdentity}
	}
	m.cfg = cfg
	m.git.SetTimeout(cfg.MaxReadinessTimeout())
	if m.logLevel != nil {
		m.logLevel.Set(slogLevel(cfg.Logging.Level))
	}
	m.lastReload = time.Now()
	m.reloadError = ""
	m.roots[newRoot] = true
	m.mu.Unlock()
	// EnsureActiveRootは旧rootをinactiveとして残し、そのslotを再発見可能にする。
	m.registerRootGeneration(context.Background(), newRoot, newIdentity)
	m.loadRootGenerations(context.Background())
	m.jobQueue.setInteractiveLimit(cfg.Pool.PreparationConcurrency)
	select {
	case m.reloads <- struct{}{}:
	default:
	}
	if runGC {
		// 設定が変わったときだけ再探索と GC を要求する。要求先は定期保守と同じ経路で、重複した一巡にはならない。
		m.startBackground(m.runMaintenance)
	}
	return nil
}
