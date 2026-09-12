package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestV2LocalRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "src", "product")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version: 2\nsystem:\n  storage:\n    worktree_root: $HOME/wx\n  pool:\n    preparation_concurrency: 2\nworkspace_defaults:\n  worktree: ask\n  warm_count: 2\nrepository_defaults:\n  default_branch: main\n  submodules: true\n  readiness:\n    mode: early\n  storage:\n    copy_mode: auto\nworkspaces:\n  $HOME/src/product:\n    worktree: hot\n    repository_defaults:\n      readiness:\n        timeout: 15m\n    repositories:\n      frontend:\n        readiness:\n          mode: full\n        dir_name: web\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	got, raw, err := LoadWithRaw()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.WorktreeMode(root) != "hot" {
		t.Fatalf("got version/mode=%d/%s", got.Version, got.WorktreeMode(root))
	}
	r := got.RepositoryFor(root, "frontend", filepath.Join(root, "frontend"))
	if r.Readiness.Mode != "full" || r.DirName != "web" || r.DefaultBranch != "main" {
		t.Fatalf("repository=%+v", r)
	}
	if got.RepositoryFor(root, "frontend", "").Readiness.Timeout.Duration == 0 {
		t.Fatalf("workspace readiness was not inherited")
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "workspace_defaults:") || strings.Contains(string(data), "worktree:\n  undefined") {
		t.Fatalf("saved=%s", data)
	}
}

func TestV2SourcesDoNotTreatBuiltinsAsGlobal(t *testing.T) {
	raw := Config{Version: 2, v2Explicit: true}
	effective := Merge(Defaults(), raw)
	fields := V2Fields(effective, raw, V2ScopeRepository, "", ".")
	for _, field := range fields {
		if field.Key == "default_branch" && field.Source != "default" {
			t.Fatalf("default_branch source=%q, want default", field.Source)
		}
		if field.Key == "submodules" && field.Source != "default" {
			t.Fatalf("submodules source=%q, want default", field.Source)
		}
	}

	trueValue := true
	raw.RepositoryDefaults.Submodules = &trueValue
	raw.present = map[string]bool{"repository_defaults": true, "repository_defaults.submodules": true}
	effective = Merge(Defaults(), raw)
	fields = V2Fields(effective, raw, V2ScopeRepository, "", ".")
	for _, field := range fields {
		if field.Key == "submodules" && field.Source != "global" {
			t.Fatalf("explicit submodules source=%q, want global", field.Source)
		}
	}
}

func TestV2RejectsLegacyWorkspaceSubmodules(t *testing.T) {
	value := true
	c := Config{
		Version: 2, v2Explicit: true,
		System:             SystemConfig{Storage: SystemStorage{WorktreeRoot: "/tmp/wx"}},
		WorkspaceDefaults:  WorkspaceDefaults{Worktree: "ask", ReuseStandby: &value, WarmCount: new(int)},
		RepositoryDefaults: RepositoryDefaults{DefaultBranch: "main", DirSource: RepoDirSourceRemote, COWMinSizeKiB: new(int), Submodules: &value, Readiness: RepositoryReadiness{Mode: "early", Timeout: durationPointer(1)}},
		Workspaces:         map[string]Workspace{"/tmp/project": {Submodules: &value}},
	}
	if err := ValidateV2Rules(&c); err == nil || !strings.Contains(err.Error(), "submodules") {
		t.Fatalf("ValidateV2Rules error=%v, want workspace submodules rejection", err)
	}
}

func TestV2ExplicitEmptyListsOverrideTheirParents(t *testing.T) {
	globalCopy := []string{"global-copy"}
	globalEarly := []string{"global-early"}
	raw := Config{
		Version: 2, v2Explicit: true,
		WorkspaceDefaults:  WorkspaceDefaults{Copy: []string{}},
		RepositoryDefaults: RepositoryDefaults{Readiness: RepositoryReadiness{EarlyPaths: []string{}}},
	}
	effective := Merge(Defaults(), raw)
	if effective.WorkspaceDefaults.Copy == nil || len(effective.WorkspaceDefaults.Copy) != 0 {
		t.Fatalf("workspace default copy=%v, want explicit empty list", effective.WorkspaceDefaults.Copy)
	}
	if effective.RepositoryDefaults.Readiness.EarlyPaths == nil || len(effective.RepositoryDefaults.Readiness.EarlyPaths) != 0 {
		t.Fatalf("repository default early_paths=%v, want explicit empty list", effective.RepositoryDefaults.Readiness.EarlyPaths)
	}
	// workspace/repository 解決時の clone でも、明示空 list を nil に変えず区別を保つ。
	raw.WorkspaceDefaults.Copy = globalCopy
	raw.RepositoryDefaults.Readiness.EarlyPaths = globalEarly
	raw.Workspaces = map[string]Workspace{"/workspace": {Copy: []string{}}}
	effective = Merge(Defaults(), raw)
	workspace := effective.WorkspaceFor("/workspace")
	if workspace.Copy == nil || len(workspace.Copy) != 0 {
		t.Fatalf("workspace copy=%v, want explicit empty list", workspace.Copy)
	}
	if got := effective.RepositoryFor("/workspace", ".", "").Readiness.EarlyPaths; got == nil || len(got) != len(globalEarly) {
		t.Fatalf("repository early_paths=%v, want inherited values", got)
	}
}

func TestV2SystemSessionPathsMergePerTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version: 2\nsystem:\n  sessions:\n    paths:\n      claude:\n        sessions: [~/custom-claude]\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sessions.SessionPaths("claude"); len(got) != 1 || got[0] != "~/custom-claude" {
		t.Fatalf("claude session paths=%v", got)
	}
	if got := cfg.Sessions.SessionPaths("codex"); len(got) != 1 || got[0] != "~/.codex/sessions" {
		t.Fatalf("codex session paths=%v, want built-in fallback", got)
	}
}
