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
	"common.error":                       {EN: "error", JA: "エラー"},
	"common.saved":                       {EN: "saved and reloaded", JA: "保存して再読み込みしました"},
	"common.saved_pending":               {EN: "saved; daemon reload pending", JA: "保存しました（daemon の再読み込み待ち）"},
	"common.already_running":             {EN: "already running", JA: "すでに起動しています"},
	"common.already_stopped":             {EN: "already stopped", JA: "すでに停止しています"},
	"common.started":                     {EN: "started", JA: "起動しました"},
	"common.stopped":                     {EN: "stopped", JA: "停止しました"},
	"common.restarted":                   {EN: "restarted", JA: "再起動しました"},
	"common.installed":                   {EN: "installed", JA: "インストールしました"},
	"common.uninstalled":                 {EN: "uninstalled", JA: "アンインストールしました"},
	"common.cancelled":                   {EN: "cancelled", JA: "キャンセルしました"},
	"common.yes":                         {EN: "Yes", JA: "はい"},
	"common.no":                          {EN: "No", JA: "いいえ"},
	"common.none":                        {EN: "(none)", JA: "（なし）"},
	"common.unknown":                     {EN: "unknown", JA: "不明"},
	"common.pending":                     {EN: "pending", JA: "待機中"},
	"config.display_name":                {EN: "Display language", JA: "表示言語"},
	"config.language.english":            {EN: "English", JA: "English"},
	"config.language.japanese":           {EN: "Japanese", JA: "日本語"},
	"config.language.invalid":            {EN: "language must be en or ja", JA: "language は en または ja で指定してください"},
	"config.language.saved":              {EN: "display language changed to {{.Language}}", JA: "表示言語を {{.Language}} に変更しました"},
	"config.language.default":            {EN: "English", JA: "英語"},
	"setup.prerequisites":                {EN: "Prerequisites", JA: "前提条件"},
	"setup.worktree_root":                {EN: "Worktree root", JA: "Worktree root"},
	"setup.launch_agent":                 {EN: "LaunchAgent", JA: "LaunchAgent"},
	"setup.daemon":                       {EN: "Daemon", JA: "Daemon"},
	"setup.display_language.title":       {EN: "Display language / 表示言語", JA: "表示言語 / Display language"},
	"setup.display_language.prompt":      {EN: "Choose the display language", JA: "表示言語を選択してください"},
	"setup.display_language.english":     {EN: "English", JA: "English"},
	"setup.display_language.japanese":    {EN: "日本語", JA: "日本語"},
	"setup.cancelled":                    {EN: "cancelled; the items already applied are kept. Run wx setup again to finish.", JA: "キャンセルしました。適用済みの項目は保持されています。wx setup を再実行して残りを完了してください。"},
	"setup.needs_terminal":               {EN: "wx setup needs a terminal for its questions", JA: "wx setup の質問には端末が必要です"},
	"setup.check_hint":                   {EN: "run wx setup --check to see the current state without a terminal", JA: "端末なしで現在の状態を見るには wx setup --check を実行してください"},
	"setup.attention":                    {EN: "need attention", JA: "対応が必要です"},
	"setup.items_changed":                {EN: "{{.Count}} item(s) no longer match what wx would write.", JA: "{{.Count}} 件の項目が wx の書き込む内容と一致しません。"},
	"setup.no_terminal_update":           {EN: "wx setup: {{.Items}} need attention; run wx setup to review them", JA: "wx setup: {{.Items}} に対応が必要です。確認するには wx setup を実行してください"},
	"setup.leftover":                     {EN: "leftover", JA: "leftover"},
	"progress.starting":                  {EN: "starting", JA: "起動中"},
	"progress.restarting":                {EN: "restarting", JA: "再起動中"},
	"progress.stopping":                  {EN: "stopping", JA: "停止中"},
	"progress.clearing":                  {EN: "clearing", JA: "削除中"},
	"progress.diagnosing":                {EN: "diagnosing", JA: "診断中"},
	"progress.preparing":                 {EN: "preparing", JA: "準備中"},
	"rpc.daemon_unavailable":             {EN: "wx daemon is not running or still starting; try again shortly", JA: "wx daemon は起動していないか、起動中です。しばらくしてから再試行してください"},
	"rpc.readable_but_failed":            {EN: "wx daemon is reachable but this request did not complete", JA: "wx daemon には接続できますが、要求を完了できませんでした"},
	"rpc.readiness_blocked":              {EN: "wx readiness blocked operation", JA: "wx の準備完了条件により操作を中止しました"},
	"rpc.no_workspace":                   {EN: "no workspace was created", JA: "workspace は作成されませんでした"},
	"rpc.sqlite_unavailable":             {EN: "SQLite state is unavailable", JA: "SQLite の状態を利用できません"},
	"rpc.restore_backup":                 {EN: "restore a verified backup from", JA: "検証済みバックアップから復元するか"},
	"rpc.preserve_for_doctor":            {EN: "or preserve the database for wx doctor", JA: "データベースを保全して wx doctor で調査してください"},
	"rpc.degraded_read_only":             {EN: "wx daemon is read-only degraded: {{.Message}}", JA: "wx daemon は読み取り専用の縮退状態です: {{.Message}}"},
	"cli.interrupted":                    {EN: "interrupted before the workspace was leased", JA: "workspace の貸出前に中断されました"},
	"cli.interrupted_preparing":          {EN: "interrupted while the workspace was being prepared; releasing it", JA: "workspace の準備中に中断されました。貸出を返却します"},
	"cli.workspace_preparation":          {EN: "workspace preparation", JA: "workspace の準備"},
	"cli.resume_cancelled":               {EN: "resume cancelled; no workspace was created", JA: "再開をキャンセルしました。workspace は作成されませんでした"},
	"cli.clear_stop":                     {EN: "wx clear asked this session to stop before the agent started", JA: "agent の起動前に wx clear から停止要求を受けました"},
	"cli.run_needs_command":              {EN: "wx run needs a command after --", JA: "wx run には -- の後にコマンドが必要です"},
	"cli.reload_worktree_policy":         {EN: "reload worktree policy:", JA: "worktree 方針の再読み込み:"},
	"cli.session_is_lease":               {EN: "wx session {{.SessionID}} holds a lease, not an agent conversation; use wx shell --resume {{.SessionID}}", JA: "wx session {{.SessionID}} は lease を保持しており agent の会話ではありません。wx shell --resume {{.SessionID}} を使ってください"},
	"cli.no_working_directory":           {EN: "selected conversation has no working directory", JA: "選択した会話に作業ディレクトリがありません"},
	"cli.create_operation_identity":      {EN: "create operation identity", JA: "操作識別子を作成"},
	"cli.pin_workspace_cwd":              {EN: "pin workspace CWD", JA: "workspace の CWD を固定"},
	"cli.locate_helper":                  {EN: "locate wx descriptor helper", JA: "wx descriptor helper を見つける"},
	"cli.prepare_agent":                  {EN: "prepare agent", JA: "agent を準備"},
	"cli.register_agent_process":         {EN: "register agent process", JA: "agent process を登録"},
	"cli.lease_cancelled":                {EN: "lease cancelled; no workspace was created", JA: "貸出をキャンセルしました。workspace は作成されませんでした"},
	"cli.launch_cancelled":               {EN: "launch cancelled; no workspace was created", JA: "起動をキャンセルしました。workspace は作成されませんでした"},
	"cli.daemon_incomplete":              {EN: "wx daemon is reachable but this request did not complete ({{.Error}}); refusing to restart a socket that may still be serving other sessions, run wx doctor, or wx daemon restart if the daemon predates this wx", JA: "wx daemon には接続できますが、要求を完了できませんでした（{{.Error}}）。他の session を処理中かもしれない socket は再起動しません。wx doctor を実行するか、daemon がこの wx より古ければ wx daemon restart を実行してください"},
	"cli.daemon_unavailable_doctor":      {EN: "wx daemon is unavailable ({{.Error}}); run wx doctor", JA: "wx daemon は利用できません（{{.Error}}）。wx doctor を実行してください"},
	"cli.daemon_unavailable_install":     {EN: "wx daemon is unavailable ({{.Error}}); LaunchAgent plist is stale; run wx daemon install", JA: "wx daemon は利用できません（{{.Error}}）。LaunchAgent plist が古いため wx daemon install を実行してください"},
	"cli.daemon_not_ready_doctor":        {EN: "wx daemon did not become ready; run wx doctor", JA: "wx daemon の準備が完了しませんでした。wx doctor を実行してください"},
	"cli.daemon_not_ready_install":       {EN: "wx daemon did not become ready; LaunchAgent plist is stale; run wx daemon install", JA: "wx daemon の準備が完了しませんでした。LaunchAgent plist が古いため wx daemon install を実行してください"},
	"cli.worktree_disabled":              {EN: "workspace {{.Root}} is configured not to use a worktree; change worktree.undefined or the workspace policy {{.Marker}}", JA: "workspace {{.Root}} は worktree を使わない設定です。worktree.undefined または workspace policy {{.Marker}} を変更してください"},
	"cli.policy_reload_failed":           {EN: "policy saved but daemon reload failed: {{.Error}}", JA: "方針は保存しましたが daemon の再読み込みに失敗しました: {{.Error}}"},
	"cli.worktree_needs_terminal":        {EN: "worktree policy requires a terminal; use wx --worktree or wx --no-worktree, or configure worktree.undefined", JA: "worktree の方針選択には端末が必要です。wx --worktree / wx --no-worktree を使うか worktree.undefined を設定してください"},
	"cli.resolve_history":                {EN: "resolve workspace history: {{.Error}}; run wx daemon restart if the daemon has not been updated", JA: "workspace history を解決できません: {{.Error}}。daemon が更新されていなければ wx daemon restart を実行してください"},
	"cli.no_conversation":                {EN: "no conversation found for this workspace", JA: "この workspace に会話が見つかりません"},
	"cli.unknown_resume_intent":          {EN: "unknown resume intent", JA: "不明な resume 指示です"},
	"cli.lease_root_changed":             {EN: "lease root identity changed (expected {{.Expected}}, got {{.Actual}})", JA: "lease root の identity が変わりました（期待値 {{.Expected}}、実際 {{.Actual}}）"},
	"cli.codex_resume_notice":            {EN: "notice: codex exec resume needs a session ID or --last; starting in a new workspace without restoring a snapshot", JA: "通知: codex exec resume には session ID か --last が必要です。snapshot を復元せず新しい workspace で起動します"},
	"cli.resume_without_worktree":        {EN: "notice: resuming without a worktree; wx has no record of conversation {{.SessionID}}", JA: "通知: wx に会話 {{.SessionID}} の記録がないため、worktree を作らずに再開します"},
	"cli.released":                       {EN: "released {{.SessionID}}", JA: "{{.SessionID}} を返却しました"},
	"cli.released_discarded":             {EN: "released {{.SessionID}} and scheduled its worktree for removal without requiring a snapshot", JA: "{{.SessionID}} を返却しました。snapshot を作成せず worktree の削除を予約しました"},
	"cli.released_already_removed":       {EN: "released {{.SessionID}}; its worktree is already removed or scheduled for removal", JA: "{{.SessionID}} を返却しました。worktree は削除済みか削除予約済みです"},
	"cli.released_saving":                {EN: "released {{.SessionID}}; the worktree is still being saved, so run wx release --discard {{.SessionID}} again to remove it", JA: "{{.SessionID}} を返却しました。worktree は保存中のため、削除するには wx release --discard {{.SessionID}} を再実行してください"},
	"cli.fresh_cannot_restore":           {EN: "wx session {{.SessionID}} cannot restore its recorded worktree: {{.Reason}}", JA: "wx session {{.SessionID}} は記録された worktree を復元できません: {{.Reason}}"},
	"cli.fresh_auto":                     {EN: "notice: resume.auto_fresh is enabled; resuming the conversation in a new workspace from the current base", JA: "通知: resume.auto_fresh が有効なため、現在の base から新しい workspace で会話を再開します"},
	"cli.fresh_no_terminal":              {EN: "notice: no terminal is attached for the confirmation; resuming the conversation in a new workspace from the current base", JA: "通知: 確認用の端末がないため、現在の base から新しい workspace で会話を再開します"},
	"cli.fresh_title":                    {EN: "Recovery worktree is unavailable. Resume the conversation in a new workspace?", JA: "復元用 worktree を利用できません。新しい workspace で会話を再開しますか？"},
	"cli.fresh_yes":                      {EN: "create a new worktree from the current base", JA: "現在の base から新しい worktree を作成"},
	"cli.fresh_no":                       {EN: "cancel the launch", JA: "起動をキャンセル"},
	"cli.linked_worktree_base":           {EN: "the current directory is a linked worktree of {{.Path}} at HEAD {{.Head}}; wx leases a workspace from the main worktree {{.MainPath}} at HEAD {{.MainHead}}", JA: "現在のディレクトリは {{.Path}} の linked worktree（HEAD {{.Head}}）です。wx は main worktree {{.MainPath}}（HEAD {{.MainHead}}）から workspace を貸し出します"},
	"cli.linked_worktree_no_terminal":    {EN: "notice: no terminal is attached for the confirmation; leasing a workspace from the main worktree's HEAD", JA: "通知: 確認用の端末がないため、main worktree の HEAD から workspace を貸し出します"},
	"cli.linked_worktree_title":          {EN: "The current directory is a linked worktree. Lease a workspace from the main worktree's HEAD?", JA: "現在のディレクトリは linked worktree です。main worktree の HEAD から workspace を貸し出しますか？"},
	"cli.linked_worktree_yes":            {EN: "use the main worktree's HEAD {{.Head}}", JA: "main worktree の HEAD {{.Head}} を使う"},
	"cli.linked_worktree_no":             {EN: "cancel; rerun from the main worktree or pass --branch", JA: "キャンセル。main worktree から再実行するか --branch を指定"},
	"cli.probing":                        {EN: "probing {{.Root}}", JA: "検査中 {{.Root}}"},
	"cli.release_probe_failed":           {EN: "warning: release probe lease {{.SessionID}}:", JA: "警告: probe 用貸出 {{.SessionID}} の返却に失敗:"},
	"worktree.policy_title":              {EN: "Worktree policy", JA: "Worktree の方針"},
	"worktree.hot_label":                 {EN: "Hot standby", JA: "Hot standby"},
	"worktree.hot_description":           {EN: "keep a worktree ready for faster launches", JA: "高速起動のため worktree を準備しておく"},
	"worktree.cold_label":                {EN: "Cold start", JA: "Cold start"},
	"worktree.cold_description":          {EN: "create a worktree when launching an agent", JA: "agent 起動時に worktree を作成する"},
	"worktree.off_label":                 {EN: "No worktree", JA: "Worktree なし"},
	"worktree.off_description":           {EN: "run the agent in the current directory", JA: "現在のディレクトリで agent を実行する"},
	"flag.fresh_requires_resume":         {EN: "--fresh requires a resume operation", JA: "--fresh には resume 操作が必要です"},
	"flag.branch_fresh_require_worktree": {EN: "--branch and --fresh require a worktree", JA: "--branch と --fresh には worktree が必要です"},
	"lease.resolving":                    {EN: "Resolving workspace", JA: "workspace を解決中"},
	"lease.route.ready":                  {EN: "Ready standby", JA: "準備済み standby"},
	"lease.route.update":                 {EN: "Standby update", JA: "standby を更新中"},
	"lease.route.cold":                   {EN: "Cold start", JA: "cold start"},
	"lease.route.restore":                {EN: "Restoring workspace", JA: "workspace を復元中"},
	"lease.route.preparing":              {EN: "Preparing workspace", JA: "workspace を準備中"},
	"lease.queued":                       {EN: "queued", JA: "待機中"},
	"bench.flag_runs":                    {EN: "--runs must be at least 1", JA: "--runs は 1 以上で指定してください"},
	"bench.flag_reuse":                   {EN: "--config and --sweep cannot be combined with --reuse; each configuration is measured as a cold start", JA: "--config と --sweep は --reuse と併用できません。各設定は cold start として計測します"},
	"bench.no_workspace":                 {EN: "cannot resolve a wx workspace from {{.Path}}; run wx bench --reuse to measure without retiring standby worktrees", JA: "{{.Path}} から wx workspace を解決できません。standby worktree を回収せずに計測するには wx bench --reuse を実行してください"},
	"bench.release_failed":               {EN: "warning: release bench lease {{.SessionID}}:", JA: "警告: bench の貸出 {{.SessionID}} の返却に失敗:"},
	"bench.run":                          {EN: "run", JA: "測定"},
	"bench.lease":                        {EN: "lease", JA: "貸出"},
	"bench.retired":                      {EN: "retired", JA: "退役"},
	"bench.early_ready":                  {EN: "EARLY READY", JA: "早期準備完了"},
	"bench.full_ready":                   {EN: "FULL READY", JA: "準備完了"},
	"bench.slot_usage":                   {EN: "slot usage", JA: "slot 使用量"},
	"bench.prepare_job":                  {EN: "prepare job", JA: "準備 job"},
	"bench.standby_slots":                {EN: "standby slot(s)", JA: "standby slot"},
	"bench.exclusive":                    {EN: "exclusive", JA: "専有"},
	"bench.shared":                       {EN: "shared", JA: "共有"},
	"bench.summary":                      {EN: "summary of {{.Count}} successful run(s)", JA: "成功した測定 {{.Count}} 件の概要"},
	"bench.config":                       {EN: "config", JA: "設定"},
	"bench.runs":                         {EN: "runs", JA: "回数"},
	"bench.fail":                         {EN: "fail", JA: "失敗"},
	"bench.headline_distribution":        {EN: "comparison by configuration (min/median/max, usage is the median of the measured runs)", JA: "設定ごとの比較 (min/median/max, 使用量は測定値の中央値)"},
	"bench.headline":                     {EN: "comparison by configuration (usage is the median of the measured runs)", JA: "設定ごとの比較 (使用量は測定値の中央値)"},
	"dashboard.status":                   {EN: "System status", JA: "システム状態"},
	"dashboard.loading":                  {EN: "Loading from the daemon…", JA: "daemon から読み込み中…"},
	"dashboard.refresh_failed":           {EN: "Refresh failed: {{.Error}}", JA: "更新に失敗しました: {{.Error}}"},
	"dashboard.last_response":            {EN: "Showing the last successful response.", JA: "最後に成功した応答を表示しています。"},
	"dashboard.no_status":                {EN: "No status is available.", JA: "状態を取得できません。"},
	"dashboard.choose_launch":            {EN: "Choose what to launch", JA: "起動するものを選択"},
	"dashboard.editable_settings":        {EN: "Editable settings", JA: "編集できる設定"},
	"dashboard.environments":             {EN: "Environments", JA: "環境"},
	"dashboard.diagnostic_mode":          {EN: "Diagnostic mode", JA: "診断モード"},
	"dashboard.maintenance":              {EN: "Maintenance operation", JA: "保守操作"},
	"dashboard.integrations":             {EN: "Integrations and daemon", JA: "連携と daemon"},
	"dashboard.global":                   {EN: "Global", JA: "全体"},
	"dashboard.workspace":                {EN: "Workspace", JA: "Workspace"},
	"dashboard.repository":               {EN: "Repository", JA: "Repository"},
	"dashboard.enabled":                  {EN: "Enabled", JA: "有効"},
	"dashboard.disabled":                 {EN: "Disabled", JA: "無効"},
	"dashboard.reset_default":            {EN: "Reset to default", JA: "既定値に戻す"},
	"dashboard.enter_custom":             {EN: "Enter a custom value…", JA: "値を入力…"},
	"dashboard.add_value":                {EN: "Add a value…", JA: "値を追加…"},
	"dashboard.remove_value":             {EN: "Remove a value…", JA: "値を削除…"},
	"dashboard.confirm":                  {EN: "Run this operation?", JA: "この操作を実行しますか？"},
	"dashboard.running":                  {EN: "Running…", JA: "実行中…"},
	"dashboard.result_empty":             {EN: "The operation produced no output.", JA: "操作の出力はありません。"},
	"dashboard.tab.status":               {EN: "Status", JA: "状態"},
	"dashboard.tab.launch":               {EN: "Launch", JA: "起動"},
	"dashboard.tab.settings":             {EN: "Settings", JA: "設定"},
	"dashboard.tab.doctor":               {EN: "Doctor", JA: "診断"},
	"dashboard.tab.maintenance":          {EN: "Maintenance", JA: "保守"},
	"dashboard.tab.system":               {EN: "System", JA: "システム"},
	"dashboard.config_load_failed":       {EN: "Could not load configuration: {{.Error}}", JA: "設定を読み込めませんでした: {{.Error}}"},
	"dashboard.updated":                  {EN: "Updated {{.Time}}", JA: "{{.Time}} に更新"},
	"dashboard.updated_age":              {EN: "Updated {{.Time}} ({{.Age}} ago)", JA: "{{.Time}} に更新（{{.Age}} 前）"},
	"dashboard.no_actions":               {EN: "No actions are available.", JA: "実行できる操作がありません。"},
	"dashboard.system":                   {EN: "System", JA: "システム"},
	"dashboard.workspace_defaults":       {EN: "Workspace defaults", JA: "Workspace の既定値"},
	"dashboard.repository_defaults":      {EN: "Repository defaults", JA: "Repository の既定値"},
	"dashboard.not_discovered":           {EN: "(not discovered)", JA: "（未検出）"},
	"dashboard.scope":                    {EN: "Scope: {{.Scope}}", JA: "スコープ: {{.Scope}}"},
	"dashboard.effective_settings":       {EN: "Effective settings", JA: "実効設定"},
	"dashboard.impact":                   {EN: "Impact", JA: "影響"},
	"dashboard.behavior":                 {EN: "Behavior", JA: "動作"},
	"dashboard.attention":                {EN: "Attention", JA: "注意"},
	"dashboard.choices":                  {EN: "Choices: {{.Choices}}", JA: "選択肢: {{.Choices}}"},
	"dashboard.choose_value":             {EN: "Choose a value or action:", JA: "値または操作を選択:"},
	"dashboard.current":                  {EN: "Current: {{.Value}}", JA: "現在値: {{.Value}}"},
	"dashboard.change":                   {EN: "Change: {{.Value}}", JA: "変更: {{.Value}}"},
	"dashboard.target":                   {EN: "Target: {{.Value}}", JA: "対象: {{.Value}}"},
	"dashboard.input":                    {EN: "Input: {{.Value}}", JA: "入力: {{.Value}}"},
	"dashboard.inherited":                {EN: "inherited / unset", JA: "継承・未設定"},
	"dashboard.destructive_note":         {EN: "This operation may delete data.", JA: "この操作はデータを削除する可能性があります。"},
	"dashboard.destructive_confirm":      {EN: "This may delete data. Check the target carefully.", JA: "データを削除する可能性があります。対象を確認してください。"},
	"dashboard.confirm_run":              {EN: "Enter / y run", JA: "Enter / y で実行"},
	"dashboard.confirm_back":             {EN: "Esc / n back", JA: "Esc / n で戻る"},
	"dashboard.running_note":             {EN: "The result will appear here when the operation finishes.", JA: "操作が完了すると結果がここに表示されます。"},
	"dashboard.result_title":             {EN: "{{.Label}} — exit {{.Code}}", JA: "{{.Label}} — 終了コード {{.Code}}"},
	"dashboard.value_prompt":             {EN: "Value", JA: "値"},
	"dashboard.workspace_path":           {EN: "Workspace path", JA: "workspace の path"},
	"dashboard.keep_current":             {EN: "Keep current value: {{.Value}}", JA: "現在値を保持: {{.Value}}"},
	"dashboard.all_workspaces":           {EN: "All registered workspaces", JA: "登録済みの全 workspace"},
	"dashboard.another_path":             {EN: "Enter another path…", JA: "別の path を入力…"},
	"dashboard.custom_arguments":         {EN: "Enter custom arguments…", JA: "引数を入力…"},
	"dashboard.default_launch":           {EN: "Launch with default options", JA: "既定の設定で起動"},
	"dashboard.default_open":             {EN: "Open with default options", JA: "既定の設定で開く"},
	"dashboard.default_base":             {EN: "Use the default base", JA: "既定の base を使う"},
	"dashboard.preview_changes":          {EN: "Preview changes", JA: "変更を確認"},
	"dashboard.run_gc":                   {EN: "Run garbage collection", JA: "ガベージコレクションを実行"},
	"dashboard.clear_preview_default":    {EN: "Preview default targets", JA: "既定の対象を確認"},
	"dashboard.clear_default":            {EN: "Clear default targets", JA: "既定の対象を削除"},
	"dashboard.clear_preview_standby":    {EN: "Preview including standbys", JA: "standby を含めて確認"},
	"dashboard.clear_standby":            {EN: "Clear including standbys", JA: "standby を含めて削除"},
	"dashboard.prune_preview_safe":       {EN: "Preview safe refs", JA: "安全な ref を確認"},
	"dashboard.prune_safe":               {EN: "Prune safe refs", JA: "安全な ref を整理"},
	"dashboard.prune_preview_all":        {EN: "Preview all refs", JA: "全 ref を確認"},
	"dashboard.prune_all":                {EN: "Prune all refs", JA: "全 ref を整理"},
	"dashboard.release_save":             {EN: "Save work and release", JA: "作業を保存して返却"},
	"dashboard.release_discard":          {EN: "Discard work and release", JA: "作業を破棄して返却"},
	"dashboard.bench_cold":               {EN: "Run one cold preparation", JA: "cold start を 1 回計測"},
	"dashboard.bench_sweep":              {EN: "Run the standard configuration sweep", JA: "標準の設定を一通り計測"},
	"dashboard.bench_reuse":              {EN: "Measure standby reuse", JA: "standby の再利用を計測"},
	"dashboard.footer.tabs_full":         {EN: "←/→ or Tab/Shift+Tab tabs", JA: "←/→ または Tab/Shift+Tab でタブ切替"},
	"dashboard.footer.tabs":              {EN: "←/→ tabs", JA: "←/→ でタブ切替"},
	"dashboard.footer.tabs_right":        {EN: "→ tabs", JA: "→ でタブ切替"},
	"dashboard.footer.select":            {EN: "↑/↓ select", JA: "↑/↓ で選択"},
	"dashboard.footer.select_env":        {EN: "↑/↓ select environment", JA: "↑/↓ で環境を選択"},
	"dashboard.footer.scroll":            {EN: "↑/↓ scroll result", JA: "↑/↓ で結果をスクロール"},
	"dashboard.footer.enter_confirm":     {EN: "Enter confirm", JA: "Enter で確定"},
	"dashboard.footer.enter_open":        {EN: "Enter open", JA: "Enter で開く"},
	"dashboard.footer.enter_edit":        {EN: "Enter edit", JA: "Enter で編集"},
	"dashboard.footer.refresh":           {EN: "r refresh", JA: "r で更新"},
	"dashboard.footer.esc_exit":          {EN: "Esc exit", JA: "Esc で終了"},
	"dashboard.footer.esc_back":          {EN: "Esc back", JA: "Esc で戻る"},
	"dashboard.footer.left_esc_back":     {EN: "←/Esc back", JA: "←/Esc で戻る"},
	"dashboard.footer.env_back":          {EN: "←/Esc environments", JA: "←/Esc で環境一覧"},
	"dashboard.footer.enter_esc_back":    {EN: "Enter/Esc back", JA: "Enter/Esc で戻る"},
	"menu.claude.label":                  {EN: "Launch Claude", JA: "Claude を起動"},
	"menu.claude.description":            {EN: "Launch Claude in a worktree for the selected workspace.", JA: "選択した workspace の worktree で Claude を起動します。"},
	"menu.claude.impact":                 {EN: "Uses the same worktree policy and readiness rules as the CLI.", JA: "worktree の方針と準備完了の条件は CLI と同じです。"},
	"menu.claude.input":                  {EN: "Claude arguments", JA: "Claude の引数"},
	"menu.codex.label":                   {EN: "Launch Codex", JA: "Codex を起動"},
	"menu.codex.description":             {EN: "Launch Codex in a worktree for the selected workspace.", JA: "選択した workspace の worktree で Codex を起動します。"},
	"menu.codex.impact":                  {EN: "Hands terminal control to Codex and releases the lease after it exits.", JA: "端末の制御を Codex へ渡し、終了後に貸出を返却します。"},
	"menu.codex.input":                   {EN: "Codex arguments", JA: "Codex の引数"},
	"menu.resume.label":                  {EN: "Resume a conversation", JA: "会話を再開"},
	"menu.resume.description":            {EN: "Restore saved work for a wx session ID.", JA: "wx の session ID に保存した作業を復元します。"},
	"menu.resume.impact":                 {EN: "Does not silently fall back to a fresh worktree if restoration fails.", JA: "復元に失敗したとき、黙って新しい worktree へ切り替えません。"},
	"menu.session_id.input":              {EN: "wx session ID", JA: "wx の session ID"},
	"menu.shell.label":                   {EN: "Open a shell", JA: "シェルを開く"},
	"menu.shell.description":             {EN: "Open a shell in a leased worktree for the selected workspace.", JA: "選択した workspace の貸出 worktree でシェルを開きます。"},
	"menu.shell.impact":                  {EN: "Saves unfinished work and releases the lease when the shell exits.", JA: "シェルの終了時に未完了の作業を保存し、貸出を返却します。"},
	"menu.shell.input":                   {EN: "Shell arguments", JA: "シェルの引数"},
	"menu.run.label":                     {EN: "Run a command", JA: "コマンドを実行"},
	"menu.run.description":               {EN: "Run one command and its arguments in a leased worktree.", JA: "貸出 worktree で 1 つのコマンドとその引数を実行します。"},
	"menu.run.impact":                    {EN: "Passes the executable and arguments as separate values.", JA: "実行ファイルと引数は別々の値として渡します。"},
	"menu.run.input":                     {EN: "command and arguments", JA: "コマンドと引数"},
	"menu.new.label":                     {EN: "Create a path lease", JA: "path の貸出を作成"},
	"menu.new.description":               {EN: "Create a worktree lease and print its path and session ID.", JA: "worktree の貸出を作成し、path と session ID を表示します。"},
	"menu.new.impact":                    {EN: "The lease remains until its parent session, an explicit release, or its TTL ends it.", JA: "貸出は親 session の終了・明示的な返却・TTL のいずれかまで残ります。"},
	"menu.new.input":                     {EN: "Lease arguments", JA: "貸出の引数"},
	"menu.doctor.label":                  {EN: "Standard diagnostics", JA: "標準診断"},
	"menu.doctor.description":            {EN: "Check configuration, the daemon, database, and slots.", JA: "設定・daemon・データベース・slot を確認します。"},
	"menu.doctor.impact":                 {EN: "Still reports facts available locally when the daemon is unavailable.", JA: "daemon が使えないときも、手元で分かる事実は報告します。"},
	"menu.doctor_verbose.label":          {EN: "Verbose diagnostics", JA: "詳細診断"},
	"menu.doctor_verbose.description":    {EN: "Include passing checks and additional diagnostic detail.", JA: "成功した確認項目と追加の診断情報も表示します。"},
	"menu.doctor_verbose.impact":         {EN: "Reads more information without changing managed state.", JA: "管理状態は変えずに、より多くの情報を読み取ります。"},
	"menu.doctor_probe.label":            {EN: "Worktree probe", JA: "Worktree 検査"},
	"menu.doctor_probe.description":      {EN: "Prepare a worktree in every registered workspace and inspect it.", JA: "登録済みの全 workspace で worktree を用意して検査します。"},
	"menu.doctor_probe.impact":           {EN: "Retires standby slots, so confirm before starting.", JA: "standby slot を回収するため、開始前に確認してください。"},
	"menu.gc.label":                      {EN: "Garbage collection", JA: "ガベージコレクション"},
	"menu.gc.description":                {EN: "Collect managed data whose retention period has elapsed.", JA: "保持期間を過ぎた管理データを回収します。"},
	"menu.gc.impact":                     {EN: "The daemon rechecks every candidate when the operation runs.", JA: "操作の実行時に daemon が候補を再確認します。"},
	"menu.clear.label":                   {EN: "Clear sessions and standbys", JA: "session と standby を削除"},
	"menu.clear.description":             {EN: "Request removal of sessions and standby slots.", JA: "session と standby slot の削除を要求します。"},
	"menu.clear.impact":                  {EN: "Uncommitted work is discarded only when --discard is explicitly supplied.", JA: "未コミットの作業は --discard を明示したときだけ破棄します。"},
	"menu.clear.input":                   {EN: "Clear arguments", JA: "削除の引数"},
	"menu.prune.label":                   {EN: "Prune recovery refs", JA: "復旧 ref を整理"},
	"menu.prune.description":             {EN: "Remove recovery refs that are safe to delete.", JA: "削除しても安全な復旧 ref を削除します。"},
	"menu.prune.impact":                  {EN: "Every target is checked again when the operation runs.", JA: "対象は操作の実行時に再度確認します。"},
	"menu.prune.input":                   {EN: "Prune arguments", JA: "整理の引数"},
	"menu.retry_standby.label":           {EN: "Retry standby replenishment", JA: "standby 補充を再試行"},
	"menu.retry_standby.description":     {EN: "Resume stopped standby replenishment for a workspace.", JA: "停止した workspace の standby 補充を再開します。"},
	"menu.retry_standby.impact":          {EN: "Does not modify quarantined slots.", JA: "隔離した slot は変更しません。"},
	"menu.release.label":                 {EN: "Release a lease", JA: "lease を返却"},
	"menu.release.description":           {EN: "Explicitly release a lease created by wx new.", JA: "wx new が作成した貸出を明示的に返却します。"},
	"menu.release.impact":                {EN: "Normally creates a snapshot before releasing the lease.", JA: "通常は返却の前にスナップショットを作成します。"},
	"menu.discard_recovery.label":        {EN: "Discard recovery state", JA: "復旧状態を破棄"},
	"menu.discard_recovery.desc":         {EN: "Discard quarantined recovery snapshots for a workspace.", JA: "workspace の隔離した復旧スナップショットを破棄します。"},
	"menu.discard_recovery.impact":       {EN: "The original working state can no longer be restored.", JA: "元の作業状態は復元できなくなります。"},
	"menu.forget.label":                  {EN: "Forget a workspace", JA: "workspace の管理を解除"},
	"menu.forget.description":            {EN: "Remove a registered workspace from wx management.", JA: "登録済みの workspace を wx の管理から外します。"},
	"menu.forget.impact":                 {EN: "Physical removal still follows the daemon ownership rules.", JA: "実体の削除は daemon の所有権規則に従います。"},
	"menu.bench.label":                   {EN: "Benchmark preparation", JA: "準備をベンチマーク"},
	"menu.bench.description":             {EN: "Measure preparation time and storage use for a workspace.", JA: "workspace の準備時間と使用量を計測します。"},
	"menu.bench.impact":                  {EN: "Retires standby slots by default to measure a cold start.", JA: "cold start を測るため、既定で standby slot を回収します。"},
	"menu.bench.input":                   {EN: "Benchmark arguments", JA: "ベンチマークの引数"},
	"menu.daemon_start.label":            {EN: "Start daemon", JA: "daemon を起動"},
	"menu.daemon_start.description":      {EN: "Start the daemon through its LaunchAgent and wait for a response.", JA: "LaunchAgent 経由で daemon を起動し、応答を待ちます。"},
	"menu.daemon_start.impact":           {EN: "Distinguishes request acceptance from successful startup.", JA: "要求の受理と起動の成功を区別します。"},
	"menu.daemon_stop.label":             {EN: "Stop daemon", JA: "daemon を停止"},
	"menu.daemon_stop.description":       {EN: "Ask the daemon to stop safely and wait for it to exit.", JA: "daemon に安全な停止を依頼し、終了を待ちます。"},
	"menu.daemon_stop.impact":            {EN: "In-flight operations continue until the existing idle gate allows shutdown.", JA: "実行中の操作は、既存の idle 条件が停止を許すまで続きます。"},
	"menu.daemon_restart.label":          {EN: "Restart daemon", JA: "daemon を再起動"},
	"menu.daemon_restart.description":    {EN: "Stop the daemon and wait for a new process to respond.", JA: "daemon を停止し、新しい process の応答を待ちます。"},
	"menu.daemon_restart.impact":         {EN: "Also activates an updated wx binary.", JA: "更新した wx バイナリも有効になります。"},
	"config.language.description":        {EN: "Language used for human-readable CLI, TUI, and daemon messages.", JA: "CLI・TUI・daemon の人間向け表示に使う言語。"},
	"config.language.impact":             {EN: "Changes display text only; JSON output remains in English.", JA: "表示だけを変更し、JSON 出力は英語のままです。"},
	"help.usage":                         {EN: "Usage", JA: "使い方"},
	"help.global_options":                {EN: "Global options:", JA: "全体オプション:"},
	"help.commands":                      {EN: "Commands:", JA: "コマンド:"},
	"help.show_help":                     {EN: "show help", JA: "ヘルプを表示"},
	"help.show_version":                  {EN: "show version", JA: "バージョンを表示"},
	// status.* は wx status の描画時ローカライズで使う。訳文には固定文だけを置き、
	// path・ID・状態値・時刻・外部エラーはテンプレートのプレースホルダへ不透明値として渡す。
	"status.section.additional":                 {EN: "Additional", JA: "追加情報"},
	"status.section.workspaces":                 {EN: "Workspaces", JA: "ワークスペース"},
	"status.section.repositories":               {EN: "Repositories ({{.Zone}})", JA: "リポジトリ ({{.Zone}})"},
	"status.section.sessions":                   {EN: "Sessions", JA: "セッション"},
	"status.section.daemon":                     {EN: "Daemon", JA: "daemon"},
	"status.section.config":                     {EN: "Config", JA: "設定"},
	"status.section.backup":                     {EN: "Backup", JA: "バックアップ"},
	"status.section.pool":                       {EN: "Pool", JA: "プール"},
	"status.section.jobs":                       {EN: "Jobs", JA: "ジョブ"},
	"status.section.snapshots":                  {EN: "Snapshots", JA: "スナップショット"},
	"status.section.storage":                    {EN: "Storage", JA: "ストレージ"},
	"status.section.retention":                  {EN: "Retention", JA: "保持期間"},
	"status.section.quarantine":                 {EN: "Quarantine", JA: "隔離"},
	"status.section.standby_replenishment":      {EN: "Standby replenishment", JA: "standby 補充"},
	"status.section.root":                       {EN: "Root {{.Index}}", JA: "root {{.Index}}"},
	"status.table.workspace":                    {EN: "WORKSPACE", JA: "ワークスペース"},
	"status.table.policy":                       {EN: "POLICY", JA: "方針"},
	"status.table.ready":                        {EN: "READY", JA: "READY"},
	"status.table.in_use":                       {EN: "IN USE", JA: "使用中"},
	"status.table.last_used":                    {EN: "LAST USED", JA: "最終使用"},
	"status.table.last_used_zone":               {EN: "LAST USED ({{.Zone}})", JA: "最終使用 ({{.Zone}})"},
	"status.table.note":                         {EN: "NOTE", JA: "注記"},
	"status.table.id":                           {EN: "ID", JA: "ID"},
	"status.table.path":                         {EN: "PATH", JA: "パス"},
	"status.table.generation":                   {EN: "GENERATION", JA: "世代"},
	"status.table.repositories":                 {EN: "REPOSITORIES", JA: "リポジトリ"},
	"status.table.failed_detail":                {EN: "FAILED (FAILED + QUARANTINED)", JA: "失敗 (失敗 + 隔離)"},
	"status.table.hot":                          {EN: "HOT", JA: "HOT"},
	"status.table.standby_ready":                {EN: "STANDBY READY", JA: "standby 準備完了"},
	"status.table.standby_expires":              {EN: "STANDBY EXPIRES", JA: "standby 期限"},
	"status.table.agent":                        {EN: "AGENT", JA: "エージェント"},
	"status.table.state":                        {EN: "STATE", JA: "状態"},
	"status.table.created_zone":                 {EN: "CREATED ({{.Zone}})", JA: "作成 ({{.Zone}})"},
	"status.table.elapsed":                      {EN: "ELAPSED", JA: "経過"},
	"status.table.kind":                         {EN: "KIND", JA: "種別"},
	"status.table.reason":                       {EN: "REASON", JA: "理由"},
	"status.label.daemon":                       {EN: "Daemon", JA: "daemon"},
	"status.label.disk":                         {EN: "Disk", JA: "ディスク"},
	"status.label.unmanaged":                    {EN: "Unmanaged", JA: "未管理"},
	"status.daemon.degraded":                    {EN: "Daemon degraded", JA: "daemon（縮退）"},
	"status.daemon.degraded_error":              {EN: "Daemon degraded · {{.Error}}", JA: "daemon（縮退） · {{.Error}}"},
	"status.daemon.summary_jobs":                {EN: "{{.State}} · Jobs {{.Pending}} pending / {{.Running}} running / {{.Failed}} failed / {{.Discarded}} discarded", JA: "{{.State}} · ジョブ {{.Pending}} pending / {{.Running}} running / {{.Failed}} failed / {{.Discarded}} discarded"},
	"status.disk.failed":                        {EN: "measurement failed · {{.Path}} · {{.Error}}", JA: "計測に失敗 · {{.Path}} · {{.Error}}"},
	"status.disk.measuring":                     {EN: "measuring · {{.Path}}", JA: "計測中 · {{.Path}}"},
	"status.disk.unavailable":                   {EN: "measurement unavailable · {{.Path}}", JA: "計測できません · {{.Path}}"},
	"status.disk.managed":                       {EN: "{{.Size}} managed · {{.Path}}", JA: "{{.Size}} 管理対象 · {{.Path}}"},
	"status.disk.managed_measured":              {EN: "{{.Size}} managed · {{.Path}} · measured {{.Time}} {{.Zone}}", JA: "{{.Size}} 管理対象 · {{.Path}} · 計測 {{.Time}} {{.Zone}}"},
	"status.disk.unmanaged":                     {EN: "{{.Size}} · excluded from cleanup", JA: "{{.Size}} · cleanup 対象外"},
	"status.standby.stopped":                    {EN: "standby replenishment stopped", JA: "standby 補充が停止しました"},
	"status.standby.stopped_clean":              {EN: "standby replenishment stopped after wx clear", JA: "wx clear 後に standby 補充が停止しました"},
	"status.standby.stopped_prepare":            {EN: "standby replenishment stopped after a preparation failure", JA: "準備失敗後に standby 補充が停止しました"},
	"status.standby.plan_failed":                {EN: "standby replenishment failed to plan new worktrees", JA: "standby 補充の新しい worktree 計画に失敗しました"},
	"status.standby.note":                       {EN: "! {{.Reason}}", JA: "! {{.Reason}}"},
	"status.standby.note_action":                {EN: "! {{.Reason}}; run {{.Action}}", JA: "! {{.Reason}}。{{.Action}} を実行してください"},
	"status.summary.archived":                   {EN: "{{.Count}} (earliest archived {{.Earliest}}, latest expiry {{.Latest}})", JA: "{{.Count}}（最古のアーカイブ {{.Earliest}}、最も遅い期限 {{.Latest}}）"},
	"status.notice.policy_unavailable":          {EN: "POLICY unavailable: daemon JSON schema {{.Schema}} has no workspace policy; update the daemon.", JA: "方針を利用できません: daemon の JSON schema {{.Schema}} は workspace の方針を持ちません。daemon を更新してください。"},
	"status.notice.last_used_unavailable":       {EN: "LAST USED unavailable: daemon JSON schema {{.Schema}} has no workspace history; update the daemon.", JA: "最終使用を利用できません: daemon の JSON schema {{.Schema}} は workspace の履歴を持ちません。daemon を更新してください。"},
	"status.notice.archived_unavailable":        {EN: "Archived unavailable: daemon JSON schema {{.Schema}} has no archived session summary; the table above still lists archived sessions. Update the daemon.", JA: "アーカイブ済み情報を利用できません: daemon の JSON schema {{.Schema}} はアーカイブ済み session の集計を持ちません。上の表にはアーカイブ済み session が残ります。daemon を更新してください。"},
	"status.notice.hidden_workspace":            {EN: "1 registered workspace uses no worktree; run wx status --verbose to list it", JA: "worktree を使わない登録済み workspace が 1 件あります。一覧するには wx status --verbose を実行してください"},
	"status.notice.hidden_workspaces":           {EN: "{{.Count}} registered workspaces use no worktree; run wx status --verbose to list them", JA: "worktree を使わない登録済み workspace が {{.Count}} 件あります。一覧するには wx status --verbose を実行してください"},
	"status.notice.quarantine_slot":             {EN: "slot: wx clear deletes quarantined slots right away, without waiting out retention.quarantined.", JA: "slot: wx clear は retention.quarantined を待たずに隔離 slot を削除します。"},
	"status.notice.quarantine_refs":             {EN: "unknown_refs: wx prune deletes the recovery refs it can prove are safe to lose; wx prune --dry-run reports them first.", JA: "unknown_refs: wx prune は失っても安全と確認できた復旧 ref を削除します。wx prune --dry-run で先に一覧できます。"},
	"status.field.database":                     {EN: "Database", JA: "データベース"},
	"status.field.version":                      {EN: "Version", JA: "バージョン"},
	"status.field.protocol_version":             {EN: "Protocol version", JA: "プロトコルバージョン"},
	"status.field.pid":                          {EN: "PID", JA: "PID"},
	"status.field.uptime":                       {EN: "Uptime", JA: "稼働時間"},
	"status.field.degraded":                     {EN: "Degraded", JA: "縮退"},
	"status.field.restart_pending":              {EN: "Restart pending", JA: "再起動待ち"},
	"status.field.stop_pending":                 {EN: "Stop pending", JA: "停止待ち"},
	"status.field.error":                        {EN: "Error", JA: "エラー"},
	"status.field.json_schema_version":          {EN: "JSON schema version", JA: "JSON schema バージョン"},
	"status.field.db_schema_version":            {EN: "DB schema version", JA: "DB schema バージョン"},
	"status.field.path":                         {EN: "Path", JA: "パス"},
	"status.field.last_reload":                  {EN: "Last reload", JA: "最終再読み込み"},
	"status.field.reload_error":                 {EN: "Reload error", JA: "再読み込みエラー"},
	"status.field.worktree_root_error":          {EN: "Worktree root error", JA: "worktree root エラー"},
	"status.field.last_backup":                  {EN: "Last backup", JA: "最終バックアップ"},
	"status.field.workspaces":                   {EN: "Workspaces", JA: "ワークスペース"},
	"status.field.repositories":                 {EN: "Repositories", JA: "リポジトリ"},
	"status.field.active_sessions":              {EN: "Active sessions", JA: "アクティブなセッション"},
	"status.field.snapshots":                    {EN: "Snapshots", JA: "スナップショット"},
	"status.field.slots_ready":                  {EN: "Slots ready", JA: "準備済み slot"},
	"status.field.slots_in_use":                 {EN: "Slots in use", JA: "使用中 slot"},
	"status.field.slots_failed":                 {EN: "Slots failed", JA: "失敗 slot"},
	"status.field.slots_quarantined":            {EN: "Slots quarantined", JA: "隔離 slot"},
	"status.field.slots":                        {EN: "Slots", JA: "slot"},
	"status.field.total":                        {EN: "Total", JA: "合計"},
	"status.field.queued":                       {EN: "Queued", JA: "キュー待ち"},
	"status.field.pending":                      {EN: "Pending", JA: "待機中"},
	"status.field.running":                      {EN: "Running", JA: "実行中"},
	"status.field.failed":                       {EN: "Failed", JA: "失敗"},
	"status.field.discarded":                    {EN: "Discarded", JA: "破棄済み"},
	"status.field.details":                      {EN: "Details", JA: "詳細"},
	"status.field.earliest_expiry":              {EN: "Earliest expiry", JA: "最も早い期限"},
	"status.field.active":                       {EN: "Active", JA: "有効"},
	"status.field.logical_size":                 {EN: "Logical size", JA: "論理サイズ"},
	"status.field.disk_size":                    {EN: "Disk size", JA: "ディスクサイズ"},
	"status.field.allocated":                    {EN: "Allocated", JA: "割当"},
	"status.field.shared":                       {EN: "Shared", JA: "共有"},
	"status.field.measurement":                  {EN: "Measurement", JA: "計測"},
	"status.field.measured_at":                  {EN: "Measured at", JA: "計測日時"},
	"status.field.generation":                   {EN: "Generation", JA: "世代"},
	"status.field.reason":                       {EN: "Reason", JA: "理由"},
	"status.field.detail":                       {EN: "Detail", JA: "詳細"},
	"status.field.suspended":                    {EN: "Suspended", JA: "停止日時"},
	"status.field.action":                       {EN: "Action", JA: "操作"},
	"status.field.failure_code":                 {EN: "Failure code", JA: "失敗コード"},
	"status.field.failure_reason":               {EN: "Failure reason", JA: "失敗理由"},
	"status.field.failure_log":                  {EN: "Failure log", JA: "失敗ログ"},
	"status.field.archived":                     {EN: "Archived", JA: "アーカイブ済み"},
	"doctor.probe_hint":                         {EN: "Nothing above was checked by preparing a worktree; run wx doctor --probe to prepare one in each registered workspace and inspect it.", JA: "上記の項目は worktree を準備して検査していません。登録済み各 workspace を検査するには wx doctor --probe を実行してください。"},
	"flag.workspace_repository_exclusive":       {EN: "--workspace and --repository cannot be combined", JA: "--workspace と --repository は併用できません"},
	"flag.branch_fresh_require_agent":           {EN: "--branch and --fresh require an agent", JA: "--branch と --fresh には agent が必要です"},
	"flag.unknown_command":                      {EN: "unknown command or agent {{.Name}}", JA: "不明な command または agent {{.Name}} です"},
	"flag.agent_invalid":                        {EN: "agent must be claude or codex", JA: "agent は claude または codex で指定してください"},
	"flag.branch_requires_fresh":                {EN: "--branch requires --fresh when resuming", JA: "resume 時の --branch には --fresh が必要です"},
	"flag.branch_resume_exclusive":              {EN: "--branch and --resume choose different bases; use one of them", JA: "--branch と --resume は異なる base を選ぶため、どちらか一方を使ってください"},
	"dashboard.action_finished":                 {EN: "{{.Command}} finished (exit {{.Code}})", JA: "{{.Command}} が終了しました（終了コード {{.Code}}）"},
	"dashboard.target_not_directory":            {EN: "dashboard target {{.Target}} is not an accessible directory", JA: "dashboard の対象 {{.Target}} は利用可能な directory ではありません"},
	"doctor.probe.label":                        {EN: "probe", JA: "検査"},
	"doctor.probe.lease":                        {EN: "lease", JA: "貸出"},
	"doctor.probe.early_ready":                  {EN: "EARLY READY", JA: "早期準備完了"},
	"doctor.probe.full_ready":                   {EN: "FULL READY", JA: "準備完了"},
	"doctor.probe.prepare_job":                  {EN: "prepare job", JA: "準備 job"},
	"doctor.probe.disk":                         {EN: "disk", JA: "ディスク"},
	"doctor.probe.breakdown_unavailable":        {EN: "breakdown unavailable; the daemon no longer holds the measurement", JA: "準備の内訳を利用できません。daemon に計測値が残っていません"},
	"doctor.probe.exclusive":                    {EN: "exclusive", JA: "専有"},
	"doctor.probe.shared":                       {EN: "shared", JA: "共有"},
	"doctor.probe.no_repository":                {EN: "no repository was measured in the prepared slot", JA: "準備済み slot で測定された repository はありません"},
	"doctor.probe.usage_incomplete":             {EN: "{{.State}}; the daemon had not finished measuring the prepared slot", JA: "{{.State}}。daemon は準備済み slot の測定を完了していません"},
	"clean.no_targets":                          {EN: "no managed worktrees to clear", JA: "削除対象の管理 worktree はありません"},
	"clean.dry_run":                             {EN: "dry run: nothing was changed, and these are estimates from the time of this check", JA: "dry run: 変更はありません。この時点の確認による推定値です"},
	"clean.summary_zero":                        {EN: "0 targets", JA: "0 件"},
	"clean.summary_total":                       {EN: "{{.Count}} target(s)", JA: "{{.Count}} 件"},
	"clean.summary_item":                        {EN: ", {{.Key}} {{.Count}}", JA: "、{{.Key}} {{.Count}}"},
	"clean.rejoin":                              {EN: "the daemon keeps working on clear {{.RunID}}; run wx clear again to rejoin it", JA: "daemon は clear {{.RunID}} の処理を続けています。再参加するには wx clear をもう一度実行してください"},
	"gc.candidates":                             {EN: "candidates: {{.Count}}", JA: "候補: {{.Count}}"},
	"gc.scheduled":                              {EN: "scheduled: {{.Count}}", JA: "予約済み: {{.Count}}"},
	"gc.completed":                              {EN: "completed: {{.Count}}", JA: "完了: {{.Count}}"},
	"gc.pending":                                {EN: "pending: {{.Count}}", JA: "待機中: {{.Count}}"},
	"gc.failed":                                 {EN: "failed: {{.Count}}", JA: "失敗: {{.Count}}"},
	"prune.deletable":                           {EN: "deletable: {{.Count}}", JA: "削除可能: {{.Count}}"},
	"prune.deleted":                             {EN: "deleted: {{.Count}}", JA: "削除済み: {{.Count}}"},
	"prune.kept":                                {EN: "kept: {{.Count}}", JA: "保持: {{.Count}}"},
	"prune.kept_ref":                            {EN: "kept {{.Ref}} ({{.Repository}}): {{.Count}} objects would become unreachable", JA: "保持 {{.Ref}} ({{.Repository}}): {{.Count}} 個の object が到達不能になります"},
	"forget.done":                               {EN: "forgotten {{.Path}}", JA: "{{.Path}} の管理を解除しました"},
	"forget.reclaimed":                          {EN: "reclaimed {{.Slots}} worktree(s)", JA: "worktree {{.Slots}} 件を回収しました"},
	"forget.reclaimed_discarded":                {EN: "reclaimed {{.Slots}} worktree(s); discarded {{.Sessions}} session(s), {{.Snapshots}} snapshot(s), {{.WorkspaceSnapshots}} workspace snapshot(s)", JA: "worktree {{.Slots}} 件を回収し、session {{.Sessions}} 件・snapshot {{.Snapshots}} 件・workspace snapshot {{.WorkspaceSnapshots}} 件を破棄しました"},
	"discard_recovery.none":                     {EN: "no quarantined recovery state for {{.Path}}", JA: "隔離された復旧状態はありません: {{.Path}}"},
	"discard_recovery.session":                  {EN: "session {{.SessionID}} ({{.Snapshots}} snapshot(s), {{.WorkspaceSnapshots}} workspace snapshot(s))", JA: "session {{.SessionID}}（snapshot {{.Snapshots}} 件、workspace snapshot {{.WorkspaceSnapshots}} 件）"},
	"discard_recovery.slot":                     {EN: "slot {{.SlotID}} {{.State}} {{.Path}}", JA: "slot {{.SlotID}} {{.State}} {{.Path}}"},
	"discard_recovery.dry_run_count":            {EN: "{{.Count}} session(s) would be discarded", JA: "session {{.Count}} 件を破棄します"},
	"discard_recovery.dry_run":                  {EN: "dry run: nothing was changed", JA: "dry run: 変更はありません"},
	"discard_recovery.result":                   {EN: "discarded {{.Sessions}} session(s), retired {{.Slots}} slot(s)", JA: "session {{.Sessions}} 件を破棄し、slot {{.Slots}} 件を退役しました"},
	"retry_standby.resumed_scheduled":           {EN: "standby replenishment resumed for {{.Root}} (generation {{.Generation}}; retry scheduled)", JA: "standby 補充を 再開しました: {{.Root}}（世代 {{.Generation}}、再試行を予約しました）"},
	"retry_standby.resumed_in_progress":         {EN: "standby replenishment resumed for {{.Root}} (generation {{.Generation}}; retry already in progress)", JA: "standby 補充を 再開しました: {{.Root}}（世代 {{.Generation}}、再試行は進行中です）"},
	"retry_standby.running_scheduled":           {EN: "standby replenishment was not stopped for {{.Root}} (generation {{.Generation}}; retry scheduled)", JA: "standby 補充を 停止状態ではありません: {{.Root}}（世代 {{.Generation}}、再試行を予約しました）"},
	"retry_standby.running_in_progress":         {EN: "standby replenishment was not stopped for {{.Root}} (generation {{.Generation}}; retry already in progress)", JA: "standby 補充を 停止状態ではありません: {{.Root}}（世代 {{.Generation}}、再試行は進行中です）"},
	"retry_standby.resumed_scheduled_removed":   {EN: "standby replenishment resumed for {{.Root}} (generation {{.Generation}}; retry scheduled; {{.Removed}} failed worktrees scheduled for removal)", JA: "standby 補充を 再開しました: {{.Root}}（世代 {{.Generation}}、再試行を予約しました、失敗した worktree {{.Removed}} 件を削除予約）"},
	"retry_standby.resumed_in_progress_removed": {EN: "standby replenishment resumed for {{.Root}} (generation {{.Generation}}; retry already in progress; {{.Removed}} failed worktrees scheduled for removal)", JA: "standby 補充を 再開しました: {{.Root}}（世代 {{.Generation}}、再試行は進行中です、失敗した worktree {{.Removed}} 件を削除予約）"},
	"retry_standby.running_scheduled_removed":   {EN: "standby replenishment was not stopped for {{.Root}} (generation {{.Generation}}; retry scheduled; {{.Removed}} failed worktrees scheduled for removal)", JA: "standby 補充を 停止状態ではありません: {{.Root}}（世代 {{.Generation}}、再試行を予約しました、失敗した worktree {{.Removed}} 件を削除予約）"},
	"retry_standby.running_in_progress_removed": {EN: "standby replenishment was not stopped for {{.Root}} (generation {{.Generation}}; retry already in progress; {{.Removed}} failed worktrees scheduled for removal)", JA: "standby 補充を 停止状態ではありません: {{.Root}}（世代 {{.Generation}}、再試行は進行中です、失敗した worktree {{.Removed}} 件を削除予約）"},
	"retry_standby.failed":                      {EN: "retry-standby {{.Root}}: {{.Error}}", JA: "retry-standby {{.Root}}: 再試行に失敗しました: {{.Error}}"},
	"retry_standby.none":                        {EN: "no workspace has standby replenishment stopped", JA: "standby 補充が停止している workspace はありません"},
	"config.describe.type":                      {EN: "Type", JA: "型"},
	"config.describe.scopes":                    {EN: "Scopes", JA: "スコープ"},
	"config.describe.choices":                   {EN: "Choices", JA: "選択肢"},
	"config.describe.impact":                    {EN: "Impact", JA: "影響"},
	"config.show.path":                          {EN: "Config: {{.Path}}", JA: "設定: {{.Path}}"},
	"config.show.source":                        {EN: "source: {{.Value}}", JA: "出典: {{.Value}}"},
	"config.show.scope":                         {EN: "{{.Title}}: {{.Target}}", JA: "{{.Title}}: {{.Target}}"},
	"daemon.stop_cancelled":                     {EN: "cancelled the pending stop of {{.Label}}", JA: "{{.Label}} の保留中の停止要求をキャンセルしました"},
	"daemon.stopping_timeout":                   {EN: "{{.Label}} is stopping but did not exit within {{.Timeout}}", JA: "{{.Label}} は停止処理中ですが、{{.Timeout}} 以内に終了しませんでした"},
	"daemon.no_answer":                          {EN: "launchd was asked to start {{.Label}} but no daemon answered {{.Socket}} within {{.Timeout}}", JA: "launchd に {{.Label}} の起動を依頼しましたが、{{.Timeout}} 以内に {{.Socket}} から daemon の応答がありませんでした"},
	"daemon.install_first":                      {EN: "run wx daemon install to register the LaunchAgent first", JA: "LaunchAgent を登録するには wx daemon install を実行してください"},
	"daemon.stop_already_requested":             {EN: "stop was already requested; waiting for the daemon to exit", JA: "停止要求は送信済みです。daemon の終了を待っています"},
	"daemon.stop_timeout":                       {EN: "{{.Label}} accepted the stop request but did not exit within {{.Timeout}}", JA: "{{.Label}} は停止要求を受理しましたが、{{.Timeout}} 以内に終了しませんでした"},
	"daemon.restart_timeout":                    {EN: "{{.Label}} accepted the restart request but was not replaced within {{.Timeout}}", JA: "{{.Label}} は再起動要求を受理しましたが、{{.Timeout}} 以内に次の daemon へ置き換わりませんでした"},
	"daemon.not_launchd_managed":                {EN: "the daemon answering {{.Socket}} is not managed by launchd, so it cannot restart itself", JA: "{{.Socket}} に応答している daemon は launchd に管理されていないため、自身を再起動できません"},
	"daemon.restart_manually":                   {EN: "stop it with wx daemon stop and start it again with wx daemon start", JA: "wx daemon stop で停止し、wx daemon start で再起動してください"},
	"daemon.already_stopping":                   {EN: "the daemon is already stopping; wait for it to exit and run wx daemon start", JA: "daemon は停止処理中です。終了を待って wx daemon start を実行してください"},
	"daemon.already_restarting":                 {EN: "the daemon is already restarting; run the command again once the replacement is up", JA: "daemon は再起動処理中です。置き換え後にもう一度実行してください"},
	"daemon.gate_queued_jobs":                   {EN: "{{.Count}} job(s) were still queued when the request was accepted; the daemon waits for them to finish", JA: "要求の受理時に job が {{.Count}} 件残っていました。daemon はその完了を待ちます"},
	"daemon.gate_inflight":                      {EN: "{{.Count}} other request(s) were still in flight when the request was accepted", JA: "要求の受理時に他の要求が {{.Count}} 件実行中でした"},
	"daemon.gate_idle":                          {EN: "the daemon was idle when the request was accepted, so a long-running request or job arrived after that; check the daemon log for the requests and jobs that followed", JA: "要求の受理時に daemon は idle だったため、その後に長時間の要求か job が届いています。続く要求と job は daemon のログで確認してください"},
	"daemon.gate_idle_log":                      {EN: "the daemon was idle when the request was accepted, so a long-running request or job arrived after that; check {{.Path}} for the requests and jobs that followed", JA: "要求の受理時に daemon は idle だったため、その後に長時間の要求か job が届いています。続く要求と job は {{.Path}} で確認してください"},
	"setup.action_unavailable":                  {EN: "setup item {{.Item}} does not offer action {{.Action}}", JA: "setup 項目 {{.Item}} は操作 {{.Action}} を提供していません"},
	"setup.available_actions":                   {EN: "; available actions: {{.Actions}}", JA: "。利用可能な操作: {{.Actions}}"},
	"setup.manual_requires_value":               {EN: "setup item {{.Item}} action manual requires --value", JA: "setup 項目 {{.Item}} の manual 操作には --value が必要です"},
	"setup.column.item":                         {EN: "ITEM", JA: "項目"},
	"setup.column.state":                        {EN: "STATE", JA: "状態"},
	"setup.column.action":                       {EN: "ACTION", JA: "操作"},
	"setup.column.detail":                       {EN: "DETAIL", JA: "詳細"},
	"setup.item.shell_path":                     {EN: "Shell PATH", JA: "Shell の PATH"},
	"setup.item.hooks_claude":                   {EN: "Agent hooks (claude)", JA: "Agent hook (claude)"},
	"setup.item.hooks_codex":                    {EN: "Agent hooks (codex)", JA: "Agent hook (codex)"},
	"setup.language.english":                    {EN: "wx の表示を英語にする", JA: "wx の表示を英語にする"},
	"setup.language.japanese":                   {EN: "wx の表示を日本語にする", JA: "wx の表示を日本語にする"},
	"setup.action.write":                        {EN: "write {{.Change}} to {{.Target}}", JA: "{{.Change}} を {{.Target}} に書き込みます"},
	"setup.action.replace":                      {EN: "replace the wx part of {{.Target}} with {{.Change}}", JA: "{{.Target}} の wx 部分を {{.Change}} に更新します"},
	"setup.action.keep":                         {EN: "leave {{.Target}} as it is", JA: "{{.Target}} をそのままにします"},
	"setup.action.remove":                       {EN: "remove what wx manages from {{.Target}}", JA: "{{.Target}} から wx の管理部分を削除します"},
	"setup.action.skip":                         {EN: "do nothing now; wx setup can be run again later", JA: "今回は何もしません。後で wx setup を再実行できます"},
	"setup.action.manual":                       {EN: "type another path to write to {{.Target}}", JA: "{{.Target}} に書き込む別の path を入力します"},
	"setup.daemon.start":                        {EN: "start the wx daemon and wait for the local socket to answer", JA: "wx daemon を起動し、local socket の応答を待ちます"},
	"setup.daemon.restart":                      {EN: "ask the daemon to restart once it is idle, then wait for the replacement to answer", JA: "daemon が idle になってから再起動し、置き換わった daemon の応答を待ちます"},
	"setup.daemon.keep":                         {EN: "leave the running daemon as it is", JA: "実行中の daemon をそのままにします"},
	"setup.warning_prefix":                      {EN: "warning: ", JA: "警告: "},
	"setup.still_present":                       {EN: "{{.Item}} is still {{.State}} after remove", JA: "{{.Item}} は remove 後も {{.State}} です"},
	"setup.unexpected_state":                    {EN: "{{.Item}} is {{.State}} after {{.Action}}", JA: "{{.Item}} は {{.Action}} 後も {{.State}} です"},
	"setup.nothing_to_remove":                   {EN: "nothing to remove", JA: "削除するものはありません"},
	"setup.kept_paths":                          {EN: "wx kept these; they hold saved work and records:", JA: "wx は保存済みの作業と記録を含む次の path を残しました:"},
	"setup.display_language.detail":             {EN: "Choose English or 日本語 for wx messages.", JA: "Choose English or 日本語 for wx messages."},
	"setup.worktree_root_prompt":                {EN: "Enter the worktree root path", JA: "Worktree root の path を入力してください"},
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

// validationData は ValidateCatalog が template へ渡す代表値である。
// text/template は map の欠損キーを <no value> にするだけで失敗しないため、
// 検査が空文字だけでなく実際の展開を見られるよう、使うフィールド名をここへ揃える。
var validationData = map[string]any{
	"Count": 1, "Items": "", "Language": "", "Path": "", "Size": "", "Error": "", "Schema": 0,
	"State": "", "Action": "", "Zone": "", "Time": "", "Index": 1, "Reason": "", "Key": "",
	"Value": "", "Title": "", "Target": "", "Source": "", "Root": "", "Generation": 0, "Removed": 0, "Socket": "", "Timeout": "",
	"Item": "", "Actions": "", "Change": "", "Command": "", "Name": "", "Marker": "",
	"Expected": "", "Actual": "", "Head": "", "MainPath": "", "MainHead": "",
	"Ref": "", "Repository": "", "RunID": "", "SessionID": "", "SlotID": "", "Slots": 0, "Sessions": 0,
	"Snapshots": 0, "WorkspaceSnapshots": 0, "Earliest": "", "Latest": "", "Pending": "", "Running": "",
	"Failed": "", "Discarded": "", "Age": "", "Scope": "", "Choices": "", "Label": "", "Code": 0, "Message": "",
}

// ValidateCatalog は両言語の空欄と、go-i18n が解釈できない template を検出する。
func ValidateCatalog() error {
	for id, entry := range catalog {
		if strings.TrimSpace(entry.EN) == "" || strings.TrimSpace(entry.JA) == "" {
			return fmt.Errorf("message %q must have non-empty en and ja text", id)
		}
		for _, lang := range []Language{English, Japanese} {
			localizer := New(string(lang))
			if strings.TrimSpace(localizer.Localize(id, validationData)) == "" {
				return fmt.Errorf("message %q could not be localized for %s", id, lang)
			}
		}
	}
	return nil
}
