package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

type managedRoot struct {
	// retired rootは操作またはleaseの参照が残る間だけ開き、最後のreleaseで閉じる。
	root     *os.Root
	identity string
	refs     int
	retired  bool
	closed   bool
}

func (m *Manager) beginRootClose() {
	// 新規のdescriptor取得を止め、貸出中のroot参照を解放する。
	// 進行中の操作は完了まで参照を保持し、closeRootHandles が待機する。
	m.mu.Lock()
	m.ensureRootStateLocked()
	m.rootClosing = true
	leases := make([]func(), 0, len(m.leases))
	for id, release := range m.leases {
		leases = append(leases, release)
		delete(m.leases, id)
	}
	m.mu.Unlock()
	for _, release := range leases {
		if release != nil {
			release()
		}
	}
}

func (m *Manager) closeRootHandles() {
	m.mu.Lock()
	m.ensureRootStateLocked()
	for m.rootReferenceCountLocked() > 0 {
		m.rootCond.Wait()
	}
	for path, entry := range m.rootRefs {
		if entry != nil && !entry.closed && entry.root != nil {
			entry.closed = true
			_ = entry.root.Close()
		}
		delete(m.rootRefs, path)
	}
	for path, entries := range m.retiredRefs {
		for _, entry := range entries {
			if entry != nil && !entry.closed && entry.root != nil {
				entry.closed = true
				_ = entry.root.Close()
			}
		}
		delete(m.retiredRefs, path)
	}
	m.mu.Unlock()
}

func (m *Manager) rootReferenceCountLocked() int {
	count := 0
	for _, entry := range m.rootRefs {
		if entry != nil && !entry.closed {
			count += entry.refs
		}
	}
	for _, entries := range m.retiredRefs {
		for _, entry := range entries {
			if entry != nil && !entry.closed {
				count += entry.refs
			}
		}
	}
	return count
}

func (m *Manager) rootHasReferencesLocked(path string) bool {
	if entry := m.rootRefs[path]; entry != nil && !entry.closed && entry.refs > 0 {
		return true
	}
	for _, entry := range m.retiredRefs[path] {
		if entry != nil && !entry.closed && entry.refs > 0 {
			return true
		}
	}
	return false
}

var errManagerClosed = errors.New("daemon manager is closed")

func (m *Manager) ensureRootStateLocked() {
	if m.rootRefs == nil {
		m.rootRefs = map[string]*managedRoot{}
	}
	if m.retiredRefs == nil {
		m.retiredRefs = map[string][]*managedRoot{}
	}
	if m.rootIdentities == nil {
		m.rootIdentities = map[string]string{}
	}
	if m.rootIDs == nil {
		m.rootIDs = map[string]string{}
	}
	if m.roots == nil {
		m.roots = map[string]bool{}
	}
	if m.leases == nil {
		m.leases = map[string]func(){}
	}
	if m.rootCond == nil {
		m.rootCond = sync.NewCond(&m.mu)
	}
}

func (m *Manager) acquireRootLocked(root string, includeRetired bool) (*os.Root, *managedRoot, bool, error) {
	m.ensureRootStateLocked()
	if m.rootClosing {
		return nil, nil, false, errManagerClosed
	}
	if entry := m.rootRefs[root]; entry != nil && !entry.closed && !entry.retired && entry.root != nil {
		entry.refs++
		return entry.root, entry, true, nil
	}
	if includeRetired {
		retired := m.retiredRefs[root]
		for index := len(retired) - 1; index >= 0; index-- {
			entry := retired[index]
			if entry != nil && !entry.closed && entry.root != nil {
				entry.refs++
				return entry.root, entry, true, nil
			}
		}
	}
	return nil, nil, false, nil
}

func rootReleaseOnce(m *Manager, path string, entry *managedRoot) func() {
	var once sync.Once
	return func() {
		once.Do(func() { m.releaseRoot(path, entry) })
	}
}

func (m *Manager) releaseRoot(path string, entry *managedRoot) {
	if entry == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.closed || entry.refs <= 0 {
		return
	}
	entry.refs--
	if entry.refs == 0 && (entry.retired || m.rootClosing) {
		m.closeRootLocked(path, entry)
	}
	if m.rootCond != nil {
		m.rootCond.Broadcast()
	}
}

func (m *Manager) closeRootLocked(path string, entry *managedRoot) {
	if entry == nil || entry.closed {
		return
	}
	entry.closed = true
	if m.rootRefs[path] == entry {
		delete(m.rootRefs, path)
	}
	if entry.root != nil {
		_ = entry.root.Close()
	}
	if retired := m.retiredRefs[path]; len(retired) > 0 {
		kept := retired[:0]
		for _, candidate := range retired {
			if candidate != entry {
				kept = append(kept, candidate)
			}
		}
		if len(kept) == 0 {
			delete(m.retiredRefs, path)
		} else {
			m.retiredRefs[path] = kept
		}
	}
	if !m.roots[path] {
		delete(m.roots, path)
	}
}

func (m *Manager) adoptRoot(path string, opened *os.Root, active bool) (*os.Root, func(), error) {
	// 競合で既存descriptorが勝った場合とshutdown開始後は、開いたdescriptorを必ず閉じる。
	path = filepath.Clean(path)
	identity, err := descriptorIdentity(opened)
	if err != nil {
		_ = opened.Close()
		return nil, func() {}, fmt.Errorf("inspect opened worktree root: %w", err)
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	if m.rootClosing {
		m.mu.Unlock()
		_ = opened.Close()
		return nil, func() {}, errManagerClosed
	}
	if expected := m.rootIdentities[path]; expected != "" && expected != identity {
		m.mu.Unlock()
		_ = opened.Close()
		return nil, func() {}, fmt.Errorf("worktree root inode changed for %s (expected %s, got %s)", path, expected, identity)
	}
	if existing := m.rootRefs[path]; existing != nil && !existing.closed && !existing.retired && existing.root != nil {
		existing.refs++
		m.mu.Unlock()
		_ = opened.Close()
		return existing.root, rootReleaseOnce(m, path, existing), nil
	}
	current, known := m.roots[path]
	isActive := active && (!known || current)
	entry := &managedRoot{root: opened, identity: identity, refs: 1, retired: !isActive}
	m.rootIdentities[path] = identity
	if isActive {
		m.rootRefs[path] = entry
		m.roots[path] = true
	} else {
		m.retiredRefs[path] = append(m.retiredRefs[path], entry)
	}
	m.mu.Unlock()
	return opened, rootReleaseOnce(m, path, entry), nil
}

func (m *Manager) rootDescriptor(root string) (*os.Root, func(), error) {
	root = filepath.Clean(root)
	m.mu.Lock()
	ownedRoot, entry, found, err := m.acquireRootLocked(root, false)
	m.mu.Unlock()
	if err != nil {
		return nil, func() {}, err
	}
	if found {
		return ownedRoot, rootReleaseOnce(m, root, entry), nil
	}
	_, opened, err := ensureWorktreeRootDescriptor(root)
	if err != nil {
		return nil, func() {}, err
	}
	return m.adoptRoot(root, opened, true)
}

func (m *Manager) existingRootDescriptor(root string) (*os.Root, func(), error) {
	// READY検証やreconcileでは、欠けたrootを作り直して所有権を偽装してはならない。
	root = filepath.Clean(root)
	m.mu.Lock()
	ownedRoot, entry, found, err := m.acquireRootLocked(root, true)
	m.mu.Unlock()
	if err != nil {
		return nil, func() {}, err
	}
	if found {
		return ownedRoot, rootReleaseOnce(m, root, entry), nil
	}
	ownedRoot, _, err = domain.OpenOwnedRoot(root, root)
	if err != nil {
		return nil, func() {}, err
	}
	return m.adoptRoot(root, ownedRoot, false)
}

func descriptorIdentity(root *os.Root) (string, error) {
	if root == nil {
		return "", errors.New("worktree root descriptor is nil")
	}
	// identityはvolumeを含むため、Lstatのmetadataではなくdescriptor自体から引く。
	directory, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer func() { _ = directory.Close() }()
	info, err := directory.Stat()
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("worktree root descriptor is not a physical directory")
	}
	return domain.FileIdentity(directory)
}

func (m *Manager) retireRootLocked(path string) {
	entry := m.rootRefs[path]
	if entry == nil {
		delete(m.roots, path)
		return
	}
	if entry.closed {
		return
	}
	delete(m.rootRefs, path)
	entry.retired = true
	if entry.refs == 0 {
		m.closeRootLocked(path, entry)
		return
	}
	m.retiredRefs[path] = append(m.retiredRefs[path], entry)
}

func (m *Manager) rootHandleForPath(path string) *os.Root {
	// 周囲のholdRootForPathがdescriptorの寿命を保持する。ここで参照数は増減させない。
	root, ok := m.rootForPath(path)
	if !ok {
		return nil
	}
	return m.rootHandleForRoot(root)
}

func (m *Manager) rootHandleForRoot(root string) *os.Root {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if entry := m.rootRefs[root]; entry != nil && !entry.closed && entry.root != nil {
		return entry.root
	}
	entries := m.retiredRefs[root]
	for index := len(entries) - 1; index >= 0; index-- {
		if entry := entries[index]; entry != nil && !entry.closed && entry.root != nil {
			return entry.root
		}
	}
	return nil
}

func (m *Manager) holdRootForPath(path string) (func(), error) {
	// 同期操作の全期間でrootを保持し、reload後もworkerがretired descriptorを使い切れるようにする。
	root, ok := m.rootForPath(path)
	if !ok {
		configured, expandErr := config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if expandErr != nil {
			return func() {}, fmt.Errorf("%w: resolve configured wx root: %w", state.ErrOwnership, expandErr)
		}
		if !domain.IsWithin(configured, path) {
			return func() {}, nil
		}
		root = filepath.Clean(configured)
	}
	m.mu.RLock()
	active, known := m.roots[root]
	m.mu.RUnlock()
	var release func()
	var err error
	if ok {
		if !known || !active {
			_, release, err = m.existingRootDescriptor(root)
		} else {
			_, release, err = m.rootDescriptor(root)
		}
	} else {
		_, release, err = m.rootDescriptor(root)
	}
	if err != nil {
		if errors.Is(err, errManagerClosed) {
			return func() {}, errManagerClosed
		}
		return func() {}, fmt.Errorf("%w: hold wx root descriptor: %w", state.ErrOwnership, err)
	}
	return release, nil
}

func (m *Manager) holdVerifiedRootForPath(path string) (string, func(), error) {
	// 遅延cleanupはREMOVINGへ遷移する前に、historical rootのdescriptorとpath名の両方を検証する。
	root, ok := m.rootForPath(path)
	if !ok {
		return "", func() {}, fmt.Errorf("%w: path is outside known wx roots", state.ErrOwnership)
	}
	release, err := m.holdRootForPath(path)
	if err != nil {
		return "", func() {}, err
	}
	owner := m.rootHandleForRoot(root)
	if owner == nil {
		release()
		return "", func() {}, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		release()
		return "", func() {}, err
	}
	return root, release, nil
}

func (m *Manager) retainLease(sessionID, path string) error {
	// foreground leaseへroot参照を移し、retired rootもsessionのreleaseまで閉じない。
	_, ok := m.rootForPath(path)
	if !ok {
		configured, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if err != nil || !domain.IsWithin(configured, path) {
			if err == nil {
				err = errors.New("lease path is outside known wx roots")
			}
			return fmt.Errorf("%w: %w", state.ErrOwnership, err)
		}
	}
	release, err := m.holdRootForPath(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	if m.rootClosing {
		m.mu.Unlock()
		release()
		return errManagerClosed
	}
	previous := m.leases[sessionID]
	m.leases[sessionID] = release
	m.mu.Unlock()
	if previous != nil {
		previous()
	}
	return nil
}

func (m *Manager) releaseLease(sessionID string) {
	m.mu.Lock()
	release := m.leases[sessionID]
	delete(m.leases, sessionID)
	m.mu.Unlock()
	if release != nil {
		release()
	}
}

func relativeWithinRoot(root, path string) (relative string, ok bool) {
	// filepath.IsLocalで".."、絶対path、未cleanなroot相対pathをまとめて拒否する。
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || !filepath.IsLocal(relative) {
		return "", false
	}
	return relative, true
}

func verifyRootDescriptorPath(path string, owner *os.Root) error {
	// descriptorが固定したinodeとpath名がまだ同じnamespaceを指すことを確認してから状態を更新する。
	if owner == nil {
		return fmt.Errorf("%w: wx root descriptor is unavailable", state.ErrOwnership)
	}
	current, _, err := domain.OpenOwnedRoot(path, path)
	if err != nil {
		return fmt.Errorf("%w: wx root path changed: %w", state.ErrOwnership, err)
	}
	defer func() { _ = current.Close() }()
	heldInfo, err := owner.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect pinned wx root: %w", state.ErrOwnership, err)
	}
	currentInfo, err := current.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect wx root path: %w", state.ErrOwnership, err)
	}
	if !os.SameFile(heldInfo, currentInfo) {
		return fmt.Errorf("%w: wx root path names a different directory", state.ErrOwnership)
	}
	return nil
}
