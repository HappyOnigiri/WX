package config

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestV2EditTracksLeafPresenceForSourceDisplay(t *testing.T) {
	raw := Config{}
	if err := SetV2Field(&raw, V2ScopeRepositoryDefaults, "", "", "default_branch", "trunk"); err != nil {
		t.Fatal(err)
	}
	if !raw.present["repository_defaults.default_branch"] {
		t.Fatalf("presence=%v", raw.present)
	}
	effective := Merge(Defaults(), raw)
	fields := V2Fields(effective, raw, V2ScopeRepositoryDefaults, "", "")
	for _, field := range fields {
		if field.Key == "default_branch" && field.Source != "global" {
			t.Fatalf("default_branch source=%q, want global", field.Source)
		}
	}
	if err := ResetV2Field(&raw, V2ScopeRepositoryDefaults, "", "", "default_branch"); err != nil {
		t.Fatal(err)
	}
	if raw.present["repository_defaults.default_branch"] {
		t.Fatalf("reset presence=%v", raw.present)
	}
}

func TestV2EditValueMutationBoundariesFindsMatchingField(t *testing.T) {
	raw := Config{Version: 2, System: SystemConfig{Language: LanguageJapanese}, v2Explicit: true}
	got, err := editValue(raw, EditRequest{V2: true, Scope: V2ScopeSystem, Key: "language"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, LanguageJapanese) {
		t.Fatalf("editValue=%q, want Japanese value", got)
	}
}

func TestV2FetchDefaultBranchAcceptsOnAndOff(t *testing.T) {
	var raw Config
	if err := SetV2Field(&raw, V2ScopeWorkspaceDefaults, "", "", "fetch_default_branch", "on"); err != nil {
		t.Fatal(err)
	}
	if raw.WorkspaceDefaults.FetchDefaultBranch == nil || !*raw.WorkspaceDefaults.FetchDefaultBranch {
		t.Fatalf("fetch_default_branch=%v, want true", raw.WorkspaceDefaults.FetchDefaultBranch)
	}
	if err := SetV2Field(&raw, V2ScopeWorkspaceDefaults, "", "", "fetch_default_branch", "off"); err != nil {
		t.Fatal(err)
	}
	if raw.WorkspaceDefaults.FetchDefaultBranch == nil || *raw.WorkspaceDefaults.FetchDefaultBranch {
		t.Fatalf("fetch_default_branch=%v, want false", raw.WorkspaceDefaults.FetchDefaultBranch)
	}
}

// v2 の未知 key は struct の全 field を調べ終えた時点で拒否し、末尾の
// field を越えて reflect が panic しない。
func TestV2EditRejectsUnknownKeyAtStructBoundary(t *testing.T) {
	var raw Config
	err := SetV2Field(&raw, V2ScopeSystem, "", "", "not_a_config_key", "value")
	if err == nil || !strings.Contains(err.Error(), "unknown system config key") {
		t.Fatalf("unknown key error=%v", err)
	}
}

func TestV2ResetLastFieldKeepsAnExplicitSchemaSection(t *testing.T) {
	var raw Config
	if err := SetV2Field(&raw, V2ScopeSystem, "", "", "pool.preparation_concurrency", "3"); err != nil {
		t.Fatal(err)
	}
	if err := ResetV2Field(&raw, V2ScopeSystem, "", "", "pool.preparation_concurrency"); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "version: 2") || !strings.Contains(string(data), "system: {}") {
		t.Fatalf("reset left an invalid sparse document: %s", data)
	}
}

func TestV2EditOperationsAcrossWorkspaceAndRepositoryScopes(t *testing.T) {
	root := t.TempDir()
	raw := Config{
		Version:    2,
		v2Explicit: true,
		Workspaces: map[string]Workspace{root: {Repositories: map[string]Repository{"backend": {}}}},
	}
	// raw が v2 のときは request.V2 を省略しても scope の判定が v2 へ到達する。
	if err := applyEdit(&raw, EditRequest{Scope: V2ScopeWorkspace, Target: root, Key: "warm_count", Value: "3", Operation: EditSet}); err != nil {
		t.Fatal(err)
	}
	if err := applyEdit(&raw, EditRequest{V2: true, Scope: V2ScopeWorkspace, Target: root, Key: "discovery.exclude", Value: "custom", Operation: EditAdd}); err != nil {
		t.Fatal(err)
	}
	if err := applyEdit(&raw, EditRequest{V2: true, Scope: V2ScopeWorkspace, Target: root, Key: "discovery.exclude", Value: "./custom", Operation: EditRemove}); err != nil {
		t.Fatal(err)
	}
	if err := applyEdit(&raw, EditRequest{Scope: V2ScopeWorkspace, Target: root, Key: "discovery.exclude", Operation: EditReset}); err != nil {
		t.Fatal(err)
	}

	if err := applyEdit(&raw, EditRequest{V2: true, Scope: V2ScopeWorkspace, Target: root, Key: "repository_defaults.readiness.early_paths", Value: "workspace-early", Operation: EditAdd}); err != nil {
		t.Fatal(err)
	}
	if err := applyEdit(&raw, EditRequest{Scope: V2ScopeWorkspace, Target: root, Key: "repository_defaults.readiness.early_paths", Operation: EditReset}); err != nil {
		t.Fatal(err)
	}

	if err := applyEdit(&raw, EditRequest{V2: true, Scope: V2ScopeRepository, Target: root, Repository: "backend", Key: "prepare.command", Value: "prepare", Operation: EditAdd}); err != nil {
		t.Fatal(err)
	}
	if err := applyEdit(&raw, EditRequest{V2: true, Scope: V2ScopeRepository, Target: root, Repository: "backend", Key: "prepare.command", Value: "prepare", Operation: EditRemove}); err != nil {
		t.Fatal(err)
	}
	if err := applyEdit(&raw, EditRequest{V2: true, Scope: V2ScopeRepository, Target: root, Repository: "backend", Key: "prepare.command", Operation: EditReset}); err != nil {
		t.Fatal(err)
	}
	if err := applyEdit(&raw, EditRequest{V2: true, Scope: V2ScopeRepository, Target: root, Repository: "backend", Key: "prepare.command", Operation: EditOperation("replace")}); err == nil {
		t.Fatal("unknown v2 operation unexpectedly succeeded")
	}
}

func TestV2RepositorySourcesIncludeWorkspaceAndMembershipOverrides(t *testing.T) {
	root := t.TempDir()
	workspaceBranch := "workspace-branch"
	membershipBranch := "membership-branch"
	raw := Config{
		Version: 2, v2Explicit: true,
		RepositoryDefaults: RepositoryDefaults{DefaultBranch: "global-branch"},
		Workspaces: map[string]Workspace{root: {
			RepositoryDefaults: RepositoryDefaults{DefaultBranch: workspaceBranch},
			Repositories:       map[string]Repository{"backend": {DefaultBranch: membershipBranch}},
		}},
	}
	effective := Merge(Defaults(), raw)
	fields := V2Fields(effective, raw, V2ScopeRepository, root, "backend")
	sources := map[string]string{}
	for _, field := range fields {
		sources[field.Key] = field.Source
	}
	if sources["default_branch"] != "repository" {
		t.Fatalf("default_branch source=%q, want repository", sources["default_branch"])
	}
	if got := effective.RepositoryFor(root, "backend", "").DefaultBranch; got != membershipBranch {
		t.Fatalf("default branch=%q, want %q", got, membershipBranch)
	}
	if got := effective.RepositoryFor(root, "frontend", "").DefaultBranch; got != workspaceBranch {
		t.Fatalf("workspace default branch=%q, want %q", got, workspaceBranch)
	}
}

// YAML の membership key は正規化されていない。`./api` と書かれた entry を
// 正規化名 `api` の target で編集しても、key を増やさず同じ entry を更新する。
func TestV2RepositoryEditUsesTheExistingRawMembershipKey(t *testing.T) {
	writeConfigFile(t, "version: 2\nworkspaces:\n  \"$HOME/ws\":\n    repositories:\n      \"./api\":\n        readiness:\n          mode: full\n")
	root, err := canonicalPath("$HOME/ws")
	if err != nil {
		t.Fatal(err)
	}
	request := EditRequest{Scope: V2ScopeRepository, V2: true, Target: root, Repository: "api", Key: "readiness.mode", Value: "early", Operation: EditSet}
	commitConfigEdit(t, request)
	_, document := readConfigDocument(t)
	repositories := documentRepositories(t, document)
	if len(repositories) != 1 {
		t.Fatalf("repositories=%#v, want only the existing ./api key", repositories)
	}
	if _, ok := repositories["./api"]; !ok {
		t.Fatalf("repositories=%#v, want key ./api", repositories)
	}
	effective, _, err := LoadWithRaw()
	if err != nil {
		t.Fatalf("LoadWithRaw after set: %v", err)
	}
	if got := effective.Workspaces[root].Repositories["api"].Readiness.Mode; got != "early" {
		t.Fatalf("readiness.mode=%q, want early", got)
	}
	request.Operation = EditReset
	request.Value = ""
	commitConfigEdit(t, request)
	raw, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw after reset: %v", err)
	}
	if got := raw.Workspaces["$HOME/ws"].Repositories; len(got) != 0 {
		t.Fatalf("repositories after reset=%#v, want the override removed", got)
	}
}

// documentRepositories は保存された document から唯一の workspace の membership map を取り出す。
func documentRepositories(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	workspaces, ok := document["workspaces"].(map[string]any)
	if !ok || len(workspaces) != 1 {
		t.Fatalf("workspaces=%#v, want a single workspace", document["workspaces"])
	}
	for _, workspace := range workspaces {
		section, ok := workspace.(map[string]any)
		if !ok {
			t.Fatalf("workspace=%#v", workspace)
		}
		repositories, ok := section["repositories"].(map[string]any)
		if !ok {
			t.Fatalf("workspace=%#v, want repositories", section)
		}
		return repositories
	}
	return nil
}

// 継承した list への append は、requested root の workspace override を seed にする。
// workspace key が `$HOME/...` 表記だと exact lookup では override を見失う。
func TestAppendV2ListSeedsFromTheWorkspaceOverride(t *testing.T) {
	writeConfigFile(t, "version: 2\nrepository_defaults:\n  prepare:\n    command: [global-command]\nworkspaces:\n  \"$HOME/ws\":\n    repository_defaults:\n      prepare:\n        command: [workspace-command]\n    repositories:\n      api: {}\n")
	root, err := canonicalPath("$HOME/ws")
	if err != nil {
		t.Fatal(err)
	}
	commitConfigEdit(t, EditRequest{Scope: V2ScopeRepository, V2: true, Target: root, Repository: "api", Key: "prepare.command", Value: "extra", Operation: EditAdd})
	effective, _, err := LoadWithRaw()
	if err != nil {
		t.Fatalf("LoadWithRaw after add: %v", err)
	}
	got := effective.RepositoryFor(root, "api", "").Prepare.Command
	want := []string{"workspace-command", "extra"}
	if !slices.Equal(got, want) {
		t.Fatalf("prepare.command=%q, want %q", got, want)
	}
}
