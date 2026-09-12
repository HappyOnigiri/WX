package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

// V2 は config v2 の判定を一箇所へ閉じ込める。Version を省略した設定は
// 旧形式との区別ができないため、従来どおり version 1 として扱う。
func (c Config) V2() bool {
	if c.Version == 2 || !c.SystemIsZero() || !c.WorkspaceDefaultsIsZero() || !c.RepositoryDefaultsIsZero() {
		return true
	}
	return c.present != nil && (c.present["system"] || c.present["workspace_defaults"] || c.present["repository_defaults"])
}

func (c Config) SystemIsZero() bool            { return reflect.ValueOf(c.System).IsZero() }
func (c Config) WorkspaceDefaultsIsZero() bool { return reflect.ValueOf(c.WorkspaceDefaults).IsZero() }

func (c Config) RepositoryDefaultsIsZero() bool {
	return reflect.ValueOf(c.RepositoryDefaults).IsZero()
}

// DefaultsV2 は組み込み値を config v2 の節へ配置した Config を返す。既存の
// Defaults は legacy caller 用に残し、ロード時にこの値へ正規化する。
func DefaultsV2() Config {
	legacy := Defaults()
	trueValue, warm := legacy.Worktree.ReuseStandby, legacy.Pool.WarmPerWorkspace
	submodules := legacy.Worktree.Submodules
	cow := legacy.Storage.COWMinSizeKiB
	include := legacy.Includes.DefaultAgentRules
	progress := legacy.Readiness.Progress
	return Config{
		Version:    2,
		v2Explicit: true,
		System: SystemConfig{
			Language:  legacy.Language,
			Storage:   SystemStorage{WorktreeRoot: legacy.Storage.WorktreeRoot, BackupGenerations: legacy.Storage.BackupGenerations, BackupRetention: legacy.Storage.BackupRetention},
			Pool:      SystemPool{PreparationConcurrency: legacy.Pool.PreparationConcurrency},
			Retention: SystemRetention{Quarantined: legacy.Retention.Quarantined, RecoverySnapshot: legacy.Retention.RecoverySnapshot, ExpiredSessionTombstone: legacy.Retention.ExpiredSessionTombstone, FailedJob: legacy.Retention.FailedJob, EventLog: legacy.Retention.EventLog},
			Discovery: SystemDiscovery{MaxEntries: legacy.Discovery.MaxEntries, Timeout: legacy.Discovery.Timeout, ReconcileInterval: legacy.Discovery.ReconcileInterval},
			Resume:    legacy.Resume, Lease: legacy.Lease, Sessions: legacy.Sessions, Logging: legacy.Logging,
		},
		WorkspaceDefaults: WorkspaceDefaults{
			Worktree:     legacy.Worktree.Undefined,
			ReuseStandby: &trueValue, WarmCount: &warm, Agent: WorkspaceAgent{AddDir: legacy.Agent.AddDir},
			Retention: WorkspaceRetention{HotStandby: &legacy.Retention.HotStandby, EndedWorktree: &legacy.Retention.EndedWorktree},
			Discovery: WorkspaceDiscovery{MaxDepth: &legacy.Discovery.MaxDepth, Exclude: cloneStrings(legacy.Discovery.Exclude)},
		},
		RepositoryDefaults: RepositoryDefaults{
			DefaultBranch: "main", DirSource: legacy.Storage.RepoDirSource, COWMinSizeKiB: &cow, Submodules: &submodules,
			Includes: RepositoryIncludes{DefaultAgentRules: &include}, Readiness: RepositoryReadiness{Mode: legacy.Readiness.Mode, EarlyPaths: cloneStrings(legacy.Readiness.EarlyPaths), Timeout: &legacy.Readiness.Timeout, Progress: &progress},
			Storage: RepositoryStorage{CopyMode: legacy.Storage.CopyMode},
		},
		Workspaces: map[string]Workspace{},
	}
}

// effectiveV2Defaults は raw v2 を組み込み値へ重ね、legacy flatten view も
// 同じ優先順位で生成する。v2 の raw section は sparse のまま別途保持する。
func effectiveV2Defaults(raw Config) Config {
	d := DefaultsV2()
	// top-level section は疎なため、raw に存在する値だけを重ねる。
	overlaySystem(&d.System, raw.System, raw)
	overlayWorkspaceDefaults(&d.WorkspaceDefaults, raw.WorkspaceDefaults, raw)
	overlayRepositoryDefaults(&d.RepositoryDefaults, raw.RepositoryDefaults, raw)
	d.Workspaces = cloneWorkspaces(raw.Workspaces)
	// legacy field は既存 lifecycle code が読む互換 view である。
	flattenV2(&d)
	// file codec が raw v2 map と presence 情報を使えるように保持する。
	d.Workspaces = cloneWorkspaces(raw.Workspaces)
	d.Repositories = cloneRepositories(raw.Repositories)
	d.present = clonePresent(raw.present)
	if d.present == nil {
		d.present = inferV2Present(raw)
	}
	markLegacyPresentFromValue(raw, d.present)
	// Defaults().Version だけを変更した legacy caller と区別するため、直接生成する caller は v2Explicit を設定する。
	d.v2Explicit = raw.v2Explicit || (raw.Version == 2 && (raw.has("system", !raw.SystemIsZero()) || raw.has("workspace_defaults", !raw.WorkspaceDefaultsIsZero()) || raw.has("repository_defaults", !raw.RepositoryDefaultsIsZero()) || raw.has("workspaces", raw.Workspaces != nil)))
	d.Version = 2
	return d
}

func markLegacyPresentFromValue(raw Config, present map[string]bool) {
	value := reflect.ValueOf(raw)
	typeOfValue := value.Type()
	for _, tag := range []string{"worktree", "storage", "pool", "retention", "discovery", "readiness", "resume", "lease", "includes", "agent", "sessions", "logging"} {
		for index := 0; index < typeOfValue.NumField(); index++ {
			fieldInfo := typeOfValue.Field(index)
			fieldTag, _, _ := strings.Cut(fieldInfo.Tag.Get("yaml"), ",")
			if fieldTag != tag {
				continue
			}
			if field := value.Field(index); !field.IsZero() {
				present[tag] = true
			}
			break
		}
	}
	if raw.Repositories != nil {
		present["repositories"] = true
	}
}

// inferV2Present は YAML decode を経ずに作られた raw v2 Config 用に明示 key を補う。
// zero 値は省略扱いにし、組み込み値の source が default のままになるよう疎な map を返す。
func inferV2Present(raw Config) map[string]bool {
	present := map[string]bool{}
	add := func(section string, value reflect.Value) {
		if !value.IsValid() || value.IsZero() {
			return
		}
		present[section] = true
		walkV2Fields(value, "", func(key string, _ reflect.Value) {
			present[section+"."+key] = true
		})
	}
	add("system", reflect.ValueOf(raw.System))
	add("workspace_defaults", reflect.ValueOf(raw.WorkspaceDefaults))
	add("repository_defaults", reflect.ValueOf(raw.RepositoryDefaults))
	if raw.Workspaces != nil {
		present["workspaces"] = true
	}
	return present
}

func clonePresent(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneWorkspaces(in map[string]Workspace) map[string]Workspace {
	if in == nil {
		return nil
	}
	out := make(map[string]Workspace, len(in))
	for k, v := range in {
		v.Copy = cloneStrings(v.Copy)
		v.Link = cloneStrings(v.Link)
		v.Discovery.Exclude = cloneStrings(v.Discovery.Exclude)
		v.RepositoryDefaults.Readiness.EarlyPaths = cloneStrings(v.RepositoryDefaults.Readiness.EarlyPaths)
		v.Repositories = cloneRepositories(v.Repositories)
		out[k] = v
	}
	return out
}

func cloneRepositories(in map[string]Repository) map[string]Repository {
	if in == nil {
		return nil
	}
	out := make(map[string]Repository, len(in))
	for k, v := range in {
		v.Prepare.Command = cloneStrings(v.Prepare.Command)
		v.Readiness.EarlyPaths = cloneStrings(v.Readiness.EarlyPaths)
		out[k] = v
	}
	return out
}

func overlaySystem(dst *SystemConfig, src SystemConfig, raw Config) {
	if raw.has("system.language", src.Language != "") {
		dst.Language = src.Language
	}
	if raw.has("system.storage.worktree_root", src.Storage.WorktreeRoot != "") {
		dst.Storage.WorktreeRoot = src.Storage.WorktreeRoot
	}
	if raw.has("system.storage.backup_generations", src.Storage.BackupGenerations != 0) {
		dst.Storage.BackupGenerations = src.Storage.BackupGenerations
	}
	if raw.has("system.storage.backup_retention", src.Storage.BackupRetention.Duration != 0) {
		dst.Storage.BackupRetention = src.Storage.BackupRetention
	}
	if raw.has("system.pool.preparation_concurrency", src.Pool.PreparationConcurrency != 0) {
		dst.Pool.PreparationConcurrency = src.Pool.PreparationConcurrency
	}
	if raw.has("system.retention.quarantined", src.Retention.Quarantined.Duration != 0) {
		dst.Retention.Quarantined = src.Retention.Quarantined
	}
	if raw.has("system.retention.recovery_snapshot", src.Retention.RecoverySnapshot.Duration != 0) {
		dst.Retention.RecoverySnapshot = src.Retention.RecoverySnapshot
	}
	if raw.has("system.retention.expired_session_tombstone", src.Retention.ExpiredSessionTombstone.Duration != 0) {
		dst.Retention.ExpiredSessionTombstone = src.Retention.ExpiredSessionTombstone
	}
	if raw.has("system.retention.failed_job", src.Retention.FailedJob.Duration != 0) {
		dst.Retention.FailedJob = src.Retention.FailedJob
	}
	if raw.has("system.retention.event_log", src.Retention.EventLog.Duration != 0) {
		dst.Retention.EventLog = src.Retention.EventLog
	}
	if raw.has("system.discovery.max_entries", src.Discovery.MaxEntries != 0) {
		dst.Discovery.MaxEntries = src.Discovery.MaxEntries
	}
	if raw.has("system.discovery.timeout", src.Discovery.Timeout.Duration != 0) {
		dst.Discovery.Timeout = src.Discovery.Timeout
	}
	if raw.has("system.discovery.reconcile_interval", src.Discovery.ReconcileInterval.Duration != 0) {
		dst.Discovery.ReconcileInterval = src.Discovery.ReconcileInterval
	}
	if raw.has("system.resume.auto_fresh", src.Resume.AutoFresh) {
		dst.Resume.AutoFresh = src.Resume.AutoFresh
	}
	if raw.has("system.lease.ttl", src.Lease.TTL.Duration != 0) {
		dst.Lease.TTL = src.Lease.TTL
	}
	if raw.has("system.lease.shell", src.Lease.Shell != "") {
		dst.Lease.Shell = src.Lease.Shell
	}
	if raw.has("system.logging.level", src.Logging.Level != "") {
		dst.Logging.Level = src.Logging.Level
	}
	// sessions.paths は tool ごとの list を持つため、片方だけを指定した
	// sparse section でももう片方の組み込み値を失わないよう leaf 単位で重ねる。
	if raw.has("system.sessions.paths.claude.sessions", src.Sessions.Paths.Claude.Sessions != nil) {
		dst.Sessions.Paths.Claude.Sessions = cloneStrings(src.Sessions.Paths.Claude.Sessions)
	}
	if raw.has("system.sessions.paths.codex.sessions", src.Sessions.Paths.Codex.Sessions != nil) {
		dst.Sessions.Paths.Codex.Sessions = cloneStrings(src.Sessions.Paths.Codex.Sessions)
	}
}

func overlayWorkspaceDefaults(dst *WorkspaceDefaults, src WorkspaceDefaults, raw Config) {
	if raw.has("workspace_defaults.worktree", src.Worktree != "") {
		dst.Worktree = src.Worktree
	}
	if raw.has("workspace_defaults.copy", src.Copy != nil) {
		dst.Copy = cloneStrings(src.Copy)
	}
	if raw.has("workspace_defaults.link", src.Link != nil) {
		dst.Link = cloneStrings(src.Link)
	}
	if raw.has("workspace_defaults.reuse_standby", src.ReuseStandby != nil) {
		dst.ReuseStandby = src.ReuseStandby
	}
	if raw.has("workspace_defaults.warm_count", src.WarmCount != nil) {
		dst.WarmCount = src.WarmCount
	}
	if raw.has("workspace_defaults.agent.add_dir", src.Agent.AddDir != "") {
		dst.Agent.AddDir = src.Agent.AddDir
	}
	if raw.has("workspace_defaults.retention.hot_standby", src.Retention.HotStandby != nil) {
		dst.Retention.HotStandby = src.Retention.HotStandby
	}
	if raw.has("workspace_defaults.retention.ended_worktree", src.Retention.EndedWorktree != nil) {
		dst.Retention.EndedWorktree = src.Retention.EndedWorktree
	}
	if raw.has("workspace_defaults.discovery.max_depth", src.Discovery.MaxDepth != nil) {
		dst.Discovery.MaxDepth = src.Discovery.MaxDepth
	}
	if raw.has("workspace_defaults.discovery.exclude", src.Discovery.Exclude != nil) {
		dst.Discovery.Exclude = cloneStrings(src.Discovery.Exclude)
	}
}

func overlayRepositoryDefaults(dst *RepositoryDefaults, src RepositoryDefaults, raw Config) {
	if raw.has("repository_defaults.default_branch", src.DefaultBranch != "") {
		dst.DefaultBranch = src.DefaultBranch
	}
	if raw.has("repository_defaults.dir_source", src.DirSource != "") {
		dst.DirSource = src.DirSource
	}
	if raw.has("repository_defaults.cow_min_size_kib", src.COWMinSizeKiB != nil) {
		dst.COWMinSizeKiB = src.COWMinSizeKiB
	}
	if raw.has("repository_defaults.submodules", src.Submodules != nil) {
		dst.Submodules = src.Submodules
	}
	if raw.has("repository_defaults.prepare.command", src.Prepare.Command != nil) {
		dst.Prepare.Command = cloneStrings(src.Prepare.Command)
	}
	if raw.has("repository_defaults.prepare.timeout", src.Prepare.Timeout.Duration != 0) {
		dst.Prepare.Timeout = src.Prepare.Timeout
	}
	if raw.has("repository_defaults.prepare.version", src.Prepare.Version != "") {
		dst.Prepare.Version = src.Prepare.Version
	}
	if raw.has("repository_defaults.includes.default_agent_rules", src.Includes.DefaultAgentRules != nil) {
		dst.Includes.DefaultAgentRules = src.Includes.DefaultAgentRules
	}
	if raw.has("repository_defaults.readiness.mode", src.Readiness.Mode != "") {
		dst.Readiness.Mode = src.Readiness.Mode
	}
	if raw.has("repository_defaults.readiness.early_paths", src.Readiness.EarlyPaths != nil) {
		dst.Readiness.EarlyPaths = cloneStrings(src.Readiness.EarlyPaths)
	}
	if raw.has("repository_defaults.readiness.timeout", src.Readiness.Timeout != nil) {
		dst.Readiness.Timeout = src.Readiness.Timeout
	}
	if raw.has("repository_defaults.readiness.progress", src.Readiness.Progress != nil) {
		dst.Readiness.Progress = src.Readiness.Progress
	}
	if raw.has("repository_defaults.storage.copy_mode", src.Storage.CopyMode != "") {
		dst.Storage.CopyMode = src.Storage.CopyMode
	}
}

// flattenV2 は v2 の既定値を legacy 実効 field へ投影する。
// 投影結果を決定的にし、workspace membership map は変更しない。
func flattenV2(c *Config) {
	s, w, r := c.System, c.WorkspaceDefaults, c.RepositoryDefaults
	c.Language = s.Language
	c.Worktree.Undefined, c.Worktree.ReuseStandby = w.Worktree, derefBool(w.ReuseStandby)
	c.Worktree.Submodules = derefBool(r.Submodules)
	c.Storage.WorktreeRoot, c.Storage.BackupGenerations, c.Storage.BackupRetention = s.Storage.WorktreeRoot, s.Storage.BackupGenerations, s.Storage.BackupRetention
	c.Storage.CopyMode, c.Storage.COWMinSizeKiB, c.Storage.RepoDirSource = r.Storage.CopyMode, derefInt(r.COWMinSizeKiB), r.DirSource
	c.Pool.WarmPerWorkspace, c.Pool.PreparationConcurrency = derefInt(w.WarmCount), s.Pool.PreparationConcurrency
	c.Retention.HotStandby, c.Retention.EndedWorktree = derefDuration(w.Retention.HotStandby), derefDuration(w.Retention.EndedWorktree)
	c.Retention.Quarantined, c.Retention.RecoverySnapshot, c.Retention.ExpiredSessionTombstone, c.Retention.FailedJob, c.Retention.EventLog = s.Retention.Quarantined, s.Retention.RecoverySnapshot, s.Retention.ExpiredSessionTombstone, s.Retention.FailedJob, s.Retention.EventLog
	c.Discovery.MaxDepth, c.Discovery.Exclude = derefInt(w.Discovery.MaxDepth), cloneStrings(w.Discovery.Exclude)
	c.Discovery.MaxEntries, c.Discovery.Timeout, c.Discovery.ReconcileInterval = s.Discovery.MaxEntries, s.Discovery.Timeout, s.Discovery.ReconcileInterval
	c.Readiness.Mode, c.Readiness.EarlyPaths, c.Readiness.Timeout, c.Readiness.Progress = r.Readiness.Mode, cloneStrings(r.Readiness.EarlyPaths), derefDuration(r.Readiness.Timeout), derefBool(r.Readiness.Progress)
	c.Includes.DefaultAgentRules = derefBool(r.Includes.DefaultAgentRules)
	c.Agent.AddDir = w.Agent.AddDir
	c.Resume, c.Lease, c.Sessions, c.Logging = s.Resume, s.Lease, s.Sessions, s.Logging
}

func derefBool(v *bool) bool {
	if v == nil {
		return false
	}
	return *v
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefDuration(v *Duration) Duration {
	if v == nil {
		return Duration{}
	}
	return *v
}

// RepositoryFor は workspace 文脈で membership の設定を解決する。
// mainPath は任意で、指定時も legacy v1 の repository map にだけ使う。
func (c Config) RepositoryFor(workspaceRoot, relativePath, mainPath string) Repository {
	base := repositoryDefaultsAsRepository(c.RepositoryDefaults)
	if !c.V2() {
		base = Repository{}
	}
	if workspaceRoot != "" {
		if w, ok := c.Workspaces[workspaceRoot]; ok {
			mergeRepository(&base, w.RepositoryDefaults)
			rel := cleanRepositoryRelative(relativePath)
			if member, ok := w.Repositories[rel]; ok {
				mergeRepositoryValue(&base, member)
			}
		}
	}
	if mainPath != "" {
		if legacy, ok := c.Repositories[mainPath]; ok {
			mergeRepositoryValue(&base, legacy)
		}
	}
	return base
}

// WorkspaceFor は root の Workspace 実効 profile を返す。
// v2 workspace defaults と root entry を重ね、RepositoryFor 用の nested map を保つ。
func (c Config) WorkspaceFor(root string) Workspace {
	base := Workspace{}
	if c.V2() {
		d := c.WorkspaceDefaults
		base.Worktree = d.Worktree
		base.Copy = cloneStrings(d.Copy)
		base.Link = cloneStrings(d.Link)
		base.ReuseStandby = d.ReuseStandby
		base.WarmCount = d.WarmCount
		base.Agent = d.Agent
		base.Retention = d.Retention
		base.Discovery = d.Discovery
		base.RepositoryDefaults = c.RepositoryDefaults
	}
	if override, ok := c.Workspaces[root]; ok {
		mergeWorkspace(&base, override)
	}
	return base
}

func mergeWorkspace(dst *Workspace, src Workspace) {
	if src.Worktree != "" {
		dst.Worktree = src.Worktree
	}
	if src.Copy != nil {
		dst.Copy = cloneStrings(src.Copy)
	}
	if src.Link != nil {
		dst.Link = cloneStrings(src.Link)
	}
	if src.ReuseStandby != nil {
		dst.ReuseStandby = src.ReuseStandby
	}
	if src.Submodules != nil {
		dst.Submodules = src.Submodules
	}
	if src.WarmCount != nil {
		dst.WarmCount = src.WarmCount
	}
	if src.Agent.AddDir != "" {
		dst.Agent.AddDir = src.Agent.AddDir
	}
	if src.Retention.HotStandby != nil {
		dst.Retention.HotStandby = src.Retention.HotStandby
	}
	if src.Retention.EndedWorktree != nil {
		dst.Retention.EndedWorktree = src.Retention.EndedWorktree
	}
	if src.Discovery.MaxDepth != nil {
		dst.Discovery.MaxDepth = src.Discovery.MaxDepth
	}
	if src.Discovery.Exclude != nil {
		dst.Discovery.Exclude = cloneStrings(src.Discovery.Exclude)
	}
	mergeRepositoryDefaults(&dst.RepositoryDefaults, src.RepositoryDefaults)
	if src.Repositories != nil {
		dst.Repositories = cloneRepositories(src.Repositories)
	}
}

func mergeRepositoryDefaults(dst *RepositoryDefaults, src RepositoryDefaults) {
	if src.DefaultBranch != "" {
		dst.DefaultBranch = src.DefaultBranch
	}
	if src.DirSource != "" {
		dst.DirSource = src.DirSource
	}
	if src.COWMinSizeKiB != nil {
		dst.COWMinSizeKiB = src.COWMinSizeKiB
	}
	if src.Submodules != nil {
		dst.Submodules = src.Submodules
	}
	if src.Prepare.Command != nil {
		dst.Prepare.Command = cloneStrings(src.Prepare.Command)
	}
	if src.Prepare.Timeout.Duration != 0 {
		dst.Prepare.Timeout = src.Prepare.Timeout
	}
	if src.Prepare.Version != "" {
		dst.Prepare.Version = src.Prepare.Version
	}
	if src.Includes.DefaultAgentRules != nil {
		dst.Includes.DefaultAgentRules = src.Includes.DefaultAgentRules
	}
	if src.Readiness.Mode != "" {
		dst.Readiness.Mode = src.Readiness.Mode
	}
	if src.Readiness.EarlyPaths != nil {
		dst.Readiness.EarlyPaths = cloneStrings(src.Readiness.EarlyPaths)
	}
	if src.Readiness.Timeout != nil {
		dst.Readiness.Timeout = src.Readiness.Timeout
	}
	if src.Readiness.Progress != nil {
		dst.Readiness.Progress = src.Readiness.Progress
	}
	if src.Storage.CopyMode != "" {
		dst.Storage.CopyMode = src.Storage.CopyMode
	}
}

// ResolveRepository は公開する文脈付き resolver で、実効値と status/TUI 表示用の
// source map をまとめて返す。
type RepositoryResolution struct {
	Config  Repository
	Sources map[string]string
}

func (c Config) ResolveRepository(workspaceRoot, relativePath, mainPath string) RepositoryResolution {
	sources := map[string]string{}
	for key := range repositoryKeys() {
		sources[key] = "default"
	}
	if c.V2() {
		markRepositorySources(sources, c, workspaceRoot, relativePath)
	}
	resolved := c.RepositoryFor(workspaceRoot, relativePath, mainPath)
	return RepositoryResolution{Config: resolved, Sources: sources}
}

func repositoryKeys() map[string]struct{} {
	return map[string]struct{}{"default_branch": {}, "dir_source": {}, "cow_min_size_kib": {}, "submodules": {}, "prepare.command": {}, "prepare.timeout": {}, "prepare.version": {}, "includes.default_agent_rules": {}, "readiness.mode": {}, "readiness.early_paths": {}, "readiness.timeout": {}, "readiness.progress": {}, "storage.copy_mode": {}, "dir_name": {}}
}

func markRepositorySources(s map[string]string, c Config, root, rel string) {
	global := c.RepositoryDefaults
	// c は通常 built-in を含む実効 config である。raw presence map があれば使い、
	// built-in の source が default のままになるようにする。
	present := func(key string, fallback bool) bool {
		if c.present != nil {
			return c.present["repository_defaults."+key]
		}
		// DefaultsV2 は意図的に raw presence map を持たず、全値が built-in である。
		// 非 default の直接生成 Config では互換 caller 向けに nonzero fallback を使う。
		if c.v2Explicit {
			return false
		}
		return fallback
	}
	for key, explicit := range map[string]bool{
		"default_branch":               present("default_branch", global.DefaultBranch != ""),
		"dir_source":                   present("dir_source", global.DirSource != ""),
		"cow_min_size_kib":             present("cow_min_size_kib", global.COWMinSizeKiB != nil),
		"submodules":                   present("submodules", global.Submodules != nil),
		"prepare.command":              present("prepare.command", global.Prepare.Command != nil),
		"prepare.timeout":              present("prepare.timeout", global.Prepare.Timeout.Duration != 0),
		"prepare.version":              present("prepare.version", global.Prepare.Version != ""),
		"includes.default_agent_rules": present("includes.default_agent_rules", global.Includes.DefaultAgentRules != nil),
		"readiness.mode":               present("readiness.mode", global.Readiness.Mode != ""),
		"readiness.early_paths":        present("readiness.early_paths", global.Readiness.EarlyPaths != nil),
		"readiness.timeout":            present("readiness.timeout", global.Readiness.Timeout != nil),
		"readiness.progress":           present("readiness.progress", global.Readiness.Progress != nil),
		"storage.copy_mode":            present("storage.copy_mode", global.Storage.CopyMode != ""),
	} {
		if explicit {
			s[key] = "global"
		}
	}
	if w, ok := c.Workspaces[root]; ok {
		markRepositoryOverrideSources(s, w.RepositoryDefaults, "workspace")
		if member, ok := w.Repositories[cleanRepositoryRelative(rel)]; ok {
			markRepositoryValueSources(s, member, "repository")
		}
	}
}

func markRepositoryOverrideSources(s map[string]string, d RepositoryDefaults, source string) {
	markRepositoryValueSources(s, repositoryDefaultsAsRepository(d), source)
}

func markRepositoryValueSources(s map[string]string, d Repository, source string) {
	if d.DefaultBranch != "" {
		s["default_branch"] = source
	}
	if d.DirSource != "" {
		s["dir_source"] = source
	}
	if d.COWMinSizeKiB != nil {
		s["cow_min_size_kib"] = source
	}
	if d.Submodules != nil {
		s["submodules"] = source
	}
	if d.Prepare.Command != nil {
		s["prepare.command"] = source
	}
	if d.Prepare.Timeout.Duration != 0 {
		s["prepare.timeout"] = source
	}
	if d.Prepare.Version != "" {
		s["prepare.version"] = source
	}
	if d.Includes.DefaultAgentRules != nil {
		s["includes.default_agent_rules"] = source
	}
	if d.Readiness.Mode != "" {
		s["readiness.mode"] = source
	}
	if d.Readiness.EarlyPaths != nil {
		s["readiness.early_paths"] = source
	}
	if d.Readiness.Timeout != nil {
		s["readiness.timeout"] = source
	}
	if d.Readiness.Progress != nil {
		s["readiness.progress"] = source
	}
	if d.Storage.CopyMode != "" {
		s["storage.copy_mode"] = source
	}
	if d.DirName != "" {
		s["dir_name"] = source
	}
}

func mergeRepository(dst *Repository, src RepositoryDefaults) {
	mergeRepositoryValue(dst, repositoryDefaultsAsRepository(src))
}

func mergeRepositoryValue(dst *Repository, src Repository) {
	if src.DefaultBranch != "" {
		dst.DefaultBranch = src.DefaultBranch
	}
	if src.DirSource != "" {
		dst.DirSource = src.DirSource
	}
	if src.DirName != "" {
		dst.DirName = src.DirName
	}
	if src.COWMinSizeKiB != nil {
		dst.COWMinSizeKiB = src.COWMinSizeKiB
	}
	if src.Submodules != nil {
		dst.Submodules = src.Submodules
	}
	if src.Prepare.Command != nil {
		dst.Prepare.Command = cloneStrings(src.Prepare.Command)
	}
	if src.Prepare.Timeout.Duration != 0 {
		dst.Prepare.Timeout = src.Prepare.Timeout
	}
	if src.Prepare.Version != "" {
		dst.Prepare.Version = src.Prepare.Version
	}
	if src.Includes.DefaultAgentRules != nil {
		dst.Includes.DefaultAgentRules = src.Includes.DefaultAgentRules
	}
	if src.Readiness.Mode != "" {
		dst.Readiness.Mode = src.Readiness.Mode
	}
	if src.Readiness.EarlyPaths != nil {
		dst.Readiness.EarlyPaths = cloneStrings(src.Readiness.EarlyPaths)
	}
	if src.Readiness.Timeout != nil {
		dst.Readiness.Timeout = src.Readiness.Timeout
	}
	if src.Readiness.Progress != nil {
		dst.Readiness.Progress = src.Readiness.Progress
	}
	if src.Storage.CopyMode != "" {
		dst.Storage.CopyMode = src.Storage.CopyMode
	}
}

func cleanRepositoryRelative(path string) string {
	if path == "" {
		return "."
	}
	return filepath.Clean(path)
}

// NormalizeRepositoryRelative は v2 workspace membership key を検証・正規化する。
// membership は常に workspace root 相対で、absolute path・root 自身・parent escape・NUL を拒否する。
func NormalizeRepositoryRelative(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("repository relative path is required")
	}
	if strings.IndexByte(value, 0) >= 0 || filepath.IsAbs(value) {
		return "", fmt.Errorf("repository relative path %q must be relative", value)
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || !filepath.IsLocal(clean) {
		return "", fmt.Errorf("repository relative path %q escapes the workspace root", value)
	}
	return clean, nil
}

// ValidateV2Rules は membership key の安全性と v2 専用制約を検証する。
func ValidateV2Rules(c *Config) error {
	if !c.V2() {
		return nil
	}
	if c.Version != 2 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	// v2 document は canonical な nesting model を一つだけ持つ。
	// legacy flat section を併記すると優先順位が曖昧になり、scope にない値を
	// consumer が読むため受け付けない。
	if c.present != nil {
		if c.present["language"] {
			return errors.New("config version 2 does not allow top-level language; set system.language")
		}
		for _, section := range []string{"worktree", "storage", "pool", "retention", "discovery", "readiness", "resume", "lease", "includes", "agent", "sessions", "repositories", "logging"} {
			if c.present[section] {
				return fmt.Errorf("config version 2 does not allow legacy section %q", section)
			}
		}
	}
	for root, w := range c.Workspaces {
		if w.Submodules != nil {
			return fmt.Errorf("workspaces.%s.submodules is not valid in config version 2; set repository_defaults.submodules or a repository override", root)
		}
		var err error
		w.RepositoryDefaults, err = validateRepositoryDefaults("workspaces."+root+".repository_defaults", w.RepositoryDefaults)
		if err != nil {
			return err
		}
		normalizedMembers := make(map[string]Repository, len(w.Repositories))
		for rel, r := range w.Repositories {
			clean, normalizeErr := NormalizeRepositoryRelative(rel)
			if normalizeErr != nil {
				return fmt.Errorf("workspaces.%s.repositories.%s must be a workspace-relative path", root, rel)
			}
			if _, exists := normalizedMembers[clean]; exists {
				return fmt.Errorf("workspaces.%s.repositories collide at relative path %s", root, clean)
			}
			if r.DirName == "" { /* permitted */
			}
			normalized, err := validateRepositoryOverride("workspaces."+root+".repositories."+clean, r)
			if err != nil {
				return err
			}
			normalizedMembers[clean] = normalized
		}
		if w.Repositories != nil {
			w.Repositories = normalizedMembers
		}
		c.Workspaces[root] = w
	}
	var err error
	c.RepositoryDefaults, err = validateRepositoryDefaults("repository_defaults", c.RepositoryDefaults)
	if err != nil {
		return err
	}
	return nil
}

func validateRepositoryDefaults(key string, d RepositoryDefaults) (RepositoryDefaults, error) {
	if d.DirSource != "" && d.DirSource != RepoDirSourceRemote && d.DirSource != RepoDirSourceDirectory {
		return RepositoryDefaults{}, fmt.Errorf("%s.dir_source must be %s or %s", key, RepoDirSourceRemote, RepoDirSourceDirectory)
	}
	if d.COWMinSizeKiB != nil && (*d.COWMinSizeKiB < 0 || *d.COWMinSizeKiB > MaxCOWMinSizeKiB) {
		return RepositoryDefaults{}, fmt.Errorf("%s.cow_min_size_kib must be between 0 and %d", key, MaxCOWMinSizeKiB)
	}
	if d.Submodules != nil { /* bool is already typed */
	}
	if d.Prepare.Timeout.Duration < 0 {
		return RepositoryDefaults{}, fmt.Errorf("%s.prepare.timeout must not be negative", key)
	}
	if d.Readiness.Mode != "" && d.Readiness.Mode != "early" && d.Readiness.Mode != "full" {
		return RepositoryDefaults{}, fmt.Errorf("%s.readiness.mode must be early or full", key)
	}
	if d.Readiness.Timeout != nil && d.Readiness.Timeout.Duration <= 0 {
		return RepositoryDefaults{}, fmt.Errorf("%s.readiness.timeout must be positive", key)
	}
	if d.Readiness.EarlyPaths != nil {
		paths, err := validateEarlyPaths(d.Readiness.EarlyPaths, key+".readiness.early_paths")
		if err != nil {
			return RepositoryDefaults{}, err
		}
		d.Readiness.EarlyPaths = paths
	}
	if d.Storage.CopyMode != "" && d.Storage.CopyMode != CopyModeAuto && d.Storage.CopyMode != CopyModeCOW && d.Storage.CopyMode != CopyModeCopy {
		return RepositoryDefaults{}, fmt.Errorf("%s.storage.copy_mode must be %s, %s, or %s", key, CopyModeAuto, CopyModeCOW, CopyModeCopy)
	}
	return d, nil
}
