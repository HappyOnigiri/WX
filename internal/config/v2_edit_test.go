package config

import (
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
