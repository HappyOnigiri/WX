package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// v2AllKeysDocument は system・workspace_defaults・repository_defaults の全 key を
// 組み込み既定値と異なる値で書いた設定である。key を追加して overlay を書き忘れると
// TestV2OverlayAppliesEveryDocumentedKey がその key を検出する。
const v2AllKeysDocument = `version: 2
system:
  language: ja
  storage:
    worktree_root: $HOME/custom-wx
    backup_generations: 7
    backup_retention: 100h
  pool:
    preparation_concurrency: 5
  retention:
    quarantined: 101h
    recovery_snapshot: 102h
    expired_session_tombstone: 103h
    failed_job: 104h
    event_log: 105h
  discovery:
    max_entries: 4321
    timeout: 33s
    reconcile_interval: 44s
  resume:
    auto_fresh: true
  lease:
    ttl: 77m
    shell: /bin/bash
  sessions:
    paths:
      claude:
        sessions:
          - ~/custom-claude
      codex:
        sessions:
          - ~/custom-codex
  logging:
    level: debug
workspace_defaults:
  worktree: cold
  copy:
    - .env
  link:
    - node_modules
  reuse_standby: false
  warm_count: 3
  agent:
    add_dir: worktree
  retention:
    hot_standby: 9h
    ended_worktree: 8h
  discovery:
    max_depth: 4
    exclude:
      - build
repository_defaults:
  default_branch: trunk
  dir_source: directory
  cow_min_size_kib: 64
  submodules: false
  prepare:
    command:
      - make
      - setup
    timeout: 12m
    version: v9
  includes:
    default_agent_rules: false
  readiness:
    mode: full
    early_paths:
      - README.md
    timeout: 21m
    progress: false
  storage:
    copy_mode: copy
`

// TestV2OverlayAppliesEveryDocumentedKey は、v2 の各節に書いた値が実効設定へ
// 反映されることを key 単位で確かめる。overlay の分岐を書き忘れた key は
// 組み込み既定値のまま残るため、既定値と一致する key を失敗として報告する。
func TestV2OverlayAppliesEveryDocumentedKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(v2AllKeysDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	effective, raw, err := LoadWithRaw()
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultsV2()
	for _, section := range []struct {
		name            string
		loaded, builtin reflect.Value
	}{
		{"system", reflect.ValueOf(effective.System), reflect.ValueOf(defaults.System)},
		{"workspace_defaults", reflect.ValueOf(effective.WorkspaceDefaults), reflect.ValueOf(defaults.WorkspaceDefaults)},
		{"repository_defaults", reflect.ValueOf(effective.RepositoryDefaults), reflect.ValueOf(defaults.RepositoryDefaults)},
	} {
		builtin := map[string]string{}
		walkV2Fields(section.builtin, "", func(key string, field reflect.Value) {
			builtin[key] = formatScopeValue(field)
		})
		walkV2Fields(section.loaded, "", func(key string, field reflect.Value) {
			value := formatScopeValue(field)
			if value == builtin[key] {
				t.Errorf("%s.%s stayed at the built-in value %q", section.name, key, value)
			}
			if !v2RawFieldPresent(raw, section.name, key) {
				t.Errorf("%s.%s was not recorded as explicit", section.name, key)
			}
		})
	}
	// legacy flatten view も同じ値を指し、既存の lifecycle code が v2 の値を読む。
	if effective.DisplayLanguage() != LanguageJapanese || effective.Logging.Level != "debug" || effective.Pool.WarmPerWorkspace != 3 {
		t.Fatalf("flattened view=%q/%q/%d", effective.DisplayLanguage(), effective.Logging.Level, effective.Pool.WarmPerWorkspace)
	}
}
