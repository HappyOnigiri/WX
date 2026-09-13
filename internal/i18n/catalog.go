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
	"rpc.readiness_blocked":           {EN: "wx readiness blocked operation", JA: "wx の準備完了条件により操作を中止しました"},
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
	"dashboard.editable_settings":     {EN: "Editable settings", JA: "編集できる設定"},
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
	"dashboard.tab.status":            {EN: "Status", JA: "状態"},
	"dashboard.tab.launch":            {EN: "Launch", JA: "起動"},
	"dashboard.tab.settings":          {EN: "Settings", JA: "設定"},
	"dashboard.tab.doctor":            {EN: "Doctor", JA: "診断"},
	"dashboard.tab.maintenance":       {EN: "Maintenance", JA: "保守"},
	"dashboard.tab.system":            {EN: "System", JA: "システム"},
	"dashboard.config_load_failed":    {EN: "Could not load configuration: {{.Error}}", JA: "設定を読み込めませんでした: {{.Error}}"},
	"dashboard.updated":               {EN: "Updated {{.Time}}", JA: "{{.Time}} に更新"},
	"dashboard.updated_age":           {EN: "Updated {{.Time}} ({{.Age}} ago)", JA: "{{.Time}} に更新（{{.Age}} 前）"},
	"dashboard.no_actions":            {EN: "No actions are available.", JA: "実行できる操作がありません。"},
	"dashboard.system":                {EN: "System", JA: "システム"},
	"dashboard.workspace_defaults":    {EN: "Workspace defaults", JA: "Workspace の既定値"},
	"dashboard.repository_defaults":   {EN: "Repository defaults", JA: "Repository の既定値"},
	"dashboard.not_discovered":        {EN: "(not discovered)", JA: "（未検出）"},
	"dashboard.scope":                 {EN: "Scope: {{.Scope}}", JA: "スコープ: {{.Scope}}"},
	"dashboard.effective_settings":    {EN: "Effective settings", JA: "実効設定"},
	"dashboard.impact":                {EN: "Impact", JA: "影響"},
	"dashboard.behavior":              {EN: "Behavior", JA: "動作"},
	"dashboard.attention":             {EN: "Attention", JA: "注意"},
	"dashboard.choices":               {EN: "Choices: {{.Choices}}", JA: "選択肢: {{.Choices}}"},
	"dashboard.choose_value":          {EN: "Choose a value or action:", JA: "値または操作を選択:"},
	"dashboard.current":               {EN: "Current: {{.Value}}", JA: "現在値: {{.Value}}"},
	"dashboard.change":                {EN: "Change: {{.Value}}", JA: "変更: {{.Value}}"},
	"dashboard.target":                {EN: "Target: {{.Value}}", JA: "対象: {{.Value}}"},
	"dashboard.input":                 {EN: "Input: {{.Value}}", JA: "入力: {{.Value}}"},
	"dashboard.inherited":             {EN: "inherited / unset", JA: "継承・未設定"},
	"dashboard.destructive_note":      {EN: "This operation may delete data.", JA: "この操作はデータを削除する可能性があります。"},
	"dashboard.destructive_confirm":   {EN: "This may delete data. Check the target carefully.", JA: "データを削除する可能性があります。対象を確認してください。"},
	"dashboard.confirm_run":           {EN: "Enter / y run", JA: "Enter / y で実行"},
	"dashboard.confirm_back":          {EN: "Esc / n back", JA: "Esc / n で戻る"},
	"dashboard.running_note":          {EN: "The result will appear here when the operation finishes.", JA: "操作が完了すると結果がここに表示されます。"},
	"dashboard.result_title":          {EN: "{{.Label}} — exit {{.Code}}", JA: "{{.Label}} — 終了コード {{.Code}}"},
	"dashboard.value_prompt":          {EN: "Value", JA: "値"},
	"dashboard.workspace_path":        {EN: "Workspace path", JA: "workspace の path"},
	"dashboard.keep_current":          {EN: "Keep current value: {{.Value}}", JA: "現在値を保持: {{.Value}}"},
	"dashboard.all_workspaces":        {EN: "All registered workspaces", JA: "登録済みの全 workspace"},
	"dashboard.another_path":          {EN: "Enter another path…", JA: "別の path を入力…"},
	"dashboard.custom_arguments":      {EN: "Enter custom arguments…", JA: "引数を入力…"},
	"dashboard.default_launch":        {EN: "Launch with default options", JA: "既定の設定で起動"},
	"dashboard.default_open":          {EN: "Open with default options", JA: "既定の設定で開く"},
	"dashboard.default_base":          {EN: "Use the default base", JA: "既定の base を使う"},
	"dashboard.preview_changes":       {EN: "Preview changes", JA: "変更を確認"},
	"dashboard.run_gc":                {EN: "Run garbage collection", JA: "ガベージコレクションを実行"},
	"dashboard.clear_preview_default": {EN: "Preview default targets", JA: "既定の対象を確認"},
	"dashboard.clear_default":         {EN: "Clear default targets", JA: "既定の対象を削除"},
	"dashboard.clear_preview_standby": {EN: "Preview including standbys", JA: "standby を含めて確認"},
	"dashboard.clear_standby":         {EN: "Clear including standbys", JA: "standby を含めて削除"},
	"dashboard.prune_preview_safe":    {EN: "Preview safe refs", JA: "安全な ref を確認"},
	"dashboard.prune_safe":            {EN: "Prune safe refs", JA: "安全な ref を整理"},
	"dashboard.prune_preview_all":     {EN: "Preview all refs", JA: "全 ref を確認"},
	"dashboard.prune_all":             {EN: "Prune all refs", JA: "全 ref を整理"},
	"dashboard.release_save":          {EN: "Save work and release", JA: "作業を保存して返却"},
	"dashboard.release_discard":       {EN: "Discard work and release", JA: "作業を破棄して返却"},
	"dashboard.bench_cold":            {EN: "Run one cold preparation", JA: "cold start を 1 回計測"},
	"dashboard.bench_sweep":           {EN: "Run the standard configuration sweep", JA: "標準の設定を一通り計測"},
	"dashboard.bench_reuse":           {EN: "Measure standby reuse", JA: "standby の再利用を計測"},
	"dashboard.footer.tabs_full":      {EN: "←/→ or Tab/Shift+Tab tabs", JA: "←/→ または Tab/Shift+Tab でタブ切替"},
	"dashboard.footer.tabs":           {EN: "←/→ tabs", JA: "←/→ でタブ切替"},
	"dashboard.footer.tabs_right":     {EN: "→ tabs", JA: "→ でタブ切替"},
	"dashboard.footer.select":         {EN: "↑/↓ select", JA: "↑/↓ で選択"},
	"dashboard.footer.select_env":     {EN: "↑/↓ select environment", JA: "↑/↓ で環境を選択"},
	"dashboard.footer.scroll":         {EN: "↑/↓ scroll result", JA: "↑/↓ で結果をスクロール"},
	"dashboard.footer.enter_confirm":  {EN: "Enter confirm", JA: "Enter で確定"},
	"dashboard.footer.enter_open":     {EN: "Enter open", JA: "Enter で開く"},
	"dashboard.footer.enter_edit":     {EN: "Enter edit", JA: "Enter で編集"},
	"dashboard.footer.refresh":        {EN: "r refresh", JA: "r で更新"},
	"dashboard.footer.esc_exit":       {EN: "Esc exit", JA: "Esc で終了"},
	"dashboard.footer.esc_back":       {EN: "Esc back", JA: "Esc で戻る"},
	"dashboard.footer.left_esc_back":  {EN: "←/Esc back", JA: "←/Esc で戻る"},
	"dashboard.footer.env_back":       {EN: "←/Esc environments", JA: "←/Esc で環境一覧"},
	"dashboard.footer.enter_esc_back": {EN: "Enter/Esc back", JA: "Enter/Esc で戻る"},
	"menu.claude.label":               {EN: "Launch Claude", JA: "Claude を起動"},
	"menu.claude.description":         {EN: "Launch Claude in a worktree for the selected workspace.", JA: "選択した workspace の worktree で Claude を起動します。"},
	"menu.claude.impact":              {EN: "Uses the same worktree policy and readiness rules as the CLI.", JA: "worktree の方針と準備完了の条件は CLI と同じです。"},
	"menu.claude.input":               {EN: "Claude arguments", JA: "Claude の引数"},
	"menu.codex.label":                {EN: "Launch Codex", JA: "Codex を起動"},
	"menu.codex.description":          {EN: "Launch Codex in a worktree for the selected workspace.", JA: "選択した workspace の worktree で Codex を起動します。"},
	"menu.codex.impact":               {EN: "Hands terminal control to Codex and releases the lease after it exits.", JA: "端末の制御を Codex へ渡し、終了後に貸出を返却します。"},
	"menu.codex.input":                {EN: "Codex arguments", JA: "Codex の引数"},
	"menu.resume.label":               {EN: "Resume a conversation", JA: "会話を再開"},
	"menu.resume.description":         {EN: "Restore saved work for a wx session ID.", JA: "wx の session ID に保存した作業を復元します。"},
	"menu.resume.impact":              {EN: "Does not silently fall back to a fresh worktree if restoration fails.", JA: "復元に失敗したとき、黙って新しい worktree へ切り替えません。"},
	"menu.session_id.input":           {EN: "wx session ID", JA: "wx の session ID"},
	"menu.shell.label":                {EN: "Open a shell", JA: "シェルを開く"},
	"menu.shell.description":          {EN: "Open a shell in a leased worktree for the selected workspace.", JA: "選択した workspace の貸出 worktree でシェルを開きます。"},
	"menu.shell.impact":               {EN: "Saves unfinished work and releases the lease when the shell exits.", JA: "シェルの終了時に未完了の作業を保存し、貸出を返却します。"},
	"menu.shell.input":                {EN: "Shell arguments", JA: "シェルの引数"},
	"menu.run.label":                  {EN: "Run a command", JA: "コマンドを実行"},
	"menu.run.description":            {EN: "Run one command and its arguments in a leased worktree.", JA: "貸出 worktree で 1 つのコマンドとその引数を実行します。"},
	"menu.run.impact":                 {EN: "Passes the executable and arguments as separate values.", JA: "実行ファイルと引数は別々の値として渡します。"},
	"menu.run.input":                  {EN: "command and arguments", JA: "コマンドと引数"},
	"menu.new.label":                  {EN: "Create a path lease", JA: "path の貸出を作成"},
	"menu.new.description":            {EN: "Create a worktree lease and print its path and session ID.", JA: "worktree の貸出を作成し、path と session ID を表示します。"},
	"menu.new.impact":                 {EN: "The lease remains until its parent session, an explicit release, or its TTL ends it.", JA: "貸出は親 session の終了・明示的な返却・TTL のいずれかまで残ります。"},
	"menu.new.input":                  {EN: "Lease arguments", JA: "貸出の引数"},
	"menu.doctor.label":               {EN: "Standard diagnostics", JA: "標準診断"},
	"menu.doctor.description":         {EN: "Check configuration, the daemon, database, and slots.", JA: "設定・daemon・データベース・slot を確認します。"},
	"menu.doctor.impact":              {EN: "Still reports facts available locally when the daemon is unavailable.", JA: "daemon が使えないときも、手元で分かる事実は報告します。"},
	"menu.doctor_verbose.label":       {EN: "Verbose diagnostics", JA: "詳細診断"},
	"menu.doctor_verbose.description": {EN: "Include passing checks and additional diagnostic detail.", JA: "成功した確認項目と追加の診断情報も表示します。"},
	"menu.doctor_verbose.impact":      {EN: "Reads more information without changing managed state.", JA: "管理状態は変えずに、より多くの情報を読み取ります。"},
	"menu.doctor_probe.label":         {EN: "Worktree probe", JA: "Worktree 検査"},
	"menu.doctor_probe.description":   {EN: "Prepare a worktree in every registered workspace and inspect it.", JA: "登録済みの全 workspace で worktree を用意して検査します。"},
	"menu.doctor_probe.impact":        {EN: "Retires standby slots, so confirm before starting.", JA: "standby slot を回収するため、開始前に確認してください。"},
	"menu.gc.label":                   {EN: "Garbage collection", JA: "ガベージコレクション"},
	"menu.gc.description":             {EN: "Collect managed data whose retention period has elapsed.", JA: "保持期間を過ぎた管理データを回収します。"},
	"menu.gc.impact":                  {EN: "The daemon rechecks every candidate when the operation runs.", JA: "操作の実行時に daemon が候補を再確認します。"},
	"menu.clear.label":                {EN: "Clear sessions and standbys", JA: "session と standby を削除"},
	"menu.clear.description":          {EN: "Request removal of sessions and standby slots.", JA: "session と standby slot の削除を要求します。"},
	"menu.clear.impact":               {EN: "Uncommitted work is discarded only when --discard is explicitly supplied.", JA: "未コミットの作業は --discard を明示したときだけ破棄します。"},
	"menu.clear.input":                {EN: "Clear arguments", JA: "削除の引数"},
	"menu.prune.label":                {EN: "Prune recovery refs", JA: "復旧 ref を整理"},
	"menu.prune.description":          {EN: "Remove recovery refs that are safe to delete.", JA: "削除しても安全な復旧 ref を削除します。"},
	"menu.prune.impact":               {EN: "Every target is checked again when the operation runs.", JA: "対象は操作の実行時に再度確認します。"},
	"menu.prune.input":                {EN: "Prune arguments", JA: "整理の引数"},
	"menu.retry_standby.label":        {EN: "Retry standby replenishment", JA: "standby 補充を再試行"},
	"menu.retry_standby.description":  {EN: "Resume stopped standby replenishment for a workspace.", JA: "停止した workspace の standby 補充を再開します。"},
	"menu.retry_standby.impact":       {EN: "Does not modify quarantined slots.", JA: "隔離した slot は変更しません。"},
	"menu.release.label":              {EN: "Release a lease", JA: "lease を返却"},
	"menu.release.description":        {EN: "Explicitly release a lease created by wx new.", JA: "wx new が作成した貸出を明示的に返却します。"},
	"menu.release.impact":             {EN: "Normally creates a snapshot before releasing the lease.", JA: "通常は返却の前にスナップショットを作成します。"},
	"menu.discard_recovery.label":     {EN: "Discard recovery state", JA: "復旧状態を破棄"},
	"menu.discard_recovery.desc":      {EN: "Discard quarantined recovery snapshots for a workspace.", JA: "workspace の隔離した復旧スナップショットを破棄します。"},
	"menu.discard_recovery.impact":    {EN: "The original working state can no longer be restored.", JA: "元の作業状態は復元できなくなります。"},
	"menu.forget.label":               {EN: "Forget a workspace", JA: "workspace の管理を解除"},
	"menu.forget.description":         {EN: "Remove a registered workspace from wx management.", JA: "登録済みの workspace を wx の管理から外します。"},
	"menu.forget.impact":              {EN: "Physical removal still follows the daemon ownership rules.", JA: "実体の削除は daemon の所有権規則に従います。"},
	"menu.bench.label":                {EN: "Benchmark preparation", JA: "準備をベンチマーク"},
	"menu.bench.description":          {EN: "Measure preparation time and storage use for a workspace.", JA: "workspace の準備時間と使用量を計測します。"},
	"menu.bench.impact":               {EN: "Retires standby slots by default to measure a cold start.", JA: "cold start を測るため、既定で standby slot を回収します。"},
	"menu.bench.input":                {EN: "Benchmark arguments", JA: "ベンチマークの引数"},
	"menu.daemon_start.label":         {EN: "Start daemon", JA: "daemon を起動"},
	"menu.daemon_start.description":   {EN: "Start the daemon through its LaunchAgent and wait for a response.", JA: "LaunchAgent 経由で daemon を起動し、応答を待ちます。"},
	"menu.daemon_start.impact":        {EN: "Distinguishes request acceptance from successful startup.", JA: "要求の受理と起動の成功を区別します。"},
	"menu.daemon_stop.label":          {EN: "Stop daemon", JA: "daemon を停止"},
	"menu.daemon_stop.description":    {EN: "Ask the daemon to stop safely and wait for it to exit.", JA: "daemon に安全な停止を依頼し、終了を待ちます。"},
	"menu.daemon_stop.impact":         {EN: "In-flight operations continue until the existing idle gate allows shutdown.", JA: "実行中の操作は、既存の idle 条件が停止を許すまで続きます。"},
	"menu.daemon_restart.label":       {EN: "Restart daemon", JA: "daemon を再起動"},
	"menu.daemon_restart.description": {EN: "Stop the daemon and wait for a new process to respond.", JA: "daemon を停止し、新しい process の応答を待ちます。"},
	"menu.daemon_restart.impact":      {EN: "Also activates an updated wx binary.", JA: "更新した wx バイナリも有効になります。"},
	"config.language.description":     {EN: "Language used for human-readable CLI, TUI, and daemon messages.", JA: "CLI・TUI・daemon の人間向け表示に使う言語。"},
	"config.language.impact":          {EN: "Changes display text only; JSON output remains in English.", JA: "表示だけを変更し、JSON 出力は英語のままです。"},
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
