package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrintStatusSummaryUsesWorkspaceRepositoryMappingAndRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := home + "/dev/wx"
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })

	payload := map[string]any{
		"workspace_details": []map[string]any{
			{"id": "chez", "root": home + "/.local/share/chezmoi", "repositories": 1, "ready": 1, "leased": 0},
			{"id": "prx", "root": home + "/dev/prx", "repositories": 1, "ready": 1, "leased": 0},
			{"id": "wx", "root": current, "repositories": 1, "ready": 1, "leased": 2},
		},
		"repository_details": []map[string]any{
			{"id": "chez-repo", "main_path": home + "/.local/share/chezmoi", "last_used_at": "2026-09-04T23:09:00Z"},
			{"id": "prx-repo", "main_path": home + "/dev/prx", "last_used_at": "2026-09-04T22:37:00Z"},
			{"id": "wx-repo", "main_path": home + "/elsewhere", "last_used_at": "2026-09-04T22:16:00Z"},
		},
		"job_details":     map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots":  []map[string]any{{"path": home + "/wx", "active": true, "bytes": 1, "allocated_bytes": int64(365 * 1024 * 1024)}},
		"restart_pending": false,
		"stop_pending":    false,
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false)
	got := output.String()
	for _, want := range []string{
		"WORKSPACE",
		"LAST USED (JST)",
		"~/.local/share/chezmoi",
		"~/dev/prx",
		"~/dev/wx *",
		"09/05 08:09",
		"09/05 07:37",
		"—",
		"Daemon running · Jobs 0 pending / 0 running / 0 failed",
		"Disk   365 MiB allocated · ~/wx",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "HOT UNTIL") || strings.Contains(got, "quarantine") || strings.Contains(got, "session_details") {
		t.Fatalf("summary leaked detail-only output:\n%s", got)
	}
}

func TestPrintVerboseStatusRetainsDetailsAndUnknownFields(t *testing.T) {
	payload := map[string]any{
		"schema_version": 5, "db_schema_version": 1, "daemon_version": "1.2.3", "protocol_version": 1,
		"pid": 42, "uptime_seconds": 604800, "degraded": false, "restart_pending": false, "stop_pending": false,
		"config_path": "/Users/example/.config/wx/config.yaml", "config_last_reload": "2026-09-05T00:00:00Z", "config_reload_error": "",
		"worktree_root_error": "", "sqlite_last_backup": "2026-09-05T00:01:00Z", "sqlite_backup_error": "",
		"workspaces": 1, "repositories": 1, "active_sessions": 1, "snapshots": 2, "queued_jobs": 1,
		"slots":              map[string]any{"ready": 1, "leased": 1, "failed": 0, "quarantined": 0},
		"workspace_details":  []map[string]any{{"id": "w1", "root": "/repo", "generation": 3, "repositories": 1, "ready": 1, "leased": 1, "failed": 2, "future": "kept"}},
		"repository_details": []map[string]any{{"id": "r1", "main_path": "/repo", "hot": false, "last_used_at": "2026-09-05T00:00:00Z"}},
		"session_details":    []map[string]any{{"id": "s1", "agent": "codex", "state": "ACTIVE", "created_at": "2026-09-05T00:00:00Z", "base_oids": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "age_seconds": 61}},
		"job_details":        map[string]any{"pending": 1, "running": 0, "failed": 2},
		"snapshot_details":   map[string]any{"count": 2, "earliest_expiry": "2026-09-06T00:00:00Z"},
		"worktree_roots":     []map[string]any{{"path": "/repo/wx", "active": false, "bytes": 123, "allocated_bytes": 456, "measurement": "st_blocks_x_512", "error": ""}},
		"retention_seconds":  map[string]any{"hot_standby": 604800, "ended_worktree": 3600, "recovery_snapshot": 0, "expired_session_tombstone": 31536000, "failed_job": 1, "event_log": 2},
		"quarantine":         []map[string]any{{"id": "q1", "path": "/bad/one", "failure_code": "OWNERSHIP"}, {"id": "q2", "path": "/bad/two", "failure_code": "OWNERSHIP"}},
		"new_top_level":      map[string]any{"answer": 0},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{
		"Workspaces", "FAILED (FAILED + QUARANTINED)", "Repositories", "Sessions", "Daemon", "Config", "Backup", "Pool", "Jobs", "Snapshots", "Storage", "Retention", "Quarantine",
		"future: kept", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "123 bytes", "456 bytes", "604800s (7 days)", "Reason: OWNERSHIP (2)", "new_top_level.answer: 0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose output missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Degraded: false") || !strings.Contains(got, "Hot: false") {
		t.Fatalf("false values were not retained:\n%s", got)
	}
}

func TestPrintDegradedStatusDoesNotInventCounts(t *testing.T) {
	var output bytes.Buffer
	printStatusDisplay(&output, map[string]any{"degraded": true, "database_path": "/state.db", "error": "SQLite is unavailable"}, false)
	got := output.String()
	if !strings.Contains(got, "Daemon degraded · SQLite is unavailable") || !strings.Contains(got, "Database: /state.db") {
		t.Fatalf("degraded output=%q", got)
	}
	if strings.Contains(got, "Jobs 0") || strings.Contains(got, "Workspaces: 0") {
		t.Fatalf("degraded output invented zero counts: %q", got)
	}
}
