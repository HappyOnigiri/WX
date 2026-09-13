package i18n

// catalog_setup.go は wx setup の項目（dashboard の「システム」タブと同じ内容）と、
// agent hook 設定の判定理由の表示文を持つ。path・event 名・外部コマンドの出力・JSON parser の
// エラーは機械値なので訳さず、テンプレートのプレースホルダへ不透明値として渡す。

var setupCatalog = map[string]Entry{
	// setup.state.* は項目の状態を利用者向けの言葉にする。state の値そのもの（absent など）は
	// JSON と --item の引数で使う機械値なので、ここでは表示だけを置き換える。
	"setup.state.absent":         {EN: "not configured yet", JA: "未設定"},
	"setup.state.present":        {EN: "configured as wx expects", JA: "設定済み"},
	"setup.state.divergent":      {EN: "differs from what wx writes", JA: "wx の内容と不一致"},
	"setup.state.unknown":        {EN: "cannot be judged", JA: "判定できません"},
	"setup.state.not_applicable": {EN: "nothing for wx to do", JA: "対象外"},

	// setup.action.* は選択肢そのものの短いラベルである。何が起きるかの詳しい説明は
	// wx.setup.action.* が持ち、こちらは一覧で読める長さに保つ。
	"setup.action.install": {EN: "Write the wx settings", JA: "wx の設定を書き込む"},
	"setup.action.update":  {EN: "Update to what wx writes", JA: "wx の内容へ更新する"},
	"setup.action.keep":    {EN: "Leave it as it is", JA: "現状のままにする"},
	"setup.action.remove":  {EN: "Remove what wx manages", JA: "wx の管理部分を削除する"},
	"setup.action.skip":    {EN: "Do nothing for now", JA: "今回は何もしない"},
	"setup.action.default": {EN: "Use the default path", JA: "既定の path を使う"},
	"setup.action.manual":  {EN: "Type a path yourself", JA: "path を自分で入力する"},
	"setup.action.start":   {EN: "Start the daemon", JA: "daemon を起動する"},
	"setup.action.restart": {EN: "Replace the running daemon", JA: "実行中の daemon を入れ替える"},

	// setup.summary.* は表の 1 行に収める要約で、wx setup --check の DETAIL 列に出す。
	// 何のための項目かという説明は setup.detail.* が持つ。
	"setup.summary.prerequisites":       {EN: "git and the wx executable for hooks are checked; nothing is changed", JA: "git と hook 用の wx 実行ファイルを確認します（変更なし）"},
	"setup.summary.worktree_root_unset": {EN: "not written; wx would use {{.Default}}", JA: "未記載のため wx は {{.Default}} を使います"},
	"setup.summary.worktree_root_path":  {EN: "{{.Path}}", JA: "{{.Path}}"},
	"setup.summary.shell_path_block":    {EN: "adds {{.Directory}} to PATH for new terminals", JA: "新しい端末の PATH に {{.Directory}} を追加します"},
	"setup.summary.shell_path_external": {EN: "{{.Path}} already adds {{.Directory}} to PATH", JA: "{{.Path}} は既に {{.Directory}} を PATH へ追加しています"},
	"setup.summary.launch_agent":        {EN: "starts the wx daemon at login", JA: "ログイン時に wx daemon を起動します"},
	"setup.summary.hooks_entries":       {EN: "{{.Count}} wx entries ({{.Events}}) in {{.Path}}", JA: "{{.Path}} の wx エントリ {{.Count}} 件（{{.Events}}）"},
	"setup.summary.hooks_agent_missing": {EN: "{{.Agent}} is not on PATH", JA: "{{.Agent}} が PATH にありません"},
	"setup.summary.daemon":              {EN: "wx daemon answers the local socket", JA: "wx daemon が local socket に応答します"},

	// setup.detail.* は項目ごとの説明で、その項目が何のために存在するのかを 1〜2 文で示す。
	"setup.detail.prerequisites": {
		EN: "Facts the other items depend on: wx needs git, and it needs to know which wx executable the hooks should name. This item only reads; it changes nothing.",
		JA: "他の項目が前提にしている事実です。wx は git と、hook に書き込む wx 実行ファイルの場所を必要とします。この項目は読み取るだけで、何も変更しません。",
	},
	"setup.detail.prerequisites_binary": {
		EN: "git is available, and hooks would name the wx executable at {{.Binary}}.",
		JA: "git を利用できます。hook には {{.Binary}} の wx 実行ファイルを書き込みます。",
	},
	"setup.detail.worktree_root_unset": {
		EN: "storage.worktree_root is not written in the configuration file, so wx would create every worktree under {{.Default}}.",
		JA: "設定ファイルに storage.worktree_root が書かれていないため、wx は worktree をすべて {{.Default}} の下に作ります。",
	},
	"setup.detail.worktree_root_path": {
		EN: "wx creates the worktrees it lends to agents under {{.Path}}; the source repository is never touched.",
		JA: "wx はエージェントへ貸し出す worktree を {{.Path}} の下に作ります。ソースリポジトリは変更しません。",
	},
	"setup.detail.shell_path_block": {
		EN: "Adds {{.Directory}} to PATH in {{.Path}} so that new terminals can run the wx command.",
		JA: "新しい端末で wx コマンドを実行できるよう、{{.Path}} に {{.Directory}} を PATH へ加える行を書きます。",
	},
	"setup.detail.shell_path_external": {
		EN: "{{.Path}} already adds {{.Directory}} to PATH in its own way, so wx has no reason to edit it.",
		JA: "{{.Path}} は独自の書き方で {{.Directory}} を PATH へ加えているため、wx が編集する必要はありません。",
	},
	"setup.detail.launch_agent": {
		EN: "A LaunchAgent plist that starts the wx daemon at login and restarts it if it exits; without it, every wx command has to wait for a daemon that nobody started.",
		JA: "ログイン時に wx daemon を起動し、終了したら起動し直す LaunchAgent の plist です。これが無いと、daemon を誰も起動していない状態で wx コマンドを実行することになります。",
	},
	"setup.detail.hooks_agent_missing": {
		EN: "{{.Agent}} is not on PATH, so wx does not configure its hooks. Install {{.Agent}} and run wx setup again if you want to launch it from wx.",
		JA: "{{.Agent}} が PATH にないため、wx は hook を設定しません。wx から起動したい場合は {{.Agent}} を導入してから wx setup を実行し直してください。",
	},
	"setup.detail.hooks_entries": {
		EN: "wx registers {{.Count}} hook entries ({{.Events}}) in {{.Path}}. They let wx know when {{.Agent}} starts, is working, and ends, so it can lend a worktree and save the work on the way out.",
		JA: "wx は {{.Path}} に {{.Count}} 件の hook（{{.Events}}）を登録します。{{.Agent}} の開始・作業中・終了を wx が知るためのもので、worktree の貸出と終了時の作業保存に使います。",
	},
	"setup.detail.hooks_entries_command": {
		EN: "wx registers {{.Count}} hook entries ({{.Events}}) in {{.Path}}. They let wx know when {{.Agent}} starts, is working, and ends, so it can lend a worktree and save the work on the way out. Each entry runs a wx command directly, for example: {{.Command}}",
		JA: "wx は {{.Path}} に {{.Count}} 件の hook（{{.Events}}）を登録します。{{.Agent}} の開始・作業中・終了を wx が知るためのもので、worktree の貸出と終了時の作業保存に使います。各エントリは wx のコマンドをそのまま実行します。例: {{.Command}}",
	},
	"setup.detail.daemon": {
		EN: "The background process that owns the worktree pool and answers the local socket; wx commands ask it to lend, return, and clean up worktrees.",
		JA: "worktree のプールを所有し、local socket に応答する常駐プロセスです。worktree の貸出・返却・後片付けは wx コマンドがこの daemon へ依頼します。",
	},

	// setup.reason.* は項目の状態が present でないときの理由で、TUI では「注意」として出す。
	"setup.reason.git_unavailable": {
		EN: "git could not be run, and wx needs it for every worktree operation: {{.Error}}",
		JA: "git を実行できません。wx は worktree の操作すべてに git を使います: {{.Error}}",
	},
	"setup.reason.hook_binary_unresolved": {
		EN: "wx cannot decide which wx executable to write into the hooks: {{.Error}}",
		JA: "hook に書き込む wx 実行ファイルを決められません: {{.Error}}",
	},
	"setup.reason.running_wx_unknown": {
		EN: "the running wx executable cannot be identified, so wx cannot tell whether the installed hooks point at it: {{.Error}}",
		JA: "実行中の wx 実行ファイルを特定できないため、登録済みの hook がそれを指しているか判定できません: {{.Error}}",
	},
	"setup.reason.development_build": {
		EN: "this wx runs from {{.Running}} but the hooks would name {{.Binary}}; install wx first so that both agree, or the entries will look outdated as soon as they are written",
		JA: "実行中の wx は {{.Running}} ですが、hook には {{.Binary}} を書き込みます。先に wx をインストールして両者を揃えないと、書き込んだ直後に不一致として表示されます",
	},
	"setup.reason.path_problem": {
		EN: "{{.Path}}: {{.Reason}}",
		JA: "{{.Path}}: {{.Reason}}",
	},
	// path_problem_raw は diag が message を持たない判定（Lstat の失敗）専用で、本文は外部由来の原文である。
	"setup.reason.path_problem_raw": {
		EN: "{{.Path}}: {{.Detail}}",
		JA: "{{.Path}}: {{.Detail}}",
	},
	"setup.reason.config_unreadable": {
		EN: "the wx configuration file could not be read: {{.Error}}",
		JA: "wx の設定ファイルを読み取れません: {{.Error}}",
	},
	"setup.reason.plist_uncomparable": {
		EN: "the installed LaunchAgent plist cannot be compared with what wx would write: {{.Error}}",
		JA: "登録済みの LaunchAgent plist を、wx が書き込む内容と比較できません: {{.Error}}",
	},
	"setup.reason.plist_stale": {
		EN: "the installed plist does not match what this wx would write; updating it rewrites the plist and reloads the LaunchAgent",
		JA: "登録済みの plist は、この wx が書き込む内容と一致しません。更新すると plist を書き直して LaunchAgent を読み込み直します",
	},
	"setup.reason.home_unresolved": {
		EN: "the home directory cannot be resolved, so wx cannot tell where to write",
		JA: "ホームディレクトリを解決できないため、書き込み先を決められません",
	},
	"setup.reason.shell_unknown": {
		EN: "wx cannot tell which startup file this shell reads; add a line that puts the wx bin directory on PATH yourself",
		JA: "この shell が読む起動ファイルを判定できません。wx の bin ディレクトリを PATH へ加える行を手動で追記してください",
	},
	"setup.reason.startup_symlink": {
		EN: "{{.Path}} is a symlink, most likely managed by a dotfile repository; take it out of that management or add the PATH line yourself, because wx will not replace the link",
		JA: "{{.Path}} は symlink で、dotfile 管理下にあると考えられます。wx は link を置き換えないため、管理から外すか PATH の行を手動で追記してください",
	},
	"setup.reason.startup_not_regular": {
		EN: "{{.Path}} is not a regular file, so wx will not write to it",
		JA: "{{.Path}} は通常ファイルではないため、wx は書き込みません",
	},
	"setup.reason.startup_unreadable": {
		EN: "{{.Path}} could not be read: {{.Error}}",
		JA: "{{.Path}} を読み取れません: {{.Error}}",
	},
	"setup.reason.block_unterminated": {
		EN: "{{.Path}} has the {{.Begin}} marker without {{.End}}, so wx cannot tell where its own block ends; restore the missing end marker or delete the block yourself",
		JA: "{{.Path}} に {{.Begin}} はありますが {{.End}} がないため、wx の管理範囲を確定できません。終了 marker を戻すか、ブロックを手動で削除してください",
	},
	"setup.reason.block_divergent": {
		EN: "the block managed by wx does not match what wx would write; updating it replaces only the lines between the wx markers",
		JA: "wx が管理するブロックは、wx が書き込む内容と一致しません。更新しても wx の marker で囲まれた行だけを置き換えます",
	},
	"setup.reason.path_session_only": {
		EN: "{{.Directory}} is on PATH in this session, but no line in {{.Path}} adds it, so new terminals will not find the wx command",
		JA: "このセッションの PATH には {{.Directory}} がありますが、{{.Path}} に追加する行が無いため、新しい端末では wx コマンドが見つかりません",
	},
	"setup.reason.daemon_unqueryable": {
		EN: "the daemon cannot be queried from here, so its state is unknown",
		JA: "ここからは daemon に問い合わせできないため、状態が分かりません",
	},
	"setup.reason.daemon_broken": {
		EN: "the daemon answered but the request failed, so it is running in a state wx cannot use; restarting replaces the process: {{.Error}}",
		JA: "daemon は応答しましたが要求が失敗したため、wx が使えない状態で動いています。再起動するとプロセスを入れ替えます: {{.Error}}",
	},

	// setup.note.* は適用の結果として、差分から読み取れない事実（書き換えた実体と控え）を伝える。
	"setup.note.wrote":           {EN: "wrote {{.Path}}", JA: "{{.Path}} に書き込みました"},
	"setup.note.wrote_backup":    {EN: "wrote {{.Path}}; the previous file is kept at {{.Backup}}", JA: "{{.Path}} に書き込みました。直前の内容は {{.Backup}} に残しています"},
	"setup.note.no_hook_config":  {EN: "no hook configuration at {{.Path}}", JA: "{{.Path}} に hook 設定はありません"},
	"setup.note.no_launch_agent": {EN: "no LaunchAgent at {{.Path}}", JA: "{{.Path}} に LaunchAgent はありません"},
	"setup.note.removed":         {EN: "removed {{.Path}}", JA: "{{.Path}} を削除しました"},
	"setup.note.deleted":         {EN: "deleted {{.Path}}", JA: "{{.Path}} を削除しました"},

	// setup.error.* は適用が失敗したときに利用者へ返す文である。
	"setup.error.launch_agent_remove_unavailable":  {EN: "removing the LaunchAgent is not available here", JA: "ここでは LaunchAgent の削除を実行できません"},
	"setup.error.launch_agent_install_unavailable": {EN: "installing the LaunchAgent is not available here", JA: "ここでは LaunchAgent のインストールを実行できません"},
	"setup.error.daemon_restart_unavailable":       {EN: "restarting the daemon is not available here", JA: "ここでは daemon の再起動を実行できません"},
	"setup.error.daemon_start_unavailable":         {EN: "starting the daemon is not available here", JA: "ここでは daemon の起動を実行できません"},
	"setup.error.startup_file_unknown":             {EN: "the shell startup file is unknown, so wx has nothing to write to", JA: "shell の起動ファイルが不明なため、書き込み先がありません"},
	"setup.error.daemon_rejected_config": {
		EN: "{{.Path}} was saved but the running daemon rejected it: {{.Error}}",
		JA: "{{.Path}} は保存しましたが、実行中の daemon が拒否しました: {{.Error}}",
	},
	"setup.error.hooks_not_ready": {
		EN: "{{.Path}} was written but the hooks are still not in a form wx accepts: {{.Reason}}",
		JA: "{{.Path}} には書き込みましたが、hook は wx が受理する形になっていません: {{.Reason}}",
	},
	"setup.error.unknown_step":   {EN: "unknown setup item {{.Item}}", JA: "setup 項目 {{.Item}} は存在しません"},
	"setup.error.step_not_apply": {EN: "setup item {{.Item}} cannot be applied", JA: "setup 項目 {{.Item}} は適用できません"},
	"setup.error.needs_terminal": {EN: "wx setup needs a terminal for its questions", JA: "wx setup の質問には端末が必要です"},

	// hook.finding.raw は message ID を持たない finding の逃げ道で、機械向けの英語をそのまま本文にする。
	"hook.finding.raw": {EN: "{{.Detail}}", JA: "{{.Detail}}"},

	// hook.finding.* は agent hook 設定を wx が受理しなかった理由である。
	// 1 つの Code に複数の事情がある場合は、事情ごとに ID を分ける。
	"hook.finding.unsupported_agent":   {EN: "wx does not know how to configure hooks for {{.Detail}}", JA: "wx は {{.Detail}} の hook 設定に対応していません"},
	"hook.finding.agent_not_installed": {EN: "{{.Detail}} is not installed, so wx does not write its hooks", JA: "{{.Detail}} が導入されていないため、wx は hook を書き込みません"},
	"hook.finding.target_unreadable":   {EN: "{{.Path}} could not be read: {{.Detail}}", JA: "{{.Path}} を読み取れません: {{.Detail}}"},
	"hook.finding.target_not_regular":  {EN: "{{.Path}} is not a regular file, so wx will neither judge nor edit it", JA: "{{.Path}} は通常ファイルではないため、wx は判定も編集もしません"},
	"hook.finding.target_empty":        {EN: "{{.Path}} is empty, and the agent cannot read hooks from an empty file", JA: "{{.Path}} が空です。空のファイルから agent は hook を読み取れません"},
	"hook.finding.target_too_large":    {EN: "{{.Path}} is larger than the 4 MiB wx will parse", JA: "{{.Path}} は wx が解析する上限（4 MiB）を超えています"},
	"hook.finding.target_unparsable":   {EN: "{{.Path}} is not valid JSON, so wx cannot tell what is registered: {{.Detail}}", JA: "{{.Path}} は JSON として壊れているため、登録内容を判定できません: {{.Detail}}"},
	"hook.finding.target_duplicate_key": {
		EN: "{{.Path}} has duplicate JSON keys; wx will not edit the file until they are removed: {{.Detail}}",
		JA: "{{.Path}} に JSON キーの重複があります。取り除くまで wx はこのファイルを編集しません: {{.Detail}}",
	},
	"hook.finding.target_symlink_broken": {EN: "{{.Path}} is a symlink that cannot be resolved: {{.Detail}}", JA: "{{.Path}} は解決できない symlink です: {{.Detail}}"},
	"hook.finding.target_symlink": {
		EN: "{{.Path}} is a symlink to {{.Resolved}}; wx writes the file it points at",
		JA: "{{.Path}} は {{.Resolved}} への symlink です。wx はリンク先の実体へ書き込みます",
	},
	"hook.finding.target_in_repository": {
		EN: "{{.Path}} is inside the Git repository {{.Repository}}; commit what wx writes, or the next dotfile apply restores the old file",
		JA: "{{.Path}} は Git リポジトリ {{.Repository}} の中にあります。wx の書き込みを commit しないと、次の dotfile 適用で元に戻ります",
	},
	"hook.finding.local_settings_shadow": {
		EN: "settings.local.json takes precedence over {{.Path}}, so wx reads and writes the local file instead",
		JA: "settings.local.json が {{.Path}} より優先されるため、wx は local 側を読み書きします",
	},
	"hook.finding.all_hooks_disabled": {
		EN: "disableAllHooks in this file rejects every hook, so the wx entries would never run",
		JA: "このファイルの disableAllHooks が全 hook を無効にするため、wx のエントリは実行されません",
	},
	"hook.finding.hooks_missing": {EN: "the file has no hooks object yet", JA: "このファイルにはまだ hooks の項目がありません"},
	"hook.finding.event_missing": {EN: "the {{.Event}} hook is not registered", JA: "{{.Event}} の hook が登録されていません"},
	"hook.finding.event_unknown_field": {
		EN: "the {{.Event}} entry contains a field wx does not recognize, so the whole event is rejected: {{.Detail}}",
		JA: "{{.Event}} のエントリに wx が解釈しない項目があるため、その event 全体を受理できません: {{.Detail}}",
	},
	"hook.finding.command_missing": {
		EN: "no hook under {{.Event}} runs exactly the wx command wx expects ({{.Command}})",
		JA: "{{.Event}} の hook に、wx が期待する形のコマンド（{{.Command}}）がありません",
	},
	"hook.finding.command_skipped_disabled": {EN: "the {{.Event}} hook is marked disabled", JA: "{{.Event}} の hook は disabled になっています"},
	"hook.finding.command_skipped_async": {
		EN: "the {{.Event}} hook is async, and async hooks do not make the agent wait, so wx cannot rely on it",
		JA: "{{.Event}} の hook は async です。async の hook は agent を待たせないため、wx はこれに依存できません",
	},
	"hook.finding.command_skipped_once": {EN: "the {{.Event}} hook is marked once, so it does not run for every event", JA: "{{.Event}} の hook は once のため、毎回は実行されません"},
	"hook.finding.command_other_binary": {
		EN: "the {{.Event}} hook runs {{.Actual}}, not the running wx at {{.Expected}}",
		JA: "{{.Event}} の hook は {{.Actual}} を実行しており、実行中の wx（{{.Expected}}）ではありません",
	},
	"hook.finding.command_unresolvable": {
		EN: "the {{.Event}} hook names {{.Actual}}, which does not resolve to an executable; it should run {{.Expected}}",
		JA: "{{.Event}} の hook が指す {{.Actual}} は実行ファイルとして解決できません。{{.Expected}} を実行する必要があります",
	},
	"hook.finding.command_not_wx": {
		EN: "the {{.Event}} hook command does not name a wx executable; it should run {{.Expected}}",
		JA: "{{.Event}} の hook コマンドは wx の実行ファイルを指していません。{{.Expected}} を実行する必要があります",
	},
	"hook.finding.group_disabled":       {EN: "the hook group sets disabled, so wx skips it", JA: "hook の group が disabled のため、wx は読み飛ばします"},
	"hook.finding.group_matcher":        {EN: "the hook group matcher does not apply to every event; omit matcher or use \"*\"", JA: "hook の group の matcher が全 event に当たりません。matcher を省くか \"*\" にしてください"},
	"hook.finding.group_empty":          {EN: "the hook group contains no hooks", JA: "hook の group に hook がありません"},
	"hook.finding.group_malformed_peer": {EN: "another hook in the same group is malformed, so the whole group is rejected", JA: "同じ group の別の hook が不正な形のため、group 全体が却下されます"},
	"hook.finding.executable_unknown":   {EN: "the running wx executable cannot be identified: {{.Detail}}", JA: "実行中の wx 実行ファイルを特定できません: {{.Detail}}"},
	"hook.finding.codex_config_unusable": {
		EN: "the Codex configuration at {{.Path}} cannot be used, so wx treats Codex hooks as disabled: {{.Detail}}",
		JA: "Codex の設定 {{.Path}} を利用できないため、wx は Codex の hook を無効として扱います: {{.Detail}}",
	},
	"hook.finding.codex_home_unknown":     {EN: "the home directory cannot be resolved, so the Codex configuration cannot be checked: {{.Detail}}", JA: "ホームディレクトリを解決できないため、Codex の設定を確認できません: {{.Detail}}"},
	"hook.finding.codex_config_too_large": {EN: "{{.Path}} is larger than the 4 MiB wx will parse", JA: "{{.Path}} は wx が解析する上限（4 MiB）を超えています"},
	"hook.finding.codex_config_unparsable": {
		EN: "wx cannot interpret the TOML in {{.Path}}, so Codex hooks are treated as disabled",
		JA: "{{.Path}} の TOML を解釈できないため、Codex の hook を無効として扱います",
	},
	"hook.finding.codex_feature_disabled": {
		EN: "Codex hooks are turned off in {{.Path}}; set hooks = true under [features] to let Codex run user hooks",
		JA: "{{.Path}} で Codex の hook が無効になっています。[features] の hooks = true を設定すると user hook が実行されます",
	},
}
