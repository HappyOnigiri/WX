package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDefaultsAndZeroDurationOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	raw := Config{}
	if err := SetField(&raw, "retention.hot_standby", "0s"); err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "hot_standby: 0s") || strings.Contains(got, "worktree_root") {
		t.Fatalf("unexpected sparse config:\n%s", got)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention.HotStandby.Duration != 0 {
		t.Fatalf("hot standby = %s", cfg.Retention.HotStandby)
	}
	if cfg.Retention.RecoverySnapshot.Duration != 720*time.Hour {
		t.Fatal("default was not merged")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestDefaultAgentRulesResolution(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	cfg := Defaults()
	if !cfg.DefaultAgentRulesEnabled(repository) {
		t.Fatal("default agent rules are disabled by default")
	}
	cfg.Repositories[repository] = Repository{}
	if !cfg.DefaultAgentRulesEnabled(repository) {
		t.Fatal("nil repository override did not fall back to the global setting")
	}
	cfg.Includes.DefaultAgentRules = false
	if cfg.DefaultAgentRulesEnabled(repository) {
		t.Fatal("nil repository override ignored the disabled global setting")
	}
	enabled := true
	cfg.Repositories[repository] = Repository{Includes: RepositoryIncludes{DefaultAgentRules: &enabled}}
	if !cfg.DefaultAgentRulesEnabled(repository) {
		t.Fatal("explicit repository enable was ignored")
	}
	disabled := false
	cfg.Repositories[repository] = Repository{Includes: RepositoryIncludes{DefaultAgentRules: &disabled}}
	if cfg.DefaultAgentRulesEnabled(repository) {
		t.Fatal("explicit repository disable was ignored")
	}
	delete(cfg.Repositories, repository)
	if cfg.DefaultAgentRulesEnabled(repository) {
		t.Fatal("missing repository override did not use the disabled global setting")
	}
}

func TestDefaultAgentRulesOverridesLoadFromYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repository := filepath.Join(home, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := `version: 1
includes:
  default_agent_rules: false
repositories:
  $HOME/repository:
    includes:
      default_agent_rules: true
`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Includes.DefaultAgentRules {
		t.Fatal("global default agent rules override was not loaded")
	}
	override, ok := cfg.Repositories[repository]
	if !ok || override.Includes.DefaultAgentRules == nil || !*override.Includes.DefaultAgentRules {
		t.Fatalf("repository override=%+v, want normalized explicit true", override)
	}
	if !cfg.DefaultAgentRulesEnabled(repository) {
		t.Fatal("loaded repository override was not applied")
	}
}

func TestValidateRejectsEachPolicyClass(t *testing.T) {
	valid := Defaults()
	valid.Storage.WorktreeRoot = "/tmp/wx"
	tests := []Config{
		func() Config { c := valid; c.Version = 2; return c }(),
		func() Config { c := valid; c.Storage.WorktreeRoot = "relative"; return c }(),
		func() Config { c := valid; c.Storage.BackupGenerations = 0; return c }(),
		func() Config { c := valid; c.Pool.WarmPerWorkspace = -1; return c }(),
		func() Config { c := valid; c.Retention.HotStandby.Duration = -1; return c }(),
		func() Config {
			c := valid
			c.Repositories = map[string]Repository{"/tmp/repository": {Prepare: Prepare{Timeout: Duration{Duration: -1}}}}
			return c
		}(),
		func() Config { c := valid; c.Discovery.Timeout.Duration = 0; return c }(),
		func() Config { c := valid; c.Readiness.Timeout.Duration = 0; return c }(),
		func() Config { c := valid; c.Discovery.MaxDepth = 0; return c }(),
		func() Config { c := valid; c.Logging.Level = "verbose"; return c }(),
	}
	for i := range tests {
		if err := Validate(&tests[i]); err == nil {
			t.Errorf("invalid policy case %d succeeded", i)
		}
	}
}

func TestSparseCollectionsAndConfigFilesystemFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := `version: 1
discovery:
  exclude: []
workspaces:
  $HOME/workspace:
    copy: [AGENTS.md]
repositories:
  $HOME/repository:
    default_branch: trunk
`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	effective := Merge(Defaults(), raw)
	if len(effective.Discovery.Exclude) != 0 || len(effective.Workspaces) != 1 || len(effective.Repositories) != 1 {
		t.Fatalf("merged sparse collections=%+v", effective)
	}
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"version: 1", "exclude: []", "workspaces:", "repositories:"} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("sparse YAML missing %q:\n%s", expected, data)
		}
	}

	loop := filepath.Join(home, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	bad := Defaults()
	bad.Storage.WorktreeRoot = loop
	if err := NormalizePaths(&bad); err == nil {
		t.Fatal("symlink loop normalized")
	}
	bad = Defaults()
	bad.Storage.WorktreeRoot = home
	bad.Workspaces = map[string]Workspace{"relative": {}}
	if err := NormalizePaths(&bad); err == nil || !strings.Contains(err.Error(), "workspace override") {
		t.Fatalf("relative workspace override error=%v", err)
	}
	bad = Defaults()
	bad.Storage.WorktreeRoot = home
	bad.Repositories = map[string]Repository{"relative": {}}
	if err := NormalizePaths(&bad); err == nil || !strings.Contains(err.Error(), "repository override") {
		t.Fatalf("relative repository override error=%v", err)
	}

	blockingHome := filepath.Join(home, "not-a-directory")
	if err := os.WriteFile(blockingHome, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blockingHome)
	var update Config
	if err := SetField(&update, "logging.level", "debug"); err != nil {
		t.Fatal(err)
	}
	if err := Save(update); err == nil {
		t.Fatal("config save through regular-file HOME succeeded")
	}
}

func TestConfigPathShapeAndWorkspaceCollisionFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRaw(); err == nil {
		t.Fatal("config directory was read as a regular config file")
	}
	if err := Save(Config{}); err == nil {
		t.Fatal("config directory was replaced by a config file")
	}

	real := filepath.Join(home, "real-workspace")
	alias := filepath.Join(home, "workspace-alias")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Storage.WorktreeRoot = home
	cfg.Workspaces = map[string]Workspace{real: {}, alias: {}}
	if err := NormalizePaths(&cfg); err == nil || !strings.Contains(err.Error(), "workspace overrides collide") {
		t.Fatalf("workspace collision error=%v", err)
	}
}

func TestCopyModeConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got, err := Load(); err != nil || got.Storage.CopyMode != CopyModeAuto {
		t.Fatalf("default=%q err=%v", got.Storage.CopyMode, err)
	}
	for _, mode := range []string{CopyModeAuto, CopyModeCOW, CopyModeCopy} {
		raw := Config{}
		if err := SetField(&raw, "storage.copy_mode", mode); err != nil {
			t.Fatal(err)
		}
		if err := Save(raw); err != nil {
			t.Fatal(err)
		}
		got, err := Load()
		if err != nil || got.Storage.CopyMode != mode {
			t.Fatalf("mode=%q got=%q err=%v", mode, got.Storage.CopyMode, err)
		}
	}
	for _, mode := range []string{"", "COW", "invalid"} {
		cfg := Defaults()
		cfg.Storage.CopyMode = mode
		if err := Validate(&cfg); err == nil {
			t.Fatalf("invalid mode accepted: %q", mode)
		}
	}
}
