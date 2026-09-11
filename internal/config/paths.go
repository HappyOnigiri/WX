package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// homePath はユーザーの home directory に parts を結合し、home を取得できない場合は失敗閉鎖する。
func homePath(parts ...string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{h}, parts...)...), nil
}

func Path() (string, error) {
	return homePath(".config", "wx", "config.yaml")
}

func StatePath() (string, error) {
	return homePath("Library", "Application Support", "wx", "state.db")
}

func SocketPath() (string, error) {
	return homePath("Library", "Application Support", "wx", "run", "wxd.sock")
}

func LogPath() (string, error) {
	return homePath("Library", "Logs", "wx", "wxd.log")
}

func NormalizePaths(c *Config) error {
	root, err := canonicalPath(c.Storage.WorktreeRoot)
	if err != nil {
		return fmt.Errorf("storage.worktree_root: %w", err)
	}
	c.Storage.WorktreeRoot = root
	workspaces := make(map[string]Workspace, len(c.Workspaces))
	for path, override := range c.Workspaces {
		canonical, err := canonicalPath(path)
		if err != nil {
			return fmt.Errorf("workspace override %q: %w", path, err)
		}
		if _, exists := workspaces[canonical]; exists {
			return fmt.Errorf("workspace overrides collide at canonical path %s", canonical)
		}
		workspaces[canonical] = override
	}
	c.Workspaces = workspaces
	repositories := make(map[string]Repository, len(c.Repositories))
	for path, override := range c.Repositories {
		canonical, err := canonicalPath(path)
		if err != nil {
			return fmt.Errorf("repository override %q: %w", path, err)
		}
		if _, exists := repositories[canonical]; exists {
			return fmt.Errorf("repository overrides collide at canonical path %s", canonical)
		}
		repositories[canonical] = override
	}
	c.Repositories = repositories
	return nil
}

func canonicalPath(path string) (string, error) {
	expanded, err := ExpandHome(path)
	if err != nil {
		return "", err
	}
	current := expanded
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func ExpandHome(path string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path = expandTilde(path, h)
	if strings.Contains(path, "~") {
		// `~user`（他ユーザーのホーム）はwxが解決手段を持たないため展開せず拒否する。
		return "", errors.New("~ is only supported as a leading ~ or ~/ prefix; use $HOME for other cases")
	}
	if strings.Contains(path, "$") && path != "$HOME" && !strings.HasPrefix(path, "$HOME"+string(filepath.Separator)) {
		return "", errors.New("only $HOME expansion is supported")
	}
	path = strings.ReplaceAll(path, "$HOME", h)
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	return filepath.Clean(path), nil
}

// expandTilde は先頭の `~`（単体または `~/` prefix）だけを home に展開する。
// `~user` 形式は home を特定できないため素通りさせ、呼び出し側の検証に委ねる。
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[len("~/"):])
	}
	return path
}

// SetWorkspaceWorktree は既存の個別設定を保ち、同じ実体を指すキーへ worktree 方針を保存する。
// enum 検査を CLI と共有するため、汎用の SetScopeField ではなくここを入口にする。
func SetWorkspaceWorktree(c *Config, root, mode string) error {
	if !validWorktreeMode(mode, false) {
		return fmt.Errorf("invalid worktree mode %q", mode)
	}
	return SetScopeField(c, ScopeWorkspace, root, "worktree", mode)
}
