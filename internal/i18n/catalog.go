// Package i18n は wx の利用者向け表示を言語ごとに解決する。
// 機械向けの JSON 値・状態値・外部コマンドの出力はこの package を通さず、
// 画面へ出す短い説明だけを message ID で管理する。
package i18n

import (
	"context"
	"fmt"
	"strings"

	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

// Language は wx が表示に対応する言語である。
type Language string

const (
	English  Language = "en"
	Japanese Language = "ja"
)

// Entry は 1 message ID の英語・日本語訳である。
type Entry struct {
	EN string
	JA string
}

// catalog は機能単位の短い表示文をまとめたもの。新しい ID を追加するときは
// 英語と日本語を同じブロックへ置き、ValidateCatalog が空欄を検出できるようにする。
var catalog = map[string]Entry{
	"common.error":                    {EN: "error", JA: "エラー"},
	"common.saved":                    {EN: "saved and reloaded", JA: "保存して再読み込みしました"},
	"common.saved_pending":            {EN: "saved; daemon reload pending", JA: "保存しました（daemon の再読み込み待ち）"},
	"common.already_running":          {EN: "already running", JA: "すでに起動しています"},
	"common.already_stopped":          {EN: "already stopped", JA: "すでに停止しています"},
	"common.started":                  {EN: "started", JA: "起動しました"},
	"common.stopped":                  {EN: "stopped", JA: "停止しました"},
	"common.restarted":                {EN: "restarted", JA: "再起動しました"},
	"common.installed":                {EN: "installed", JA: "インストールしました"},
	"common.uninstalled":              {EN: "uninstalled", JA: "アンインストールしました"},
	"common.cancelled":                {EN: "cancelled", JA: "キャンセルしました"},
	"common.yes":                      {EN: "Yes", JA: "はい"},
	"common.no":                       {EN: "No", JA: "いいえ"},
	"common.none":                     {EN: "(none)", JA: "（なし）"},
	"common.unknown":                  {EN: "unknown", JA: "不明"},
	"common.pending":                  {EN: "pending", JA: "待機中"},
	"config.display_name":             {EN: "Display language", JA: "表示言語"},
	"config.language.english":         {EN: "English", JA: "English"},
	"config.language.japanese":        {EN: "Japanese", JA: "日本語"},
	"config.language.invalid":         {EN: "language must be en or ja", JA: "language は en または ja で指定してください"},
	"config.language.saved":           {EN: "display language changed to {{.Language}}", JA: "表示言語を {{.Language}} に変更しました"},
	"config.language.default":         {EN: "English", JA: "英語"},
	"setup.prerequisites":             {EN: "Prerequisites", JA: "前提条件"},
	"setup.worktree_root":             {EN: "Worktree root", JA: "Worktree root"},
	"setup.launch_agent":              {EN: "LaunchAgent", JA: "LaunchAgent"},
	"setup.daemon":                    {EN: "Daemon", JA: "Daemon"},
	"setup.display_language.title":    {EN: "Display language / 表示言語", JA: "表示言語 / Display language"},
	"setup.display_language.prompt":   {EN: "Choose the display language", JA: "表示言語を選択してください"},
	"setup.display_language.english":  {EN: "English", JA: "English"},
	"setup.display_language.japanese": {EN: "日本語", JA: "日本語"},
	"setup.cancelled":                 {EN: "cancelled; the items already applied are kept. Run wx setup again to finish.", JA: "キャンセルしました。適用済みの項目は保持されています。wx setup を再実行して残りを完了してください。"},
	"setup.needs_terminal":            {EN: "wx setup needs a terminal for its questions", JA: "wx setup の質問には端末が必要です"},
	"setup.check_hint":                {EN: "run wx setup --check to see the current state without a terminal", JA: "端末なしで現在の状態を見るには wx setup --check を実行してください"},
	"setup.attention":                 {EN: "need attention", JA: "対応が必要です"},
	"setup.items_changed":             {EN: "{{.Count}} item(s) no longer match what wx would write.", JA: "{{.Count}} 件の項目が wx の書き込む内容と一致しません。"},
	"setup.no_terminal_update":        {EN: "wx setup: {{.Items}} need attention; run wx setup to review them", JA: "wx setup: {{.Items}} に対応が必要です。確認するには wx setup を実行してください"},
	"setup.leftover":                  {EN: "leftover", JA: "leftover"},
	"progress.starting":               {EN: "starting", JA: "起動中"},
	"progress.restarting":             {EN: "restarting", JA: "再起動中"},
	"progress.stopping":               {EN: "stopping", JA: "停止中"},
	"progress.clearing":               {EN: "clearing", JA: "削除中"},
	"progress.diagnosing":             {EN: "diagnosing", JA: "診断中"},
	"progress.preparing":              {EN: "preparing", JA: "準備中"},
	"rpc.daemon_unavailable":          {EN: "wx daemon is not running or still starting; try again shortly", JA: "wx daemon は起動していないか、起動中です。しばらくしてから再試行してください"},
	"rpc.readable_but_failed":         {EN: "wx daemon is reachable but this request did not complete", JA: "wx daemon には接続できますが、要求を完了できませんでした"},
	"rpc.readiness_blocked":           {EN: "wx readiness blocked operation", JA: "wx の準備完了条件が操作を停止しました"},
	"rpc.no_workspace":                {EN: "no workspace was created", JA: "workspace は作成されませんでした"},
	"rpc.sqlite_unavailable":          {EN: "SQLite state is unavailable", JA: "SQLite の状態を利用できません"},
	"rpc.restore_backup":              {EN: "restore a verified backup from", JA: "検証済みバックアップから復元するか"},
	"rpc.preserve_for_doctor":         {EN: "or preserve the database for wx doctor", JA: "データベースを保全して wx doctor で調査してください"},
	"rpc.degraded_read_only":          {EN: "wx daemon is read-only degraded: {{.Message}}", JA: "wx daemon は読み取り専用の縮退状態です: {{.Message}}"},
	"cli.interrupted":                 {EN: "interrupted before the workspace was leased", JA: "workspace の貸出前に中断されました"},
	"cli.interrupted_preparing":       {EN: "interrupted while the workspace was being prepared; releasing it", JA: "workspace の準備中に中断されました。貸出を返却します"},
	"cli.workspace_preparation":       {EN: "workspace preparation", JA: "workspace の準備"},
	"cli.resume_cancelled":            {EN: "resume cancelled; no workspace was created", JA: "再開をキャンセルしました。workspace は作成されませんでした"},
	"cli.clear_stop":                  {EN: "wx clear asked this session to stop before the agent started", JA: "agent の起動前に wx clear から停止要求を受けました"},
	"dashboard.status":                {EN: "System status", JA: "システム状態"},
	"dashboard.loading":               {EN: "Loading from the daemon…", JA: "daemon から読み込み中…"},
	"dashboard.refresh_failed":        {EN: "Refresh failed: {{.Error}}", JA: "更新に失敗しました: {{.Error}}"},
	"dashboard.last_response":         {EN: "Showing the last successful response.", JA: "最後に成功した応答を表示しています。"},
	"dashboard.no_status":             {EN: "No status is available.", JA: "状態を取得できません。"},
	"dashboard.choose_launch":         {EN: "Choose what to launch", JA: "起動するものを選択"},
	"dashboard.editable_settings":     {EN: "Editable settings", JA: "設定を編集"},
	"dashboard.environments":          {EN: "Environments", JA: "環境"},
	"dashboard.diagnostic_mode":       {EN: "Diagnostic mode", JA: "診断モード"},
	"dashboard.maintenance":           {EN: "Maintenance operation", JA: "保守操作"},
	"dashboard.integrations":          {EN: "Integrations and daemon", JA: "連携と daemon"},
	"dashboard.global":                {EN: "Global", JA: "全体"},
	"dashboard.workspace":             {EN: "Workspace", JA: "Workspace"},
	"dashboard.repository":            {EN: "Repository", JA: "Repository"},
	"dashboard.enabled":               {EN: "Enabled", JA: "有効"},
	"dashboard.disabled":              {EN: "Disabled", JA: "無効"},
	"dashboard.reset_default":         {EN: "Reset to default", JA: "既定値に戻す"},
	"dashboard.enter_custom":          {EN: "Enter a custom value…", JA: "値を入力…"},
	"dashboard.add_value":             {EN: "Add a value…", JA: "値を追加…"},
	"dashboard.remove_value":          {EN: "Remove a value…", JA: "値を削除…"},
	"dashboard.confirm":               {EN: "Run this operation?", JA: "この操作を実行しますか？"},
	"dashboard.running":               {EN: "Running…", JA: "実行中…"},
	"dashboard.result_empty":          {EN: "The operation produced no output.", JA: "操作の出力はありません。"},
	"help.usage":                      {EN: "Usage", JA: "使い方"},
	"help.global_options":             {EN: "Global options:", JA: "全体オプション:"},
	"help.commands":                   {EN: "Commands:", JA: "コマンド:"},
	"help.show_help":                  {EN: "show help", JA: "ヘルプを表示"},
	"help.show_version":               {EN: "show version", JA: "バージョンを表示"},
}

type contextKey struct{}

// Parse は対応する言語だけを受け付ける。空文字は未指定として英語を返す。
func Parse(value string) (Language, error) {
	switch Language(strings.ToLower(strings.TrimSpace(value))) {
	case "", English:
		return English, nil
	case Japanese:
		return Japanese, nil
	default:
		return "", fmt.Errorf("language must be en or ja")
	}
}

// Normalize は壊れた設定でも起動初期に表示を失わないための安全な fallback である。
func Normalize(value string) Language {
	lang, err := Parse(value)
	if err != nil {
		return English
	}
	return lang
}

// WithLanguage は表示言語を context へ明示的に載せる。
func WithLanguage(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextKey{}, Normalize(value))
}

// LanguageFromContext は context の言語を返し、指定がなければ英語を返す。
func LanguageFromContext(ctx context.Context) Language {
	if ctx == nil {
		return English
	}
	if value, ok := ctx.Value(contextKey{}).(Language); ok {
		return Normalize(string(value))
	}
	return English
}

// HasLanguage は context に明示的な言語が載っているかを返す。
func HasLanguage(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(contextKey{}).(Language)
	return ok
}

// Localizer は明示言語を持つ message resolver である。
type Localizer struct {
	language Language
	value    *goi18n.Localizer
}

// New は言語を正規化した Localizer を返す。
func New(value string) *Localizer {
	lang := Normalize(value)
	bundle := goi18n.NewBundle(language.English)
	for id, entry := range catalog {
		_ = bundle.AddMessages(language.English, &goi18n.Message{ID: id, Other: entry.EN})
		_ = bundle.AddMessages(language.Japanese, &goi18n.Message{ID: id, Other: entry.JA})
	}
	return &Localizer{language: lang, value: goi18n.NewLocalizer(bundle, string(lang))}
}

// NewLocalizer は文字列 API の別名で、CLI の初期化コードから使う。
func NewLocalizer(value string) *Localizer { return New(value) }

// Language は Localizer の言語を返す。
func (l *Localizer) Language() Language {
	if l == nil {
		return English
	}
	return l.language
}

// Localize は message ID を展開する。未知 ID は機械的な欠落が分かるよう ID 自体を返す。
func (l *Localizer) Localize(id string, data any) string {
	if l == nil {
		l = New(string(English))
	}
	entry, known := catalog[id]
	if !known {
		return id
	}
	text, err := l.value.Localize(&goi18n.LocalizeConfig{
		MessageID:      id,
		TemplateData:   data,
		DefaultMessage: &goi18n.Message{ID: id, Other: entry.EN},
	})
	if err != nil || text == "" {
		return entry.EN
	}
	return text
}

// T は context の言語で message ID を解決する短縮 API である。
func T(ctx context.Context, id string, data any) string {
	return New(string(LanguageFromContext(ctx))).Localize(id, data)
}

// Catalog は静的検査やテストが利用できる読み取り専用コピーを返す。
func Catalog() map[string]Entry {
	out := make(map[string]Entry, len(catalog))
	for id, entry := range catalog {
		out[id] = entry
	}
	return out
}

// ValidateCatalog は両言語の空欄と、go-i18n が解釈できない template を検出する。
func ValidateCatalog() error {
	for id, entry := range catalog {
		if strings.TrimSpace(entry.EN) == "" || strings.TrimSpace(entry.JA) == "" {
			return fmt.Errorf("message %q must have non-empty en and ja text", id)
		}
		for _, lang := range []Language{English, Japanese} {
			localizer := New(string(lang))
			if strings.TrimSpace(localizer.Localize(id, map[string]any{"Count": 1, "Items": "", "Language": ""})) == "" {
				return fmt.Errorf("message %q could not be localized for %s", id, lang)
			}
		}
	}
	return nil
}
