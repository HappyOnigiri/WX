package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAllScalarFieldsCanBeSetAndReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	values := map[string]string{
		"worktree.undefined":    "cold",
		"storage.worktree_root": "$HOME/wx", "storage.copy_mode": "cow", "storage.repo_dir_source": "directory", "storage.backup_generations": "4", "storage.backup_retention": "24h",
		"pool.warm_per_workspace": "2", "pool.preparation_concurrency": "3",
		"retention.hot_standby": "1h", "retention.ended_worktree": "2h", "retention.quarantined": "12h", "retention.recovery_snapshot": "3h", "retention.expired_session_tombstone": "4h", "retention.failed_job": "5h", "retention.event_log": "6h",
		"discovery.max_depth": "4", "discovery.max_entries": "500", "discovery.timeout": "7s", "discovery.reconcile_interval": "8s", "readiness.timeout": "9s", "resume.auto_fresh": "true", "lease.ttl": "48h", "lease.shell": "/bin/zsh", "includes.default_agent_rules": "false", "logging.level": "debug",
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
