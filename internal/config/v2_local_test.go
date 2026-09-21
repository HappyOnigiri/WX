package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultsV2LeavesDefaultBranchForDiscovery(t *testing.T) {
	if got := DefaultsV2().RepositoryDefaults.DefaultBranch; got != "" {
		t.Fatalf("default branch=%q, want unset so discovery can resolve it", got)
	}
}

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

func TestV2NestedExplicitEmptyListsSurviveSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version: 2\nworkspaces:\n  $HOME/project:\n    copy: []\n    repositories:\n      nested:\n        prepare:\n          inputs: []\n        readiness:\n          early_paths: []\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"copy: []", "inputs: []", "early_paths: []"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("saved configuration does not contain %q:\n%s", want, saved)
		}
	}
	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	workspace := reloaded.Workspaces["$HOME/project"]
	if workspace.Copy == nil || workspace.Repositories["nested"].Prepare.Inputs == nil || workspace.Repositories["nested"].Readiness.EarlyPaths == nil {
		t.Fatalf("nested explicit empty lists lost: %+v", workspace)
	}
}

func TestV2ExplicitZeroValuesSurviveSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version: 2\nsystem:\n  pool:\n    preparation_concurrency: 0\n  resume:\n    auto_fresh: false\n  retention:\n    event_log: 0s\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"preparation_concurrency: 0", "auto_fresh: false", "event_log: 0s"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("saved configuration does not contain %q:\n%s", want, saved)
		}
	}
}

// prepare.inputs は global・workspace・membership の各 list を追加合成せず、
// 値が明示された階層で置き換える。明示 empty も親の値を消す指定として保つ。
func TestV2PrepareInputsInheritByReplacement(t *testing.T) {
	raw := Config{
		Version: 2, v2Explicit: true,
		RepositoryDefaults: RepositoryDefaults{Prepare: Prepare{Inputs: []string{"global"}}},
		Workspaces: map[string]Workspace{
			"/workspace": {
				RepositoryDefaults: RepositoryDefaults{Prepare: Prepare{Inputs: []string{"workspace"}}},
				Repositories: map[string]Repository{
					"backend": {Prepare: Prepare{Inputs: []string{"member"}}},
					"empty":   {Prepare: Prepare{Inputs: []string{}}},
				},
			},
		},
	}
	effective := Merge(Defaults(), raw)
	if err := Validate(&effective); err != nil {
		t.Fatal(err)
	}
	if got := effective.RepositoryFor("/workspace", "backend", "").Prepare.Inputs; !reflect.DeepEqual(got, []string{"member"}) {
		t.Fatalf("membership inputs=%v, want member replacement", got)
	}
	if got := effective.RepositoryFor("/workspace", "frontend", "").Prepare.Inputs; !reflect.DeepEqual(got, []string{"workspace"}) {
		t.Fatalf("workspace inputs=%v, want workspace replacement", got)
	}
	if got := effective.RepositoryFor("/workspace", "empty", "").Prepare.Inputs; got == nil || len(got) != 0 {
		t.Fatalf("empty membership inputs=%v, want explicit empty replacement", got)
	}
	if got := effective.RepositoryFor("/other", "frontend", "").Prepare.Inputs; !reflect.DeepEqual(got, []string{"global"}) {
		t.Fatalf("global inputs=%v, want global fallback", got)
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

func TestV2LanguageLivesInSystemSection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 2\nsystem:\n  language: ja\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	effective, raw, err := LoadWithRaw()
	if err != nil {
		t.Fatal(err)
	}
	if effective.DisplayLanguage() != LanguageJapanese || effective.LanguageForRPC() != LanguageJapanese {
		t.Fatalf("language=%q rpc=%q", effective.DisplayLanguage(), effective.LanguageForRPC())
	}
	if got := LoadLanguage(); got != LanguageJapanese {
		t.Fatalf("LoadLanguage=%q, want ja", got)
	}
	if !LanguageConfigured(raw) {
		t.Fatal("system.language was not detected as configured")
	}
	// v2 の保存は system 節だけを書き出すため、top-level へ漏らさない。
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "language: ja") || strings.Contains(string(data), "\nlanguage:") {
		t.Fatalf("saved=%s", data)
	}
}

func TestV2RejectsTopLevelLanguage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version: 2\nlanguage: ja\nsystem:\n  pool:\n    preparation_concurrency: 2\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "top-level language") {
		t.Fatalf("Load error=%v, want top-level language rejection", err)
	}
}

// prepare.timeout の明示 `0s` は「上位の timeout を継承せず readiness timeout へ
// fallback する」指定なので、workspace と membership の解決・source・保存で保つ。
func TestV2ExplicitZeroPrepareTimeoutOverridesParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version: 2\nrepository_defaults:\n  prepare:\n    timeout: 5s\nworkspaces:\n  $HOME/project:\n    repository_defaults:\n      prepare:\n        timeout: 0s\n    repositories:\n      backend:\n        prepare:\n          timeout: 30s\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	effective, raw, err := LoadWithRaw()
	if err != nil {
		t.Fatal(err)
	}
	resolution := effective.ResolveRepository(root, ".", "")
	timeout := resolution.Config.Prepare.Timeout
	if timeout == nil || timeout.Duration != 0 {
		t.Fatalf("workspace prepare timeout=%v, want the explicit zero instead of the global 5s", timeout)
	}
	if source := resolution.Sources["prepare.timeout"]; source != "workspace" {
		t.Fatalf("prepare.timeout source=%q, want workspace", source)
	}
	// 明示 zero は下位 scope の明示値を隠さない。
	if member := effective.RepositoryFor(root, "backend", "").Prepare.Timeout; member == nil || member.Duration != 30*time.Second {
		t.Fatalf("membership prepare timeout=%v, want the membership value", member)
	}
	// workspace 指定の無い root では global 値を継承する。
	if other := effective.RepositoryFor(filepath.Join(home, "other"), ".", "").Prepare.Timeout; other == nil || other.Duration != 5*time.Second {
		t.Fatalf("unrelated workspace prepare timeout=%v, want the global 5s", other)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "timeout: 0s") {
		t.Fatalf("saved configuration dropped the explicit zero:\n%s", data)
	}
}
