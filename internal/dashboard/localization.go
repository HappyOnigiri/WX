package dashboard

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

var dashboardPathPattern = regexp.MustCompile(`/[^\s\x1b]+`)

// dashboardLanguage は設定から表示用の言語を取り出す。未設定・不正値は英語。
func dashboardLanguage(value string) i18n.Language { return i18n.Normalize(value) }

// translateDashboard は View の固定ラベルを置き換える。Action の command、
// path、設定キー、daemon が返す状態値は置換表に含めず、操作結果の意味を保つ。
func translateDashboard(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	// 表示全体には daemon の status や workspace の絶対 path も含まれる。
	// 固定ラベルの置換がそれらの値を壊さないよう、path だけ一時退避する。
	paths := make([]string, 0)
	text = dashboardPathPattern.ReplaceAllStringFunc(text, func(path string) string {
		index := len(paths)
		paths = append(paths, path)
		return "\x00wx-dashboard-path-" + strconv.Itoa(index) + "\x00"
	})
	// LaunchAgent は macOS の機械的な service 名であり、単語 `Launch` の
	// 置換に巻き込まない。画面上の固定ラベルだけを翻訳する。
	const launchAgentPlaceholder = "\x00wx-launch-agent\x00"
	text = strings.ReplaceAll(text, "LaunchAgent", launchAgentPlaceholder)
	replacements := []struct{ en, ja string }{
		{"System status", "システム状態"},
		{"Status", "状態"},
		{"Launch", "起動"},
		{"Settings", "設定"},
		{"Doctor", "診断"},
		{"Maintenance", "保守"},
		{"System", "システム"},
		{"Loading from the daemon…", "daemon から読み込み中…"},
		{"Refresh failed: ", "更新に失敗しました: "},
		{"Showing the last successful response.", "最後に成功した応答を表示しています。"},
		{"No status is available.", "状態を取得できません。"},
		{"Could not load configuration: ", "設定を読み込めませんでした: "},
		{"Choose what to launch", "起動するものを選択"},
		{"Editable settings", "設定を編集"},
		{"Environments", "環境"},
		{"Diagnostic mode", "診断モード"},
		{"Maintenance operation", "保守操作"},
		{"Integrations and daemon", "連携と daemon"},
		{"Choose a value or action:", "値または操作を選択:"},
		{"Run this operation?", "この操作を実行しますか？"},
		{"Running…", "実行中…"},
		{"The result will appear here when the operation finishes.", "操作が完了すると結果がここに表示されます。"},
		{"The operation produced no output.", "操作の出力はありません。"},
		{"Enter confirm", "Enter で確定"},
		{"Esc exit", "Esc で終了"},
		{"Esc back", "Esc で戻る"},
		{"Enter/Esc back", "Enter/Esc で戻る"},
		{"selected workspace", "選択した workspace"},
		{"Standard diagnostics", "標準診断"},
		{"Verbose diagnostics", "詳細診断"},
		{"Worktree probe", "Worktree 検査"},
		{"Garbage collection", "ガベージコレクション"},
		{"Clear sessions and standbys", "セッションと standby を削除"},
		{"Prune recovery refs", "復旧 ref を整理"},
		{"Retry standby replenishment", "standby 補充を再試行"},
		{"Release a lease", "lease を返却"},
		{"Discard recovery state", "復旧状態を破棄"},
		{"Forget a workspace", "workspace の管理を解除"},
		{"Benchmark preparation", "準備をベンチマーク"},
		{"Start daemon", "daemon を起動"},
		{"Stop daemon", "daemon を停止"},
		{"Restart daemon", "daemon を再起動"},
		{"Global", "全体"},
		{"Workspace", "Workspace"},
		{"Repository", "Repository"},
		{"Scope: ", "スコープ: "},
		{"Effective settings", "実効設定"},
		{"Display language", "表示言語"},
		{"Language used for human-readable CLI, TUI, and daemon messages.", "CLI・TUI・daemon の人間向け表示に使う言語。"},
		{"Changes display text only; JSON output remains in English.", "表示だけを変更し、JSON 出力は英語のままです。"},
		{"Choices: ", "選択肢: "},
		{"Impact", "影響"},
		{"Behavior", "動作"},
		{"This operation may delete data.", "この操作はデータを削除する可能性があります。"},
		{"This may delete data. Check the target carefully.", "データを削除する可能性があります。対象を確認してください。"},
		{"Current: ", "現在値: "},
		{"Change: ", "変更: "},
		{"Target: ", "対象: "},
		{"Input: ", "入力: "},
		{"Keep current value: ", "現在値を保持: "},
		{"Enabled", "有効"},
		{"Disabled", "無効"},
		{"Enter a custom value…", "値を入力…"},
		{"Add a value…", "値を追加…"},
		{"Remove a value…", "値を削除…"},
		{"Reset to default", "既定値に戻す"},
		{"All registered workspaces", "登録済みの全 workspace"},
		{"Enter another path…", "別の path を入力…"},
	}
	for _, replacement := range replacements {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	text = strings.ReplaceAll(text, launchAgentPlaceholder, "LaunchAgent")
	for index, path := range paths {
		text = strings.ReplaceAll(text, "\x00wx-dashboard-path-"+strconv.Itoa(index)+"\x00", path)
	}
	return text
}
