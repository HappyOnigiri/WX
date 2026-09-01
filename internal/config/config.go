package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
	Version      int                   `yaml:"version,omitempty"`
	Storage      Storage               `yaml:"storage,omitempty"`
	Pool         Pool                  `yaml:"pool,omitempty"`
	Retention    Retention             `yaml:"retention,omitempty"`
	Discovery    Discovery             `yaml:"discovery,omitempty"`
	Readiness    Readiness             `yaml:"readiness,omitempty"`
	Workspaces   map[string]Workspace  `yaml:"workspaces,omitempty"`
	Repositories map[string]Repository `yaml:"repositories,omitempty"`
	Logging      Logging               `yaml:"logging,omitempty"`
	present      map[string]bool
}
type Storage struct {
	WorktreeRoot string `yaml:"worktree_root,omitempty"`
}
type Pool struct {
	WarmPerWorkspace            int `yaml:"warm_per_workspace,omitempty"`
	PreparationConcurrency      int `yaml:"preparation_concurrency,omitempty"`
	GitConcurrencyPerRepository int `yaml:"git_concurrency_per_repository,omitempty"`
}
type Retention struct {
	HotStandby              Duration `yaml:"hot_standby,omitempty"`
	EndedWorktree           Duration `yaml:"ended_worktree,omitempty"`
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
	Timeout Duration `yaml:"timeout,omitempty"`
}
type Workspace struct {
	Copy []string `yaml:"copy,omitempty"`
	Link []string `yaml:"link,omitempty"`
}
type Repository struct {
	DefaultBranch string  `yaml:"default_branch,omitempty"`
	Prepare       Prepare `yaml:"prepare,omitempty"`
}
type Prepare struct {
	Command []string `yaml:"command,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty"`
	Version string   `yaml:"version,omitempty"`
}
type Logging struct {
	Level string `yaml:"level,omitempty"`
}

type Field struct{ Key, Value string }

func Defaults() Config {
	return Config{
		Version: 1, Storage: Storage{WorktreeRoot: "$HOME/dev/worktrees/wx"},
		Pool:      Pool{WarmPerWorkspace: 1, PreparationConcurrency: 2, GitConcurrencyPerRepository: 1},
		Retention: Retention{Duration{168 * time.Hour}, Duration{time.Hour}, Duration{720 * time.Hour}, Duration{8760 * time.Hour}, Duration{168 * time.Hour}, Duration{168 * time.Hour}},
		Discovery: Discovery{MaxDepth: 6, MaxEntries: 100000, Timeout: Duration{30 * time.Second}, ReconcileInterval: Duration{10 * time.Minute}, Exclude: []string{"node_modules", "vendor", ".venv", "venv", "tmp", "log"}},
		Readiness: Readiness{Timeout: Duration{10 * time.Minute}}, Logging: Logging{Level: "info"},
		Workspaces: map[string]Workspace{}, Repositories: map[string]Repository{},
	}
}

func Path() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".config", "wx", "config.yaml"), nil
}

func StatePath() (string, error) {
	h, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(h, "Library", "Application Support", "wx", "state.db"), nil
}
func SocketPath() (string, error) {
	h, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(h, "Library", "Application Support", "wx", "run", "wxd.sock"), nil
}
func LogPath() (string, error) {
	h, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(h, "Library", "Logs", "wx", "wxd.log"), nil
}

func Load() (Config, error) {
	raw, err := LoadRaw()
	if err != nil {
		return Config{}, err
	}
	effective := Merge(Defaults(), raw)
	if err := Validate(&effective); err != nil {
		return Config{}, err
	}
	return effective, nil
}

func LoadRaw() (Config, error) {
	p, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", p, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("config contains multiple YAML documents")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Config{}, err
	}
	c.present = collectKeys(&doc)
	return c, nil
}

func collectKeys(doc *yaml.Node) map[string]bool {
	out := map[string]bool{}
	if len(doc.Content) == 0 {
		return out
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i].Value, root.Content[i+1]
		out[key] = true
		if value.Kind == yaml.MappingNode && key != "workspaces" && key != "repositories" {
			for j := 0; j+1 < len(value.Content); j += 2 {
				out[key+"."+value.Content[j].Value] = true
			}
		}
	}
	return out
}

func (c Config) has(key string, fallback bool) bool {
	if c.present != nil {
		return c.present[key]
	}
	return fallback
}

func Merge(d, raw Config) Config {
	r := d
	if raw.has("version", raw.Version != 0) {
		r.Version = raw.Version
	}
	if raw.has("storage.worktree_root", raw.Storage.WorktreeRoot != "") {
		r.Storage = raw.Storage
	}
	if raw.has("pool.warm_per_workspace", raw.Pool.WarmPerWorkspace != 0) {
		r.Pool.WarmPerWorkspace = raw.Pool.WarmPerWorkspace
	}
	if raw.has("pool.preparation_concurrency", raw.Pool.PreparationConcurrency != 0) {
		r.Pool.PreparationConcurrency = raw.Pool.PreparationConcurrency
	}
	if raw.has("pool.git_concurrency_per_repository", raw.Pool.GitConcurrencyPerRepository != 0) {
		r.Pool.GitConcurrencyPerRepository = raw.Pool.GitConcurrencyPerRepository
	}
	mergeDur := func(key string, dst *Duration, src Duration) {
		if raw.has(key, src.Duration != 0) {
			*dst = src
		}
	}
	mergeDur("retention.hot_standby", &r.Retention.HotStandby, raw.Retention.HotStandby)
	mergeDur("retention.ended_worktree", &r.Retention.EndedWorktree, raw.Retention.EndedWorktree)
	mergeDur("retention.recovery_snapshot", &r.Retention.RecoverySnapshot, raw.Retention.RecoverySnapshot)
	mergeDur("retention.expired_session_tombstone", &r.Retention.ExpiredSessionTombstone, raw.Retention.ExpiredSessionTombstone)
	mergeDur("retention.failed_job", &r.Retention.FailedJob, raw.Retention.FailedJob)
	mergeDur("retention.event_log", &r.Retention.EventLog, raw.Retention.EventLog)
	if raw.has("discovery.max_depth", raw.Discovery.MaxDepth != 0) {
		r.Discovery.MaxDepth = raw.Discovery.MaxDepth
	}
	if raw.has("discovery.max_entries", raw.Discovery.MaxEntries != 0) {
		r.Discovery.MaxEntries = raw.Discovery.MaxEntries
	}
	mergeDur("discovery.timeout", &r.Discovery.Timeout, raw.Discovery.Timeout)
	mergeDur("discovery.reconcile_interval", &r.Discovery.ReconcileInterval, raw.Discovery.ReconcileInterval)
	if raw.has("discovery.exclude", raw.Discovery.Exclude != nil) {
		r.Discovery.Exclude = raw.Discovery.Exclude
	}
	mergeDur("readiness.timeout", &r.Readiness.Timeout, raw.Readiness.Timeout)
	if raw.has("workspaces", raw.Workspaces != nil) {
		r.Workspaces = raw.Workspaces
	}
	if raw.has("repositories", raw.Repositories != nil) {
		r.Repositories = raw.Repositories
	}
	if raw.has("logging.level", raw.Logging.Level != "") {
		r.Logging = raw.Logging
	}
	return r
}

func ExpandHome(path string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if strings.Contains(path, "~") {
		return "", errors.New("~ is not expanded; use $HOME")
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

func Validate(c *Config) error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if _, err := ExpandHome(c.Storage.WorktreeRoot); err != nil {
		return fmt.Errorf("storage.worktree_root: %w", err)
	}
	if c.Pool.WarmPerWorkspace < 0 || c.Pool.PreparationConcurrency < 1 || c.Pool.GitConcurrencyPerRepository < 1 {
		return errors.New("pool counts must be non-negative and concurrency must be at least 1")
	}
	for k, v := range map[string]time.Duration{"retention.hot_standby": c.Retention.HotStandby.Duration, "retention.ended_worktree": c.Retention.EndedWorktree.Duration, "retention.recovery_snapshot": c.Retention.RecoverySnapshot.Duration, "retention.expired_session_tombstone": c.Retention.ExpiredSessionTombstone.Duration, "retention.failed_job": c.Retention.FailedJob.Duration, "retention.event_log": c.Retention.EventLog.Duration, "discovery.timeout": c.Discovery.Timeout.Duration, "discovery.reconcile_interval": c.Discovery.ReconcileInterval.Duration, "readiness.timeout": c.Readiness.Timeout.Duration} {
		if v < 0 {
			return fmt.Errorf("%s must not be negative", k)
		}
	}
	if c.Discovery.MaxDepth < 1 || c.Discovery.MaxEntries < 1 {
		return errors.New("discovery limits must be positive")
	}
	if c.Logging.Level != "debug" && c.Logging.Level != "info" && c.Logging.Level != "warn" && c.Logging.Level != "error" {
		return errors.New("logging.level must be debug, info, warn, or error")
	}
	return nil
}

func Save(c Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, p); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(p))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func (c Config) MarshalYAML() (any, error) {
	out := map[string]any{}
	put := func(section, key string, value any) {
		if !c.has(section+"."+key, false) {
			return
		}
		m, _ := out[section].(map[string]any)
		if m == nil {
			m = map[string]any{}
			out[section] = m
		}
		m[key] = value
	}
	if c.has("version", false) {
		out["version"] = c.Version
	}
	put("storage", "worktree_root", c.Storage.WorktreeRoot)
	put("pool", "warm_per_workspace", c.Pool.WarmPerWorkspace)
	put("pool", "preparation_concurrency", c.Pool.PreparationConcurrency)
	put("pool", "git_concurrency_per_repository", c.Pool.GitConcurrencyPerRepository)
	put("retention", "hot_standby", c.Retention.HotStandby.String())
	put("retention", "ended_worktree", c.Retention.EndedWorktree.String())
	put("retention", "recovery_snapshot", c.Retention.RecoverySnapshot.String())
	put("retention", "expired_session_tombstone", c.Retention.ExpiredSessionTombstone.String())
	put("retention", "failed_job", c.Retention.FailedJob.String())
	put("retention", "event_log", c.Retention.EventLog.String())
	put("discovery", "max_depth", c.Discovery.MaxDepth)
	put("discovery", "max_entries", c.Discovery.MaxEntries)
	put("discovery", "timeout", c.Discovery.Timeout.String())
	put("discovery", "reconcile_interval", c.Discovery.ReconcileInterval.String())
	put("discovery", "exclude", c.Discovery.Exclude)
	put("readiness", "timeout", c.Readiness.Timeout.String())
	put("logging", "level", c.Logging.Level)
	if c.has("workspaces", c.Workspaces != nil) {
		out["workspaces"] = c.Workspaces
	}
	if c.has("repositories", c.Repositories != nil) {
		out["repositories"] = c.Repositories
	}
	return out, nil
}

func Fields(c Config) []Field {
	return []Field{{"storage.worktree_root", c.Storage.WorktreeRoot}, {"pool.warm_per_workspace", strconv.Itoa(c.Pool.WarmPerWorkspace)}, {"pool.preparation_concurrency", strconv.Itoa(c.Pool.PreparationConcurrency)}, {"pool.git_concurrency_per_repository", strconv.Itoa(c.Pool.GitConcurrencyPerRepository)}, {"retention.hot_standby", c.Retention.HotStandby.String()}, {"retention.ended_worktree", c.Retention.EndedWorktree.String()}, {"retention.recovery_snapshot", c.Retention.RecoverySnapshot.String()}, {"retention.expired_session_tombstone", c.Retention.ExpiredSessionTombstone.String()}, {"retention.failed_job", c.Retention.FailedJob.String()}, {"retention.event_log", c.Retention.EventLog.String()}, {"discovery.max_depth", strconv.Itoa(c.Discovery.MaxDepth)}, {"discovery.max_entries", strconv.Itoa(c.Discovery.MaxEntries)}, {"discovery.timeout", c.Discovery.Timeout.String()}, {"discovery.reconcile_interval", c.Discovery.ReconcileInterval.String()}, {"readiness.timeout", c.Readiness.Timeout.String()}, {"logging.level", c.Logging.Level}}
}

func SetField(c *Config, key, value string) error {
	n := func() (int, error) { return strconv.Atoi(value) }
	d := func() (Duration, error) { v, e := time.ParseDuration(value); return Duration{v}, e }
	switch key {
	case "storage.worktree_root":
		c.Storage.WorktreeRoot = value
	case "pool.warm_per_workspace":
		v, e := n()
		if e != nil {
			return e
		}
		c.Pool.WarmPerWorkspace = v
	case "pool.preparation_concurrency":
		v, e := n()
		if e != nil {
			return e
		}
		c.Pool.PreparationConcurrency = v
	case "pool.git_concurrency_per_repository":
		v, e := n()
		if e != nil {
			return e
		}
		c.Pool.GitConcurrencyPerRepository = v
	case "retention.hot_standby":
		v, e := d()
		if e != nil {
			return e
		}
		c.Retention.HotStandby = v
	case "retention.ended_worktree":
		v, e := d()
		if e != nil {
			return e
		}
		c.Retention.EndedWorktree = v
	case "retention.recovery_snapshot":
		v, e := d()
		if e != nil {
			return e
		}
		c.Retention.RecoverySnapshot = v
	case "retention.expired_session_tombstone":
		v, e := d()
		if e != nil {
			return e
		}
		c.Retention.ExpiredSessionTombstone = v
	case "retention.failed_job":
		v, e := d()
		if e != nil {
			return e
		}
		c.Retention.FailedJob = v
	case "retention.event_log":
		v, e := d()
		if e != nil {
			return e
		}
		c.Retention.EventLog = v
	case "discovery.max_depth":
		v, e := n()
		if e != nil {
			return e
		}
		c.Discovery.MaxDepth = v
	case "discovery.max_entries":
		v, e := n()
		if e != nil {
			return e
		}
		c.Discovery.MaxEntries = v
	case "discovery.timeout":
		v, e := d()
		if e != nil {
			return e
		}
		c.Discovery.Timeout = v
	case "discovery.reconcile_interval":
		v, e := d()
		if e != nil {
			return e
		}
		c.Discovery.ReconcileInterval = v
	case "readiness.timeout":
		v, e := d()
		if e != nil {
			return e
		}
		c.Readiness.Timeout = v
	case "logging.level":
		c.Logging.Level = value
	default:
		return fmt.Errorf("unknown config key %q; run wx config to list available keys", key)
	}
	if c.present == nil {
		c.present = map[string]bool{}
	}
	c.present[key] = true
	return nil
}
