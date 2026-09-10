package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
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
		"slots":                    map[string]any{"ready": 1, "leased": 1, "failed": 0, "quarantined": 0},
		"workspace_details":        []map[string]any{{"id": "w1", "root": "/repo", "generation": 3, "repositories": 1, "ready": 1, "leased": 1, "failed": 2, "last_used_at": "2026-09-05T00:02:00Z", "future": "kept"}},
		"repository_details":       []map[string]any{{"id": "r1", "main_path": "/repo", "hot": false, "last_used_at": "2026-09-05T00:00:00Z"}},
		"session_details":          []map[string]any{{"id": "s1", "agent": "codex", "state": "ACTIVE", "created_at": "2026-09-05T00:00:00Z", "base_oids": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "age_seconds": 61}},
		"archived_session_details": map[string]any{"count": 434, "earliest_archived_at": "2026-08-01T00:00:00Z", "latest_expires_at": "2026-10-01T00:00:00Z"},
		"job_details":              map[string]any{"pending": 1, "running": 0, "failed": 2},
		"snapshot_details":         map[string]any{"count": 2, "earliest_expiry": "2026-09-06T00:00:00Z"},
		"worktree_roots":           []map[string]any{{"path": "/repo/wx", "active": false, "bytes": 123, "allocated_bytes": 456, "shared_bytes": 400, "exclusive_bytes": 56, "measurement": "st_blocks_x_512", "error": ""}},
		"retention_seconds":        map[string]any{"hot_standby": 604800, "ended_worktree": 3600, "quarantined": 86400, "recovery_snapshot": 0, "expired_session_tombstone": 31536000, "failed_job": 1, "event_log": 2, "lease_ttl": 259200},
		"quarantine":               []map[string]any{{"id": "q1", "path": "/bad/one", "failure_code": "OWNERSHIP"}, {"id": "q2", "path": "/bad/two", "failure_code": "OWNERSHIP"}},
		"new_top_level":            map[string]any{"answer": 0},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{
		"Workspaces", "FAILED (FAILED + QUARANTINED)", "LAST USED", "2026-09-05T00:02:00Z", "Repositories", "Sessions", "Daemon", "Config", "Backup", "Pool", "Jobs", "Snapshots", "Storage", "Retention", "Quarantine",
		"future: kept", "123 bytes", "456 bytes", "604800s (7 days)", "q1 —    OWNERSHIP /bad/one", "new_top_level.answer: 0",
		"Archived: 434 (earliest archived 2026-08-01T00:00:00Z, latest expiry 2026-10-01T00:00:00Z)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose output missing %q:\n%s", want, got)
		}
	}
	// last_used_at は既知キーなので、列として出るだけで Additional 側には現れない。
	if strings.Contains(got, "workspaces[0].last_used_at") {
		t.Fatalf("known workspace key leaked into additional fields:\n%s", got)
	}
	// base_oids は表の列から外したが既知キーのままなので、どこにも出ない。
	if strings.Contains(got, "sessions[0].base_oids") || strings.Contains(got, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("base_oids leaked into the verbose output:\n%s", got)
	}
	// HOT 列は表になっても false を落とさない。
	if !strings.Contains(got, "Degraded: false") || !strings.Contains(got, "r1 /repo false") {
		t.Fatalf("false values were not retained:\n%s", got)
	}
}

// verbose は登録の診断が目的なので、要約から外れる workspace も policy 付きで残す。
func TestPrintVerboseStatusKeepsWorkspacesHiddenFromTheSummary(t *testing.T) {
	payload := map[string]any{
		"schema_version": 14,
		"workspace_details": []map[string]any{
			{"id": "w1", "root": "/repo", "policy": "hot", "generation": 1, "repositories": 1, "ready": 1, "leased": 0},
			{"id": "w2", "root": "/archive", "policy": "off", "generation": 1, "repositories": 1, "ready": 0, "leased": 0},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{"POLICY", "HOT", "OFF", "/archive"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose output missing %q:\n%s", want, got)
		}
	}
	// policy は既知キーなので、列として出るだけで Additional 側には現れない。
	if strings.Contains(got, "workspaces[0].policy") {
		t.Fatalf("known workspace key leaked into additional fields:\n%s", got)
	}
}

// TestPrintVerboseStatusListsSessionsAsATable は Sessions の表の列と、行が無いときの出し分けを固定する。
func TestPrintVerboseStatusListsSessionsAsATable(t *testing.T) {
	payload := map[string]any{
		"schema_version":           19,
		"session_details":          []map[string]any{{"id": "s1", "agent": "codex", "state": "ACTIVE", "created_at": "2026-09-05T00:00:00Z", "base_oids": "aaaa", "age_seconds": 61}},
		"archived_session_details": map[string]any{"count": 0},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{"ID", "AGENT", "STATE", "CREATED (", "ELAPSED", "1m 1s", "Archived: 0 (earliest archived —, latest expiry —)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose session table missing %q:\n%s", want, got)
		}
	}
	// 縦積みをやめたので、1 session あたりの見出しと BASE OIDS 列は出さない。
	if strings.Contains(got, "BASE OIDS") || strings.Contains(got, "Session 1") {
		t.Fatalf("verbose session table kept the per-session layout:\n%s", got)
	}

	empty := map[string]any{"schema_version": 19, "session_details": []map[string]any{}, "archived_session_details": map[string]any{}}
	output.Reset()
	printStatusDisplay(&output, empty, true)
	if got := output.String(); !strings.Contains(got, "(none)") || !strings.Contains(got, "Archived: (none)") {
		t.Fatalf("empty session details did not render (none):\n%s", got)
	}

	output.Reset()
	printStatusDisplay(&output, map[string]any{"schema_version": 19, "workspace_details": []map[string]any{}}, true)
	if got := output.String(); !strings.Contains(got, "(unset)") || !strings.Contains(got, "Archived: —") {
		t.Fatalf("missing session details did not render (unset):\n%s", got)
	}
}

// TestPrintVerboseStatusKeepsArchivedSessionsFromLegacyDaemons は集計を返さない daemon での劣化表示を固定する。
// 旧 daemon の session_details には ARCHIVED が混ざるため、行は間引かず注記だけを添える。
func TestPrintVerboseStatusKeepsArchivedSessionsFromLegacyDaemons(t *testing.T) {
	payload := map[string]any{
		"schema_version": 18,
		"session_details": []map[string]any{
			{"id": "s1", "agent": "codex", "state": "ACTIVE", "created_at": "2026-09-05T00:00:00Z", "age_seconds": 61},
			{"id": "s2", "agent": "claude", "state": "ARCHIVED", "created_at": "2026-09-04T00:00:00Z", "age_seconds": 90061},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{"Archived: unknown", "daemon JSON schema 18 has no archived session summary", "s2", "ARCHIVED"} {
		if !strings.Contains(got, want) {
			t.Fatalf("legacy session output missing %q:\n%s", want, got)
		}
	}
}

// TestPrintVerboseStatusListsRepositoriesAsATable は Repositories の表の列と、行が無いときの出し分けを固定する。
func TestPrintVerboseStatusListsRepositoriesAsATable(t *testing.T) {
	// 期待値に時刻を書くので、CI (UTC) でも同じ表示になるよう表示タイムゾーンを固定する。
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })

	payload := map[string]any{
		"schema_version": 19,
		"repository_details": []map[string]any{
			{"id": "r2", "main_path": "/repo/b", "hot": false, "last_used_at": "2026-09-05T00:00:00Z", "future": "kept"},
			{"id": "r1", "main_path": "/repo/a", "hot": true, "last_used_at": "2026-09-05T00:00:00Z", "standby_ready_at": "2026-09-05T00:10:00Z", "standby_expires_at": "2026-09-12T00:10:00Z"},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{"ID", "PATH", "HOT", "LAST USED", "STANDBY READY", "STANDBY EXPIRES", "r1 /repo/a true", "09/05 09:10", "09/12 09:10"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose repository table missing %q:\n%s", want, got)
		}
	}
	// 表の見出しにタイムゾーンをまとめたので、列見出し側では繰り返さない。
	if !strings.Contains(got, "Repositories ("+statusZoneLabel()+")") || strings.Contains(got, "LAST USED ("+statusZoneLabel()+")") {
		t.Fatalf("verbose repository table did not label the zone once:\n%s", got)
	}
	// 縦積みをやめたので、1 repository あたりの見出しは出さない。
	if strings.Contains(got, "Repository 1") {
		t.Fatalf("verbose repository table kept the per-repository layout:\n%s", got)
	}
	// 未知キーは列にできないので Additional へ落とす。
	if !strings.Contains(got, "repositories[1].future: kept") {
		t.Fatalf("unknown repository key was dropped:\n%s", got)
	}

	output.Reset()
	printStatusDisplay(&output, map[string]any{"schema_version": 19, "repository_details": []map[string]any{}}, true)
	if got := output.String(); !strings.Contains(got, "(none)") {
		t.Fatalf("empty repository details did not render (none):\n%s", got)
	}

	output.Reset()
	printStatusDisplay(&output, map[string]any{"schema_version": 19, "worktree_roots": []map[string]any{}}, true)
	if got := output.String(); !strings.Contains(got, "(unset)") {
		t.Fatalf("missing repository details did not render (unset):\n%s", got)
	}
}

// TestPrintVerboseStatusListsQuarantineAsATable は Quarantine の表の列と並び順を固定する。
func TestPrintVerboseStatusListsQuarantineAsATable(t *testing.T) {
	payload := map[string]any{
		// 構造化応答として扱わせるため、空でも表の対象になるキーを 1 つ添える。
		"schema_version": 19, "worktree_roots": []map[string]any{},
		"quarantine": []map[string]any{
			{"id": "q2", "path": "/bad/two", "kind": "slot", "failure_code": "PREPARE_AMBIGUOUS", "future": "kept"},
			{"id": "q1", "path": "/bad/one", "kind": "slot", "failure_code": "OWNERSHIP"},
			{"id": "q3", "path": "/bad/three"},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true)
	got := output.String()
	for _, want := range []string{"ID KIND REASON", "PATH", "q1 slot OWNERSHIP", "q2 slot PREPARE_AMBIGUOUS", "q3 —"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose quarantine table missing %q:\n%s", want, got)
		}
	}
	// kind・reason の順に並べるので、failure_code を持たない行は kind の "" が先に来る。
	if !strings.Contains(got, "(unset)") {
		t.Fatalf("quarantine row without failure_code lost its reason:\n%s", got)
	}
	if strings.Index(got, "q3 ") > strings.Index(got, "q1 ") {
		t.Fatalf("quarantine rows were not sorted by kind:\n%s", got)
	}
	// 理由ごとの見出しをやめたので、グループ行は出さない。
	if strings.Contains(got, "Reason: ") {
		t.Fatalf("verbose quarantine table kept the grouped layout:\n%s", got)
	}
	if !strings.Contains(got, "quarantine[0].future: kept") {
		t.Fatalf("unknown quarantine key was dropped:\n%s", got)
	}
	// slot は wx clear で消せるので、表の下に削除手段を案内する。
	if !strings.Contains(got, "slot: wx clear deletes quarantined slots") {
		t.Fatalf("quarantine table did not point at wx clear:\n%s", got)
	}
	if strings.Contains(got, "wx prune") {
		t.Fatalf("quarantine table pointed at wx prune without unknown_refs rows:\n%s", got)
	}

	// unknown_refs は wx prune で消せるが、削除コマンドが無い mismatched_refs には案内を出さない。
	output.Reset()
	printStatusDisplay(&output, map[string]any{
		"schema_version": 19, "worktree_roots": []map[string]any{},
		"quarantine": []map[string]any{
			{"id": "", "path": "repo:refs/wx/recovery/a/b/head", "kind": "unknown_refs", "failure_code": "recovery ref is not explained by the current database"},
			{"id": "", "path": "repo:refs/wx/recovery/a/b/index", "kind": "mismatched_refs", "failure_code": "recovery ref points at an object the current database does not expect"},
		},
	}, true)
	got = output.String()
	if !strings.Contains(got, "unknown_refs: wx prune deletes the recovery refs") {
		t.Fatalf("quarantine table did not point at wx prune:\n%s", got)
	}
	if strings.Contains(got, "mismatched_refs: wx") {
		t.Fatalf("quarantine table invented a command for mismatched_refs:\n%s", got)
	}

	output.Reset()
	printStatusDisplay(&output, map[string]any{"schema_version": 19, "worktree_roots": []map[string]any{}, "quarantine": []map[string]any{}}, true)
	if got := output.String(); !strings.Contains(got, "Quarantine\n") || !strings.Contains(got, "(none)") {
		t.Fatalf("empty quarantine did not render (none):\n%s", got)
	}
}
