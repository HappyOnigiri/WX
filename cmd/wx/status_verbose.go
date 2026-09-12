package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// verboseStatusRenderer は RPC map を変更せず、既知項目の後ろに未対応項目を追加する状態を持つ。
type verboseStatusRenderer struct {
	w          io.Writer
	payload    map[string]any
	knownTop   map[string]bool
	additional []displayPair
}

// printVerboseStatus は診断応答を運用上のまとまりに分け、全ての項目を失わずに表示する。
func printVerboseStatus(w io.Writer, payload map[string]any) {
	renderer := &verboseStatusRenderer{w: w, payload: payload, knownTop: map[string]bool{}}
	renderer.renderWorkspaces()
	renderer.renderRepositories()
	renderer.renderSessions()
	renderer.renderDaemon()
	renderer.renderConfig()
	renderer.renderBackup()
	renderer.renderPool()
	renderer.renderJobs()
	renderer.renderSnapshots()
	renderer.renderStorage()
	renderer.renderRetention()
	renderer.renderQuarantine()
	renderer.renderStandbyReplenishment()
	renderer.renderAdditional()
}

func (r *verboseStatusRenderer) line(value string) { writeStatusLine(r.w, value) }

func (r *verboseStatusRenderer) field(label, value string) { writeStatusField(r.w, label, value) }

func (r *verboseStatusRenderer) mark(keys ...string) {
	for _, key := range keys {
		r.knownTop[key] = true
	}
}

func (r *verboseStatusRenderer) renderWorkspaces() {
	r.line("Workspaces")
	value, present := r.payload["workspace_details"]
	items := statusObjectsSortedBy(statusObjectList(value), "root")
	r.mark("workspace_details")
	rows := make([][]string, 0, len(items))
	for index, item := range items {
		rows = append(rows, []string{statusValue(item, "id"), statusHomeValue(item, "root"), statusWorkspacePolicy(r.payload, item), statusValue(item, "generation"), statusWorkspaceRepositories(item), statusValue(item, "ready"), statusValue(item, "leased"), statusValue(item, "failed"), statusWorkspaceLastUsedVerbose(r.payload, item)})
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("workspaces[%d]", index), item, map[string]bool{"id": true, "root": true, "kind": true, "policy": true, "generation": true, "repositories": true, "repository_memberships": true, "repository_count": true, "ready": true, "leased": true, "failed": true, "last_used_at": true})
	}
	// verbose は登録の診断が目的のため、要約と違い worktree を使わない workspace も残す。
	r.lineTable([]string{"ID", "PATH", "POLICY", "GENERATION", "REPOSITORIES", "READY", "IN USE", "FAILED (FAILED + QUARANTINED)", "LAST USED"}, rows, present)
	if notice := statusWorkspaceLastUsedNotice(r.payload); notice != "" {
		r.line("  " + notice)
	}
	if notice := statusWorkspacePolicyNotice(r.payload); notice != "" {
		r.line("  " + notice)
	}
}

// statusWorkspaceRepositories は旧 count と v2 membership 配列のどちらからも表示用件数を作る。
func statusWorkspaceRepositories(item map[string]any) string {
	if count, ok := statusInt(item, "repository_count"); ok {
		return strconv.FormatInt(count, 10)
	}
	if count, ok := statusInt(item, "repositories"); ok {
		return strconv.FormatInt(count, 10)
	}
	if members, ok := item["repositories"].([]any); ok {
		return strconv.Itoa(len(members))
	}
	if members, ok := item["repository_memberships"].([]any); ok {
		return strconv.Itoa(len(members))
	}
	return "—"
}

func (r *verboseStatusRenderer) lineTable(headers []string, rows [][]string, present bool) {
	writeStatusTable(r.w, headers, rows)
	if len(rows) == 0 {
		if present {
			r.line("  (none)")
		} else {
			r.line("  (unset)")
		}
	}
}

// renderRepositories は repository を 1 行ずつの表にする。
// 時刻列が 3 つあり列見出しごとにタイムゾーンを繰り返すと表が横に広がるため、見出し行にまとめて添える。
func (r *verboseStatusRenderer) renderRepositories() {
	r.line("")
	r.line("Repositories (" + statusZoneLabel() + ")")
	value, present := r.payload["repository_details"]
	items := statusObjectsSortedBy(statusObjectList(value), "main_path")
	r.mark("repository_details")
	rows := make([][]string, 0, len(items))
	for index, item := range items {
		rows = append(rows, []string{
			statusValue(item, "id"), statusHomeValue(item, "main_path"), statusValue(item, "hot"),
			statusLocalDate(statusValueRaw(item, "last_used_at")), statusLocalDate(statusValueRaw(item, "standby_ready_at")), statusLocalDate(statusValueRaw(item, "standby_expires_at")),
		})
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("repositories[%d]", index), item, map[string]bool{"id": true, "main_path": true, "hot": true, "last_used_at": true, "standby_ready_at": true, "standby_expires_at": true})
	}
	r.lineTable([]string{"ID", "PATH", "HOT", "LAST USED", "STANDBY READY", "STANDBY EXPIRES"}, rows, present)
}

// renderSessions は daemon が絞り込んだ非終端 session を 1 行ずつ出し、その後ろに ARCHIVED の集計を添える。
func (r *verboseStatusRenderer) renderSessions() {
	r.line("")
	r.line("Sessions")
	value, present := r.payload["session_details"]
	items := statusObjectsSortedBy(statusObjectList(value), "created_at")
	r.mark("session_details", "archived_session_details")
	rows := make([][]string, 0, len(items))
	for index, item := range items {
		elapsed := "—"
		if age, ok := statusInt(item, "age_seconds"); ok {
			elapsed = humanDurationSeconds(age)
		}
		rows = append(rows, []string{statusValue(item, "id"), statusValue(item, "agent"), statusValue(item, "state"), statusLocalDate(statusValueRaw(item, "created_at")), elapsed})
		// base_oids は列に出すと 1 行が長くなりすぎるため表から外すが、Additional へ落ちないよう既知キーとして残す。
		// appendStatusUnknown は描画の有無ではなく known map への登録だけを見るためである。
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("sessions[%d]", index), item, map[string]bool{"id": true, "agent": true, "state": true, "created_at": true, "age_seconds": true, "base_oids": true})
	}
	r.lineTable([]string{"ID", "AGENT", "STATE", "CREATED (" + statusZoneLabel() + ")", "ELAPSED"}, rows, present)
	r.renderArchivedSessions()
}

// renderArchivedSessions は復元待ちで保持している ARCHIVED session を、件数と保持期間の両端だけの 1 行にまとめる。
func (r *verboseStatusRenderer) renderArchivedSessions() {
	value, present := r.payload["archived_session_details"]
	archived, isMap := value.(map[string]any)
	switch {
	case isMap && len(archived) > 0:
		r.field("  Archived", fmt.Sprintf("%s (earliest archived %s, latest expiry %s)", statusValue(archived, "count"),
			statusDash(statusValueRaw(archived, "earliest_archived_at")), statusDash(statusValueRaw(archived, "latest_expires_at"))))
		r.additional = appendStatusUnknown(r.additional, "archived_session_details", archived, map[string]bool{"count": true, "earliest_archived_at": true, "latest_expires_at": true})
	case present:
		r.field("  Archived", "(none)")
	case statusArchivedSessionsUnavailable(r.payload):
		r.field("  Archived", "unknown")
		r.line("  " + statusArchivedSessionNotice(r.payload))
	default:
		r.field("  Archived", "—")
	}
}

func (r *verboseStatusRenderer) renderDaemon() {
	r.line("")
	r.line("Daemon")
	r.mark("schema_version", "db_schema_version", "daemon_version", "protocol_version", "pid", "uptime_seconds", "degraded", "error", "database_path", "restart_pending", "stop_pending")
	r.field("  Version", statusValue(r.payload, "daemon_version"))
	r.field("  Protocol version", statusValue(r.payload, "protocol_version"))
	r.field("  PID", statusValue(r.payload, "pid"))
	if uptime, ok := statusInt(r.payload, "uptime_seconds"); ok {
		r.field("  Uptime", formatDurationSeconds(uptime))
	} else {
		r.field("  Uptime", "—")
	}
	r.field("  Degraded", statusValue(r.payload, "degraded"))
	r.field("  Restart pending", statusValue(r.payload, "restart_pending"))
	r.field("  Stop pending", statusValue(r.payload, "stop_pending"))
	if _, ok := r.payload["error"]; ok {
		r.field("  Error", statusValue(r.payload, "error"))
	}
	if _, ok := r.payload["database_path"]; ok {
		r.field("  Database", statusHomeValue(r.payload, "database_path"))
	}
}

func (r *verboseStatusRenderer) renderConfig() {
	r.line("")
	r.line("Config")
	r.mark("config_path", "config_last_reload", "config_reload_error", "worktree_root_error")
	r.field("  JSON schema version", statusValue(r.payload, "schema_version"))
	r.field("  DB schema version", statusValue(r.payload, "db_schema_version"))
	r.field("  Path", statusHomeValue(r.payload, "config_path"))
	r.field("  Last reload", statusValue(r.payload, "config_last_reload"))
	r.field("  Reload error", statusValue(r.payload, "config_reload_error"))
	r.field("  Worktree root error", statusValue(r.payload, "worktree_root_error"))
}

func (r *verboseStatusRenderer) renderBackup() {
	r.line("")
	r.line("Backup")
	r.mark("sqlite_last_backup", "sqlite_backup_error")
	r.field("  Last backup", statusValue(r.payload, "sqlite_last_backup"))
	r.field("  Error", statusValue(r.payload, "sqlite_backup_error"))
}

func (r *verboseStatusRenderer) renderPool() {
	r.line("")
	r.line("Pool")
	r.mark("workspaces", "repositories", "slots", "active_sessions", "snapshots", "queued_jobs")
	r.field("  Workspaces", statusValue(r.payload, "workspaces"))
	r.field("  Repositories", statusValue(r.payload, "repositories"))
	r.field("  Active sessions", statusValue(r.payload, "active_sessions"))
	r.field("  Snapshots", statusValue(r.payload, "snapshots"))
	slots, present := r.payload["slots"]
	slotMap, isMap := slots.(map[string]any)
	switch {
	case isMap && len(slotMap) > 0:
		r.field("  Slots ready", statusValue(slotMap, "ready"))
		r.field("  Slots in use", statusValue(slotMap, "leased"))
		r.field("  Slots failed", statusValue(slotMap, "failed"))
		r.field("  Slots quarantined", statusValue(slotMap, "quarantined"))
		r.additional = appendStatusUnknown(r.additional, "slots", slotMap, map[string]bool{"ready": true, "leased": true, "failed": true, "quarantined": true})
	case present:
		r.field("  Slots", statusRawValue(slots))
	default:
		r.field("  Slots", "—")
	}
}

func (r *verboseStatusRenderer) renderJobs() {
	r.line("")
	r.line("Jobs")
	value, present := r.payload["job_details"]
	jobs, isMap := value.(map[string]any)
	r.mark("job_details")
	switch {
	case isMap && len(jobs) > 0:
		pending, pendingOK := statusInt(jobs, "pending")
		running, runningOK := statusInt(jobs, "running")
		failed, failedOK := statusInt(jobs, "failed")
		// discarded は failed と排他の内訳なので Total へ加える。持たない旧 payload では failed 側に含まれており、0 として足しても総数は変わらない。
		discarded, _ := statusInt(jobs, "discarded")
		if pendingOK && runningOK && failedOK {
			r.field("  Total", strconv.FormatInt(pending+running+failed+discarded, 10))
		} else {
			r.field("  Total", "—")
		}
		r.field("  Queued", statusValue(r.payload, "queued_jobs"))
		r.field("  Pending", statusValue(jobs, "pending"))
		r.field("  Running", statusValue(jobs, "running"))
		r.field("  Failed", statusValue(jobs, "failed"))
		r.field("  Discarded", statusValue(jobs, "discarded"))
		r.additional = appendStatusUnknown(r.additional, "job_details", jobs, map[string]bool{"pending": true, "running": true, "failed": true, "discarded": true})
	case present:
		r.field("  Details", "(none)")
		r.field("  Queued", statusValue(r.payload, "queued_jobs"))
	default:
		r.field("  Queued", statusValue(r.payload, "queued_jobs"))
	}
}

func (r *verboseStatusRenderer) renderSnapshots() {
	r.line("")
	r.line("Snapshots")
	value, present := r.payload["snapshot_details"]
	snapshots, isMap := value.(map[string]any)
	r.mark("snapshot_details")
	switch {
	case isMap && len(snapshots) > 0:
		r.field("  Total", statusValue(snapshots, "count"))
		r.field("  Earliest expiry", statusValue(snapshots, "earliest_expiry"))
		r.additional = appendStatusUnknown(r.additional, "snapshot_details", snapshots, map[string]bool{"count": true, "earliest_expiry": true})
	case present:
		r.field("  Details", "(none)")
	default:
		r.field("  Total", statusValue(r.payload, "snapshots"))
	}
}

func (r *verboseStatusRenderer) renderStorage() {
	r.line("")
	r.line("Storage")
	value, present := r.payload["worktree_roots"]
	roots := statusObjectsSortedBy(statusObjectList(value), "path")
	r.mark("worktree_roots")
	if len(roots) == 0 {
		if present {
			r.line("  (none)")
		} else {
			r.line("  (unset)")
		}
	}
	for index, root := range roots {
		r.line(fmt.Sprintf("  Root %d", index+1))
		r.field("    Path", statusHomeValue(root, "path"))
		r.field("    Active", statusValue(root, "active"))
		r.field("    Logical size", statusExactBytes(root, "bytes"))
		// Disk size は要約の Disk と同じ量で、Allocated と Shared はその内訳として du との差を説明するために出す。
		r.field("    Disk size", statusExactBytes(root, "exclusive_bytes"))
		r.field("    Allocated", statusExactBytes(root, "allocated_bytes"))
		r.field("    Shared", statusExactBytes(root, "shared_bytes"))
		r.field("    Measurement", statusValue(root, "measurement"))
		r.field("    Measured at", statusValue(root, "measured_at"))
		r.field("    Error", statusValue(root, "error"))
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("worktree_roots[%d]", index), root, map[string]bool{"path": true, "active": true, "bytes": true, "allocated_bytes": true, "shared_bytes": true, "exclusive_bytes": true, "measurement": true, "measured_at": true, "error": true})
	}
}

func (r *verboseStatusRenderer) renderRetention() {
	r.line("")
	r.line("Retention")
	value, present := r.payload["retention_seconds"]
	retention, isMap := value.(map[string]any)
	r.mark("retention_seconds")
	keys := []string{"hot_standby", "ended_worktree", "quarantined", "recovery_snapshot", "expired_session_tombstone", "failed_job", "event_log", "lease_ttl"}
	known := map[string]bool{}
	for _, key := range keys {
		known[key] = true
	}
	if !isMap {
		if present {
			r.line("  " + statusRawValue(value))
		} else {
			r.line("  (unset)")
		}
		return
	}
	if len(retention) == 0 {
		r.line("  (none)")
		return
	}
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		if item, ok := retention[key]; ok {
			fields = append(fields, key+": "+formatRetentionValue(item))
		} else {
			fields = append(fields, key+": —")
		}
	}
	for len(fields) > 0 {
		count := 2
		if len(fields) < count {
			count = len(fields)
		}
		r.line("  " + strings.Join(fields[:count], "    "))
		fields = fields[count:]
	}
	r.additional = appendStatusUnknown(r.additional, "retention_seconds", retention, known)
}

// renderQuarantine は隔離された実体を 1 行ずつの表にする。
// 同じ kind・reason が並ぶと見分けにくいため、行は kind・reason・path・id の順に並べる。
func (r *verboseStatusRenderer) renderQuarantine() {
	r.line("")
	r.line("Quarantine")
	value, present := r.payload["quarantine"]
	items := statusObjectList(value)
	r.mark("quarantine")
	sorted := append([]map[string]any(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left := []string{statusValueRaw(sorted[i], "kind"), statusQuarantineReason(sorted[i]), statusValueRaw(sorted[i], "path"), statusValueRaw(sorted[i], "id")}
		right := []string{statusValueRaw(sorted[j], "kind"), statusQuarantineReason(sorted[j]), statusValueRaw(sorted[j], "path"), statusValueRaw(sorted[j], "id")}
		for index := range left {
			if left[index] != right[index] {
				return left[index] < right[index]
			}
		}
		return false
	})
	rows := make([][]string, 0, len(sorted))
	for _, item := range sorted {
		// quarantined_artifacts 由来の行は slot ではないので id が空である。列には "(empty)" ではなく欠測と同じ記号を出す。
		id, _ := statusRawString(item, "id")
		kind, _ := statusRawString(item, "kind")
		rows = append(rows, []string{statusDash(id), statusDash(kind), statusQuarantineReason(item), statusHomeValue(item, "path")})
	}
	r.lineTable([]string{"ID", "KIND", "REASON", "PATH"}, rows, present)
	for _, notice := range statusQuarantineCleanupNotices(sorted) {
		r.line("  " + notice)
	}
	for index, item := range items {
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("quarantine[%d]", index), item, map[string]bool{"id": true, "path": true, "kind": true, "failure_code": true})
	}
}

func (r *verboseStatusRenderer) renderStandbyReplenishment() {
	value := r.payload["standby_replenishment"]
	items := statusObjectsSortedBy(statusObjectList(value), "root")
	r.mark("standby_replenishment")
	if len(items) == 0 {
		return
	}
	r.line("")
	r.line("Standby replenishment")
	for index, item := range items {
		r.field("  Path", statusHomeValue(item, "root"))
		r.field("  Generation", statusValue(item, "generation"))
		r.field("  Reason", statusValue(item, "reason"))
		r.field("  Detail", statusValue(item, "detail"))
		r.field("  Suspended", statusValue(item, "suspended_at"))
		// 補充計画の失敗は停止行ではないため、停止時刻の代わりに失敗時刻を持つ。
		if failedAt, _ := statusRawString(item, "failed_at"); failedAt != "" {
			r.field("  Failed", failedAt)
		}
		r.field("  Action", statusValue(item, "action"))
		// 準備失敗で止まった停止だけが失敗情報を持つ。`wx clear` による停止では行を作らない。
		for _, failure := range []struct{ label, key string }{
			{"  Failure code", "failure_code"}, {"  Failure reason", "failure_message"}, {"  Failure log", "detail_path"},
		} {
			if value, _ := statusRawString(item, failure.key); value != "" {
				r.field(failure.label, value)
			}
		}
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("standby_replenishment[%d]", index), item, map[string]bool{
			"workspace_id": true, "root": true, "generation": true, "reason": true, "detail": true, "suspended_at": true, "failed_at": true, "action": true,
			"failure_code": true, "failure_message": true, "detail_path": true,
		})
	}
}

func (r *verboseStatusRenderer) renderAdditional() {
	for key, value := range r.payload {
		if !r.knownTop[key] {
			r.additional = appendDisplayPairs(r.additional, key, value)
		}
	}
	if len(r.additional) == 0 {
		return
	}
	sort.SliceStable(r.additional, func(i, j int) bool { return r.additional[i].key < r.additional[j].key })
	r.line("")
	r.line("Additional")
	for _, pair := range r.additional {
		r.field("  "+pair.key, pair.value)
	}
}
