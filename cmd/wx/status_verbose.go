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
		rows = append(rows, []string{statusValue(item, "id"), statusHomeValue(item, "root"), statusWorkspacePolicy(r.payload, item), statusValue(item, "generation"), statusValue(item, "repositories"), statusValue(item, "ready"), statusValue(item, "leased"), statusValue(item, "failed"), statusWorkspaceLastUsedVerbose(r.payload, item)})
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("workspaces[%d]", index), item, map[string]bool{"id": true, "root": true, "policy": true, "generation": true, "repositories": true, "ready": true, "leased": true, "failed": true, "last_used_at": true})
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

func (r *verboseStatusRenderer) renderRepositories() {
	r.line("")
	r.line("Repositories")
	value, present := r.payload["repository_details"]
	items := statusObjectsSortedBy(statusObjectList(value), "main_path")
	r.mark("repository_details")
	if len(items) == 0 {
		if present {
			r.line("  (none)")
		} else {
			r.line("  (unset)")
		}
	}
	for index, item := range items {
		r.line(fmt.Sprintf("  Repository %d", index+1))
		r.field("    ID", statusValue(item, "id"))
		r.field("    Path", statusHomeValue(item, "main_path"))
		r.field("    Hot", statusValue(item, "hot"))
		r.field("    Last used", statusValue(item, "last_used_at"))
		r.field("    Standby ready", statusValue(item, "standby_ready_at"))
		r.field("    Standby expires", statusValue(item, "standby_expires_at"))
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("repositories[%d]", index), item, map[string]bool{"id": true, "main_path": true, "hot": true, "last_used_at": true, "standby_ready_at": true, "standby_expires_at": true})
	}
}

func (r *verboseStatusRenderer) renderSessions() {
	r.line("")
	r.line("Sessions")
	value, present := r.payload["session_details"]
	items := statusObjectsSortedBy(statusObjectList(value), "created_at")
	r.mark("session_details")
	if len(items) == 0 {
		if present {
			r.line("  (none)")
		} else {
			r.line("  (unset)")
		}
	}
	for index, item := range items {
		r.line(fmt.Sprintf("  Session %d", index+1))
		r.field("    ID", statusValue(item, "id"))
		r.field("    Agent", statusValue(item, "agent"))
		r.field("    State", statusValue(item, "state"))
		r.field("    Created", statusValue(item, "created_at"))
		if age, ok := statusInt(item, "age_seconds"); ok {
			r.field("    Elapsed", formatDurationSeconds(age))
		} else {
			r.field("    Elapsed", "—")
		}
		r.field("    Base OIDs", statusValue(item, "base_oids"))
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("sessions[%d]", index), item, map[string]bool{"id": true, "agent": true, "state": true, "created_at": true, "age_seconds": true, "base_oids": true})
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
		if pendingOK && runningOK && failedOK {
			r.field("  Total", strconv.FormatInt(pending+running+failed, 10))
		} else {
			r.field("  Total", "—")
		}
		r.field("  Queued", statusValue(r.payload, "queued_jobs"))
		r.field("  Pending", statusValue(jobs, "pending"))
		r.field("  Running", statusValue(jobs, "running"))
		r.field("  Failed", statusValue(jobs, "failed"))
		r.additional = appendStatusUnknown(r.additional, "job_details", jobs, map[string]bool{"pending": true, "running": true, "failed": true})
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

type statusQuarantineGroup struct {
	kind   string
	reason string
	items  []map[string]any
}

func (g *statusQuarantineGroup) label() string {
	if g.kind == "" {
		return g.reason
	}
	return g.kind + " / " + g.reason
}

func (r *verboseStatusRenderer) renderQuarantine() {
	r.line("")
	r.line("Quarantine")
	value, present := r.payload["quarantine"]
	items := statusObjectList(value)
	r.mark("quarantine")
	if len(items) == 0 {
		if present {
			r.line("  (none)")
		} else {
			r.line("  (unset)")
		}
	} else {
		groups := make(map[string]*statusQuarantineGroup)
		for _, item := range items {
			kind, reason := statusValueRaw(item, "kind"), statusQuarantineReason(item)
			key := kind + "\x00" + reason
			group := groups[key]
			if group == nil {
				group = &statusQuarantineGroup{kind: kind, reason: reason}
				groups[key] = group
			}
			group.items = append(group.items, item)
		}
		ordered := make([]*statusQuarantineGroup, 0, len(groups))
		for _, group := range groups {
			sort.SliceStable(group.items, func(i, j int) bool {
				left, right := statusValueRaw(group.items[i], "path"), statusValueRaw(group.items[j], "path")
				if left == right {
					return statusValueRaw(group.items[i], "id") < statusValueRaw(group.items[j], "id")
				}
				return left < right
			})
			ordered = append(ordered, group)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].label() < ordered[j].label() })
		for _, group := range ordered {
			r.line(fmt.Sprintf("  Reason: %s (%d)", group.label(), len(group.items)))
			for _, item := range group.items {
				r.field("    ID", statusValue(item, "id"))
				r.field("    Path", statusHomeValue(item, "path"))
			}
		}
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
			"workspace_id": true, "root": true, "generation": true, "reason": true, "detail": true, "suspended_at": true, "action": true,
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
