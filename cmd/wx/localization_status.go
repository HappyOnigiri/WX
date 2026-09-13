package main

import (
	"strings"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// localization_status.go は status・doctor などの人間向け表示を訳す表示層である。
// 訳すのは固定ラベルと表の見出しだけで、path・ID・状態値・外部コマンドの原文は残す。

// statusTableHeaders は表の見出しの訳で、列を持つ表示の桁を決める前に使う。
// 括弧付きの見出し（LAST USED (JST) など）があるため前方一致で照合し、残りは原文で連結する。
// translateHumanOutput と同じ訳語を使うが、こちらは表示層より前に適用するので二重には当たらない。
var statusTableHeaders = []translation{
	{"WORKSPACE", "ワークスペース"},
	{"LAST USED", "最終使用"},
	{"IN USE", "使用中"},
	{"POLICY", "方針"},
	{"NOTE", "注記"},
}

// localizeStatusHeader は表の見出し 1 つを訳す。見出し以外の行には使わない。
func localizeStatusHeader(value string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return value
	}
	for _, replacement := range statusTableHeaders {
		if strings.HasPrefix(value, replacement.en) {
			return replacement.ja + value[len(replacement.en):]
		}
	}
	return value
}

// translateHumanOutput は固定ラベルだけを置き換える軽量な表示層である。
// payload の path・ID・状態値・外部コマンドの原文は変更しないため、JSON と
// 診断の可変値を同じ renderer から安全に再利用できる。
func translateHumanOutput(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	replacements := []translation{
		{"System status", "システム状態"},
		{"! standby replenishment stopped after wx clear", "! wx clear 後に standby 補充が停止しました"},
		{"! standby replenishment stopped after a preparation failure", "! 準備失敗後に standby 補充が停止しました"},
		{"! standby replenishment failed to plan new worktrees", "! standby 補充の新しい worktree 計画に失敗しました"},
		{"Disk   measurement failed · ", "ディスク   計測に失敗 · "},
		{"Disk   measurement unavailable · ", "ディスク   計測できません · "},
		{"Disk   measuring · ", "ディスク   計測中 · "},
		{"Disk   ", "ディスク   "},
		{" · measured ", " · 計測 "},
		{" · Jobs ", " · ジョブ "},
		{"excluded from cleanup", "cleanup 対象外"},
		{"POLICY unavailable:", "方針を利用できません:"},
		{"LAST USED unavailable:", "最終使用を利用できません:"},
		{"Archived unavailable:", "アーカイブ済み情報を利用できません:"},
		{"Daemon degraded", "daemon（縮退）"},
		{"Daemon ", "daemon "},
		{"WORKSPACE", "ワークスペース"},
		{"POLICY", "方針"},
		{"IN USE", "使用中"},
		{"LAST USED", "最終使用"},
		{"NOTE", "注記"},
		{"Additional", "追加情報"},
		{"Database", "データベース"},
		{"Disk", "ディスク"},
		{"Unmanaged", "未管理"},
		{"measurement failed", "計測に失敗"},
		{"measurement unavailable", "計測できません"},
		{"measuring", "計測中"},
		{"managed", "管理対象"},
		{"measured", "計測日時"},
		{"Repositories", "リポジトリ"},
		{"Sessions", "セッション"},
		{"Jobs", "ジョブ"},
		{"Snapshots", "スナップショット"},
		{"Storage", "ストレージ"},
		{"Retention", "保持期間"},
		{"Workspaces", "ワークスペース"},
		{"Daemon", "daemon"},
		{"Active sessions", "アクティブなセッション"},
		{"Restart pending", "再起動待ち"},
		{"Stop pending", "停止待ち"},
		{"Last backup", "最終バックアップ"},
		{"Last reload", "最終再読み込み"},
		{"Reload error", "再読み込みエラー"},
		{"Worktree root error", "worktree root エラー"},
		{"JSON schema version", "JSON schema バージョン"},
		{"DB schema version", "DB schema バージョン"},
		{"Degraded", "縮退"},
		{"Restart", "再起動"},
		{"Stop", "停止"},
		{"Slots ready", "準備済み slot"},
		{"Slots in use", "使用中 slot"},
		{"Slots failed", "失敗 slot"},
		{"Slots quarantined", "隔離 slot"},
		{"Slots", "slot"},
		{"Total", "合計"},
		{"Queued", "キュー待ち"},
		{"Pending", "待機中"},
		{"Running", "実行中"},
		{"Failed", "失敗"},
		{"Discarded", "破棄済み"},
		{"Details", "詳細"},
		{"Earliest expiry", "最も早い期限"},
		{"Logical size", "論理サイズ"},
		{"Disk size", "ディスクサイズ"},
		{"Allocated", "割当"},
		{"Shared", "共有"},
		{"Measurement", "計測"},
		{"Measured at", "計測日時"},
		{"Quarantine", "隔離"},
		{"Standby replenishment", "standby 補充"},
		{"Generation", "世代"},
		{"Reason", "理由"},
		{"Detail", "詳細"},
		{"Suspended", "停止日時"},
		{"Action", "操作"},
		{"Root ", "root "},
		{"Config", "設定"},
		{"Backup", "バックアップ"},
		{"Pool", "プール"},
		{"Version", "バージョン"},
		{"Protocol version", "プロトコルバージョン"},
		{"Uptime", "稼働時間"},
		{"Error", "エラー"},
		{"error", "エラー"},
		{"Type", "型"},
		{"Scopes", "スコープ"},
		{"source", "出典"},
		{"Path", "パス"},
		{"Scope", "スコープ"},
		{"Choices", "選択肢"},
		{"Impact", "影響"},
		{"Language used for human-readable CLI, TUI, and daemon messages.", "CLI・TUI・daemon の人間向け表示に使う言語です。"},
		{"Changes display text only; JSON output remains in English.", "表示だけを変更し、JSON 出力は英語のままです。"},
		{"Updated ", "更新 "},
		{" ago", "前"},
		{"No status is available.", "状態を取得できません。"},
		{"Refresh failed: ", "更新に失敗しました: "},
		{"Showing the last successful response.", "最後に成功した応答を表示しています。"},
		{"Nothing above was checked by preparing a worktree; run wx doctor --probe to prepare one in each registered workspace and inspect it.", "上記の項目は worktree を準備して検査していません。登録済み各 workspace を検査するには wx doctor --probe を実行してください。"},
		{"Additional diagnostics", "追加の診断"},
		{"problem", "問題"},
		{"warning", "警告"},
		{"warning:", "警告:"},
		{"error:", "エラー:"},
		{"started", "起動しました"},
		{"stopped", "停止しました"},
		{"restarted", "再起動しました"},
		{"installed", "インストールしました"},
		{"uninstalled", "アンインストールしました"},
		{"no managed worktrees to clear", "削除対象の管理 worktree はありません"},
		{"target(s)", "対象"},
		{"candidates:", "候補:"},
		{"scheduled:", "予約済み:"},
		{"completed:", "完了:"},
		{"pending:", "待機中:"},
		{"failed:", "失敗:"},
		{"deletable:", "削除可能:"},
		{"deleted:", "削除済み:"},
		{"kept:", "保持:"},
		{"forgotten", "管理解除しました"},
		{"dry run: nothing was changed", "dry run: 変更はありません"},
		{"no quarantined recovery state for", "隔離された復旧状態はありません:"},
		{"discarded", "破棄しました"},
		{"retired", "退役しました"},
		{"reclaimed", "回収しました"},
		{"snapshot(s)", "snapshot"},
		{"workspace snapshot(s)", "workspace snapshot"},
		{"session(s)", "セッション"},
		{"slot(s)", "slot"},
	}
	// 値の後ろにある path・ID・Git/OS の原文は翻訳しない。各行の最初の
	// `label: value` の label 部分だけを置き換え、表の見出しのような
	// 区切りのない固定行は行全体へ安全な置換を適用する。
	lines := strings.SplitAfter(text, "\n")
	for index, line := range lines {
		ending := ""
		body := line
		if strings.HasSuffix(body, "\n") {
			body, ending = strings.TrimSuffix(body, "\n"), "\n"
		}
		colon := strings.IndexByte(body, ':')
		if colon >= 0 {
			prefix, suffix := body[:colon], body[colon:]
			lines[index] = applyTranslations(prefix, replacements) + suffix + ending
			continue
		}
		trimmed := strings.TrimLeft(body, " \t")
		leading := body[:len(body)-len(trimmed)]
		upperHeader := strings.ToUpper(trimmed) == trimmed
		for _, replacement := range replacements {
			if strings.HasPrefix(trimmed, replacement.en) {
				rest := trimmed[len(replacement.en):]
				if rest == "" || strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t") {
					trimmed = replacement.ja + rest
					continue
				}
			}
			// 表の見出しは単一行に複数の固定 token を並べる。動的な行の値を
			// 壊さないよう、大文字見出しだけこの分岐で置換する。
			if strings.Contains(trimmed, replacement.en) && upperHeader {
				trimmed = strings.ReplaceAll(trimmed, replacement.en, replacement.ja)
			}
		}
		lines[index] = leading + trimmed + ending
	}
	return strings.Join(lines, "")
}
