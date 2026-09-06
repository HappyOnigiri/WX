package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

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
	sortKey, path, ready, leased, last, note string
}

// statusReplenishmentNote は補充停止中の workspace 行に出す注記を作る。
// 表の外へ独立行として出すと正常な行に埋もれるため、該当行自体へ理由と復帰手順を載せる。
func statusReplenishmentNote(item map[string]any) string {
	reason := "standby replenishment stopped"
	switch statusValueRaw(item, "reason") {
	case "CLEAN":
		reason += " after wx clear"
	case "STANDBY_PREPARE_FAILED":
		reason += " after a preparation failure"
	}
	action := statusValueRaw(item, "action")
	if action == "" {
		return "! " + reason
	}
	return "! " + reason + "; run " + action
}

// workspaceLastUsedSchemaVersion は workspace_details.last_used_at が導入された JSON schema 版である。
const workspaceLastUsedSchemaVersion = 6

func printStatusSummary(w io.Writer, payload map[string]any) {
	workspaces := statusObjectList(payload["workspace_details"])
	roots := statusObjectsSortedBy(statusObjectList(payload["worktree_roots"]), "path")

	// LAST USED は daemon が workspace ごとに集計した値をそのまま出す。
	// 旧 schema の応答では repository の時刻を workspace の値として補完しない。
	notes := map[string]string{}
	for _, item := range statusObjectList(payload["standby_replenishment"]) {
		root, _ := statusRawString(item, "root")
		notes[root] = statusReplenishmentNote(item)
	}

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
		row.last = statusWorkspaceLastUsed(payload, workspace)
		row.note = notes[root]
		delete(notes, root)
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].sortKey == rows[j].sortKey {
			return rows[i].path < rows[j].path
		}
		return rows[i].sortKey < rows[j].sortKey
	})

	// NOTE 列は停止中の workspace がある間だけ出し、正常時の表を広げない。
	noted := false
	for _, row := range rows {
		noted = noted || row.note != ""
	}
	header := []string{"WORKSPACE", "READY", "IN USE", "LAST USED (" + statusZoneLabel() + ")"}
	if noted {
		header = append(header, "NOTE")
	}
	writeStatusTable(w, header, func() [][]string {
		out := make([][]string, 0, len(rows))
		for _, row := range rows {
			cells := []string{row.path, row.ready, row.leased, row.last}
			if noted {
				cells = append(cells, statusDash(row.note))
			}
			out = append(out, cells)
		}
		return out
	}())
	if notice := statusWorkspaceLastUsedNotice(payload); notice != "" {
		writeStatusLine(w, notice)
	}
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
	// workspace 表に載せられなかった停止（登録が消えた workspace など）だけを残余として出す。
	remaining := make([]string, 0, len(notes))
	for root := range notes {
		remaining = append(remaining, root)
	}
	sort.Strings(remaining)
	for _, root := range remaining {
		writeStatusLine(w, statusHomePath(root)+" "+notes[root])
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
	// 使用量は daemon の周期処理が測った値で、要求時点のものではない。0 を実測値と誤読させないため未測定は数値を出さない。
	if measurement, _ := statusRawString(root, "measurement"); measurement == "pending" {
		return "Disk   measuring · " + path
	}
	// wx が言う disk 使用量は main worktree と共有していない分だけで、slot ごとの SIZE 列と同じ量を指す。
	// 満額の allocated_bytes は --json と --verbose にだけ出し、要約では単位を混ぜない。
	exclusive, ok := statusInt(root, "exclusive_bytes")
	if !ok {
		return "Disk   measurement unavailable · " + path
	}
	line := "Disk   " + formatHumanBytes(exclusive) + " · " + path
	if measuredAt, ok := statusRawString(root, "measured_at"); ok && measuredAt != "" {
		line += " · measured " + statusLocalDate(measuredAt) + " " + statusZoneLabel()
	}
	return line
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

func statusWorkspaceLastUsed(payload, workspace map[string]any) string {
	if statusWorkspaceLastUsedUnavailable(payload) {
		return "unknown"
	}
	lastUsed, ok := statusRawString(workspace, "last_used_at")
	if !ok || lastUsed == "" {
		return "—"
	}
	return statusLocalDate(lastUsed)
}

func statusWorkspaceLastUsedVerbose(payload, workspace map[string]any) string {
	if statusWorkspaceLastUsedUnavailable(payload) {
		return "unknown"
	}
	return statusValue(workspace, "last_used_at")
}

func statusWorkspaceLastUsedUnavailable(payload map[string]any) bool {
	schema, ok := statusInt(payload, "schema_version")
	return ok && schema < workspaceLastUsedSchemaVersion
}

func statusWorkspaceLastUsedNotice(payload map[string]any) string {
	if !statusWorkspaceLastUsedUnavailable(payload) {
		return ""
	}
	schema, _ := statusInt(payload, "schema_version")
	return fmt.Sprintf("LAST USED unavailable: daemon JSON schema %d has no workspace history; update the daemon.", schema)
}
