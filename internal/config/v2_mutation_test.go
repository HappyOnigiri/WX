package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func mutationBool(value bool) *bool { return &value }

func mutationInt(value int) *int { return &value }

func mutationDuration(value time.Duration) *Duration { return &Duration{Duration: value} }

func mutationPresence(keys ...string) map[string]bool {
	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		present[key] = true
	}
	return present
}

func mutationRepositoryDefaults() RepositoryDefaults {
	zero := 0
	no := false
	tiny := Duration{Duration: time.Nanosecond}
	return RepositoryDefaults{
		DefaultBranch: "main",
		DirSource:     RepoDirSourceDirectory,
		COWMinSizeKiB: &zero,
		Submodules:    &no,
		Prepare: Prepare{
			Command: []string{}, Inputs: []string{}, Timeout: tiny, Version: "v1",
		},
		Includes: RepositoryIncludes{DefaultAgentRules: &no},
		Readiness: RepositoryReadiness{
			Mode: "full", EarlyPaths: []string{}, Timeout: &tiny, Progress: &no,
		},
		Storage: RepositoryStorage{CopyMode: CopyModeCopy},
	}
}

func mutationRepositoryValue() Repository {
	d := mutationRepositoryDefaults()
	return Repository{
		DefaultBranch: d.DefaultBranch,
		DirName:       "repository",
		DirSource:     d.DirSource,
		COWMinSizeKiB: d.COWMinSizeKiB,
		Submodules:    d.Submodules,
		Prepare:       d.Prepare,
		Includes:      d.Includes,
		Readiness:     d.Readiness,
		Storage:       d.Storage,
		Onboarding:    RepositoryOnboarding{CheckedAt: "checked", DeclinedAt: "declined"},
	}
}

func repositoryMutationKeys() []string {
	return []string{
		"default_branch", "dir_source", "cow_min_size_kib", "submodules",
		"prepare.command", "prepare.inputs", "prepare.timeout", "prepare.version",
		"includes.default_agent_rules", "readiness.mode", "readiness.early_paths",
		"readiness.timeout", "readiness.progress", "storage.copy_mode",
	}
}

func TestV2MutationOverlayKeepsExplicitZeroFalseAndEmptyValues(t *testing.T) {
	zero := 0
	no := false
	zeroDuration := Duration{}
	raw := Config{
		Version: 2,
		System: SystemConfig{
			Storage:   SystemStorage{BackupGenerations: 0, BackupRetention: zeroDuration},
			Pool:      SystemPool{PreparationConcurrency: 0},
			Retention: SystemRetention{},
			Discovery: SystemDiscovery{},
			Resume:    Resume{AutoFresh: false},
			Lease:     Lease{TTL: zeroDuration, Shell: ""},
			Logging:   Logging{Level: ""},
			Update:    SystemUpdate{AutoCheck: &no},
			Daemon:    SystemDaemon{LoginShell: &no},
		},
		WorkspaceDefaults: WorkspaceDefaults{
			Copy: []string{}, Link: []string{}, ReuseStandby: &no,
			FetchDefaultBranch: &no, WarmCount: &zero,
			Retention: WorkspaceRetention{HotStandby: &zeroDuration, EndedWorktree: &zeroDuration},
			Discovery: WorkspaceDiscovery{MaxDepth: &zero, Exclude: []string{}},
		},
		RepositoryDefaults: RepositoryDefaults{
			COWMinSizeKiB: &zero,
			Submodules:    &no,
			Prepare:       Prepare{Command: []string{}, Inputs: []string{}},
			Includes:      RepositoryIncludes{DefaultAgentRules: &no},
			Readiness:     RepositoryReadiness{EarlyPaths: []string{}, Timeout: &zeroDuration, Progress: &no},
		},
	}
	raw.System.Language = ""
	raw.System.Storage.WorktreeRoot = ""
	raw.System.Discovery.MaxEntries = 0
	raw.System.Discovery.Timeout = zeroDuration
	raw.System.Discovery.ReconcileInterval = zeroDuration
	raw.System.Retention = SystemRetention{}
	raw.System.Resume = Resume{}
	raw.System.Sessions = DefaultsV2().System.Sessions
	raw.System.Sessions.Paths.Claude.Sessions = []string{}
	raw.System.Sessions.Paths.Codex.Sessions = []string{}
	raw.present = mutationPresence(
		"system", "system.language", "system.storage.worktree_root", "system.storage.backup_generations",
		"system.storage.backup_retention", "system.pool.preparation_concurrency", "system.retention.quarantined",
		"system.retention.recovery_snapshot", "system.retention.expired_session_tombstone", "system.retention.failed_job",
		"system.retention.event_log", "system.discovery.max_entries", "system.discovery.timeout",
		"system.discovery.reconcile_interval", "system.resume.auto_fresh", "system.lease.ttl", "system.lease.shell",
		"system.logging.level", "system.update.auto_check", "system.daemon.login_shell",
		"system.sessions.paths.claude.sessions", "system.sessions.paths.codex.sessions",
		"workspace_defaults", "workspace_defaults.worktree", "workspace_defaults.copy", "workspace_defaults.link",
		"workspace_defaults.reuse_standby", "workspace_defaults.fetch_default_branch", "workspace_defaults.warm_count",
		"workspace_defaults.agent.add_dir", "workspace_defaults.retention.hot_standby",
		"workspace_defaults.retention.ended_worktree", "workspace_defaults.discovery.max_depth",
		"workspace_defaults.discovery.exclude", "repository_defaults", "repository_defaults.default_branch",
		"repository_defaults.dir_source", "repository_defaults.cow_min_size_kib", "repository_defaults.submodules",
		"repository_defaults.prepare.command", "repository_defaults.prepare.inputs", "repository_defaults.prepare.timeout",
		"repository_defaults.prepare.version", "repository_defaults.includes.default_agent_rules",
		"repository_defaults.readiness.mode", "repository_defaults.readiness.early_paths",
		"repository_defaults.readiness.timeout", "repository_defaults.readiness.progress",
		"repository_defaults.storage.copy_mode",
	)

	got := effectiveV2Defaults(raw)
	if got.System.Language != "" || got.System.Storage.BackupGenerations != 0 || got.System.Storage.BackupRetention.Duration != 0 || got.System.Pool.PreparationConcurrency != 0 {
		t.Fatalf("explicit system zero values were replaced: %+v", got.System)
	}
	if !reflect.DeepEqual(got.System.Retention, SystemRetention{}) || !reflect.DeepEqual(got.System.Discovery, SystemDiscovery{}) {
		t.Fatalf("explicit system nested zero values were replaced: retention=%+v discovery=%+v", got.System.Retention, got.System.Discovery)
	}
	if got.System.Update.AutoCheck == nil || *got.System.Update.AutoCheck || got.System.Daemon.LoginShell == nil || *got.System.Daemon.LoginShell {
		t.Fatalf("explicit system false values were replaced: update=%v daemon=%v", got.System.Update.AutoCheck, got.System.Daemon.LoginShell)
	}
	if got.WorkspaceDefaults.Worktree != "" || got.WorkspaceDefaults.Copy == nil || got.WorkspaceDefaults.Link == nil || len(got.WorkspaceDefaults.Copy) != 0 || len(got.WorkspaceDefaults.Link) != 0 {
		t.Fatalf("explicit workspace defaults were replaced: %+v", got.WorkspaceDefaults)
	}
	if got.WorkspaceDefaults.ReuseStandby == nil || *got.WorkspaceDefaults.ReuseStandby || got.WorkspaceDefaults.WarmCount == nil || *got.WorkspaceDefaults.WarmCount != 0 || got.WorkspaceDefaults.Discovery.MaxDepth == nil || *got.WorkspaceDefaults.Discovery.MaxDepth != 0 {
		t.Fatalf("explicit workspace pointer values were replaced: %+v", got.WorkspaceDefaults)
	}
	if got.RepositoryDefaults.DefaultBranch != "" || got.RepositoryDefaults.DirSource != "" || got.RepositoryDefaults.COWMinSizeKiB == nil || *got.RepositoryDefaults.COWMinSizeKiB != 0 || got.RepositoryDefaults.Submodules == nil || *got.RepositoryDefaults.Submodules {
		t.Fatalf("explicit repository scalar values were replaced: %+v", got.RepositoryDefaults)
	}
	if got.RepositoryDefaults.Prepare.Command == nil || got.RepositoryDefaults.Prepare.Inputs == nil || got.RepositoryDefaults.Readiness.EarlyPaths == nil || len(got.RepositoryDefaults.Prepare.Command) != 0 || len(got.RepositoryDefaults.Prepare.Inputs) != 0 || len(got.RepositoryDefaults.Readiness.EarlyPaths) != 0 {
		t.Fatalf("explicit repository empty lists were replaced: %+v", got.RepositoryDefaults)
	}
	if got.RepositoryDefaults.Includes.DefaultAgentRules == nil || *got.RepositoryDefaults.Includes.DefaultAgentRules || got.RepositoryDefaults.Readiness.Progress == nil || *got.RepositoryDefaults.Readiness.Progress {
		t.Fatalf("explicit repository false values were replaced: %+v", got.RepositoryDefaults)
	}

	var direct Config
	if err := yaml.Unmarshal([]byte(v2AllKeysDocument), &direct); err != nil {
		t.Fatal(err)
	}
	direct.present = nil
	directEffective := effectiveV2Defaults(direct)
	if !reflect.DeepEqual(directEffective.System, direct.System) {
		t.Fatalf("direct system values were not overlaid without presence map: got=%+v want=%+v", directEffective.System, direct.System)
	}
	if !reflect.DeepEqual(directEffective.WorkspaceDefaults, direct.WorkspaceDefaults) {
		t.Fatalf("direct workspace defaults were not overlaid without presence map: got=%+v want=%+v", directEffective.WorkspaceDefaults, direct.WorkspaceDefaults)
	}
	if !reflect.DeepEqual(directEffective.RepositoryDefaults, direct.RepositoryDefaults) {
		t.Fatalf("direct repository defaults were not overlaid without presence map: got=%+v want=%+v", directEffective.RepositoryDefaults, direct.RepositoryDefaults)
	}
}

func TestV2MutationEffectiveDefaultsKeepsLegacyMembershipAndMapPresence(t *testing.T) {
	raw := Config{Version: 2, Repositories: map[string]Repository{"legacy": {DefaultBranch: "legacy"}}}
	got := effectiveV2Defaults(raw)
	if got.Workspaces["legacy"].Repositories["."].DefaultBranch != "legacy" {
		t.Fatalf("legacy repository was not adapted: %+v", got.Workspaces)
	}
	if got.Workspaces == nil {
		t.Fatal("effective v2 workspaces map is nil")
	}

	empty := effectiveV2Defaults(Config{Version: 2, Workspaces: map[string]Workspace{}})
	if empty.Workspaces == nil {
		t.Fatal("empty explicit workspaces map was lost")
	}
	if !effectiveV2Defaults(Config{Version: 2, Workspaces: map[string]Workspace{}}).v2Explicit {
		t.Fatal("empty explicit workspaces section was not recorded")
	}
}

func TestV2MutationLegacyAdapterPrefersCanonicalAndChangedLegacyValues(t *testing.T) {
	canonical := withLegacyAdapter(Config{System: SystemConfig{Language: LanguageJapanese}})
	if canonical.System.Language != LanguageJapanese {
		t.Fatalf("canonical field was overwritten: system=%q", canonical.System.Language)
	}

	builtin := DefaultsV2()
	legacyFalse := Config{
		System: SystemConfig{
			Update: SystemUpdate{AutoCheck: builtin.System.Update.AutoCheck},
			Daemon: SystemDaemon{LoginShell: builtin.System.Daemon.LoginShell},
		},
		Update: Update{AutoCheck: false},
		Daemon: Daemon{LoginShell: false},
	}
	adapted := withLegacyAdapter(legacyFalse)
	if adapted.System.Update.AutoCheck == nil || *adapted.System.Update.AutoCheck || adapted.Update.AutoCheck {
		t.Fatalf("legacy auto_check false was not adopted: canonical=%v legacy=%v", adapted.System.Update.AutoCheck, adapted.Update.AutoCheck)
	}
	if adapted.System.Daemon.LoginShell == nil || *adapted.System.Daemon.LoginShell || adapted.Daemon.LoginShell {
		t.Fatalf("legacy login_shell false was not adopted: canonical=%v legacy=%v", adapted.System.Daemon.LoginShell, adapted.Daemon.LoginShell)
	}
}

func TestV2MutationPresenceMapsOverrideValueFallbacks(t *testing.T) {
	no := false
	raw := Config{RepositoryDefaults: RepositoryDefaults{Submodules: &no}, present: map[string]bool{"repository_defaults.submodules": false}}
	if v2RawFieldPresent(raw, "repository_defaults", "submodules") {
		t.Fatal("false presence entry was treated as an explicit field")
	}
	raw.present["repository_defaults.submodules"] = true
	raw.RepositoryDefaults.Submodules = nil
	if !v2RawFieldPresent(raw, "repository_defaults", "submodules") {
		t.Fatal("true presence entry was ignored when the value was zero")
	}

	root := filepath.Join(t.TempDir(), "workspace")
	alias := filepath.Join(filepath.Dir(root), "alias")
	if err := ensureMutationSymlink(alias, root); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	raw = Config{Workspaces: map[string]Workspace{alias: {Discovery: WorkspaceDiscovery{Exclude: []string{"build"}}}}, present: map[string]bool{
		"workspaces." + alias + ".discovery.exclude": true,
	}}
	if !dynamicRawFieldPresent(raw, root, "discovery.exclude") {
		t.Fatalf("canonical workspace presence was not found: present=%v", raw.present)
	}
	raw.present = nil
	if !dynamicRawFieldPresent(raw, alias, "discovery.exclude") {
		t.Fatal("non-presence-map workspace value was not found")
	}
}

func ensureMutationSymlink(alias, target string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	return os.Symlink(target, alias)
}

func TestV2MutationRepositorySourcesCoverEveryExplicitField(t *testing.T) {
	d := mutationRepositoryDefaults()
	present := mutationPresence("repository_defaults")
	for _, key := range repositoryMutationKeys() {
		present["repository_defaults."+key] = true
	}
	config := Config{Version: 2, RepositoryDefaults: d, present: present}
	sources := make(map[string]string)
	for key := range repositoryKeys() {
		sources[key] = "default"
	}
	markRepositorySources(sources, config, "", ".")
	for key := range repositoryKeys() {
		want := "global"
		if key == "dir_name" {
			want = "default"
		}
		if sources[key] != want {
			t.Errorf("global source[%q]=%q, want %q", key, sources[key], want)
		}
	}

	sources = make(map[string]string)
	for key := range repositoryKeys() {
		sources[key] = "default"
	}
	markRepositoryValueSources(sources, mutationRepositoryValue(), "workspace")
	for key := range repositoryKeys() {
		if sources[key] != "workspace" {
			t.Errorf("workspace source[%q]=%q, want workspace", key, sources[key])
		}
	}

	sources = make(map[string]string)
	for key := range repositoryKeys() {
		sources[key] = "default"
	}
	markRepositorySources(sources, Config{Version: 2, RepositoryDefaults: d}, "", ".")
	for key := range repositoryKeys() {
		want := "global"
		if key == "dir_name" {
			want = "default"
		}
		if sources[key] != want {
			t.Errorf("fallback global source[%q]=%q, want %q", key, sources[key], want)
		}
	}
}

func TestV2MutationMergeCopiesEveryRepositoryAndOnboardingValue(t *testing.T) {
	d := mutationRepositoryDefaults()
	gotDefaults := RepositoryDefaults{}
	mergeRepositoryDefaults(&gotDefaults, d)
	if !reflect.DeepEqual(gotDefaults, d) {
		t.Fatalf("repository defaults were not merged field by field: got=%+v want=%+v", gotDefaults, d)
	}

	value := mutationRepositoryValue()
	gotValue := Repository{}
	mergeRepositoryValue(&gotValue, value)
	if !reflect.DeepEqual(gotValue, value) {
		t.Fatalf("repository value was not merged field by field: got=%+v want=%+v", gotValue, value)
	}

	gotOnboarding := RepositoryOnboarding{CheckedAt: "old", DeclinedAt: "old"}
	mergeRepositoryOnboarding(&gotOnboarding, RepositoryOnboarding{CheckedAt: "new"})
	if gotOnboarding.CheckedAt != "new" || gotOnboarding.DeclinedAt != "old" {
		t.Fatalf("checked onboarding was not merged: %+v", gotOnboarding)
	}
	mergeRepositoryOnboarding(&gotOnboarding, RepositoryOnboarding{DeclinedAt: "declined"})
	if gotOnboarding.CheckedAt != "new" || gotOnboarding.DeclinedAt != "declined" {
		t.Fatalf("declined onboarding was not merged: %+v", gotOnboarding)
	}
}

func TestV2MutationMergeWorkspaceKeepsExplicitEmptyCollections(t *testing.T) {
	oldSubmodules := true
	newSubmodules := false
	dst := Workspace{Copy: []string{"old"}, Link: []string{"old"}, Submodules: &oldSubmodules, Repositories: map[string]Repository{"old": {}}}
	src := Workspace{Copy: []string{}, Link: []string{}, Submodules: &newSubmodules, Repositories: map[string]Repository{}}
	mergeWorkspace(&dst, src)
	if dst.Copy == nil || len(dst.Copy) != 0 || dst.Link == nil || len(dst.Link) != 0 || dst.Submodules == nil || *dst.Submodules || dst.Repositories == nil || len(dst.Repositories) != 0 {
		t.Fatalf("explicit workspace values were not preserved: %+v", dst)
	}
}

func TestV2MutationValidateRulesChecksAllNestedErrorsAndPresence(t *testing.T) {
	valid := RepositoryDefaults{}
	tests := []struct {
		name   string
		config Config
	}{
		{
			name:   "workspace defaults error",
			config: Config{Version: 2, Workspaces: map[string]Workspace{"/workspace": {RepositoryDefaults: RepositoryDefaults{DirSource: "invalid"}}}},
		},
		{
			name:   "top-level error after valid workspace",
			config: Config{Version: 2, Workspaces: map[string]Workspace{"/workspace": {RepositoryDefaults: valid}}, RepositoryDefaults: RepositoryDefaults{DirSource: "invalid"}},
		},
		{
			name:   "repository override error",
			config: Config{Version: 2, Workspaces: map[string]Workspace{"/workspace": {Repositories: map[string]Repository{"repo": {Readiness: RepositoryReadiness{Mode: "invalid"}}}}}},
		},
		{
			name:   "top-level error after valid repository",
			config: Config{Version: 2, Workspaces: map[string]Workspace{"/workspace": {Repositories: map[string]Repository{"repo": {}}}}, RepositoryDefaults: RepositoryDefaults{DirSource: "invalid"}},
		},
		{
			name:   "top-level defaults error",
			config: Config{Version: 2, RepositoryDefaults: RepositoryDefaults{DirSource: "invalid"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateV2Rules(&test.config); err == nil {
				t.Fatal("invalid v2 rules were accepted")
			}
		})
	}

	keyed := Config{Version: 2, Workspaces: map[string]Workspace{"/workspace": {Repositories: map[string]Repository{"folder/../repo": {}}}}}
	if err := ValidateV2Rules(&keyed); err != nil {
		t.Fatal(err)
	}
	if _, ok := keyed.Workspaces["/workspace"].Repositories["repo"]; !ok {
		t.Fatalf("repository keys were not normalized: %+v", keyed.Workspaces["/workspace"].Repositories)
	}
	nilMap := Config{Version: 2, Workspaces: map[string]Workspace{"/workspace": {Repositories: nil}}}
	if err := ValidateV2Rules(&nilMap); err != nil {
		t.Fatal(err)
	}
	if nilMap.Workspaces["/workspace"].Repositories != nil {
		t.Fatal("nil repository map became an explicit empty map")
	}

	normalized := Config{Version: 2, RepositoryDefaults: RepositoryDefaults{
		Prepare:   Prepare{Inputs: []string{"./input", "input"}},
		Readiness: RepositoryReadiness{EarlyPaths: []string{"./README.md", "README.md"}},
	}}
	if err := ValidateV2Rules(&normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized.RepositoryDefaults.Prepare.Inputs, []string{"input"}) || !reflect.DeepEqual(normalized.RepositoryDefaults.Readiness.EarlyPaths, []string{"README.md"}) {
		t.Fatalf("repository paths were not normalized: %+v", normalized.RepositoryDefaults)
	}
	invalidInputs := Config{Version: 2, RepositoryDefaults: RepositoryDefaults{Prepare: Prepare{Inputs: []string{"../input"}}}}
	if err := ValidateV2Rules(&invalidInputs); err == nil {
		t.Fatal("unsafe prepare input was accepted")
	}
	invalidEarlyPaths := Config{Version: 2, RepositoryDefaults: RepositoryDefaults{Readiness: RepositoryReadiness{EarlyPaths: []string{"../README.md"}}}}
	if err := ValidateV2Rules(&invalidEarlyPaths); err == nil {
		t.Fatal("unsafe early path was accepted")
	}
}

func TestV2MutationEditRoutesAndTracksSectionPresence(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var raw Config
	if err := SetV2Field(&raw, V2ScopeWorkspaceDefaults, "", "", "warm_count", "0"); err != nil {
		t.Fatal(err)
	}
	if raw.WorkspaceDefaults.WarmCount == nil || *raw.WorkspaceDefaults.WarmCount != 0 {
		t.Fatalf("explicit zero was not set: %+v", raw.WorkspaceDefaults)
	}
	if err := SetV2Field(&raw, V2ScopeWorkspaceDefaults, "", "", "warm_count", "not-an-int"); err == nil {
		t.Fatal("invalid scalar was accepted")
	}

	if err := SetV2Field(&raw, V2ScopeWorkspace, root, "", "repository_defaults.default_branch", "main"); err != nil {
		t.Fatal(err)
	}
	if got := raw.Workspaces[root].RepositoryDefaults.DefaultBranch; got != "main" {
		t.Fatalf("workspace repository-default route wrote %q", got)
	}
	if err := ResetV2Field(&raw, V2ScopeWorkspace, root, "", "repository_defaults.default_branch"); err != nil {
		t.Fatal(err)
	}
	if len(raw.Workspaces) != 0 || !raw.present["workspaces"] {
		t.Fatalf("reset did not retain explicit empty section: workspaces=%v present=%v", raw.Workspaces, raw.present)
	}

	raw = Config{Version: 2, v2Explicit: true, RepositoryDefaults: RepositoryDefaults{Readiness: RepositoryReadiness{EarlyPaths: []string{"global"}}}, Workspaces: map[string]Workspace{root: {}}}
	if err := AppendV2List(&raw, V2ScopeWorkspace, root, "", "repository_defaults.readiness.early_paths", "workspace"); err != nil {
		t.Fatal(err)
	}
	if got := raw.Workspaces[root].RepositoryDefaults.Readiness.EarlyPaths; !reflect.DeepEqual(got, []string{"global", "workspace"}) {
		t.Fatalf("workspace repository list route=%v", got)
	}

	list := Config{Version: 2, WorkspaceDefaults: WorkspaceDefaults{Copy: []string{"one", "two"}}}
	if err := RemoveV2List(&list, V2ScopeWorkspaceDefaults, "", "", "copy", "missing"); err == nil {
		t.Fatal("missing list item was accepted")
	}
	if err := RemoveV2List(&list, V2ScopeWorkspaceDefaults, "", "", "copy", "./one"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.WorkspaceDefaults.Copy, []string{"two"}) {
		t.Fatalf("list removal=%v", list.WorkspaceDefaults.Copy)
	}
}

func TestV2MutationSectionPresenceDistinguishesV1AndV2(t *testing.T) {
	legacy := Config{Version: 1}
	setV2SectionPresent(&legacy, "system", false)
	if legacy.present != nil {
		t.Fatalf("v1 absent section created presence map: %v", legacy.present)
	}
	v2 := Config{Version: 2}
	setV2SectionPresent(&v2, "system", false)
	if !v2.present["system"] {
		t.Fatalf("v2 empty section was not retained: %v", v2.present)
	}
}

func TestV2MutationFieldsFilterNestedWorkspaceSections(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	raw := Config{
		Version:           2,
		v2Explicit:        true,
		WorkspaceDefaults: WorkspaceDefaults{Worktree: "ask"},
		Workspaces: map[string]Workspace{root: {
			Worktree:           "hot",
			RepositoryDefaults: RepositoryDefaults{DefaultBranch: "workspace"},
			Repositories:       map[string]Repository{"repo": {}},
		}},
	}
	effective := Merge(Defaults(), raw)
	fields := V2Fields(effective, raw, V2ScopeWorkspace, root, "")
	counts := map[string]int{}
	for _, field := range fields {
		counts[field.Key]++
	}
	if counts["repositories"] != 0 || counts["submodules"] != 0 || counts["repository_defaults"] != 0 {
		t.Fatalf("nested workspace sections leaked into fields: %v", counts)
	}
	if counts["repository_defaults.default_branch"] != 1 {
		t.Fatalf("workspace repository defaults were not listed once: %v", counts)
	}
	if counts["worktree"] != 1 {
		t.Fatalf("workspace scalar fields were not listed once: %v", counts)
	}

	globalRaw := Config{Version: 2, RepositoryDefaults: RepositoryDefaults{DefaultBranch: "global"}, Workspaces: map[string]Workspace{root: {}}}
	globalEffective := Merge(Defaults(), globalRaw)
	globalFields := V2Fields(globalEffective, globalRaw, V2ScopeWorkspace, root, "")
	var globalSource string
	for _, field := range globalFields {
		if field.Key == "repository_defaults.default_branch" {
			globalSource = field.Source
		}
	}
	if globalSource != "global" {
		t.Fatalf("workspace repository default source=%q, want global", globalSource)
	}
}

func TestV2MutationInfersEmptyWorkspaceSectionPresence(t *testing.T) {
	without := inferV2Present(Config{Version: 2})
	if without["workspaces"] {
		t.Fatal("nil workspaces were inferred as an explicit section")
	}
	with := inferV2Present(Config{Version: 2, Workspaces: map[string]Workspace{}})
	if !with["workspaces"] {
		t.Fatal("empty workspaces map was not inferred as an explicit section")
	}
}

func TestV2MutationNormalizeRepositoryRelativeRejectsEmptyAndKeepsDot(t *testing.T) {
	if _, err := NormalizeRepositoryRelative(""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("empty relative path error=%v", err)
	}
	if got, err := NormalizeRepositoryRelative("./"); err != nil || got != "." {
		t.Fatalf("dot relative path=%q err=%v", got, err)
	}
}
