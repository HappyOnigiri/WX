package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func FuzzDurationYAML(f *testing.F) {
	f.Add("10m")
	f.Add("-1s")
	f.Add("not-a-duration")
	f.Fuzz(func(t *testing.T, value string) {
		data, err := yaml.Marshal(map[string]string{"timeout": value})
		if err != nil {
			return
		}
		var decoded struct {
			Timeout Duration `yaml:"timeout"`
		}
		_ = yaml.Unmarshal(data, &decoded)
	})
}

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

func TestSavePreservesExistingPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("mode=%v err=%v", info, err)
	}
}

func TestSaveRejectsNonRegularConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Version: 1}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Save error=%v", err)
	}
}

func TestStrictDecode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pool:\n  unknown: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("Load error=%v", err)
	}
}

func TestLoadRawRejectsMultipleYAMLDocuments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\n---\nversion: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRaw(); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("multiple YAML documents error=%v", err)
	}
}

func TestExpandHomeRejectsImplicitExpansion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, path := range []string{"~/worktrees", "$TMPDIR/worktrees", "relative"} {
		if _, err := ExpandHome(path); err == nil {
			t.Errorf("ExpandHome(%q) succeeded", path)
		}
	}
	if got, err := ExpandHome("$HOME/worktrees"); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestAllScalarFieldsCanBeSetAndReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	values := map[string]string{
		"storage.worktree_root": "$HOME/wx", "storage.backup_generations": "4", "storage.backup_retention": "24h",
		"pool.warm_per_workspace": "2", "pool.preparation_concurrency": "3", "pool.git_concurrency_per_repository": "2",
		"retention.hot_standby": "1h", "retention.ended_worktree": "2h", "retention.recovery_snapshot": "3h", "retention.expired_session_tombstone": "4h", "retention.failed_job": "5h", "retention.event_log": "6h",
		"discovery.max_depth": "4", "discovery.max_entries": "500", "discovery.timeout": "7s", "discovery.reconcile_interval": "8s", "readiness.timeout": "9s", "logging.level": "debug",
	}
	var raw Config
	for key, value := range values {
		if err := SetField(&raw, key, value); err != nil {
			t.Fatalf("SetField(%s): %v", key, err)
		}
	}
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	if err := Validate(&effective); err != nil {
		t.Fatal(err)
	}
	if len(Fields(effective)) != len(values) {
		t.Fatalf("Fields count=%d", len(Fields(effective)))
	}
	for _, pathFn := range []func() (string, error){Path, StatePath, SocketPath, LogPath} {
		if path, err := pathFn(); err != nil || !strings.HasPrefix(path, home) {
			t.Fatalf("derived path=%q err=%v", path, err)
		}
	}
	if _, err := raw.Readiness.Timeout.MarshalYAML(); err != nil {
		t.Fatal(err)
	}
	if data, err := yaml.Marshal(raw); err != nil || len(data) == 0 {
		t.Fatalf("marshal complete raw config: %v", err)
	}
	if err := SetField(&raw, "unknown", "value"); err == nil {
		t.Fatal("unknown scalar field succeeded")
	}
}

func TestSetFieldRejectsEveryInvalidScalarType(t *testing.T) {
	keys := []string{
		"storage.backup_generations", "pool.warm_per_workspace", "pool.preparation_concurrency", "pool.git_concurrency_per_repository",
		"discovery.max_depth", "discovery.max_entries",
		"storage.backup_retention", "retention.hot_standby", "retention.ended_worktree", "retention.recovery_snapshot",
		"retention.expired_session_tombstone", "retention.failed_job", "retention.event_log", "discovery.timeout",
		"discovery.reconcile_interval", "readiness.timeout",
	}
	for _, key := range keys {
		var cfg Config
		if err := SetField(&cfg, key, "invalid"); err == nil {
			t.Errorf("SetField(%s) accepted invalid value", key)
		}
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

func TestLoadRawRejectsMalformedDurationAndMultipleDocuments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, document := range []string{
		"readiness:\n  timeout: [invalid]\n",
		"readiness:\n  timeout: definitely-not-a-duration\n",
		"version: 1\n---\nversion: 1\n",
	} {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRaw(); err == nil {
			t.Fatalf("malformed config succeeded: %q", document)
		}
	}
}

func TestNormalizePathsResolvesSymlinksAndRejectsCanonicalCollisions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(home, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(alias, "future")
	cfg.Repositories = map[string]Repository{real: {}, alias: {}}
	if err := NormalizePaths(&cfg); err == nil || !strings.Contains(err.Error(), "collide") {
		t.Fatalf("canonical collision error=%v", err)
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

func TestLoadReportsNormalizationAndValidationErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, document := range []string{
		"version: 1\nstorage:\n  worktree_root: relative\n",
		"version: 2\n",
	} {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("invalid effective config loaded: %q", document)
		}
	}
	if keys := collectKeys(&yaml.Node{}); len(keys) != 0 {
		t.Fatalf("empty YAML keys=%v", keys)
	}
}

func TestDerivedPathsFailClosedWithoutHome(t *testing.T) {
	previousHome, hadHome := os.LookupEnv("HOME")
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", previousHome)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	for name, operation := range map[string]func() error{
		"config path": func() error { _, err := Path(); return err },
		"state path":  func() error { _, err := StatePath(); return err },
		"socket path": func() error { _, err := SocketPath(); return err },
		"log path":    func() error { _, err := LogPath(); return err },
		"expand home": func() error { _, err := ExpandHome("$HOME/wx"); return err },
		"load raw":    func() error { _, err := LoadRaw(); return err },
		"save":        func() error { return Save(Config{}) },
	} {
		if err := operation(); err == nil {
			t.Errorf("%s succeeded without HOME", name)
		}
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
