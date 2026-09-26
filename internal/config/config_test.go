package config

import (
	"os"
	"path/filepath"
	"strconv"
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
	if got := string(data); !strings.Contains(got, "hot_standby: 0s") || !strings.Contains(got, "workspace_defaults:") || strings.Contains(got, "worktree_root") {
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

func TestLanguageDefaultsAndValidation(t *testing.T) {
	if got := Defaults().DisplayLanguage(); got != LanguageEnglish {
		t.Fatalf("default language=%q, want en", got)
	}
	valid := Defaults()
	valid.Language = LanguageJapanese
	if err := Validate(&valid); err != nil {
		t.Fatalf("Japanese language rejected: %v", err)
	}
	invalid := Defaults()
	invalid.Language = "fr"
	if err := Validate(&invalid); err == nil || !strings.Contains(err.Error(), "language must be en or ja") {
		t.Fatalf("invalid language error=%v", err)
	}
}

func TestGlobalFieldsIncludesLanguageWhenUnset(t *testing.T) {
	fields := GlobalFields(Defaults(), Config{})
	if len(fields) == 0 || fields[0].Key != "language" || fields[0].Value != LanguageEnglish || fields[0].Source != "default" {
		t.Fatalf("global language field=%+v", fields[:min(1, len(fields))])
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
	document := `version: 2
repository_defaults:
  includes:
    default_agent_rules: false
workspaces:
  $HOME:
    repositories:
      repository:
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
	if cfg.RepositoryDefaults.Includes.DefaultAgentRules == nil || *cfg.RepositoryDefaults.Includes.DefaultAgentRules {
		t.Fatal("global default agent rules override was not loaded")
	}
	override, ok := cfg.Workspaces[home].Repositories["repository"]
	if !ok || override.Includes.DefaultAgentRules == nil || !*override.Includes.DefaultAgentRules {
		t.Fatalf("repository override=%+v, want normalized explicit true", override)
	}
	if !cfg.DefaultAgentRulesForWorkspaceRepository(home, "repository", repository) {
		t.Fatal("loaded repository override was not applied")
	}
}

func TestValidateRejectsEachPolicyClass(t *testing.T) {
	valid := Defaults()
	valid.Storage.WorktreeRoot = "/tmp/wx"
	tests := []Config{
		func() Config { c := valid; c.Storage.WorktreeRoot = "relative"; return c }(),
		func() Config { c := valid; c.Storage.BackupGenerations = 0; return c }(),
		func() Config { c := valid; c.Pool.WarmPerWorkspace = -1; return c }(),
		func() Config { c := valid; c.Retention.HotStandby.Duration = -1; return c }(),
		func() Config {
			c := valid
			c.Repositories = map[string]Repository{"/tmp/repository": {Prepare: Prepare{Timeout: &Duration{Duration: -1}}}}
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

// Validate の各範囲条件は、仕様上の最小値・最大値を有効値として受け入れる。
// 境界を一つ内側へずらす変異でも、この契約を検出できるようにする。
func TestValidateAcceptsInclusiveBoundaryValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "repository cow minimum zero",
			mutate: func(c *Config) {
				value := 0
				c.Repositories["/repository"] = Repository{COWMinSizeKiB: &value}
			},
		},
		{
			name: "repository cow minimum maximum",
			mutate: func(c *Config) {
				value := MaxCOWMinSizeKiB
				c.Repositories["/repository"] = Repository{COWMinSizeKiB: &value}
			},
		},
		{
			name:   "preparation concurrency one",
			mutate: func(c *Config) { c.Pool.PreparationConcurrency = 1 },
		},
		{
			name:   "discovery max depth one",
			mutate: func(c *Config) { c.Discovery.MaxDepth = 1 },
		},
		{
			name:   "discovery max entries one",
			mutate: func(c *Config) { c.Discovery.MaxEntries = 1 },
		},
		{
			name:   "backup generations one",
			mutate: func(c *Config) { c.Storage.BackupGenerations = 1 },
		},
		{
			name:   "backup retention zero",
			mutate: func(c *Config) { c.Storage.BackupRetention = Duration{} },
		},
		{
			name: "workspace retention zero and depth one",
			mutate: func(c *Config) {
				depth := 1
				c.Workspaces["/workspace"] = Workspace{
					Retention: WorkspaceRetention{
						HotStandby:    &Duration{},
						EndedWorktree: &Duration{},
					},
					Discovery: WorkspaceDiscovery{MaxDepth: &depth},
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			test.mutate(&cfg)
			if err := Validate(&cfg); err != nil {
				t.Fatalf("boundary value rejected: %v", err)
			}
		})
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
	document := `version: 2
workspace_defaults:
  discovery:
    exclude: []
workspaces:
  $HOME/workspace:
    copy: [AGENTS.md]
    repositories:
      repository:
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
	if len(effective.WorkspaceDefaults.Discovery.Exclude) != 0 || len(effective.Workspaces) != 1 || len(effective.Workspaces["$HOME/workspace"].Repositories) != 1 {
		t.Fatalf("merged sparse collections=%+v", effective)
	}
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"version: 2", "exclude: []", "workspaces:", "repositories:"} {
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
	bad.Workspaces = map[string]Workspace{home: {Repositories: map[string]Repository{"../relative": {}}}}
	if err := NormalizePaths(&bad); err == nil || !strings.Contains(err.Error(), "workspace-relative path") {
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

// 共有下限は0が「下限なし」を意味するため、明示した0が既定値へ戻らないことまで確かめる。
func TestCOWMinSizeConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := Load()
	if err != nil || got.Storage.COWMinSizeKiB != DefaultCOWMinSizeKiB || got.Storage.COWMinShareSize() != 16<<10 {
		t.Fatalf("default=%d bytes=%d err=%v", got.Storage.COWMinSizeKiB, got.Storage.COWMinShareSize(), err)
	}
	for _, kib := range []int{0, 16, 64, MaxCOWMinSizeKiB} {
		raw := Config{}
		if err := SetField(&raw, "storage.cow_min_size_kib", strconv.Itoa(kib)); err != nil {
			t.Fatal(err)
		}
		if err := Save(raw); err != nil {
			t.Fatal(err)
		}
		got, err := Load()
		if err != nil || got.Storage.COWMinSizeKiB != kib {
			t.Fatalf("kib=%d got=%d err=%v", kib, got.Storage.COWMinSizeKiB, err)
		}
		if want := int64(kib) << 10; got.Storage.COWMinShareSize() != want {
			t.Fatalf("kib=%d bytes=%d want=%d", kib, got.Storage.COWMinShareSize(), want)
		}
	}
	for _, kib := range []int{-1, MaxCOWMinSizeKiB + 1} {
		cfg := Defaults()
		cfg.Storage.COWMinSizeKiB = kib
		if err := Validate(&cfg); err == nil {
			t.Fatalf("out-of-range minimum accepted: %d", kib)
		}
	}
}

// repository 個別の下限は、未指定なら global を継承し、明示した 0 は「下限なし」として global を上書きする。
func TestRepositoryCOWMinSizeOverridesGlobal(t *testing.T) {
	const path = "/repository"
	zero, thirtyTwo := 0, 32
	for _, c := range []struct {
		name     string
		override *int
		want     int
	}{
		{name: "inherit", want: DefaultCOWMinSizeKiB},
		{name: "explicit", override: &thirtyTwo, want: 32},
		{name: "no-minimum", override: &zero, want: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Repositories[path] = Repository{COWMinSizeKiB: c.override}
			if got := cfg.COWMinSizeKiB(path); got != c.want {
				t.Fatalf("kib=%d want=%d", got, c.want)
			}
			if got, want := cfg.COWMinShareSize(path), int64(c.want)<<10; got != want {
				t.Fatalf("bytes=%d want=%d", got, want)
			}
			// 個別指定のない repository は global のままである。
			if got := cfg.COWMinSizeKiB("/other"); got != DefaultCOWMinSizeKiB {
				t.Fatalf("other repository kib=%d", got)
			}
		})
	}
}

// 個別値も global と同じ範囲に限り、どの repository が範囲外かをメッセージで示す。
func TestRepositoryCOWMinSizeRangeIsValidated(t *testing.T) {
	const path = "/repository"
	for _, kib := range []int{-1, MaxCOWMinSizeKiB + 1} {
		cfg := Defaults()
		cfg.Repositories[path] = Repository{COWMinSizeKiB: &kib}
		err := Validate(&cfg)
		if err == nil {
			t.Fatalf("out-of-range repository minimum accepted: %d", kib)
		}
		if !strings.Contains(err.Error(), "repositories."+path+".cow_min_size_kib") {
			t.Fatalf("kib=%d error=%v", kib, err)
		}
	}
}

func TestEffectiveEqualIgnoresWhichKeysTheFileSpelledOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defaults := Defaults()
	spelledOut := Merge(Defaults(), mustParseRaw(t, "version: 2\nworkspace_defaults:\n  worktree: ask\n"))
	if !defaults.EffectiveEqual(spelledOut) {
		t.Fatal("a configuration that restates a default was reported as different")
	}
	changed := Merge(Defaults(), mustParseRaw(t, "version: 2\nworkspace_defaults:\n  worktree: hot\n"))
	if defaults.EffectiveEqual(changed) {
		t.Fatal("a changed worktree policy was reported as equal")
	}
	withWorkspace := Defaults()
	withWorkspace.Workspaces = map[string]Workspace{"/repo": {Worktree: "cold"}}
	if defaults.EffectiveEqual(withWorkspace) {
		t.Fatal("an added workspace override was reported as equal")
	}
}

func mustParseRaw(t *testing.T, document string) Config {
	t.Helper()
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAgentAddDirConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got, err := Load(); err != nil || got.Agent.AddDir != AgentAddDirAlways {
		t.Fatalf("default=%q err=%v", got.Agent.AddDir, err)
	}
	for _, mode := range []string{AgentAddDirAlways, AgentAddDirWorktree, AgentAddDirOff} {
		raw := Config{}
		if err := SetField(&raw, "agent.add_dir", mode); err != nil {
			t.Fatal(err)
		}
		if err := Save(raw); err != nil {
			t.Fatal(err)
		}
		got, err := Load()
		if err != nil || got.Agent.AddDir != mode {
			t.Fatalf("mode=%q got=%q err=%v", mode, got.Agent.AddDir, err)
		}
	}
	for _, mode := range []string{"", "ALWAYS", "invalid"} {
		cfg := Defaults()
		cfg.Agent.AddDir = mode
		if err := Validate(&cfg); err == nil {
			t.Fatalf("invalid mode accepted: %q", mode)
		}
	}
}

func TestCodexNoDaemonDefaultsOnAndHonorsWorkspaceOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if !Defaults().CodexNoDaemonForWorkspace(root) {
		t.Fatal("default Codex no-daemon policy is off")
	}
	var raw Config
	if err := SetField(&raw, "agent.codex_no_daemon", "false"); err != nil {
		t.Fatal(err)
	}
	if err := SetScopeField(&raw, ScopeWorkspace, root, "agent.codex_no_daemon", "true"); err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CodexNoDaemonForWorkspace("/other") || !loaded.CodexNoDaemonForWorkspace(root) {
		t.Fatalf("Codex no-daemon policy: other=%t repo=%t", loaded.CodexNoDaemonForWorkspace("/other"), loaded.CodexNoDaemonForWorkspace(root))
	}
}

// TestReadinessProgressDefaultsOnAndTurnsOffFromYAML は進捗表示の既定が有効で、
// 設定ファイルの false が既定へ埋め戻されずに残ることを確かめる。
func TestReadinessProgressDefaultsOnAndTurnsOffFromYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if !Defaults().Readiness.Progress {
		t.Fatal("readiness.progress default is off")
	}
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 2\nrepository_defaults:\n  readiness:\n    progress: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Readiness.Progress {
		t.Fatal("readiness.progress=false was overwritten by the default")
	}
	// 同じ節の他のキーは既定のまま残る。
	if cfg.Readiness.Mode != "early" {
		t.Fatalf("readiness.mode=%q, want the default to stay", cfg.Readiness.Mode)
	}
}

// TestUpdateAutoCheckDefaultsOnAndTurnsOff は、既定で有効な真偽値が
// v2 の system 節で false のまま残ることを確かめる。
// v2 側をポインタにし忘れると、明示した false が既定の true へ埋め戻される。
func TestUpdateAutoCheckDefaultsOnAndTurnsOff(t *testing.T) {
	if !Defaults().Update.AutoCheck {
		t.Fatal("update.auto_check default is off")
	}
	if v2 := DefaultsV2().System.Update.AutoCheck; v2 == nil || !*v2 {
		t.Fatal("the v2 built-in value for system.update.auto_check is not on")
	}
	for _, test := range []struct{ name, document string }{{name: "v2", document: "version: 2\nsystem:\n  update:\n    auto_check: false\n"}} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".config", "wx", "config.yaml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.document), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Update.AutoCheck {
				t.Fatal("update.auto_check=false was overwritten by the default")
			}
		})
	}
}

// TestConfigVersionTwoRejectsATopLevelUpdateSection は、v2 で legacy 節を併記した設定を
// 拒否する一覧へ update を入れ忘れる退行を防ぐ。忘れると、書いた値が読まれないまま静かに通る。
func TestConfigVersionTwoRejectsATopLevelUpdateSection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := "version: 2\nsystem:\n  language: ja\nupdate:\n  auto_check: false\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "update") {
		t.Fatalf("load error=%v, want the legacy update section refused", err)
	}
}
