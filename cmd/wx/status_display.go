package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 表示に使うタイムゾーン。テストが固定の地域時刻を検査するための差し替え点である。
// グローバルなtime.Localを書き換えると、並行するgoroutineのtime.Nowと競合する。
var statusDisplayLocation = time.Local

// printStatusDisplay は wx status の人間向け表示を担当する。
// RPC payload は daemon が JSON 契約を所有するため、表示側ではコピーだけを解釈する。
func printStatusDisplay(w io.Writer, payload map[string]any, verbose bool) {
	payload = normalizeStatusPayload(payload)
	if degraded, ok := payload["degraded"].(bool); ok && degraded {
		printDegradedStatus(w, payload, verbose)
		return
	}
	if !statusPayloadLooksStructured(payload) {
		// 診断項目を返さない古い daemon や小さなテスト用 handler では、欠落値を表にせず汎用表示へ戻す。
		printDisplay(w, payload)
		return
	}
	if verbose {
		printVerboseStatus(w, payload)
		return
	}
	printStatusSummary(w, payload)
}

// normalizeStatusPayload は RPC の JSON decode 結果とテスト用の型付き診断値を同じ形に揃える。
// JSON 数値は json.Number のまま保持し、大きな値の表示で丸めない。
func normalizeStatusPayload(payload map[string]any) map[string]any {
	data, err := json.Marshal(payload)
	if err != nil {
		return payload
	}
	var normalized map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil || normalized == nil {
		return payload
	}
	return normalized
}

func statusPayloadLooksStructured(payload map[string]any) bool {
	for _, key := range []string{
		"workspace_details", "session_details", "repository_details", "job_details",
		"snapshot_details", "worktree_roots", "slots", "retention_seconds",
	} {
		if _, ok := payload[key]; ok {
			return true
		}
	}
	return false
}

func printDegradedStatus(w io.Writer, payload map[string]any, verbose bool) {
	line := "Daemon degraded"
	if message, ok := payload["error"]; ok {
		line += " · " + statusRawValue(message)
	}
	writeStatusLine(w, line)

	// degraded 応答では件数を信頼できないため、error と database_path だけを表示し、ゼロ件を補わない。
	if _, ok := payload["database_path"]; ok {
		writeStatusField(w, "Database", statusHomeValue(payload, "database_path"))
	}
	if !verbose {
		return
	}
	// degraded 応答でも受信した追加項目は保持し、欠落した件数をゼロとして生成しない。
	known := map[string]bool{"degraded": true, "error": true, "database_path": true}
	var additional []displayPair
	for key, value := range payload {
		if known[key] {
			continue
		}
		additional = appendDisplayPairs(additional, key, value)
	}
	if len(additional) == 0 {
		return
	}
	sort.SliceStable(additional, func(i, j int) bool { return additional[i].key < additional[j].key })
	writeStatusLine(w, "Additional")
	for _, pair := range additional {
		writeStatusField(w, "  "+pair.key, pair.value)
	}
}

type statusWorkspaceRow struct {
	sortKey, path, ready, leased, last string
}

func printStatusSummary(w io.Writer, payload map[string]any) {
	workspaces := statusObjectList(payload["workspace_details"])
	repositories := statusObjectsSortedBy(statusObjectList(payload["repository_details"]), "main_path")
	roots := statusObjectsSortedBy(statusObjectList(payload["worktree_roots"]), "path")

	// LAST USED は workspace と単一 repository の root が一致するときだけ対応付ける。
	// パスの包含関係から repository を推測しない。
	rows := make([]statusWorkspaceRow, 0, len(workspaces))
	for _, workspace := range workspaces {
		root, _ := statusRawString(workspace, "root")
		path := statusHomePath(root)
		if statusWorkspaceIsCurrent(workspace, roots) {
			path += " *"
		}
		row := statusWorkspaceRow{
			sortKey: root,
			path:    statusDash(path),
			ready:   statusCountOrDash(workspace, "ready"),
			leased:  statusCountOrDash(workspace, "leased"),
			last:    "—",
		}
		if repositoryCount, ok := statusInt(workspace, "repositories"); ok && repositoryCount == 1 {
			for _, repository := range repositories {
				mainPath, _ := statusRawString(repository, "main_path")
				if statusPathEqual(root, mainPath) {
					if lastUsed, ok := statusRawString(repository, "last_used_at"); ok && lastUsed != "" {
						row.last = statusLocalDate(lastUsed)
					}
					break
				}
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].sortKey == rows[j].sortKey {
			return rows[i].path < rows[j].path
		}
		return rows[i].sortKey < rows[j].sortKey
	})

	writeStatusTable(w, []string{"WORKSPACE", "READY", "IN USE", "LAST USED (" + statusZoneLabel() + ")"}, func() [][]string {
		out := make([][]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, []string{row.path, row.ready, row.leased, row.last})
		}
		return out
	}())
	if len(rows) == 0 {
		// 空の registry でも表のヘッダーを残し、(none) を件数の 0 と混同させない。
		writeStatusLine(w, "(none)")
	}

	writeStatusLine(w, "")
	writeStatusLine(w, statusDaemonSummary(payload))
	for _, root := range roots {
		writeStatusLine(w, statusDiskSummary(root))
	}
	if len(roots) == 0 {
		writeStatusLine(w, "Disk   (none)")
	}
}

func statusDaemonSummary(payload map[string]any) string {
	state := "running"
	if pending, ok := statusBool(payload, "stop_pending"); ok && pending {
		state = "stopping"
	} else if pending, ok := statusBool(payload, "restart_pending"); ok && pending {
		state = "restarting"
	}
	line := "Daemon " + state
	if jobs, ok := payload["job_details"].(map[string]any); ok {
		line += " · Jobs " + statusCountOrDash(jobs, "pending") + " pending / " + statusCountOrDash(jobs, "running") + " running / " + statusCountOrDash(jobs, "failed") + " failed"
		return line
	}
	if queued, ok := statusInt(payload, "queued_jobs"); ok {
		line += " · Jobs " + strconv.FormatInt(queued, 10) + " pending / — running / — failed"
	}
	return line
}

func statusDiskSummary(root map[string]any) string {
	path, _ := statusRawString(root, "path")
	path = statusDash(statusHomePath(path))
	if message, ok := statusRawString(root, "error"); ok && message != "" {
		return "Disk   measurement failed · " + path + " · " + message
	}
	allocated, ok := statusInt(root, "allocated_bytes")
	if !ok {
		return "Disk   measurement unavailable · " + path
	}
	return "Disk   " + formatHumanBytes(allocated) + " allocated · " + path
}

func statusWorkspaceIsCurrent(workspace map[string]any, roots []map[string]any) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	root, _ := statusRawString(workspace, "root")
	if root != "" && statusPathWithin(cwd, root) {
		return true
	}
	id, _ := statusRawString(workspace, "id")
	if id == "" {
		return false
	}
	for _, storageRoot := range roots {
		storagePath, _ := statusRawString(storageRoot, "path")
		if storagePath == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(storagePath), filepath.Clean(cwd))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
		// managed slot は <storage-root>/<workspace-id>/<slot-id> の形で保存される。
		// slot 配下の repository まで CWD が進んでいても workspace を特定できる。
		if len(parts) >= 2 && parts[0] == id {
			return true
		}
	}
	return false
}

func statusPathEqual(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func statusPathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func statusHomePath(path string) string {
	if path == "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	home = filepath.Clean(home)
	path = filepath.Clean(path)
	if path == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Join("~", rel)
	}
	return path
}

func statusDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func statusHomeValue(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return "—"
	}
	return statusHomePath(statusRawValue(value))
}

func statusLocalDate(raw string) string {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "—"
	}
	return parsed.In(statusDisplayLocation).Format("01/02 15:04")
}

func statusZoneLabel() string {
	name, _ := time.Now().In(statusDisplayLocation).Zone()
	if name == "" {
		return "LOCAL"
	}
	return name
}

func formatHumanBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	negative := value < 0
	n := float64(value)
	if negative {
		n = -n
	}
	unit := 0
	for n >= 1024 && unit < len(units)-1 {
		n /= 1024
		unit++
	}
	var number string
	if unit == 0 || n == math.Trunc(n) {
		number = strconv.FormatFloat(n, 'f', 0, 64)
	} else {
		number = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(n, 'f', 2, 64), "0"), ".")
	}
	if negative {
		number = "-" + number
	}
	return number + " " + units[unit]
}

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
		rows = append(rows, []string{statusValue(item, "id"), statusHomeValue(item, "root"), statusValue(item, "generation"), statusValue(item, "repositories"), statusValue(item, "ready"), statusValue(item, "leased"), statusValue(item, "failed")})
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("workspaces[%d]", index), item, map[string]bool{"id": true, "root": true, "generation": true, "repositories": true, "ready": true, "leased": true, "failed": true})
	}
	r.lineTable([]string{"ID", "PATH", "GENERATION", "REPOSITORIES", "READY", "IN USE", "FAILED (FAILED + QUARANTINED)"}, rows, present)
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
		r.field("    Allocated", statusExactBytes(root, "allocated_bytes"))
		r.field("    Measurement", statusValue(root, "measurement"))
		r.field("    Error", statusValue(root, "error"))
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("worktree_roots[%d]", index), root, map[string]bool{"path": true, "active": true, "bytes": true, "allocated_bytes": true, "measurement": true, "error": true})
	}
}

func (r *verboseStatusRenderer) renderRetention() {
	r.line("")
	r.line("Retention")
	value, present := r.payload["retention_seconds"]
	retention, isMap := value.(map[string]any)
	r.mark("retention_seconds")
	keys := []string{"hot_standby", "ended_worktree", "recovery_snapshot", "expired_session_tombstone", "failed_job", "event_log"}
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
	reason string
	items  []map[string]any
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
			reason := statusQuarantineReason(item)
			group := groups[reason]
			if group == nil {
				group = &statusQuarantineGroup{reason: reason}
				groups[reason] = group
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
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].reason < ordered[j].reason })
		for _, group := range ordered {
			r.line(fmt.Sprintf("  Reason: %s (%d)", group.reason, len(group.items)))
			for _, item := range group.items {
				r.field("    ID", statusValue(item, "id"))
				r.field("    Path", statusHomeValue(item, "path"))
			}
		}
	}
	for index, item := range items {
		r.additional = appendStatusUnknown(r.additional, fmt.Sprintf("quarantine[%d]", index), item, map[string]bool{"id": true, "path": true, "failure_code": true})
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

func statusObjectList(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func statusObjectsSortedBy(objects []map[string]any, key string) []map[string]any {
	if len(objects) < 2 {
		return objects
	}
	out := append([]map[string]any(nil), objects...)
	sort.SliceStable(out, func(i, j int) bool {
		left := statusValueRaw(out[i], key)
		right := statusValueRaw(out[j], key)
		if left == right {
			return statusValueRaw(out[i], "id") < statusValueRaw(out[j], "id")
		}
		return left < right
	})
	return out
}

func appendStatusUnknown(pairs []displayPair, prefix string, object map[string]any, known map[string]bool) []displayPair {
	keys := make([]string, 0)
	for key := range object {
		if !known[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		pairs = appendDisplayPairs(pairs, prefix+"."+key, object[key])
	}
	return pairs
}

func statusRawString(object map[string]any, key string) (string, bool) {
	value, ok := object[key]
	if !ok {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		return "", true
	default:
		return displayScalar(typed), true
	}
}

func statusValueRaw(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return ""
	}
	return statusRawValue(value)
}

func statusValue(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return "—"
	}
	return statusRawValue(value)
}

func statusRawValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "(null)"
	case string:
		if typed == "" {
			return "(empty)"
		}
		return typed
	case []any:
		if len(typed) == 0 {
			return "(none)"
		}
	case map[string]any:
		if len(typed) == 0 {
			return "(none)"
		}
	}
	return displayScalar(value)
}

func statusCountOrDash(object map[string]any, key string) string {
	if value, ok := statusInt(object, key); ok {
		return strconv.FormatInt(value, 10)
	}
	return "—"
}

func statusInt(object map[string]any, key string) (int64, bool) {
	value, ok := object[key]
	if !ok || value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || typed > math.MaxInt64 || typed < math.MinInt64 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		f := float64(typed)
		if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
			return 0, false
		}
		return int64(f), true
	case json.Number:
		n, err := strconv.ParseInt(string(typed), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func statusBool(object map[string]any, key string) (bool, bool) {
	value, ok := object[key]
	if !ok {
		return false, false
	}
	typed, ok := value.(bool)
	return typed, ok
}

func statusExactBytes(object map[string]any, key string) string {
	value, ok := statusInt(object, key)
	if !ok {
		return "—"
	}
	return strconv.FormatInt(value, 10) + " bytes"
}

func formatDurationSeconds(seconds int64) string {
	return strconv.FormatInt(seconds, 10) + "s (" + humanDurationSeconds(seconds) + ")"
}

func humanDurationSeconds(seconds int64) string {
	if seconds == 0 {
		return "0s"
	}
	negative := seconds < 0
	var value uint64
	if negative {
		// math.MinInt64 の絶対値は int64 に収まらないため、オフセットして uint64 の大きさを求める。
		value = uint64(-(seconds + 1)) + 1
	} else {
		value = uint64(seconds)
	}
	parts := make([]string, 0, 4)
	for _, unit := range []struct {
		seconds uint64
		name    string
	}{{86400, "day"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if value >= unit.seconds {
			count := value / unit.seconds
			value %= unit.seconds
			if unit.name == "day" {
				name := "day"
				if count != 1 {
					name = "days"
				}
				parts = append(parts, fmt.Sprintf("%d %s", count, name))
			} else {
				parts = append(parts, fmt.Sprintf("%d%s", count, unit.name))
			}
		}
	}
	result := strings.Join(parts, " ")
	if negative {
		return "-" + result
	}
	return result
}

func formatRetentionValue(value any) string {
	if seconds, ok := statusInt(map[string]any{"value": value}, "value"); ok {
		return formatDurationSeconds(seconds)
	}
	return statusRawValue(value)
}

func statusQuarantineReason(item map[string]any) string {
	value, ok := item["failure_code"]
	if !ok {
		return "(unset)"
	}
	return statusRawValue(value)
}

func writeStatusLine(w io.Writer, line string) {
	_, _ = fmt.Fprintln(w, line)
}

func writeStatusField(w io.Writer, label, value string) {
	if value == "" {
		value = "(empty)"
	}
	// 長い error・path・opaque ID は値を失わないよう継続行へ送り、短い値は 1 行に収める。
	if strings.Contains(value, "\n") || utf8.RuneCountInString(value) > 120 {
		writeStatusLine(w, label+":")
		for _, line := range strings.Split(value, "\n") {
			writeStatusLine(w, "    "+line)
		}
		return
	}
	writeStatusLine(w, label+": "+value)
}

func writeStatusTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = utf8.RuneCountInString(header)
	}
	for _, row := range rows {
		for index := range headers {
			if index < len(row) {
				if width := utf8.RuneCountInString(row[index]); width > widths[index] {
					widths[index] = width
				}
			}
		}
	}
	writeRow := func(row []string) {
		var line strings.Builder
		for index, header := range headers {
			value := header
			if index < len(row) {
				value = row[index]
			}
			if index > 0 {
				line.WriteByte(' ')
			}
			line.WriteString(value)
			padding := widths[index] - utf8.RuneCountInString(value)
			if index < len(headers)-1 {
				line.WriteString(strings.Repeat(" ", padding))
			}
		}
		writeStatusLine(w, line.String())
	}
	writeRow(nil)
	for _, row := range rows {
		writeRow(row)
	}
}
