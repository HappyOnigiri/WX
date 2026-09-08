package config

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"

	sessionsconfig "github.com/HappyOnigiri/WX/internal/sessions/config"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("duration must be a string")
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	d.Duration = v
	return nil
}
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

type Config struct {
	Worktree     WorktreePolicy        `yaml:"worktree,omitempty"`
	Version      int                   `yaml:"version,omitempty"`
	Storage      Storage               `yaml:"storage,omitempty"`
	Pool         Pool                  `yaml:"pool,omitempty"`
	Retention    Retention             `yaml:"retention,omitempty"`
	Discovery    Discovery             `yaml:"discovery,omitempty"`
	Readiness    Readiness             `yaml:"readiness,omitempty"`
	Resume       Resume                `yaml:"resume,omitempty"`
	Lease        Lease                 `yaml:"lease,omitempty"`
	Includes     Includes              `yaml:"includes,omitempty"`
	Sessions     sessionsconfig.Config `yaml:"sessions,omitempty"`
	Workspaces   map[string]Workspace  `yaml:"workspaces,omitempty"`
	Repositories map[string]Repository `yaml:"repositories,omitempty"`
	Logging      Logging               `yaml:"logging,omitempty"`
	present      map[string]bool
}
type WorktreePolicy struct {
	Undefined string `yaml:"undefined,omitempty"`
}

type Storage struct {
	WorktreeRoot string `yaml:"worktree_root,omitempty"`
	CopyMode     string `yaml:"copy_mode,omitempty"`
	// RepoDirSource は slot 内の repository directory 名の導出方法を選ぶ。
	// remote は origin URL の basename、directory は main worktree の名前を使い、Repositories の個別指定を優先する。
	RepoDirSource     string   `yaml:"repo_dir_source,omitempty"`
	BackupGenerations int      `yaml:"backup_generations,omitempty"`
	BackupRetention   Duration `yaml:"backup_retention,omitempty"`
}
type Pool struct {
	WarmPerWorkspace int `yaml:"warm_per_workspace,omitempty"`
	// PreparationConcurrency は利用者が完了を待つ準備・復元・保存の同時実行数である。
	// 待機枠の補充と自動削除はこの枠を使わず、別に確保した保守枠1本で動く（既定では利用者向け2本と保守1本）。
	PreparationConcurrency int `yaml:"preparation_concurrency,omitempty"`
}
type Retention struct {
	HotStandby    Duration `yaml:"hot_standby,omitempty"`
	EndedWorktree Duration `yaml:"ended_worktree,omitempty"`
	// Quarantined は隔離slotの実体をGCが削除するまでの保持期間。
	// LEASEDから隔離へ落ちたslotを調査前に消さないよう、ended_worktreeより長く取る。
	Quarantined             Duration `yaml:"quarantined,omitempty"`
	RecoverySnapshot        Duration `yaml:"recovery_snapshot,omitempty"`
	ExpiredSessionTombstone Duration `yaml:"expired_session_tombstone,omitempty"`
	FailedJob               Duration `yaml:"failed_job,omitempty"`
	EventLog                Duration `yaml:"event_log,omitempty"`
}
type Discovery struct {
	MaxDepth          int      `yaml:"max_depth,omitempty"`
	MaxEntries        int      `yaml:"max_entries,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	ReconcileInterval Duration `yaml:"reconcile_interval,omitempty"`
	Exclude           []string `yaml:"exclude,omitempty"`
}
type Readiness struct {
	Mode       string   `yaml:"mode,omitempty"`
	EarlyPaths []string `yaml:"early_paths,omitempty"`
	Timeout    Duration `yaml:"timeout,omitempty"`
}

// Resume は会話の再開時の既定の振る舞いを決める。
type Resume struct {
	// AutoFresh は、当時の worktree を復元できないときの確認を省き、新しい worktree での再開をそのまま選ぶ。
	AutoFresh bool `yaml:"auto_fresh,omitempty"`
}

// Lease は agent 起動以外への worktree 貸出（wx shell / wx run / wx new）の設定である。
type Lease struct {
	// TTL は貸出の期限である。期限が来ても保存されてから返却され、実体は retention.ended_worktree の間残る。
	// プロセスに随伴しない wx new の貸出を、返却し忘れたまま無期限に居座らせないための保険である。
	TTL Duration `yaml:"ttl,omitempty"`
	// Shell は wx shell が起動するシェルを固定する。空なら $SHELL、それも無ければ /bin/sh を使う。
	Shell string `yaml:"shell,omitempty"`
}

type Workspace struct {
	Worktree string   `yaml:"worktree,omitempty"`
	Copy     []string `yaml:"copy,omitempty"`
	Link     []string `yaml:"link,omitempty"`
}
type Includes struct {
	DefaultAgentRules bool `yaml:"default_agent_rules,omitempty"`
}
type Repository struct {
	DefaultBranch string `yaml:"default_branch,omitempty"`
	// DirName は slot 内の repository directory 名を固定する。
	// 空なら DirSource（remote または directory）で導出する。Repositories は map のため YAML で直接指定する。
	DirName   string             `yaml:"dir_name,omitempty"`
	DirSource string             `yaml:"dir_source,omitempty"`
	Prepare   Prepare            `yaml:"prepare,omitempty"`
	Includes  RepositoryIncludes `yaml:"includes,omitempty"`
}
type RepositoryIncludes struct {
	DefaultAgentRules *bool `yaml:"default_agent_rules,omitempty"`
}
type Prepare struct {
	Command []string `yaml:"command,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty"`
	Version string   `yaml:"version,omitempty"`
}
type Logging struct {
	Level string `yaml:"level,omitempty"`
}

// RepoDirSourceRemote と RepoDirSourceDirectory は storage.repo_dir_source と repositories.<path>.dir_source の値。
const (
	RepoDirSourceRemote    = "remote"
	RepoDirSourceDirectory = "directory"
)

const (
	CopyModeAuto = "auto"
	CopyModeCOW  = "cow"
	CopyModeCopy = "copy"
)

func Defaults() Config {
	return Config{
		Worktree: WorktreePolicy{Undefined: "ask"},
		Version:  1, Storage: Storage{WorktreeRoot: "$HOME/wx", CopyMode: CopyModeAuto, RepoDirSource: RepoDirSourceRemote, BackupGenerations: 3, BackupRetention: Duration{168 * time.Hour}},
		Pool:      Pool{WarmPerWorkspace: 1, PreparationConcurrency: 2},
		Retention: Retention{Duration{168 * time.Hour}, Duration{time.Hour}, Duration{24 * time.Hour}, Duration{720 * time.Hour}, Duration{8760 * time.Hour}, Duration{168 * time.Hour}, Duration{168 * time.Hour}},
		Discovery: Discovery{MaxDepth: 6, MaxEntries: 100000, Timeout: Duration{30 * time.Second}, ReconcileInterval: Duration{10 * time.Minute}, Exclude: []string{"node_modules", "vendor", ".venv", "venv", "tmp", "log"}},
		Readiness: Readiness{Mode: "early", Timeout: Duration{10 * time.Minute}}, Resume: Resume{AutoFresh: false},
		Lease:    Lease{TTL: Duration{72 * time.Hour}},
		Includes: Includes{DefaultAgentRules: true}, Logging: Logging{Level: "info"},
		Sessions:   sessionsconfig.Defaults(),
		Workspaces: map[string]Workspace{}, Repositories: map[string]Repository{},
	}
}

// DefaultAgentRulesEnabled は repository へ既定の agent rule をコピーするか解決する。
// 個別指定が global 設定より優先される。
func (c Config) DefaultAgentRulesEnabled(mainPath string) bool {
	if override, ok := c.Repositories[mainPath]; ok && override.Includes.DefaultAgentRules != nil {
		return *override.Includes.DefaultAgentRules
	}
	return c.Includes.DefaultAgentRules
}

// EffectiveEqual は正規化・検証を終えた実効設定として2つのConfigが同じ値かを返す。
// どのキーがファイルに書かれていたかの記録は実効値に影響しないため比較から外し、
// 書式だけが変わった設定ファイルを設定変更として扱わない。
func (c Config) EffectiveEqual(other Config) bool {
	c.present = nil
	other.present = nil
	return reflect.DeepEqual(c, other)
}

func Merge(d, raw Config) Config {
	r := d
	if raw.has("version", raw.Version != 0) {
		r.Version = raw.Version
	}
	rawValue := reflect.ValueOf(raw)
	resultValue := reflect.ValueOf(&r).Elem()
	walkConfigLeaves(rawValue, "", func(key string, rawField reflect.Value) {
		nonZero := !rawField.IsZero()
		if !raw.has(key, nonZero) {
			return
		}
		configField(resultValue, key).Set(rawField)
	})
	walkConfigLists(rawValue, "", func(key string, rawField reflect.Value) {
		if rawField.IsNil() {
			return
		}
		configListField(resultValue, key).Set(rawField)
	})
	if raw.has("workspaces", raw.Workspaces != nil) {
		r.Workspaces = raw.Workspaces
	}
	if raw.has("repositories", raw.Repositories != nil) {
		r.Repositories = raw.Repositories
	}
	return r
}

func Validate(c *Config) error {
	if err := validateReadiness(&c.Readiness); err != nil {
		return err
	}
	if !validWorktreeMode(c.Worktree.Undefined, true) {
		return errors.New("worktree.undefined must be ask, hot, cold, or off")
	}
	for path, workspace := range c.Workspaces {
		if workspace.Worktree != "" && !validWorktreeMode(workspace.Worktree, false) {
			return fmt.Errorf("workspaces.%s.worktree must be hot, cold, or off", path)
		}
	}
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if _, err := ExpandHome(c.Storage.WorktreeRoot); err != nil {
		return fmt.Errorf("storage.worktree_root: %w", err)
	}
	if c.Storage.CopyMode != CopyModeAuto && c.Storage.CopyMode != CopyModeCOW && c.Storage.CopyMode != CopyModeCopy {
		return errors.New("storage.copy_mode must be auto, cow, or copy")
	}
	if c.Storage.BackupGenerations < 1 || c.Storage.BackupRetention.Duration < 0 {
		return errors.New("storage backup_generations must be positive and backup_retention must not be negative")
	}
	if c.Storage.RepoDirSource != RepoDirSourceRemote && c.Storage.RepoDirSource != RepoDirSourceDirectory {
		return fmt.Errorf("storage.repo_dir_source must be %s or %s", RepoDirSourceRemote, RepoDirSourceDirectory)
	}
	for path, override := range c.Repositories {
		if override.DirSource != "" && override.DirSource != RepoDirSourceRemote && override.DirSource != RepoDirSourceDirectory {
			return fmt.Errorf("repositories.%s.dir_source must be %s or %s", path, RepoDirSourceRemote, RepoDirSourceDirectory)
		}
		if override.Prepare.Timeout.Duration < 0 {
			return fmt.Errorf("repositories.%s.prepare.timeout must not be negative", path)
		}
	}
	if c.Pool.WarmPerWorkspace < 0 || c.Pool.PreparationConcurrency < 1 {
		return errors.New("pool counts must be non-negative and concurrency must be at least 1")
	}
	for k, v := range map[string]time.Duration{"retention.hot_standby": c.Retention.HotStandby.Duration, "retention.ended_worktree": c.Retention.EndedWorktree.Duration, "retention.quarantined": c.Retention.Quarantined.Duration, "retention.recovery_snapshot": c.Retention.RecoverySnapshot.Duration, "retention.expired_session_tombstone": c.Retention.ExpiredSessionTombstone.Duration, "retention.failed_job": c.Retention.FailedJob.Duration, "retention.event_log": c.Retention.EventLog.Duration, "discovery.timeout": c.Discovery.Timeout.Duration, "discovery.reconcile_interval": c.Discovery.ReconcileInterval.Duration, "readiness.timeout": c.Readiness.Timeout.Duration, "lease.ttl": c.Lease.TTL.Duration} {
		if v < 0 {
			return fmt.Errorf("%s must not be negative", k)
		}
	}
	for k, v := range map[string]time.Duration{"discovery.timeout": c.Discovery.Timeout.Duration, "readiness.timeout": c.Readiness.Timeout.Duration} {
		if v <= 0 {
			return fmt.Errorf("%s must be positive", k)
		}
	}
	if c.Discovery.MaxDepth < 1 || c.Discovery.MaxEntries < 1 {
		return errors.New("discovery limits must be positive")
	}
	if c.Logging.Level != "debug" && c.Logging.Level != "info" && c.Logging.Level != "warn" && c.Logging.Level != "error" {
		return errors.New("logging.level must be debug, info, warn, or error")
	}
	if err := c.Sessions.Validate(); err != nil {
		return err
	}
	return nil
}

func validWorktreeMode(mode string, allowAsk bool) bool {
	return mode == "hot" || mode == "cold" || mode == "off" || (allowAsk && mode == "ask")
}

// WorktreeMode は正規化済み workspace root の個別設定を優先し、未定義なら全体の方針を返す。
func (c Config) WorktreeMode(root string) string {
	if mode := c.Workspaces[root].Worktree; mode != "" {
		return mode
	}
	return c.Worktree.Undefined
}
