package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/textfmt"
)

// printStatusDisplay は wx status の人間向け表示を担当する。
// RPC payload は daemon が JSON 契約を所有するため、表示側ではコピーだけを解釈する。
// 固定文は描画時に lang で解決し、payload の値は訳を通さずそのまま書く。
func printStatusDisplay(w io.Writer, payload map[string]any, verbose bool, lang i18n.Language) {
	payload = normalizeStatusPayload(payload)
	r := newTextRenderer(w, lang)
	if degraded, ok := payload["degraded"].(bool); ok && degraded {
		printDegradedStatus(r, payload, verbose)
		return
	}
	if !statusPayloadLooksStructured(payload) {
		// 診断項目を返さない古い daemon や小さなテスト用 handler では、欠落値を表にせず汎用表示へ戻す。
		printDisplay(w, payload)
		return
	}
	if verbose {
		printVerboseStatus(r, payload)
		return
	}
	printStatusSummary(r, payload)
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

func printDegradedStatus(r *textRenderer, payload map[string]any, verbose bool) {
	if message, ok := payload["error"]; ok {
		r.line("status.daemon.degraded_error", map[string]any{"Error": statusRawValue(message)})
	} else {
		r.line("status.daemon.degraded", nil)
	}

	// degraded 応答では件数を信頼できないため、error と database_path だけを表示し、ゼロ件を補わない。
	if _, ok := payload["database_path"]; ok {
		r.field(0, "status.field.database", statusHomeValue(payload, "database_path"))
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
	r.line("status.section.additional", nil)
	for _, pair := range additional {
		r.dataField(2, pair.key, pair.value)
	}
}

type statusWorkspaceRow struct {
	sortKey, path, policy, ready, leased, last, note string
}

// statusReplenishmentNote は補充が進んでいない workspace 行に出す注記を作る。
// 表の外へ独立行として出すと正常な行に埋もれるため、該当行自体へ理由と復帰手順を載せる。
func statusReplenishmentNote(r *textRenderer, item map[string]any) string {
	id := "status.standby.stopped"
	switch statusValueRaw(item, "reason") {
	case "CLEAN":
		id = "status.standby.stopped_clean"
	case "STANDBY_PREPARE_FAILED":
		id = "status.standby.stopped_prepare"
	case "STANDBY_PLAN_FAILED":
		// 計画の失敗は補充を止めないので、停止とは書かずに枠が埋まっていないことを示す。
		id = "status.standby.plan_failed"
	}
	reason := r.Localize(id, nil)
	// action は daemon が返すコマンド文字列なので、訳さず不透明値として埋める。
	action := statusValueRaw(item, "action")
	if action == "" {
		return r.Localize("status.standby.note", map[string]any{"Reason": reason})
	}
	return r.Localize("status.standby.note_action", map[string]any{"Reason": reason, "Action": action})
}

// workspaceLastUsedSchemaVersion は workspace_details.last_used_at が導入された JSON schema 版である。
const workspaceLastUsedSchemaVersion = 6

// workspacePolicySchemaVersion は workspace_details.policy が導入された JSON schema 版である。
const workspacePolicySchemaVersion = 14

// archivedSessionSchemaVersion は archived_session_details が導入された JSON schema 版である。
const archivedSessionSchemaVersion = 19

func printStatusSummary(r *textRenderer, payload map[string]any) {
	workspaces := statusObjectList(payload["workspace_details"])
	roots := statusObjectsSortedBy(statusObjectList(payload["worktree_roots"]), "path")

	// LAST USED は daemon が workspace ごとに集計した値をそのまま出す。
	// 旧 schema の応答では repository の時刻を workspace の値として補完しない。
	notes := map[string]string{}
	for _, item := range statusObjectList(payload["standby_replenishment"]) {
		root, _ := statusRawString(item, "root")
		notes[root] = statusReplenishmentNote(r, item)
	}

	rows := make([]statusWorkspaceRow, 0, len(workspaces))
	for _, workspace := range workspaces {
		if !statusWorkspaceIsSummarized(payload, workspace) {
			// 表から外した workspace の補充停止は notes に残し、表の下の残余行として出す。
			continue
		}
		root, _ := statusRawString(workspace, "root")
		path := textfmt.HomePath(root)
		if statusWorkspaceIsCurrent(workspace, roots) {
			path += " *"
		}
		row := statusWorkspaceRow{
			sortKey: root,
			path:    statusDash(path),
			policy:  statusWorkspacePolicy(payload, workspace),
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
	// 見出しは表を組む前に解決する。訳し終えた文字列の表示幅で桁を決めないと、見出しだけが行の値からずれる。
	header := []string{
		r.Localize("status.table.workspace", nil), r.Localize("status.table.policy", nil), r.Localize("status.table.ready", nil),
		r.Localize("status.table.in_use", nil), r.Localize("status.table.last_used_zone", map[string]any{"Zone": statusZoneLabel()}),
	}
	if noted {
		header = append(header, r.Localize("status.table.note", nil))
	}
	writeStatusTable(r, header, func() [][]string {
		out := make([][]string, 0, len(rows))
		for _, row := range rows {
			cells := []string{row.path, row.policy, row.ready, row.leased, row.last}
			if noted {
				cells = append(cells, statusDash(row.note))
			}
			out = append(out, cells)
		}
		return out
	}())
	if notice := statusWorkspaceLastUsedNotice(r, payload); notice != "" {
		r.raw(notice)
	}
	if notice := statusWorkspacePolicyNotice(r, payload); notice != "" {
		r.raw(notice)
	}
	if len(rows) == 0 {
		// 空の registry でも表のヘッダーを残し、(none) を件数の 0 と混同させない。
		r.raw("(none)")
		// 全行を絞り込みで落としたときだけ、(none) を登録ゼロと読み違えないよう隠した件数を添える。
		if hidden := len(workspaces); hidden > 0 {
			r.raw(statusHiddenWorkspaceNotice(r, hidden))
		}
	}

	r.raw("")
	r.raw(statusDaemonSummary(r, payload))
	for _, root := range roots {
		r.raw(statusDiskSummary(r, root))
	}
	if len(roots) == 0 {
		r.raw(statusSummaryLabel(r, "status.label.disk") + "(none)")
	}
	// workspace 表に載せられなかった停止（登録が消えた workspace など）だけを残余として出す。
	remaining := make([]string, 0, len(notes))
	for root := range notes {
		remaining = append(remaining, root)
	}
	sort.Strings(remaining)
	for _, root := range remaining {
		r.raw(textfmt.HomePath(root) + " " + notes[root])
	}
}

// statusSummaryLabel は要約行の先頭ラベルを、同じ列で始まる行どうしが揃う幅まで詰めて返す。
// 訳文へ桁合わせの空白を埋め込むと、訳語を変えた瞬間に桁がずれるため、幅はここで測る。
func statusSummaryLabel(r *textRenderer, id string) string {
	width := 0
	for _, aligned := range []string{"status.label.daemon", "status.label.disk"} {
		if w := xansi.StringWidth(r.Localize(aligned, nil)); w > width {
			width = w
		}
	}
	label := r.Localize(id, nil)
	return label + strings.Repeat(" ", width-xansi.StringWidth(label)) + " "
}

// statusDaemonSummary は daemon の状態と job の内訳を 1 行にまとめる。
// 状態値と job の状態名は daemon の JSON 契約の値なので、訳文の中でも英語のまま残す。
func statusDaemonSummary(r *textRenderer, payload map[string]any) string {
	state := "running"
	if pending, ok := statusBool(payload, "stop_pending"); ok && pending {
		state = "stopping"
	} else if pending, ok := statusBool(payload, "restart_pending"); ok && pending {
		state = "restarting"
	}
	label := statusSummaryLabel(r, "status.label.daemon")
	// discarded は `wx clear` などが取り消した予定 job で、failed とは対処の要否が違う。
	// 失敗が 0 でも取り消しが積み上がるので、失敗件数の読み違いを防ぐため既定の 1 行に並べて出す。
	jobs, ok := payload["job_details"].(map[string]any)
	if !ok {
		queued, hasQueued := statusInt(payload, "queued_jobs")
		if !hasQueued {
			return label + state
		}
		jobs = map[string]any{"pending": queued}
	}
	return label + r.Localize("status.daemon.summary_jobs", map[string]any{
		"State": state, "Pending": statusCountOrDash(jobs, "pending"), "Running": statusCountOrDash(jobs, "running"),
		"Failed": statusCountOrDash(jobs, "failed"), "Discarded": statusCountOrDash(jobs, "discarded"),
	})
}

func statusDiskSummary(r *textRenderer, root map[string]any) string {
	label := statusSummaryLabel(r, "status.label.disk")
	path, _ := statusRawString(root, "path")
	path = statusDash(textfmt.HomePath(path))
	if message, ok := statusRawString(root, "error"); ok && message != "" {
		return label + r.Localize("status.disk.failed", map[string]any{"Path": path, "Error": message})
	}
	// 使用量は daemon の周期処理が測った値で、要求時点のものではない。0 を実測値と誤読させないため未測定は数値を出さない。
	if measurement, _ := statusRawString(root, "measurement"); measurement == "pending" {
		return label + r.Localize("status.disk.measuring", map[string]any{"Path": path})
	}
	// wx が言う disk 使用量は main worktree と共有していない分だけで、slot ごとの SIZE 列と同じ量を指す。
	// 満額の allocated_bytes は --json と --verbose にだけ出し、要約では単位を混ぜない。
	// managed は登録外を集計した Unmanaged 行との区別であり、専有量と満額の区別ではない。
	exclusive, ok := statusInt(root, "exclusive_bytes")
	if !ok {
		return label + r.Localize("status.disk.unavailable", map[string]any{"Path": path})
	}
	data := map[string]any{"Size": textfmt.HumanBytes(exclusive), "Path": path}
	// 計測日時の有無で ID を分け、訳文の中で日時の置き場所を日本語側が決められるようにする。
	line := label + r.Localize("status.disk.managed", data)
	if measuredAt, ok := statusRawString(root, "measured_at"); ok && measuredAt != "" {
		data["Time"], data["Zone"] = statusLocalDate(measuredAt), statusZoneLabel()
		line = label + r.Localize("status.disk.managed_measured", data)
	}
	if unmanaged, ok := statusInt(root, "unmanaged_allocated_bytes"); ok && unmanaged > 0 {
		line += "\n" + r.Localize("status.label.unmanaged", nil) + " " +
			r.Localize("status.disk.unmanaged", map[string]any{"Size": textfmt.HumanBytes(unmanaged)})
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

// statusWorkspaceIsSummarized は workspace を要約表に載せるか返す。
// worktree を作る方針か、実際に worktree を持つ workspace だけを残し、方針が off・ask で slot も無いものは --verbose と --json に委ねる。
func statusWorkspaceIsSummarized(payload, workspace map[string]any) bool {
	if statusWorkspacePolicyUnavailable(payload) {
		// policy を返さない daemon では方針を判定できないため、登録を隠さず全件出す。
		return true
	}
	switch policy, _ := statusRawString(workspace, "policy"); policy {
	case "hot", "cold":
		return true
	}
	ready, _ := statusInt(workspace, "ready")
	leased, _ := statusInt(workspace, "leased")
	return ready > 0 || leased > 0
}

// statusWorkspacePolicy は POLICY 列の表示値を返す。
// hot・cold 以外の方針も、条件を満たして表に残った行では何が有効かを読めるよう同じ形で出す。
func statusWorkspacePolicy(payload, workspace map[string]any) string {
	if statusWorkspacePolicyUnavailable(payload) {
		return "unknown"
	}
	policy, _ := statusRawString(workspace, "policy")
	return statusDash(strings.ToUpper(policy))
}

func statusWorkspacePolicyUnavailable(payload map[string]any) bool {
	schema, ok := statusInt(payload, "schema_version")
	return ok && schema < workspacePolicySchemaVersion
}

func statusWorkspacePolicyNotice(r *textRenderer, payload map[string]any) string {
	if !statusWorkspacePolicyUnavailable(payload) {
		return ""
	}
	schema, _ := statusInt(payload, "schema_version")
	return r.Localize("status.notice.policy_unavailable", map[string]any{"Schema": schema})
}

// statusHiddenWorkspaceNotice は表が空になったときだけ添える、隠した登録の件数と確認手段の案内である。
func statusHiddenWorkspaceNotice(r *textRenderer, hidden int) string {
	if hidden == 1 {
		return r.Localize("status.notice.hidden_workspace", nil)
	}
	return r.Localize("status.notice.hidden_workspaces", map[string]any{"Count": hidden})
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

func statusWorkspaceLastUsedNotice(r *textRenderer, payload map[string]any) string {
	if !statusWorkspaceLastUsedUnavailable(payload) {
		return ""
	}
	schema, _ := statusInt(payload, "schema_version")
	return r.Localize("status.notice.last_used_unavailable", map[string]any{"Schema": schema})
}

func statusArchivedSessionsUnavailable(payload map[string]any) bool {
	schema, ok := statusInt(payload, "schema_version")
	return ok && schema < archivedSessionSchemaVersion
}

// statusArchivedSessionNotice は集計を返さない daemon 向けの注記である。
// 旧 daemon の session_details には ARCHIVED が混ざるため、行を間引かず注記だけを添えて診断の欠落を防ぐ。
func statusArchivedSessionNotice(r *textRenderer, payload map[string]any) string {
	if !statusArchivedSessionsUnavailable(payload) {
		return ""
	}
	schema, _ := statusInt(payload, "schema_version")
	return r.Localize("status.notice.archived_unavailable", map[string]any{"Schema": schema})
}

// statusQuarantineCleanupNotices は隔離された実体のうち、コマンドで消せるものだけ削除手段を案内する。
// unknown_paths・mismatched_refs には削除コマンドが無く（wx clear は未登録の実体に触れず、wx prune は unknown_refs だけを対象にする）、
// 案内すると効かない操作を促すため、この 2 つには行を出さない。
// commentlint:allow-long -- 案内しないカテゴリがある理由を残すため
func statusQuarantineCleanupNotices(r *textRenderer, items []map[string]any) []string {
	kinds := map[string]bool{}
	for _, item := range items {
		kinds[statusValueRaw(item, "kind")] = true
	}
	var notices []string
	if kinds["slot"] {
		notices = append(notices, r.Localize("status.notice.quarantine_slot", nil))
	}
	if kinds["unknown_refs"] {
		notices = append(notices, r.Localize("status.notice.quarantine_refs", nil))
	}
	return notices
}
