package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// 外部の mutation hunt が見つけた設定境界を、個別の実効値で検証する。
func TestMutationConfigGeneralValidateBoundaries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "repository COW minimum zero",
			mutate: func(c *Config) {
				value := 0
				c.Repositories["/repository"] = Repository{COWMinSizeKiB: &value}
			},
		},
		{
			name: "repository COW minimum maximum",
			mutate: func(c *Config) {
				value := MaxCOWMinSizeKiB
				c.Repositories["/repository"] = Repository{COWMinSizeKiB: &value}
			},
		},
		{
			name:   "warm count zero",
			mutate: func(c *Config) { c.WorkspaceDefaults.WarmCount = intPointer(0) },
		},
		{
			name:   "preparation concurrency one",
			mutate: func(c *Config) { c.System.Pool.PreparationConcurrency = 1 },
		},
		{
			name:   "discovery max depth one",
			mutate: func(c *Config) { c.WorkspaceDefaults.Discovery.MaxDepth = intPointer(1) },
		},
		{
			name:   "discovery max entries one",
			mutate: func(c *Config) { c.System.Discovery.MaxEntries = 1 },
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

func intPointer(value int) *int { return &value }

// 旧 adapter の既定値は v2 の正本へコピーされるため、全 duration を個別に固定する。
func TestMutationConfigGeneralDefaultsKeepTheirDurations(t *testing.T) {
	cfg := defaultsLegacy()
	want := map[string]time.Duration{
		"backup_retention":          168 * time.Hour,
		"hot_standby":               168 * time.Hour,
		"ended_worktree":            time.Hour,
		"quarantined":               24 * time.Hour,
		"recovery_snapshot":         720 * time.Hour,
		"expired_session_tombstone": 8760 * time.Hour,
		"failed_job":                168 * time.Hour,
		"event_log":                 168 * time.Hour,
	}
	got := map[string]time.Duration{
		"backup_retention":          cfg.Storage.BackupRetention.Duration,
		"hot_standby":               cfg.Retention.HotStandby.Duration,
		"ended_worktree":            cfg.Retention.EndedWorktree.Duration,
		"quarantined":               cfg.Retention.Quarantined.Duration,
		"recovery_snapshot":         cfg.Retention.RecoverySnapshot.Duration,
		"expired_session_tombstone": cfg.Retention.ExpiredSessionTombstone.Duration,
		"failed_job":                cfg.Retention.FailedJob.Duration,
		"event_log":                 cfg.Retention.EventLog.Duration,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default durations=%v, want %v", got, want)
	}
}

// repository 個別指定は有効な enum と境界値を受理し、不正な timeout を拒否する。
func TestMutationConfigGeneralRepositoryValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Repository)
	}{
		{name: "remote directory source", mutate: func(r *Repository) { r.DirSource = RepoDirSourceRemote }},
		{name: "directory directory source", mutate: func(r *Repository) { r.DirSource = RepoDirSourceDirectory }},
		{name: "full readiness", mutate: func(r *Repository) { r.Readiness.Mode = "full" }},
		{name: "positive readiness timeout", mutate: func(r *Repository) { r.Readiness.Timeout = durationPointer(time.Second) }},
		{name: "auto copy mode", mutate: func(r *Repository) { r.Storage.CopyMode = CopyModeAuto }},
		{name: "cow copy mode", mutate: func(r *Repository) { r.Storage.CopyMode = CopyModeCOW }},
		{name: "copy copy mode", mutate: func(r *Repository) { r.Storage.CopyMode = CopyModeCopy }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			override := Repository{}
			test.mutate(&override)
			cfg.Repositories["/repository"] = override
			if err := Validate(&cfg); err != nil {
				t.Fatalf("valid repository override rejected: %v", err)
			}
		})
	}
	zero := Duration{}
	cfg := Defaults()
	cfg.Repositories["/repository"] = Repository{Readiness: RepositoryReadiness{Timeout: &zero}}
	if err := Validate(&cfg); err == nil {
		t.Fatal("zero repository readiness timeout was accepted")
	}
}

// v2 の language 正本と旧 flatten view は、表示と RPC で同じ fallback を使う。
func TestMutationConfigGeneralLanguageAndCatalog(t *testing.T) {
	legacy := Config{Language: LanguageJapanese}
	if got := legacy.DisplayLanguage(); got != LanguageJapanese {
		t.Fatalf("legacy display language=%q, want ja", got)
	}
	if got := legacy.LanguageForRPC(); got != LanguageJapanese {
		t.Fatalf("legacy RPC language=%q, want ja", got)
	}
	if got := (Config{System: SystemConfig{Language: LanguageJapanese}}).DisplayLanguage(); got != LanguageJapanese {
		t.Fatalf("v2 display language=%q, want ja", got)
	}

	meta, err := Describe("readiness.mode", "repository")
	if err != nil || meta.Key != "readiness.mode" || len(meta.Choices) != 2 {
		t.Fatalf("Describe metadata=%+v err=%v", meta, err)
	}
	for _, test := range []struct {
		key, scope string
	}{
		{key: "readiness.mode", scope: "system"},
		{key: "does.not.exist", scope: "repository"},
	} {
		if _, err := Describe(test.key, test.scope); err == nil {
			t.Fatalf("Describe(%q, %q) unexpectedly succeeded", test.key, test.scope)
		}
	}
}

// 未知の動的キーと明示的な空 list は、値の解釈から外しても保存時に失わない。
func TestMutationConfigGeneralPreservesUnknownDynamicKeys(t *testing.T) {
	document := `version: 2
workspaces:
  demo:
    copy: []
    repositories:
      frontend:
        future:
          mode: 7
        prepare:
          inputs: []
`
	path := writeConfigFile(t, document)
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if !raw.present["workspaces.demo.repositories.frontend.future"] {
		t.Fatalf("dynamic key was not collected: %v", raw.present)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"copy: []", "future:", "mode: 7", "inputs: []"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("saved config does not contain %q:\n%s", want, saved)
		}
	}
	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.UnknownKeys(); len(got) != 1 || got[0].Key != "workspaces.demo.repositories.frontend.future" {
		t.Fatalf("unknown dynamic keys=%v", got)
	}
}

func TestMutationConfigGeneralYAMLBoundaries(t *testing.T) {
	value, ok := nestedYAMLValue(map[string]any{
		"system": map[string]any{"pool": map[string]any{"workers": 4}},
	}, "system.pool.workers")
	if !ok || value != 4 {
		t.Fatalf("nested value=%v found=%v, want 4 and true", value, ok)
	}

	section := map[string]any{}
	present := Config{present: map[string]bool{"workspaces.demo.copy": true}}
	preserveDynamicValues(present, "demo", section, Workspace{Copy: []string{}}, []string{"copy"})
	if got, ok := section["copy"]; !ok || !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("preserved dynamic empty list=%#v, want []string{}", got)
	}

	withoutWorkspaces, err := yaml.Marshal(Config{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutWorkspaces), "workspaces:") {
		t.Fatalf("zero v2 config unexpectedly contains workspaces:\n%s", withoutWorkspaces)
	}
	withEmptyWorkspaces, err := yaml.Marshal(Config{Version: 2, Workspaces: map[string]Workspace{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withEmptyWorkspaces), "workspaces:") {
		t.Fatalf("explicit empty workspaces section was omitted:\n%s", withEmptyWorkspaces)
	}
}

// 保存前の検証失敗は既存ファイルを残し、digest は読み取り失敗を成功へ変換しない。
func TestMutationConfigGeneralSaveAndDigestErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("version: 2\nsystem:\n  logging:\n    level: info\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Version: 1}); err == nil {
		t.Fatal("unsupported config version was accepted")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(unchanged, original) {
		t.Fatalf("failed save changed config: %q", unchanged)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := configDigest(); err == nil {
		t.Fatal("digest unexpectedly succeeded for a directory")
	}
}

func bytesEqual(left, right []byte) bool { return reflect.DeepEqual(left, right) }

func TestMutationConfigGeneralSaveFilesystemErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("version: 2\nsystem:\n  logging:\n    level: info\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		configRename = os.Rename
		configOpen = os.Open
	})
	renameErr := errors.New("rename failed")
	configRename = func(string, string) error { return renameErr }
	if err := Save(Config{Version: 2}); !errors.Is(err, renameErr) {
		t.Fatalf("Save rename error=%v, want %v", err, renameErr)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(unchanged, original) {
		t.Fatalf("rename failure changed config: %q", unchanged)
	}

	configRename = os.Rename
	openErr := errors.New("directory open failed")
	configOpen = func(string) (*os.File, error) { return nil, openErr }
	if err := Save(Config{Version: 2}); !errors.Is(err, openErr) {
		t.Fatalf("Save directory-open error=%v, want %v", err, openErr)
	}
}

func TestMutationConfigGeneralEditorsAndListPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preview, err := PreviewEdit(EditRequest{Key: "logging.level", Value: "debug", Operation: EditSet})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Scope != "global" || preview.Before == preview.After || preview.After != "debug" {
		t.Fatalf("default-scope preview=%+v", preview)
	}
	if !isV2ListKey(V2ScopeWorkspace, "repository_defaults.prepare.inputs") {
		t.Fatal("workspace repository_defaults.prepare.inputs was not classified as a list")
	}
	if scope, canonical, ok := legacyV2Key("pool.preparation_concurrency"); !ok || scope != V2ScopeSystem || canonical != "pool.preparation_concurrency" {
		t.Fatalf("legacy pool key=(%q, %q, %v)", scope, canonical, ok)
	}
	if _, err := mutableConfigList(nil, "discovery.exclude"); err == nil {
		t.Fatal("nil config was accepted by mutableConfigList")
	}

	root := t.TempDir()
	t.Chdir(root)
	want, err := filepath.Abs(filepath.Join("nested", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if got := normalizeListPath(filepath.Join("nested", "..", "config")); got != want {
		t.Fatalf("normalized list path=%q, want %q", got, want)
	}
}

func TestMutationConfigGeneralScopeAndReadinessBoundaries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if normalized, err := normalizeWorkspaceMemberships(filepath.Join(home, "workspace"), Workspace{}); err != nil {
		t.Fatal(err)
	} else if normalized.Repositories != nil {
		t.Fatalf("nil repository map became non-nil: %#v", normalized.Repositories)
	}

	cfg := Defaults()
	localProgress := false
	cfg.Repositories[filepath.Join(home, "repository")] = Repository{Readiness: RepositoryReadiness{Progress: &localProgress}}
	if got := cfg.ReadinessForRepository(filepath.Join(home, "repository")); got.Progress {
		t.Fatalf("repository progress=%v, want explicit false", got.Progress)
	}

	workspace := filepath.Join(home, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	var raw Config
	if err := SetScopeField(&raw, ScopeRepository, workspace, "readiness.mode", "full"); err != nil {
		t.Fatal(err)
	}
	if err := ResetScopeField(&raw, ScopeRepository, workspace, "readiness.mode"); err != nil {
		t.Fatal(err)
	}
	if raw.present["repositories"] {
		t.Fatalf("empty repository overrides stayed present: %v", raw.present)
	}

	listValue := formatScopeValue(reflect.ValueOf([]string{"first", "second"}))
	if listValue != `["first" "second"]` {
		t.Fatalf("formatted list=%q, want quoted list", listValue)
	}
}
