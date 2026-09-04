package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	// RepoDirSource selects how a repository's directory name inside a slot
	// is derived: "remote" uses the origin URL's basename, "directory" uses
	// the main worktree's own directory name. Per-repository overrides in
	// Repositories take precedence.
	RepoDirSource     string   `yaml:"repo_dir_source,omitempty"`
	BackupGenerations int      `yaml:"backup_generations,omitempty"`
	BackupRetention   Duration `yaml:"backup_retention,omitempty"`
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
	DefaultBranch string `yaml:"default_branch,omitempty"`
	// DirName pins the repository's directory name inside a slot. DirSource
	// ("remote" or "directory") selects the derivation when DirName is empty.
	// Both live outside the scalar key space walked by walkConfigLeaves
	// (Repositories is a map), so they are set by editing the YAML directly,
	// like default_branch and prepare.
	DirName   string  `yaml:"dir_name,omitempty"`
	DirSource string  `yaml:"dir_source,omitempty"`
	Prepare   Prepare `yaml:"prepare,omitempty"`
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

// RepoDirSourceRemote and RepoDirSourceDirectory are the two accepted values
// of storage.repo_dir_source and repositories.<path>.dir_source.
const (
	RepoDirSourceRemote    = "remote"
	RepoDirSourceDirectory = "directory"
)

func Defaults() Config {
	return Config{
		Version: 1, Storage: Storage{WorktreeRoot: "$HOME/wx", RepoDirSource: RepoDirSourceRemote, BackupGenerations: 3, BackupRetention: Duration{168 * time.Hour}},
		Pool:      Pool{WarmPerWorkspace: 1, PreparationConcurrency: 2, GitConcurrencyPerRepository: 1},
		Retention: Retention{Duration{168 * time.Hour}, Duration{time.Hour}, Duration{720 * time.Hour}, Duration{8760 * time.Hour}, Duration{168 * time.Hour}, Duration{168 * time.Hour}},
		Discovery: Discovery{MaxDepth: 6, MaxEntries: 100000, Timeout: Duration{30 * time.Second}, ReconcileInterval: Duration{10 * time.Minute}, Exclude: []string{"node_modules", "vendor", ".venv", "venv", "tmp", "log"}},
		Readiness: Readiness{Timeout: Duration{10 * time.Minute}}, Logging: Logging{Level: "info"},
		Workspaces: map[string]Workspace{}, Repositories: map[string]Repository{},
	}
}

// homePath joins parts onto the user's home directory, failing closed when
// the home directory cannot be determined.
func homePath(parts ...string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{h}, parts...)...), nil
}

func Path() (string, error) {
	return homePath(".config", "wx", "config.yaml")
}

func StatePath() (string, error) {
	return homePath("Library", "Application Support", "wx", "state.db")
}

func SocketPath() (string, error) {
	return homePath("Library", "Application Support", "wx", "run", "wxd.sock")
}

func LogPath() (string, error) {
	return homePath("Library", "Logs", "wx", "wxd.log")
}

func Load() (Config, error) {
	raw, err := LoadRaw()
	if err != nil {
		return Config{}, err
	}
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		return Config{}, err
	}
	if err := Validate(&effective); err != nil {
		return Config{}, err
	}
	return effective, nil
}

func NormalizePaths(c *Config) error {
	root, err := canonicalPath(c.Storage.WorktreeRoot)
	if err != nil {
		return fmt.Errorf("storage.worktree_root: %w", err)
	}
	c.Storage.WorktreeRoot = root
	workspaces := make(map[string]Workspace, len(c.Workspaces))
	for path, override := range c.Workspaces {
		canonical, err := canonicalPath(path)
		if err != nil {
			return fmt.Errorf("workspace override %q: %w", path, err)
		}
		if _, exists := workspaces[canonical]; exists {
			return fmt.Errorf("workspace overrides collide at canonical path %s", canonical)
		}
		workspaces[canonical] = override
	}
	c.Workspaces = workspaces
	repositories := make(map[string]Repository, len(c.Repositories))
	for path, override := range c.Repositories {
		canonical, err := canonicalPath(path)
		if err != nil {
			return fmt.Errorf("repository override %q: %w", path, err)
		}
		if _, exists := repositories[canonical]; exists {
			return fmt.Errorf("repository overrides collide at canonical path %s", canonical)
		}
		repositories[canonical] = override
	}
	c.Repositories = repositories
	return nil
}

func canonicalPath(path string) (string, error) {
	expanded, err := ExpandHome(path)
	if err != nil {
		return "", err
	}
	current := expanded
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
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
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
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

// durationType is the reflect.Type of Duration, used to recognise Duration
// leaves during struct walks (they are structs, but scalar configuration
// leaves rather than nested sections).
var durationType = reflect.TypeOf(Duration{})

// walkConfigLeaves visits every scalar (string, int, or Duration) field
// reachable from v, in declaration order, along with its dotted yaml key.
// It skips unexported fields, "version" (handled separately by every
// caller), and any map or slice field (workspaces, repositories, and
// discovery.exclude), which are not part of the uniform scalar key space
// shared by Fields, SetField, Merge, and MarshalYAML.
func walkConfigLeaves(v reflect.Value, prefix string, visit func(key string, field reflect.Value)) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		tag, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		fv := v.Field(i)
		switch {
		case fv.Type() == durationType:
			visit(key, fv)
		case fv.Kind() == reflect.Struct:
			walkConfigLeaves(fv, key, visit)
		case key == "version" || fv.Kind() == reflect.Map || fv.Kind() == reflect.Slice:
			continue
		default:
			visit(key, fv)
		}
	}
}

// configField locates the scalar field addressed by key within v, which
// must be the same shape walkConfigLeaves would traverse. It returns the
// zero Value if key does not address a scalar leaf.
func configField(v reflect.Value, key string) reflect.Value {
	var target reflect.Value
	walkConfigLeaves(v, "", func(k string, fv reflect.Value) {
		if k == key {
			target = fv
		}
	})
	return target
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
	if raw.has("discovery.exclude", raw.Discovery.Exclude != nil) {
		r.Discovery.Exclude = raw.Discovery.Exclude
	}
	if raw.has("workspaces", raw.Workspaces != nil) {
		r.Workspaces = raw.Workspaces
	}
	if raw.has("repositories", raw.Repositories != nil) {
		r.Repositories = raw.Repositories
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
	}
	if c.Pool.WarmPerWorkspace < 0 || c.Pool.PreparationConcurrency < 1 || c.Pool.GitConcurrencyPerRepository < 1 {
		return errors.New("pool counts must be non-negative and concurrency must be at least 1")
	}
	for k, v := range map[string]time.Duration{"retention.hot_standby": c.Retention.HotStandby.Duration, "retention.ended_worktree": c.Retention.EndedWorktree.Duration, "retention.recovery_snapshot": c.Retention.RecoverySnapshot.Duration, "retention.expired_session_tombstone": c.Retention.ExpiredSessionTombstone.Duration, "retention.failed_job": c.Retention.FailedJob.Duration, "retention.event_log": c.Retention.EventLog.Duration, "discovery.timeout": c.Discovery.Timeout.Duration, "discovery.reconcile_interval": c.Discovery.ReconcileInterval.Duration, "readiness.timeout": c.Readiness.Timeout.Duration} {
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
	return nil
}

func Save(c Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Lstat(p); statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("config path is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
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

// leafInterface returns the value held by a scalar config field as the
// concrete type yaml.Marshal should encode it with. Duration already
// implements yaml.Marshaler (as its string form), so it can be boxed as-is.
func leafInterface(fv reflect.Value) any {
	return fv.Interface()
}

func (c Config) MarshalYAML() (any, error) {
	out := map[string]any{}
	if c.has("version", false) {
		out["version"] = c.Version
	}
	walkConfigLeaves(reflect.ValueOf(c), "", func(key string, fv reflect.Value) {
		if !c.has(key, false) {
			return
		}
		section, name, _ := strings.Cut(key, ".")
		m, _ := out[section].(map[string]any)
		if m == nil {
			m = map[string]any{}
			out[section] = m
		}
		m[name] = leafInterface(fv)
	})
	if c.has("discovery.exclude", false) {
		m, _ := out["discovery"].(map[string]any)
		if m == nil {
			m = map[string]any{}
			out["discovery"] = m
		}
		m["exclude"] = c.Discovery.Exclude
	}
	if c.has("workspaces", c.Workspaces != nil) {
		out["workspaces"] = c.Workspaces
	}
	if c.has("repositories", c.Repositories != nil) {
		out["repositories"] = c.Repositories
	}
	return out, nil
}

// Fields lists every user-settable scalar configuration key with its
// current effective value, in the same order SetField accepts them.
func Fields(c Config) []Field {
	var fields []Field
	walkConfigLeaves(reflect.ValueOf(c), "", func(key string, fv reflect.Value) {
		var value string
		switch {
		case fv.Type() == durationType:
			value = fv.Interface().(Duration).String()
		case fv.Kind() == reflect.String:
			value = fv.String()
		default:
			value = strconv.FormatInt(fv.Int(), 10)
		}
		fields = append(fields, Field{key, value})
	})
	return fields
}

// SetField parses value for the type of the scalar field addressed by key
// (a duration, an integer, or a plain string) and assigns it. Range and
// enum validity for every key is enforced later by Validate; SetField only
// performs the type-level parsing needed to store the value at all.
func SetField(c *Config, key, value string) error {
	field := configField(reflect.ValueOf(c).Elem(), key)
	if !field.IsValid() {
		return fmt.Errorf("unknown config key %q; run wx config to list available keys", key)
	}
	switch {
	case field.Type() == durationType:
		d, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(Duration{d}))
	case field.Kind() == reflect.String:
		field.SetString(value)
	default:
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		field.SetInt(int64(n))
	}
	if c.present == nil {
		c.present = map[string]bool{}
	}
	c.present[key] = true
	return nil
}
