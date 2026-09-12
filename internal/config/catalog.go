package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ValueKind は設定エディターが値ごとの入力方法を選ぶための分類である。
type ValueKind string

const (
	KindString   ValueKind = "string"
	KindInteger  ValueKind = "integer"
	KindBoolean  ValueKind = "boolean"
	KindDuration ValueKind = "duration"
	KindList     ValueKind = "list"
)

// Metadata は CLI と TUI が共有する設定項目の説明と編集制約である。
// Key と Kind は設定構造体から導出し、手書きの説明側には重複させない。
type Metadata struct {
	Key         string
	Kind        ValueKind
	Scopes      []string
	DisplayName string
	Group       string
	Description string
	Impact      string
	Choices     []string
}

type catalogText struct {
	name, description, impact string
	choices                   []string
}

var catalogTexts = map[string]catalogText{
	"worktree.undefined":                  {"Default worktree policy", "Controls worktree use for workspaces without an explicit policy.", "Changes how the next agent session starts.", []string{"ask", "hot", "cold", "off"}},
	"worktree.reuse_standby":              {"Reuse standby worktrees", "Updates an older READY worktree to the requested OID and reuses it.", "When disabled, a non-matching lease uses a cold start.", nil},
	"worktree.submodules":                 {"Prepare submodules", "Prepares worktree submodules through local clones.", "Existing standby slots are no longer reusable after this changes.", nil},
	"storage.worktree_root":               {"Worktree storage root", "Directory used for new worktree root generations.", "Existing slots stay with their registered root generation.", nil},
	"storage.copy_mode":                   {"Copy mode", "Controls APFS copy-on-write sharing for checked-out files.", "Affected repositories rebuild their standby slots.", []string{"auto", "cow", "copy"}},
	"storage.cow_min_size_kib":            {"Minimum CoW size", "Minimum file size in KiB eligible for CoW sharing.", "Lower values increase both sharing coverage and inspection cost.", nil},
	"storage.repo_dir_source":             {"Repository name source", "Chooses how repository directory names are derived inside slots.", "Affects placement names in newly prepared workspaces.", []string{"remote", "directory"}},
	"storage.backup_generations":          {"Database backup generations", "Number of state database backup generations to retain.", "Changes the amount of retained management data.", nil},
	"storage.backup_retention":            {"Database backup retention", "How long state database backups are retained.", "Shorter values collect old backups sooner.", nil},
	"pool.warm_per_workspace":             {"Standby slot count", "READY slots maintained for each hot workspace.", "Zero disables automatic replenishment.", nil},
	"pool.preparation_concurrency":        {"Preparation concurrency", "Maximum concurrent user-facing prepare, restore, and snapshot jobs.", "Higher values increase CPU and disk load.", nil},
	"retention.hot_standby":               {"Standby retention", "How long an unused READY slot remains hot.", "Zero disables automatic replenishment.", nil},
	"retention.ended_worktree":            {"Ended worktree retention", "How long an ended session worktree remains available for resume.", "Shorter values make collection before resume more likely.", nil},
	"retention.quarantined":               {"Quarantined slot retention", "How long quarantined slots remain for diagnosis.", "Shorter values remove diagnostic artifacts sooner.", nil},
	"retention.recovery_snapshot":         {"Recovery snapshot retention", "How long session recovery snapshots are retained.", "Shorter values reduce the window for restoring old work.", nil},
	"retention.expired_session_tombstone": {"Expired session retention", "How long identifiers for expired sessions are retained.", "Changes the diagnostic window for stale references and duplicates.", nil},
	"retention.failed_job":                {"Failed job retention", "How long failed daemon job records are retained.", "Changes how long failure history remains available.", nil},
	"retention.event_log":                 {"Event log retention", "How long daemon event records are retained.", "Changes the amount of history available for diagnosis.", nil},
	"discovery.max_depth":                 {"Discovery depth", "Maximum repository discovery depth within a workspace.", "Higher values increase coverage and scan time.", nil},
	"discovery.max_entries":               {"Discovery entry limit", "Maximum entries inspected during workspace discovery.", "Higher values support larger directory trees at greater cost.", nil},
	"discovery.timeout":                   {"Discovery timeout", "Maximum time allowed for repository and workspace discovery.", "Values that are too short can reject large workspaces.", nil},
	"discovery.reconcile_interval":        {"Reconcile interval", "Interval between daemon reconciliation passes.", "Shorter values detect changes sooner and run maintenance more often.", nil},
	"discovery.exclude":                   {"Discovery exclusions", "Directory names excluded from repository discovery.", "Removing exclusions expands the scan area.", nil},
	"readiness.mode":                      {"Readiness mode", "Selects the preparation stage at which an agent may start.", "Full waits for all preparation; early relies on hooks for the remainder.", []string{"early", "full"}},
	"readiness.early_paths":               {"Early placement paths", "Additional paths placed before early startup.", "Makes static startup configuration available sooner.", nil},
	"readiness.timeout":                   {"Readiness timeout", "Maximum time to wait for worktree readiness.", "Values that are too short interrupt valid preparation.", nil},
	"readiness.progress":                  {"Readiness progress", "Shows preparation progress on an interactive terminal.", "Only display changes; preparation behavior is unchanged.", nil},
	"resume.auto_fresh":                   {"Automatic fresh resume", "Starts fresh when the original worktree cannot be restored.", "When enabled, skips confirmation in favor of resuming the conversation.", nil},
	"lease.ttl":                           {"Path lease lifetime", "Time before a path lease such as wx new advances to snapshot.", "Edits after the deadline are not included in the snapshot.", nil},
	"lease.shell":                         {"Shell executable", "Shell path used by wx shell.", "An empty value uses the environment default.", nil},
	"includes.default_agent_rules":        {"Default agent assets", "Includes standard agent instruction files in worktrees.", "When disabled, only explicitly configured includes are placed.", nil},
	"agent.add_dir":                       {"Additional agent directories", "Controls when repositories are passed as additional agent directories.", "Changes which repositories and assets the agent can load.", []string{"always", "worktree", "off"}},
	"logging.level":                       {"Log level", "Controls detail written to the daemon log.", "More detailed levels increase log volume.", []string{"debug", "info", "warn", "error"}},
	"sessions.paths.claude.sessions":      {"Claude session paths", "Directories searched for Claude conversation history.", "Changes which conversations appear as resume candidates.", nil},
	"sessions.paths.codex.sessions":       {"Codex session paths", "Directories searched for Codex conversation history.", "Changes which conversations appear as resume candidates.", nil},
	"worktree":                            {"Workspace worktree policy", "Controls worktree use for this workspace.", "Changes how the next agent session starts.", []string{"hot", "cold", "off"}},
	"copy":                                {"Workspace copy paths", "Paths copied from a non-Git workspace root into a slot.", "Changes preparation content and its fingerprint.", nil},
	"link":                                {"Workspace link paths", "Paths linked back to a non-Git workspace root.", "The slot accesses source content directly.", nil},
	"reuse_standby":                       {"Workspace standby reuse", "Overrides READY slot update policy for this workspace.", "Overrides the global standby reuse setting.", nil},
	"submodules":                          {"Workspace submodules", "Overrides submodule preparation for this workspace.", "Overrides the global submodule setting.", nil},
	"warm_count":                          {"Workspace standby count", "READY slots maintained for this workspace.", "Zero disables automatic replenishment for this workspace.", nil},
	"default_branch":                      {"Default branch", "Branch used as the detached base for this repository.", "Affects the starting point of new worktrees.", nil},
	"dir_name":                            {"Placement directory name", "Fixes the repository directory name inside a slot.", "Affects placement in newly prepared workspaces.", nil},
	"dir_source":                          {"Placement name source", "Chooses how this repository directory name is derived.", "Overrides the global naming policy.", []string{"remote", "directory"}},
	"cow_min_size_kib":                    {"Repository minimum CoW size", "Minimum file size eligible for CoW sharing in this repository.", "Rebuilds standby slots for the repository.", nil},
	"prepare.command":                     {"Preparation command", "Command run in this repository after checkout.", "A failure also fails slot preparation.", nil},
	"prepare.timeout":                     {"Preparation command timeout", "Maximum time allowed for the preparation command.", "Values that are too short fail preparation.", nil},
	"prepare.version":                     {"Preparation version", "Identifier used to intentionally invalidate earlier preparation.", "Existing standby slots are no longer reusable after this changes.", nil},
	// v2 は明示 scope 内で同じ leaf 名を使う。workspace の nested
	// repository_defaults だけは workspace key と衝突するため section prefix を残す。
	"repository_defaults.default_branch":               {"Workspace repository default branch", "Default branch inherited by repositories in this workspace.", "Changes the starting point of new worktrees in this workspace.", nil},
	"repository_defaults.dir_source":                   {"Workspace repository name source", "Naming source inherited by repositories in this workspace.", "Affects newly prepared repository placement names.", []string{"remote", "directory"}},
	"repository_defaults.cow_min_size_kib":             {"Workspace repository minimum CoW size", "CoW threshold inherited by repositories in this workspace.", "Affected standby slots rebuild after this changes.", nil},
	"repository_defaults.submodules":                   {"Workspace repository submodules", "Submodule policy inherited by repositories in this workspace.", "Existing standby slots rebuild after this changes.", nil},
	"repository_defaults.prepare.command":              {"Workspace repository preparation command", "Preparation command inherited by repositories in this workspace.", "A failure fails preparation for affected repositories.", nil},
	"repository_defaults.prepare.timeout":              {"Workspace repository preparation timeout", "Preparation timeout inherited by repositories in this workspace.", "Values that are too short fail preparation.", nil},
	"repository_defaults.prepare.version":              {"Workspace repository preparation version", "Version identifier inherited by repositories in this workspace.", "Existing standby slots are no longer reusable after this changes.", nil},
	"repository_defaults.includes.default_agent_rules": {"Workspace repository agent assets", "Whether standard agent files are inherited by repositories in this workspace.", "Changes preparation content for affected repositories.", nil},
	"repository_defaults.readiness.mode":               {"Workspace repository readiness mode", "Readiness mode inherited by repositories in this workspace.", "Changes when agents may start.", []string{"early", "full"}},
	"repository_defaults.readiness.early_paths":        {"Workspace repository early paths", "Early placement paths inherited by repositories in this workspace.", "Changes startup files available before full preparation.", nil},
	"repository_defaults.readiness.timeout":            {"Workspace repository readiness timeout", "Readiness timeout inherited by repositories in this workspace.", "Values that are too short interrupt valid preparation.", nil},
	"repository_defaults.readiness.progress":           {"Workspace repository readiness progress", "Whether preparation progress is shown while repositories in this workspace become ready.", "Only display changes; preparation behavior is unchanged.", nil},
	"repository_defaults.storage.copy_mode":            {"Workspace repository copy mode", "Copy mode inherited by repositories in this workspace.", "Affected standby slots rebuild after this changes.", []string{"auto", "cow", "copy"}},
}

// Catalog は構造体から導出した全キーを、設定ファイルでの宣言順に返す。
func Catalog() []Metadata {
	byKey := map[string]*Metadata{}
	order := make([]string, 0)
	add := func(key string, field reflect.Value, scope string) {
		meta := byKey[key]
		if meta == nil {
			text := catalogTexts[key]
			group, _, _ := strings.Cut(key, ".")
			value := Metadata{Key: key, Kind: kindOf(field), DisplayName: text.name, Group: group, Description: text.description, Impact: text.impact, Choices: append([]string(nil), text.choices...)}
			meta = &value
			byKey[key] = meta
			order = append(order, key)
		}
		if !contains(meta.Scopes, scope) {
			meta.Scopes = append(meta.Scopes, scope)
		}
	}
	defaults := reflect.ValueOf(Defaults())
	walkConfigLeaves(defaults, "", func(key string, field reflect.Value) { add(key, field, "global") })
	walkConfigLists(defaults, "", func(key string, field reflect.Value) { add(key, field, "global") })
	for _, scope := range []Scope{ScopeWorkspace, ScopeRepository} {
		walkScopeFields(scope.newEntry(), "", func(key string, field reflect.Value) { add(key, field, scope.String()) })
	}
	// v2 は system/default の明示 scope でも同じ leaf を公開する。
	// section 名を除いた key を共有し、`wx config --system storage.worktree_root`
	// と v1 catalog が同じ設定を説明できるようにする。
	walkV2Fields(reflect.ValueOf(SystemConfig{}), "", func(key string, field reflect.Value) { add(key, field, V2ScopeSystem) })
	walkV2Fields(reflect.ValueOf(WorkspaceDefaults{}), "", func(key string, field reflect.Value) { add(key, field, V2ScopeWorkspaceDefaults) })
	walkV2Fields(reflect.ValueOf(RepositoryDefaults{}), "", func(key string, field reflect.Value) { add(key, field, V2ScopeRepositoryDefaults) })
	walkV2Fields(reflect.ValueOf(Repository{}), "", func(key string, field reflect.Value) { add(key, field, V2ScopeRepository) })
	walkV2Fields(reflect.ValueOf(RepositoryDefaults{}), "repository_defaults", func(key string, field reflect.Value) { add(key, field, V2ScopeWorkspace) })
	out := make([]Metadata, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}

func kindOf(field reflect.Value) ValueKind {
	for field.Kind() == reflect.Pointer {
		field = reflect.New(field.Type().Elem()).Elem()
	}
	switch {
	case field.Type() == durationType:
		return KindDuration
	case field.Kind() == reflect.Bool:
		return KindBoolean
	case field.Kind() == reflect.Int:
		return KindInteger
	case field.Kind() == reflect.Slice:
		return KindList
	default:
		return KindString
	}
}

// Describe は指定 scope で利用できる設定項目の説明を返す。
func Describe(key string, scope string) (Metadata, error) {
	for _, meta := range Catalog() {
		if meta.Key == key && (scope == "" || contains(meta.Scopes, scope)) {
			return meta, nil
		}
	}
	var keys []string
	for _, meta := range Catalog() {
		if scope == "" || contains(meta.Scopes, scope) {
			keys = append(keys, meta.Key)
		}
	}
	sort.Strings(keys)
	prefix := ""
	if scope != "" {
		prefix = scope + " "
	}
	return Metadata{}, fmt.Errorf("unknown %sconfig key %q; available keys: %s", prefix, key, strings.Join(keys, ", "))
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
