package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// statusRowMatches は表の行を path で探し、空白を 1 つに畳んだ残りの列が columns と一致するかを返す。
// 列幅は他の行の内容で変わるため、桁揃えを期待値に含めない。
func statusRowMatches(output, path, columns string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.Join(strings.Fields(line), " ")
		if rest, ok := strings.CutPrefix(trimmed, path+" "); ok {
			return rest == columns
		}
	}
	return false
}

func TestPrintStatusSummaryReadsWorkspaceLastUsedAndRoots(t *testing.T) {
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

	// cs は複数 repository の workspace で、root と一致する repository が無い。それでも workspace 側の last_used_at が時刻として出ることを検査する。
	payload := map[string]any{
		"workspace_details": []map[string]any{
			{"id": "chez", "root": home + "/.local/share/chezmoi", "repositories": 1, "ready": 1, "leased": 0, "last_used_at": "2026-09-04T23:09:00Z"},
			{"id": "cs", "root": home + "/dev/cs", "repositories": 5, "ready": 1, "leased": 1, "last_used_at": "2026-09-04T14:47:00Z"},
			{"id": "prx", "root": home + "/dev/prx", "repositories": 1, "ready": 1, "leased": 0, "last_used_at": "2026-09-04T22:37:00Z"},
			{"id": "unused", "root": home + "/dev/unused", "repositories": 1, "ready": 0, "leased": 0},
			{"id": "wx", "root": current, "repositories": 1, "ready": 1, "leased": 2, "last_used_at": "2026-09-04T22:16:00Z"},
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
		"Daemon running · Jobs 0 pending / 0 running / 0 failed",
		"Disk   365 MiB allocated · ~/wx",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	for _, want := range []struct{ path, columns string }{
		{path: "~/.local/share/chezmoi", columns: "1 0 09/05 08:09"},
		// 複数 repository でも、root と一致する repository が無くても時刻が出る。
		{path: "~/dev/cs", columns: "1 1 09/04 23:47"},
		{path: "~/dev/prx", columns: "1 0 09/05 07:37"},
		// 利用実績が無い workspace だけが「—」になる。
		{path: "~/dev/unused", columns: "0 0 —"},
		{path: "~/dev/wx *", columns: "1 2 09/05 07:16"},
	} {
		if !statusRowMatches(got, want.path, want.columns) {
			t.Fatalf("summary row %q missing columns %q:\n%s", want.path, want.columns, got)
		}
	}
	if strings.Contains(got, "HOT UNTIL") || strings.Contains(got, "quarantine") || strings.Contains(got, "session_details") {
		t.Fatalf("summary leaked detail-only output:\n%s", got)
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
		"worktree_roots":     []map[string]any{{"path": "/repo/wx", "active": false, "bytes": 123, "allocated_bytes": 456, "measurement": "st_blocks_x_512", "error": ""}},
		"retention_seconds":  map[string]any{"hot_standby": 604800, "ended_worktree": 3600, "recovery_snapshot": 0, "expired_session_tombstone": 31536000, "failed_job": 1, "event_log": 2},
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
