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

// SetWorkspaceWorktree は既存の copy/link と疎な設定を保ち、同じ実体を指すキーへ選択を保存する。
func SetWorkspaceWorktree(c *Config, root, mode string) error {
	if !validWorktreeMode(mode, false) {
		return fmt.Errorf("invalid worktree mode %q", mode)
	}
	canonical, err := canonicalPath(root)
	if err != nil {
		return err
	}
	key := canonical
	for path := range c.Workspaces {
		resolved, err := canonicalPath(path)
		if err != nil {
			return err
		}
		if resolved == canonical {
			key = path
		}
	}
	if c.Workspaces == nil {
		c.Workspaces = map[string]Workspace{}
	}
	workspace := c.Workspaces[key]
	workspace.Worktree = mode
	c.Workspaces[key] = workspace
	if c.present == nil {
		c.present = map[string]bool{}
	}
	c.present["workspaces"] = true
	return nil
}

// SetWorkspaceWarmCount は既存の workspace 個別設定を保ったまま待機枠数を保存する。
// 同じ実体を指す既存キーがあればその表記を維持し、明示的な 0 も未指定と区別して保存する。
func SetWorkspaceWarmCount(c *Config, root string, count int) error {
	if count < 0 {
		return errors.New("warm_count must not be negative")
	}
	key, err := workspaceOverrideKey(c, root)
	if err != nil {
		return err
	}
	if c.Workspaces == nil {
		c.Workspaces = map[string]Workspace{}
	}
	workspace := c.Workspaces[key]
	workspace.WarmCount = new(count)
	c.Workspaces[key] = workspace
	markWorkspacePresent(c)
	return nil
}

// ResetWorkspaceWarmCount は workspace の個別待機枠数だけを解除する。
// 他の workspace 設定が無ければ map の項目自体も削除し、疎な YAML を保つ。
func ResetWorkspaceWarmCount(c *Config, root string) error {
	key, err := workspaceOverrideKey(c, root)
	if err != nil {
		return err
	}
	workspace, ok := c.Workspaces[key]
	if !ok || workspace.WarmCount == nil {
		if len(c.Workspaces) == 0 && c.present != nil {
			c.present["workspaces"] = false
		}
		return nil
	}
	workspace.WarmCount = nil
	if workspace.Worktree == "" && len(workspace.Copy) == 0 && len(workspace.Link) == 0 && workspace.ReuseStandby == nil {
		delete(c.Workspaces, key)
	} else {
		c.Workspaces[key] = workspace
	}
	if len(c.Workspaces) == 0 {
		if c.present == nil {
			c.present = map[string]bool{}
		}
		c.present["workspaces"] = false
	} else {
		markWorkspacePresent(c)
	}
	return nil
}

func SetWorkspaceReuseStandby(c *Config, root string, enabled bool) error {
	key, err := workspaceOverrideKey(c, root)
	if err != nil {
		return err
	}
	if c.Workspaces == nil {
		c.Workspaces = map[string]Workspace{}
	}
	workspace := c.Workspaces[key]
	workspace.ReuseStandby = new(enabled)
	c.Workspaces[key] = workspace
	markWorkspacePresent(c)
	return nil
}

func ResetWorkspaceReuseStandby(c *Config, root string) error {
	key, err := workspaceOverrideKey(c, root)
	if err != nil {
		return err
	}
	workspace, ok := c.Workspaces[key]
	if !ok || workspace.ReuseStandby == nil {
		return nil
	}
	workspace.ReuseStandby = nil
	if workspace.Worktree == "" && len(workspace.Copy) == 0 && len(workspace.Link) == 0 && workspace.WarmCount == nil {
		delete(c.Workspaces, key)
	} else {
		c.Workspaces[key] = workspace
	}
	if len(c.Workspaces) == 0 {
		if c.present == nil {
			c.present = map[string]bool{}
		}
		c.present["workspaces"] = false
	} else {
		markWorkspacePresent(c)
	}
	return nil
}

// workspaceOverrideKey は指定 root に対応する既存の map key を探し、無ければ canonical path を返す。
func workspaceOverrideKey(c *Config, root string) (string, error) {
	canonical, err := canonicalPath(root)
	if err != nil {
		return "", err
	}
	for path := range c.Workspaces {
		resolved, err := canonicalPath(path)
		if err != nil {
			return "", err
		}
		if resolved == canonical {
			return path, nil
		}
	}
	return canonical, nil
}

func markWorkspacePresent(c *Config) {
	if c.present == nil {
		c.present = map[string]bool{}
	}
	c.present["workspaces"] = true
}
