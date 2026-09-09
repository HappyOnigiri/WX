package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAllScalarFieldsCanBeSetAndReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	values := map[string]string{
		"worktree.undefined": "cold", "worktree.reuse_standby": "false",
		"storage.worktree_root": "$HOME/wx", "storage.copy_mode": "cow", "storage.cow_min_size_kib": "64", "storage.repo_dir_source": "directory", "storage.backup_generations": "4", "storage.backup_retention": "24h",
		"pool.warm_per_workspace": "2", "pool.preparation_concurrency": "3",
		"retention.hot_standby": "1h", "retention.ended_worktree": "2h", "retention.quarantined": "12h", "retention.recovery_snapshot": "3h", "retention.expired_session_tombstone": "4h", "retention.failed_job": "5h", "retention.event_log": "6h",
		"discovery.max_depth": "4", "discovery.max_entries": "500", "discovery.timeout": "7s", "discovery.reconcile_interval": "8s", "readiness.timeout": "9s", "readiness.mode": "full", "resume.auto_fresh": "true", "lease.ttl": "48h", "lease.shell": "/bin/zsh", "includes.default_agent_rules": "false", "logging.level": "debug",
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
	for _, field := range Fields(effective) {
		if field.Key == "includes.default_agent_rules" && field.Value != "false" {
			t.Fatalf("default agent rules field=%q", field.Value)
		}
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
		"storage.backup_generations", "pool.warm_per_workspace", "pool.preparation_concurrency",
		"discovery.max_depth", "discovery.max_entries",
		"storage.backup_retention", "retention.hot_standby", "retention.ended_worktree", "retention.quarantined", "retention.recovery_snapshot",
		"retention.expired_session_tombstone", "retention.failed_job", "retention.event_log", "discovery.timeout", "includes.default_agent_rules",
		"discovery.reconcile_interval", "readiness.timeout",
	}
	for _, key := range keys {
		var cfg Config
		if err := SetField(&cfg, key, "invalid"); err == nil {
			t.Errorf("SetField(%s) accepted invalid value", key)
		}
	}
}

// TestResetFieldRestoresScalarDefault は scalar key の --reset が present を落とし、
// 設定ファイルからキーを消して既定値へ戻すことを確かめる。
func TestResetFieldRestoresScalarDefault(t *testing.T) {
	var raw Config
	if err := SetField(&raw, "storage.cow_min_size_kib", "64"); err != nil {
		t.Fatal(err)
	}
	if got := Merge(Defaults(), raw).Storage.COWMinSizeKiB; got != 64 {
		t.Fatalf("configured cow_min_size_kib = %d, want 64", got)
	}
	if err := ResetField(&raw, "storage.cow_min_size_kib"); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "cow_min_size_kib") {
		t.Fatalf("reset key still written to config file: %s", data)
	}
	if got := Merge(Defaults(), raw).Storage.COWMinSizeKiB; got != 16 {
		t.Fatalf("reset cow_min_size_kib = %d, want the default 16", got)
	}
}

// TestResetFieldRejectsUnknownKey は存在しないキーが list 前提ではない文言で拒否されることを確かめる。
func TestResetFieldRejectsUnknownKey(t *testing.T) {
	var raw Config
	err := ResetField(&raw, "storage.nope")
	if err == nil {
		t.Fatal("unknown scalar key was reset")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("error = %v, want the unknown config key wording", err)
	}
	if IsListKey("storage.cow_min_size_kib") || IsListKey("storage.nope") {
		t.Fatal("scalar or unknown key reported as a list key")
	}
}

// TestResetListRestoresDefaultsWithoutEmptyList は list key の --reset が空リストを書かず、
// 実効値が既定値へ戻ることを確かめる。marshal 結果だけを見ると空リストによる上書きを見逃す。
func TestResetListRestoresDefaultsWithoutEmptyList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var raw Config
	if err := AppendList(&raw, "discovery.exclude", "foo"); err != nil {
		t.Fatal(err)
	}
	if err := ResetList(&raw, "discovery.exclude"); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "exclude") {
		t.Fatalf("reset list still written to config file: %s", data)
	}
	want := []string{"node_modules", "vendor", ".venv", "venv", "tmp", "log"}
	if got := Merge(Defaults(), raw).Discovery.Exclude; !reflect.DeepEqual(got, want) {
		t.Fatalf("reset discovery.exclude = %v, want %v", got, want)
	}
}

// TestEarlyPathsListIsEditable は readiness.early_paths が list 操作の対象であることを確かめる。
func TestEarlyPathsListIsEditable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if !IsListKey("readiness.early_paths") {
		t.Fatal("readiness.early_paths is not treated as a list key")
	}
	var raw Config
	if err := AppendList(&raw, "readiness.early_paths", ".mise.toml"); err != nil {
		t.Fatal(err)
	}
	if err := AppendList(&raw, "readiness.early_paths", "config/settings.json"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveList(&raw, "readiness.early_paths", ".mise.toml"); err != nil {
		t.Fatal(err)
	}
	want := []string{"config/settings.json"}
	if got := Merge(Defaults(), raw).Readiness.EarlyPaths; !reflect.DeepEqual(got, want) {
		t.Fatalf("early_paths after --add/--remove = %v, want %v", got, want)
	}
	if err := ResetList(&raw, "readiness.early_paths"); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "early_paths") {
		t.Fatalf("reset early_paths still written to config file: %s", data)
	}
	if got := Merge(Defaults(), raw).Readiness.EarlyPaths; len(got) != 0 {
		t.Fatalf("reset early_paths = %v, want empty", got)
	}
}
