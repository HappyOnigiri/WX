package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// verboseStatusRenderer は RPC map を変更せず、既知項目の後ろに未対応項目を追加する状態を持つ。
// 固定文の解決は text が持ち、行の値はここでは訳さない。
type verboseStatusRenderer struct {
	text       *textRenderer
	payload    map[string]any
	knownTop   map[string]bool
	additional []displayPair
}

// printVerboseStatus は診断応答を運用上のまとまりに分け、全ての項目を失わずに表示する。
func printVerboseStatus(r *textRenderer, payload map[string]any) {
	renderer := &verboseStatusRenderer{text: r, payload: payload, knownTop: map[string]bool{}}
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

func (r *verboseStatusRenderer) mark(keys ...string) {
	for _, key := range keys {
		r.knownTop[key] = true
	}
}

func (r *verboseStatusRenderer) renderWorkspaces() {
	r.text.line("status.section.workspaces", nil)
	value, present := r.payload["workspace_details"]
	items := statusObjectsSortedBy(statusObjectList(value), "root")
	r.mark("workspace_details")
	rows := make([][]string, 0, len(items))
	for index, item := range items {
		rows = append(rows, []string{statusValue(item, "id"), statusHomeValue(item, "root"), statusWorkspacePolicy(r.payload, item), statusValue(item, "generation"), statusWorkspaceRepositories(item), statusValue(item, "ready"), statusValue(item, "leased"), statusValue(item, "failed"), statusWorkspaceLastUsedVerbose(r.payload, item)})
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("workspaces[%d]", index), item, map[string]bool{"id": true, "root": true, "kind": true, "policy": true, "generation": true, "repositories": true, "repository_memberships": true, "repository_count": true, "ready": true, "leased": true, "failed": true, "last_used_at": true})
	}
	// verbose は登録の診断が目的のため、要約と違い worktree を使わない workspace も残す。
	r.lineTable(r.headers("status.table.id", "status.table.path", "status.table.policy", "status.table.generation",
		"status.table.repositories", "status.table.ready", "status.table.in_use", "status.table.failed_detail", "status.table.last_used"), rows, present)
	if notice := statusWorkspaceLastUsedNotice(r.text, r.payload); notice != "" {
		r.text.raw("  " + notice)
	}
	if notice := statusWorkspacePolicyNotice(r.text, r.payload); notice != "" {
		r.text.raw("  " + notice)
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

// headers は表の見出しを桁計算より前に解決する。
func (r *verboseStatusRenderer) headers(ids ...string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.text.Localize(id, nil))
	}
	return out
}

func (r *verboseStatusRenderer) lineTable(headers []string, rows [][]string, present bool) {
	writeStatusTable(r.text, headers, rows)
	if len(rows) == 0 {
		if present {
			r.text.raw("  (none)")
		} else {
			r.text.raw("  (unset)")
		}
	}
}

// renderRepositories は repository を 1 行ずつの表にする。
// 時刻列が 3 つあり列見出しごとにタイムゾーンを繰り返すと表が横に広がるため、見出し行にまとめて添える。
func (r *verboseStatusRenderer) renderRepositories() {
	r.text.raw("")
	r.text.line("status.section.repositories", map[string]any{"Zone": statusZoneLabel()})
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
	r.lineTable(r.headers("status.table.id", "status.table.path", "status.table.hot", "status.table.last_used",
		"status.table.standby_ready", "status.table.standby_expires"), rows, present)
}

// renderSessions は daemon が絞り込んだ非終端 session を 1 行ずつ出し、その後ろに ARCHIVED の集計を添える。
func (r *verboseStatusRenderer) renderSessions() {
	r.text.raw("")
	r.text.line("status.section.sessions", nil)
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
	r.lineTable([]string{
		r.text.Localize("status.table.id", nil), r.text.Localize("status.table.agent", nil),
		r.text.Localize("status.table.state", nil), r.text.Localize("status.table.created_zone", map[string]any{"Zone": statusZoneLabel()}),
		r.text.Localize("status.table.elapsed", nil),
	}, rows, present)
	r.renderArchivedSessions()
}

// renderArchivedSessions は復元待ちで保持している ARCHIVED session を、件数と保持期間の両端だけの 1 行にまとめる。
func (r *verboseStatusRenderer) renderArchivedSessions() {
	value, present := r.payload["archived_session_details"]
	archived, isMap := value.(map[string]any)
	switch {
	case isMap && len(archived) > 0:
		r.text.field(2, "status.field.archived", r.text.Localize("status.summary.archived", map[string]any{
			"Count": statusValue(archived, "count"), "Earliest": statusDash(statusValueRaw(archived, "earliest_archived_at")),
			"Latest": statusDash(statusValueRaw(archived, "latest_expires_at")),
		}))
		r.additional = appendStatusUnknown(r.additional, "archived_session_details", archived, map[string]bool{"count": true, "earliest_archived_at": true, "latest_expires_at": true})
	case present:
		r.text.field(2, "status.field.archived", "(none)")
	case statusArchivedSessionsUnavailable(r.payload):
		r.text.field(2, "status.field.archived", "unknown")
		r.text.raw("  " + statusArchivedSessionNotice(r.text, r.payload))
	default:
		r.text.field(2, "status.field.archived", "—")
	}
}

func (r *verboseStatusRenderer) renderDaemon() {
	r.text.raw("")
	r.text.line("status.section.daemon", nil)
	r.mark("schema_version", "db_schema_version", "daemon_version", "protocol_version", "pid", "uptime_seconds", "degraded", "error", "database_path", "restart_pending", "stop_pending")
	r.text.field(2, "status.field.version", statusValue(r.payload, "daemon_version"))
	r.text.field(2, "status.field.protocol_version", statusValue(r.payload, "protocol_version"))
	r.text.field(2, "status.field.pid", statusValue(r.payload, "pid"))
	if uptime, ok := statusInt(r.payload, "uptime_seconds"); ok {
		r.text.field(2, "status.field.uptime", formatDurationSeconds(uptime))
	} else {
		r.text.field(2, "status.field.uptime", "—")
	}
	r.text.field(2, "status.field.degraded", statusValue(r.payload, "degraded"))
	r.text.field(2, "status.field.restart_pending", statusValue(r.payload, "restart_pending"))
	r.text.field(2, "status.field.stop_pending", statusValue(r.payload, "stop_pending"))
	if _, ok := r.payload["error"]; ok {
		r.text.field(2, "status.field.error", statusValue(r.payload, "error"))
	}
	if _, ok := r.payload["database_path"]; ok {
		r.text.field(2, "status.field.database", statusHomeValue(r.payload, "database_path"))
	}
}

func (r *verboseStatusRenderer) renderConfig() {
	r.text.raw("")
	r.text.line("status.section.config", nil)
	r.mark("config_path", "config_last_reload", "config_reload_error", "worktree_root_error")
	r.text.field(2, "status.field.json_schema_version", statusValue(r.payload, "schema_version"))
	r.text.field(2, "status.field.db_schema_version", statusValue(r.payload, "db_schema_version"))
	r.text.field(2, "status.field.path", statusHomeValue(r.payload, "config_path"))
	r.text.field(2, "status.field.last_reload", statusValue(r.payload, "config_last_reload"))
	r.text.field(2, "status.field.reload_error", statusValue(r.payload, "config_reload_error"))
	r.text.field(2, "status.field.worktree_root_error", statusValue(r.payload, "worktree_root_error"))
}

func (r *verboseStatusRenderer) renderBackup() {
	r.text.raw("")
	r.text.line("status.section.backup", nil)
	r.mark("sqlite_last_backup", "sqlite_backup_error")
	r.text.field(2, "status.field.last_backup", statusValue(r.payload, "sqlite_last_backup"))
	r.text.field(2, "status.field.error", statusValue(r.payload, "sqlite_backup_error"))
}

func (r *verboseStatusRenderer) renderPool() {
	r.text.raw("")
	r.text.line("status.section.pool", nil)
	r.mark("workspaces", "repositories", "slots", "active_sessions", "snapshots", "queued_jobs")
	r.text.field(2, "status.field.workspaces", statusValue(r.payload, "workspaces"))
	r.text.field(2, "status.field.repositories", statusValue(r.payload, "repositories"))
	r.text.field(2, "status.field.active_sessions", statusValue(r.payload, "active_sessions"))
	r.text.field(2, "status.field.snapshots", statusValue(r.payload, "snapshots"))
	slots, present := r.payload["slots"]
	slotMap, isMap := slots.(map[string]any)
	switch {
	case isMap && len(slotMap) > 0:
		r.text.field(2, "status.field.slots_ready", statusValue(slotMap, "ready"))
		r.text.field(2, "status.field.slots_in_use", statusValue(slotMap, "leased"))
		r.text.field(2, "status.field.slots_failed", statusValue(slotMap, "failed"))
		r.text.field(2, "status.field.slots_quarantined", statusValue(slotMap, "quarantined"))
		r.additional = appendStatusUnknown(r.additional, "slots", slotMap, map[string]bool{"ready": true, "leased": true, "failed": true, "quarantined": true})
	case present:
		r.text.field(2, "status.field.slots", statusRawValue(slots))
	default:
		r.text.field(2, "status.field.slots", "—")
	}
}

func (r *verboseStatusRenderer) renderJobs() {
	r.text.raw("")
	r.text.line("status.section.jobs", nil)
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
			r.text.field(2, "status.field.total", strconv.FormatInt(pending+running+failed+discarded, 10))
		} else {
			r.text.field(2, "status.field.total", "—")
		}
		r.text.field(2, "status.field.queued", statusValue(r.payload, "queued_jobs"))
		r.text.field(2, "status.field.pending", statusValue(jobs, "pending"))
		r.text.field(2, "status.field.running", statusValue(jobs, "running"))
		r.text.field(2, "status.field.failed", statusValue(jobs, "failed"))
		r.text.field(2, "status.field.discarded", statusValue(jobs, "discarded"))
		r.additional = appendStatusUnknown(r.additional, "job_details", jobs, map[string]bool{"pending": true, "running": true, "failed": true, "discarded": true})
	case present:
		r.text.field(2, "status.field.details", "(none)")
		r.text.field(2, "status.field.queued", statusValue(r.payload, "queued_jobs"))
	default:
		r.text.field(2, "status.field.queued", statusValue(r.payload, "queued_jobs"))
	}
}

func (r *verboseStatusRenderer) renderSnapshots() {
	r.text.raw("")
	r.text.line("status.section.snapshots", nil)
	value, present := r.payload["snapshot_details"]
	snapshots, isMap := value.(map[string]any)
	r.mark("snapshot_details")
	switch {
	case isMap && len(snapshots) > 0:
		r.text.field(2, "status.field.total", statusValue(snapshots, "count"))
		r.text.field(2, "status.field.earliest_expiry", statusValue(snapshots, "earliest_expiry"))
		r.additional = appendStatusUnknown(r.additional, "snapshot_details", snapshots, map[string]bool{"count": true, "earliest_expiry": true})
	case present:
		r.text.field(2, "status.field.details", "(none)")
	default:
		r.text.field(2, "status.field.total", statusValue(r.payload, "snapshots"))
	}
}

func (r *verboseStatusRenderer) renderStorage() {
	r.text.raw("")
	r.text.line("status.section.storage", nil)
	value, present := r.payload["worktree_roots"]
	roots := statusObjectsSortedBy(statusObjectList(value), "path")
	r.mark("worktree_roots")
	if len(roots) == 0 {
		if present {
			r.text.raw("  (none)")
		} else {
			r.text.raw("  (unset)")
		}
	}
	for index, root := range roots {
		r.text.indentLine(2, "status.section.root", map[string]any{"Index": index + 1})
		r.text.field(4, "status.field.path", statusHomeValue(root, "path"))
		r.text.field(4, "status.field.active", statusValue(root, "active"))
		r.text.field(4, "status.field.logical_size", statusExactBytes(root, "bytes"))
		// Disk size は要約の Disk と同じ量で、Allocated と Shared はその内訳として du との差を説明するために出す。
		r.text.field(4, "status.field.disk_size", statusExactBytes(root, "exclusive_bytes"))
		r.text.field(4, "status.field.allocated", statusExactBytes(root, "allocated_bytes"))
		// Unmanaged は予約 namespace 配下で DB が説明しない実体の割当量で、Allocated には含まれない。
		// 要約には出さず、対処（wx clear --unmanaged）を要する利用者だけが読む位置に置く。
		r.text.field(4, "status.field.unmanaged", statusExactBytes(root, "unmanaged_allocated_bytes"))
		r.text.field(4, "status.field.shared", statusExactBytes(root, "shared_bytes"))
		r.text.field(4, "status.field.measurement", statusValue(root, "measurement"))
		r.text.field(4, "status.field.measured_at", statusValue(root, "measured_at"))
		r.text.field(4, "status.field.error", statusValue(root, "error"))
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("worktree_roots[%d]", index), root, map[string]bool{"path": true, "active": true, "bytes": true, "allocated_bytes": true, "unmanaged_allocated_bytes": true, "shared_bytes": true, "exclusive_bytes": true, "measurement": true, "measured_at": true, "error": true})
	}
}

func (r *verboseStatusRenderer) renderRetention() {
	r.text.raw("")
	r.text.line("status.section.retention", nil)
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
			r.text.raw("  " + statusRawValue(value))
		} else {
			r.text.raw("  (unset)")
		}
		return
	}
	if len(retention) == 0 {
		r.text.raw("  (none)")
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
		r.text.raw("  " + strings.Join(fields[:count], "    "))
		fields = fields[count:]
	}
	r.additional = appendStatusUnknown(r.additional, "retention_seconds", retention, known)
}

// renderQuarantine は隔離された実体を 1 行ずつの表にする。
// 同じ kind・reason が並ぶと見分けにくいため、行は kind・reason・path・id の順に並べる。
func (r *verboseStatusRenderer) renderQuarantine() {
	r.text.raw("")
	r.text.line("status.section.quarantine", nil)
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
	r.lineTable(r.headers("status.table.id", "status.table.kind", "status.table.reason", "status.table.path"), rows, present)
	for _, notice := range statusQuarantineCleanupNotices(r.text, sorted) {
		r.text.raw("  " + notice)
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
	r.text.raw("")
	r.text.line("status.section.standby_replenishment", nil)
	for index, item := range items {
		r.text.field(2, "status.field.path", statusHomeValue(item, "root"))
		r.text.field(2, "status.field.generation", statusValue(item, "generation"))
		r.text.field(2, "status.field.reason", statusValue(item, "reason"))
		r.text.field(2, "status.field.detail", statusValue(item, "detail"))
		r.text.field(2, "status.field.suspended", statusValue(item, "suspended_at"))
		// 補充計画の失敗は停止行ではないため、停止時刻の代わりに失敗時刻を持つ。
		if failedAt, _ := statusRawString(item, "failed_at"); failedAt != "" {
			r.text.field(2, "status.field.failed", failedAt)
		}
		r.text.field(2, "status.field.action", statusValue(item, "action"))
		// 準備失敗で止まった停止だけが失敗情報を持つ。`wx clear` による停止では行を作らない。
		for _, failure := range []struct{ id, key string }{
			{"status.field.failure_code", "failure_code"}, {"status.field.failure_reason", "failure_message"}, {"status.field.failure_log", "detail_path"},
		} {
			if value, _ := statusRawString(item, failure.key); value != "" {
				r.text.dataField(2, r.text.Localize(failure.id, nil), value)
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
	r.text.raw("")
	r.text.line("status.section.additional", nil)
	for _, pair := range r.additional {
		r.text.dataField(2, pair.key, pair.value)
	}
}
