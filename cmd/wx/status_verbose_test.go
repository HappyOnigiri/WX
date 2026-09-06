package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintVerboseStatusDistinguishesLegacyWorkspaceLastUsed(t *testing.T) {
	payload := map[string]any{
		"schema_version":    5,
		"workspace_details": []map[string]any{{"id": "old", "root": "/repo/old", "repositories": 1, "ready": 1, "leased": 0}},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	if !strings.Contains(got, "unknown") || !strings.Contains(got, "LAST USED unavailable: daemon JSON schema 5") {
		t.Fatalf("verbose legacy workspace output did not distinguish unavailable LAST USED:\n%s", got)
	}
}

func TestPrintVerboseStatusRetainsDetailsAndUnknownFields(t *testing.T) {
	payload := map[string]any{
		"schema_version": 6, "db_schema_version": 1, "daemon_version": "1.2.3", "protocol_version": 1,
		"pid": 42, "uptime_seconds": 604800, "degraded": false, "restart_pending": false, "stop_pending": false,
		"config_path": "/Users/example/.config/wx/config.yaml", "config_last_reload": "2026-09-05T00:00:00Z", "config_reload_error": "",
		"worktree_root_error": "", "sqlite_last_backup": "2026-09-05T00:01:00Z", "sqlite_backup_error": "",
		"workspaces": 1, "repositories": 1, "active_sessions": 1, "snapshots": 2, "queued_jobs": 1,
		"slots":              map[string]any{"ready": 1, "leased": 1, "failed": 0, "quarantined": 0},
		"workspace_details":  []map[string]any{{"id": "w1", "root": "/repo", "generation": 3, "repositories": 1, "ready": 1, "leased": 1, "failed": 2, "last_used_at": "2026-09-05T00:02:00Z", "future": "kept"}},
		"repository_details": []map[string]any{{"id": "r1", "main_path": "/repo", "hot": false, "last_used_at": "2026-09-05T00:00:00Z"}},
		"session_details":    []map[string]any{{"id": "s1", "agent": "codex", "state": "ACTIVE", "created_at": "2026-09-05T00:00:00Z", "base_oids": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "age_seconds": 61}},
		"job_details":        map[string]any{"pending": 1, "running": 0, "failed": 2},
		"snapshot_details":   map[string]any{"count": 2, "earliest_expiry": "2026-09-06T00:00:00Z"},
		"worktree_roots":     []map[string]any{{"path": "/repo/wx", "active": false, "bytes": 123, "allocated_bytes": 456, "shared_bytes": 400, "exclusive_bytes": 56, "measurement": "st_blocks_x_512", "error": ""}},
		"retention_seconds":  map[string]any{"hot_standby": 604800, "ended_worktree": 3600, "quarantined": 86400, "recovery_snapshot": 0, "expired_session_tombstone": 31536000, "failed_job": 1, "event_log": 2},
		"quarantine":         []map[string]any{{"id": "q1", "path": "/bad/one", "failure_code": "OWNERSHIP"}, {"id": "q2", "path": "/bad/two", "failure_code": "OWNERSHIP"}},
		"new_top_level":      map[string]any{"answer": 0},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{
		"Workspaces", "FAILED (FAILED + QUARANTINED)", "LAST USED", "2026-09-05T00:02:00Z", "Repositories", "Sessions", "Daemon", "Config", "Backup", "Pool", "Jobs", "Snapshots", "Storage", "Retention", "Quarantine",
		"future: kept", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "123 bytes", "456 bytes", "604800s (7 days)", "Reason: OWNERSHIP (2)", "new_top_level.answer: 0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose output missing %q:\n%s", want, got)
		}
	}
	// last_used_at は既知キーなので、列として出るだけで Additional 側には現れない。
	if strings.Contains(got, "workspaces[0].last_used_at") {
		t.Fatalf("known workspace key leaked into additional fields:\n%s", got)
	}
	if !strings.Contains(got, "Degraded: false") || !strings.Contains(got, "Hot: false") {
		t.Fatalf("false values were not retained:\n%s", got)
	}
}
