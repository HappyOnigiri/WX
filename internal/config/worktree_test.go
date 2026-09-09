package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreePolicyDefaultsOverridesAndValidation(t *testing.T) {
	cfg := Defaults()
	if cfg.WorktreeMode("/unknown") != "ask" {
		t.Fatal("unknown must require confirmation")
	}
	for _, mode := range []string{"ask", "hot", "cold", "off"} {
		cfg.Worktree.Undefined = mode
		if err := Validate(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.WorktreeMode("/unknown") != mode {
			t.Fatal(mode)
		}
		cfg.Workspaces["/selected"] = Workspace{Worktree: "off"}
		if cfg.WorktreeMode("/selected") != "off" {
			t.Fatal("global overrode selection")
		}
	}
	cfg.Worktree.Undefined = "bad"
	if Validate(&cfg) == nil {
		t.Fatal("bad default accepted")
	}
	cfg.Worktree.Undefined = "ask"
	cfg.Workspaces["/selected"] = Workspace{Worktree: "ask"}
	if Validate(&cfg) == nil {
		t.Fatal("ask persisted for workspace")
	}
}

func TestWorktreeSelectionPreservesSparseConfigAndCanonicalKey(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	var raw Config
	if err := SetField(&raw, "includes.default_agent_rules", "false"); err != nil {
		t.Fatal(err)
	}
	raw.Workspaces = map[string]Workspace{"$HOME/alias": {Copy: []string{".env"}, Link: []string{"cache"}}}
	for _, mode := range []string{"hot", "cold", "off"} {
		if err := SetWorkspaceWorktree(&raw, repo, mode); err != nil {
			t.Fatal(err)
		}
		if err := Save(raw); err != nil {
			t.Fatal(err)
		}
		raw, err = LoadRaw()
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WorktreeMode(repo) != mode || cfg.Includes.DefaultAgentRules || len(cfg.Workspaces) != 1 || cfg.Workspaces[repo].Copy[0] != ".env" || cfg.Workspaces[repo].Link[0] != "cache" {
			t.Fatalf("cfg=%+v", cfg)
		}
	}
	path, _ := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "retention:") || !strings.Contains(string(data), "$HOME/alias:") {
		t.Fatal(string(data))
	}
	if err := SetWorkspaceWorktree(&raw, repo, "ask"); err == nil {
		t.Fatal("bad selection accepted")
	}
	if err := SetWorkspaceWorktree(&raw, "relative", "hot"); err == nil {
		t.Fatal("relative path accepted")
	}
	var empty Config
	if err := SetWorkspaceWorktree(&empty, repo, "cold"); err != nil {
		t.Fatal(err)
	}
	if empty.WorktreeMode(repo) != "cold" {
		t.Fatal("empty config selection lost")
	}
}

func TestWorkspaceWarmCountOverridePreservesOtherSettingsAndExplicitZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	raw := Config{Workspaces: map[string]Workspace{
		"$HOME/alias": {Worktree: "hot", Copy: []string{".env"}, Link: []string{"cache"}},
	}}
	if err := SetWorkspaceWarmCount(&raw, repo, 0); err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	workspace, ok := loaded.Workspaces[repo]
	if !ok || workspace.WarmCount == nil || *workspace.WarmCount != 0 || workspace.Worktree != "hot" || len(workspace.Copy) != 1 || len(workspace.Link) != 1 {
		t.Fatalf("workspace override=%+v, want preserved settings and explicit zero", workspace)
	}
	if got, overridden := loaded.WarmCountForWorkspace(repo); got != 0 || !overridden {
		t.Fatalf("warm count=%d overridden=%v, want explicit zero", got, overridden)
	}

	raw, err = LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if err := ResetWorkspaceWarmCount(&raw, repo); err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	workspace = loaded.Workspaces[repo]
	if workspace.WarmCount != nil || workspace.Worktree != "hot" || len(workspace.Copy) != 1 || len(workspace.Link) != 1 {
		t.Fatalf("reset workspace override=%+v, want other settings retained", workspace)
	}
	if got, overridden := loaded.WarmCountForWorkspace(repo); got != 1 || overridden {
		t.Fatalf("reset warm count=%d overridden=%v, want global default", got, overridden)
	}
}

func TestWorkspaceWarmCountValidationRejectsNegative(t *testing.T) {
	negative := -1
	cfg := Defaults()
	cfg.Workspaces["/repo"] = Workspace{WarmCount: &negative}
	if err := Validate(&cfg); err == nil {
		t.Fatal("negative workspace warm count was accepted")
	}
	if err := SetWorkspaceWarmCount(&cfg, "/repo", -1); err == nil {
		t.Fatal("negative workspace warm count update was accepted")
	}
}

func TestWorkspaceReuseStandbyOverridePreservesExplicitFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := Config{}
	if err := SetWorkspaceReuseStandby(&raw, repo, false); err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if enabled, overridden := loaded.ReuseStandbyForWorkspace(repo); enabled || !overridden {
		t.Fatalf("reuse standby=%t overridden=%t, want explicit false", enabled, overridden)
	}
	raw, err = LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if err := ResetWorkspaceReuseStandby(&raw, repo); err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if enabled, overridden := loaded.ReuseStandbyForWorkspace(repo); !enabled || overridden {
		t.Fatalf("reuse standby=%t overridden=%t, want global default", enabled, overridden)
	}
}
