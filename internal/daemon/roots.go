package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func (m *Manager) registerRootGeneration(ctx context.Context, root, identity string) {
	if err := m.ensureRootGeneration(ctx, root, identity); err != nil {
		m.log.Error("register worktree root generation failed", "path", root, "error", err)
	}
}

// ensureRootGeneration はroot generationを登録し、失敗理由をrootErrorへ残して返す。
// root IDなしのslotは再発見できないため、失敗時は後続のallocationをfail closedにする。
// ログは呼出元が出す。周期的な再試行が同じ失敗を繰り返し記録しないためである。
func (m *Manager) ensureRootGeneration(ctx context.Context, root, identity string) error {
	if identity == "" {
		err := fmt.Errorf("worktree root %s has no readable inode identity", root)
		m.setRootError(err.Error())
		return err
	}
	id, err := m.store.EnsureActiveRoot(ctx, root, identity)
	if err != nil {
		m.setRootError(err.Error())
		return err
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	m.rootIDs[root] = id
	m.rootError = ""
	m.rootRetryLogged = ""
	m.mu.Unlock()
	return nil
}

// retryRootGeneration は失敗したままのroot generation登録を周期処理から再試行する。
// rootを作り直した・volumeをmountし直したといった外的な回復を、daemon再起動なしで拾うためである。
func (m *Manager) retryRootGeneration(ctx context.Context) {
	m.mu.RLock()
	failing := m.rootError != ""
	m.mu.RUnlock()
	if !failing {
		return
	}
	configured := m.Config().Storage.WorktreeRoot
	root, err := config.ExpandHome(configured)
	if err != nil {
		m.recordRootRetryFailure(configured, err)
		return
	}
	root = filepath.Clean(root)
	identity, err := m.retryRootIdentity(root)
	if err != nil {
		m.recordRootRetryFailure(root, err)
		return
	}
	if err := m.ensureRootGeneration(ctx, root, identity); err != nil {
		m.recordRootRetryFailure(root, err)
		return
	}
	m.log.Info("worktree root generation registration recovered", "path", root)
}

// retryRootIdentity は再試行のためにactive rootのdescriptorを取り直し、そのidentityを返す。
// 起動時にidentityを読めなかったrootはpinが空のままなので、読めた時点でpinを埋める。
func (m *Manager) retryRootIdentity(root string) (string, error) {
	handle, release, err := m.rootDescriptor(root)
	if err != nil {
		return "", err
	}
	defer release()
	identity, err := descriptorIdentity(handle)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	if m.rootIdentities[root] == "" {
		m.rootIdentities[root] = identity
	}
	if entry := m.rootRefs[root]; entry != nil && entry.identity == "" {
		entry.identity = identity
	}
	m.mu.Unlock()
	return identity, nil
}

// recordRootRetryFailure は再試行の失敗をrootErrorへ反映し、理由が変わらない連続失敗はログを1回に抑える。
func (m *Manager) recordRootRetryFailure(root string, err error) {
	message := err.Error()
	m.mu.Lock()
	m.rootError = message
	repeated := m.rootRetryLogged == message
	m.rootRetryLogged = message
	m.mu.Unlock()
	if repeated {
		return
	}
	m.log.Error("retry worktree root generation registration failed", "path", root, "error", err)
}

func (m *Manager) setRootError(message string) {
	m.mu.Lock()
	m.rootError = message
	m.mu.Unlock()
}

func (m *Manager) loadRootGenerations(ctx context.Context) {
	// 旧rootのslotも寿命まで扱えるよう、SQLiteが参照するgenerationをretired rootとしてpinする。
	roots, err := m.store.Roots(ctx)
	if err != nil {
		m.log.Error("load worktree root generations failed", "error", err)
		return
	}
	for _, root := range roots {
		if root.Identity == "" {
			m.log.Error("worktree root generation has no recorded identity", "path", root.Path)
			continue
		}
		m.mu.Lock()
		m.ensureRootStateLocked()
		pinned := m.rootIdentities[root.Path]
		if pinned == "" {
			m.rootIdentities[root.Path] = root.Identity
		}
		m.mu.Unlock()
		if pinned != "" {
			if pinned != root.Identity {
				m.log.Error("worktree root generation is not the pinned directory", "path", root.Path, "recorded", root.Identity, "pinned", pinned)
				continue
			}
			m.mu.Lock()
			m.rootIDs[root.Path] = root.ID
			m.mu.Unlock()
			continue
		}
		_, release, openErr := m.existingRootDescriptor(root.Path)
		if openErr != nil {
			m.log.Warn("retired worktree root generation is unavailable", "path", root.Path, "error", openErr)
			continue
		}
		release()
		m.mu.Lock()
		m.rootIDs[root.Path] = root.ID
		m.mu.Unlock()
	}
}

func (m *Manager) activeRoot() (string, string, error) {
	root, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	if err != nil {
		return "", "", err
	}
	root = filepath.Clean(root)
	m.mu.RLock()
	id := m.rootIDs[root]
	rootError := m.rootError
	m.mu.RUnlock()
	if id == "" {
		if rootError != "" {
			return "", "", fmt.Errorf("%w: worktree root %s has no registered generation: %s", state.ErrOwnership, root, rootError)
		}
		return "", "", fmt.Errorf("%w: worktree root %s has no registered generation", state.ErrOwnership, root)
	}
	return root, id, nil
}

func (m *Manager) rootIDForPath(path string) (string, string, error) {
	// 遅延jobとrecoveryは設定変更後の旧rootにも属し得るため、active rootだけに絞らない。
	root, ok := m.rootForPath(path)
	if !ok {
		return "", "", fmt.Errorf("%w: path is outside known wx roots", state.ErrOwnership)
	}
	m.mu.RLock()
	id := m.rootIDs[root]
	m.mu.RUnlock()
	if id == "" {
		return "", "", fmt.Errorf("%w: worktree root %s has no registered generation", state.ErrOwnership, root)
	}
	return root, id, nil
}

func (m *Manager) rootForPath(path string) (string, bool) {
	// active/retired rootが重なる場合は最長一致を選び、map順序で別のdescriptorを借りない。
	m.mu.RLock()
	defer m.mu.RUnlock()
	path = filepath.Clean(path)
	best := ""
	for root := range m.roots {
		if domain.IsWithin(root, path) {
			if best == "" || len(root) > len(best) {
				best = root
			}
		}
	}
	for root := range m.rootIdentities {
		if domain.IsWithin(root, path) && (best == "" || len(root) > len(best)) {
			best = root
		}
	}
	return best, best != ""
}

// knownRoots は in-memory に登録済みの root へ DB の root 世代を重ねた集合を返す。値は active かどうかを表す。
func (m *Manager) knownRoots(ctx context.Context) map[string]bool {
	m.mu.RLock()
	roots := make(map[string]bool, len(m.roots))
	for root, active := range m.roots {
		roots[root] = active
	}
	m.mu.RUnlock()
	if rows, err := m.store.Roots(ctx); err == nil {
		for _, row := range rows {
			path := filepath.Clean(row.Path)
			if _, known := roots[path]; !known {
				roots[path] = row.Active
			}
		}
	}
	return roots
}

func rootPathsOverlap(first, second string) bool {
	first, second = filepath.Clean(first), filepath.Clean(second)
	return first == second || domain.IsWithin(first, second) || domain.IsWithin(second, first)
}

func ensureWorktreeRootDescriptor(value string) (string, *os.Root, error) {
	root, err := config.ExpandHome(value)
	if err != nil {
		return "", nil, err
	}
	root = filepath.Clean(root)
	ownedRoot, err := domain.EnsurePhysicalDirectoryRoot(root, 0o700)
	if err != nil {
		return "", nil, fmt.Errorf("open physical worktree root: %w", err)
	}
	if err := ownedRoot.Chmod(".", 0o700); err != nil {
		_ = ownedRoot.Close()
		return "", nil, err
	}
	return root, ownedRoot, nil
}
