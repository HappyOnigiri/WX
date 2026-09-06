package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

type WorkspaceScope struct {
	WorkspaceID string               `json:"workspace_id"`
	Root        string               `json:"root"`
	Kind        string               `json:"kind"`
	Registered  bool                 `json:"registered"`
	SlotPaths   []string             `json:"slot_paths"`
	Sessions    []state.SessionScope `json:"sessions"`
}

func (m *Manager) WorkspaceScope(ctx context.Context, cwd string) (WorkspaceScope, error) {
	scope, registered, err := m.registeredScopeWorkspace(ctx, cwd)
	if err != nil {
		return WorkspaceScope{}, err
	}
	if !registered {
		scope, registered, err = m.discoveredScopeWorkspace(ctx, cwd)
		if err != nil {
			return WorkspaceScope{}, err
		}
	}
	result := WorkspaceScope{WorkspaceID: scope.ID, Root: scope.Root, Kind: scope.Kind, Registered: registered, SlotPaths: []string{}, Sessions: []state.SessionScope{}}
	if !registered {
		return result, nil
	}
	result.SlotPaths, err = m.store.WorkspaceSlotPaths(ctx, scope.ID)
	if err != nil {
		return result, err
	}
	result.Sessions, err = m.store.WorkspaceSessionScopes(ctx, scope.ID)
	return result, err
}

// registeredScopeWorkspace は登録済み workspace の会話 scope を、workspace 配下の探索なしで解決する。
// 確実に一致する identity だけを返し、曖昧な cwd や未登録の場所は found=false として通常の探索へ委ねる。
// membership・generation・root 世代は更新せず、読み取りだけで完結する。
func (m *Manager) registeredScopeWorkspace(ctx context.Context, cwd string) (state.ScopeWorkspace, bool, error) {
	canonical, canonicalErr := domain.Canonicalize(cwd)
	if canonicalErr != nil {
		// 実体を失った cwd は字句正規化し、既知 slot の記録との照合にだけ使う。
		// 未登録の存在しない path を新しい workspace とみなさないため、他の経路へは進めない。
		absolute, err := filepath.Abs(cwd)
		if err != nil {
			return state.ScopeWorkspace{}, false, fmt.Errorf("resolve scope path %q: %w", cwd, err)
		}
		return m.store.ScopeWorkspaceForSlotPath(ctx, filepath.Clean(absolute))
	}
	path := string(canonical)
	slotScope, slotFound, err := m.store.ScopeWorkspaceForSlotPath(ctx, path)
	if err != nil {
		return state.ScopeWorkspace{}, false, err
	}
	common, isRepository, err := m.scopeCommonDir(ctx, path)
	if err != nil {
		return state.ScopeWorkspace{}, false, err
	}
	if slotFound {
		// 実体が別の repository へ置き換わっているときは記録へ強制結合せず、現在の Git identity で解決し直す。
		member := false
		if isRepository {
			if member, err = m.store.WorkspaceHasCommonDir(ctx, slotScope.ID, common); err != nil {
				return state.ScopeWorkspace{}, false, err
			}
		}
		if !isRepository || member {
			return slotScope, true, nil
		}
	}
	if !isRepository {
		// cwd 自体が repository ではないと確かめた上で、登録済み multi-repository root の完全一致だけを高速経路にする。
		return m.store.ScopeMultiWorkspaceForRoot(ctx, path)
	}
	scope, found, err := m.store.ScopeRepositoryWorkspace(ctx, common)
	if err != nil || !found {
		return state.ScopeWorkspace{}, false, err
	}
	// main worktree は移動し得るため、現在の root は DB ではなく Git の worktree registry から求める。
	root, err := m.scopeMainWorktree(ctx, path)
	if err != nil {
		return state.ScopeWorkspace{}, false, err
	}
	scope.Root = root
	return scope, true, nil
}

// discoveredScopeWorkspace は高速経路で確定できなかった cwd を従来の探索で解決し、登録の有無を返す。
func (m *Manager) discoveredScopeWorkspace(ctx context.Context, cwd string) (state.ScopeWorkspace, bool, error) {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, cwd)
	if err != nil {
		return state.ScopeWorkspace{}, false, err
	}
	w, err = m.store.CanonicalWorkspace(ctx, w)
	if err != nil {
		return state.ScopeWorkspace{}, false, err
	}
	scope := state.ScopeWorkspace{ID: string(w.ID), Root: string(w.Root), Kind: w.Kind}
	if _, err := m.store.Workspace(ctx, scope.ID); errors.Is(err, sql.ErrNoRows) {
		return scope, false, nil
	} else if err != nil {
		return state.ScopeWorkspace{}, false, err
	}
	return scope, true, nil
}

// scopeCommonDir は cwd の Git common directory を返し、repository でなければ isRepository=false を返す。
// それ以外の Git 失敗や symlink 不正はエラーとして返し、キャッシュミスへ読み替えない。
func (m *Manager) scopeCommonDir(ctx context.Context, cwd string) (string, bool, error) {
	res, err := m.git.Run(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if gitx.IsNotRepository(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve Git common directory for %s: %w", cwd, err)
	}
	common, err := domain.Canonicalize(strings.TrimSpace(res.Stdout))
	if err != nil {
		return "", false, err
	}
	return string(common), true, nil
}

// scopeMainWorktree は cwd が属する repository の現在の main worktree path を返す。
func (m *Manager) scopeMainWorktree(ctx context.Context, cwd string) (string, error) {
	res, err := m.git.Run(ctx, cwd, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	main := discovery.FirstWorktreePath(res.Stdout)
	if main == "" {
		return "", fmt.Errorf("git did not report a main worktree for %s", cwd)
	}
	canonical, err := domain.Canonicalize(main)
	return string(canonical), err
}
