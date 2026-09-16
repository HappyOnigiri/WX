package i18n

// catalog_config.go は設定項目の表示名・説明・変更の影響を持つ。
// ID は `config.<設定キー>.name` / `.description` / `.impact` で、描画側はキーから ID を
// 組み立てて引き、カタログに無いキーだけ英語の原文へ落とす。設定キー・値・選択肢は機械値なので訳さない。
// config.language.description と config.language.impact だけは、表示言語が設定コマンド以外の
// 画面からも引かれるため catalog.go にあり、ここには置かない。
// commentlint:allow-long -- ID の組み立て規則と、language だけ別ファイルにある例外は、この 2 つを知らないと重複 ID で panic する

var configCatalog = map[string]Entry{
	"config.language.name": {EN: "Display language", JA: "表示言語"},

	"config.worktree.undefined.name":        {EN: "Default worktree policy", JA: "worktree の既定の方針"},
	"config.worktree.undefined.description": {EN: "What wx does for a workspace that has no policy of its own: ask each time, keep standby worktrees ready (hot), prepare one on demand (cold), or work in the repository itself (off).", JA: "方針を決めていない workspace で wx がどうするかです。毎回尋ねる（ask）、standby を用意しておく（hot）、必要になってから準備する（cold）、リポジトリ本体で作業する（off）から選びます。"},
	"config.worktree.undefined.impact":      {EN: "Takes effect the next time an agent is launched for such a workspace.", JA: "該当する workspace で次にエージェントを起動するときから有効になります。"},

	"config.worktree.reuse_standby.name":        {EN: "Reuse standby worktrees", JA: "standby worktree を再利用する"},
	"config.worktree.reuse_standby.description": {EN: "Whether a standby worktree prepared from an older commit may be updated to the requested commit and handed over, instead of being rebuilt.", JA: "古い commit で用意した standby worktree を、作り直さずに要求された commit へ更新して貸し出してよいかどうかです。"},
	"config.worktree.reuse_standby.impact":      {EN: "When disabled, a launch that does not match a standby has to prepare from scratch and waits longer.", JA: "無効にすると、standby と一致しない起動は一から準備することになり、待ち時間が長くなります。"},

	"config.worktree.fetch_default_branch.name":        {EN: "Fetch the default branch", JA: "既定ブランチを fetch する"},
	"config.worktree.fetch_default_branch.description": {EN: "Whether wx fetches the origin default branch before a worktree lease that does not specify a branch.", JA: "branch を指定しない worktree の貸出前に、wx が origin の既定ブランチを fetch するかどうかです。"},
	"config.worktree.fetch_default_branch.impact":      {EN: "When enabled, a fast-forward origin commit can become the base without changing the source checkout; explicit --branch leases are unchanged.", JA: "有効にすると、source の checkout を変えずに fast-forward した origin commit を起点にできます。--branch を明示した貸出は変わりません。"},

	"config.worktree.submodules.name":        {EN: "Prepare submodules", JA: "submodule を準備する"},
	"config.worktree.submodules.description": {EN: "Whether the submodules of a repository are checked out in the worktree, through local clones.", JA: "リポジトリの submodule を、ローカル clone を経由して worktree にも用意するかどうかです。"},
	"config.worktree.submodules.impact":      {EN: "Changing this makes the existing standby worktrees unusable, so they are rebuilt.", JA: "変更すると既存の standby worktree は使えなくなり、作り直しになります。"},

	"config.storage.worktree_root.name":        {EN: "Worktree storage root", JA: "worktree の保存先"},
	"config.storage.worktree_root.description": {EN: "The directory under which wx creates the worktrees it lends out. The source repository is never touched.", JA: "wx が貸し出す worktree を作るディレクトリです。ソースリポジトリには手を加えません。"},
	"config.storage.worktree_root.impact":      {EN: "Only worktrees created after the change go to the new place; the existing ones stay where they are and are not moved.", JA: "変更後に作る worktree だけが新しい場所になります。既存のものはそのまま残り、移動しません。"},

	"config.storage.copy_mode.name":        {EN: "How files are copied", JA: "ファイルのコピー方式"},
	"config.storage.copy_mode.description": {EN: "Whether checked-out files share storage with the original through APFS copy-on-write (cow), are copied byte for byte (copy), or are decided per file system (auto).", JA: "チェックアウトしたファイルを APFS の copy-on-write で共有するか（cow）、実体をコピーするか（copy）、ファイルシステムを見て自動で決めるか（auto）です。"},
	"config.storage.copy_mode.impact":      {EN: "CoW sharing saves disk space and time. The standby worktrees of affected repositories are rebuilt.", JA: "CoW 共有はディスクと時間を節約します。影響を受けるリポジトリの standby worktree は作り直しになります。"},

	"config.storage.cow_min_size_kib.name":        {EN: "Smallest file to share with CoW", JA: "CoW 共有の最小ファイルサイズ"},
	"config.storage.cow_min_size_kib.description": {EN: "Files smaller than this size in KiB are copied normally instead of being shared, because checking them costs more than sharing saves.", JA: "この KiB 未満のファイルは共有せず通常のコピーにします。小さいファイルは、共有で節約できる量より判定の手間が上回るためです。"},
	"config.storage.cow_min_size_kib.impact":      {EN: "A lower value shares more files but spends more time inspecting them.", JA: "小さくすると共有できるファイルは増えますが、判定に時間がかかります。"},

	"config.storage.repo_dir_source.name":        {EN: "Where repository directory names come from", JA: "リポジトリのディレクトリ名の決め方"},
	"config.storage.repo_dir_source.description": {EN: "Whether the directory name a repository gets inside a worktree is taken from its remote URL (remote) or from its directory name on disk (directory).", JA: "worktree の中でリポジトリに与えるディレクトリ名を、remote の URL から決めるか（remote）、手元のディレクトリ名から決めるか（directory）です。"},
	"config.storage.repo_dir_source.impact":      {EN: "Only worktrees prepared after the change use the new names.", JA: "変更後に準備する worktree だけが新しい名前になります。"},

	"config.storage.backup_generations.name":        {EN: "State database backups to keep", JA: "状態データベースのバックアップ世代数"},
	"config.storage.backup_generations.description": {EN: "How many generations of the state database backup wx keeps. The database records which worktrees are lent out and what has been saved.", JA: "状態データベースのバックアップを何世代残すかです。このデータベースには、貸出中の worktree と保存済みの作業が記録されています。"},
	"config.storage.backup_generations.impact":      {EN: "More generations use more disk but reach further back when a database has to be restored.", JA: "多いほどディスクを使いますが、復元できる範囲は古くまで広がります。"},

	"config.storage.backup_retention.name":        {EN: "How long backups are kept", JA: "バックアップの保持期間"},
	"config.storage.backup_retention.description": {EN: "How long a state database backup is kept before the routine cleanup deletes it.", JA: "状態データベースのバックアップを、定期的な後片付けが削除するまでどれだけ保持するかです。"},
	"config.storage.backup_retention.impact":      {EN: "A shorter period frees disk sooner and shortens the window for restoring an old database.", JA: "短くするとディスクは早く空きますが、古いデータベースへ戻せる期間も短くなります。"},

	"config.pool.warm_per_workspace.name":        {EN: "Standby worktrees per workspace", JA: "workspace ごとの standby worktree 数"},
	"config.pool.warm_per_workspace.description": {EN: "How many prepared worktrees wx keeps waiting for each workspace that uses the hot policy, so that a launch does not have to wait for preparation.", JA: "hot 方針の workspace ごとに、準備済みの worktree をいくつ待機させておくかです。起動時に準備を待たずに済むようにするためのものです。"},
	"config.pool.warm_per_workspace.impact":      {EN: "More standbys mean faster launches and more disk in use. Zero turns automatic replenishment off.", JA: "多いほど起動は速くなり、ディスクの使用量は増えます。0 にすると自動補充を行いません。"},

	"config.pool.preparation_concurrency.name":        {EN: "How many preparations run at once", JA: "同時に走る準備処理の数"},
	"config.pool.preparation_concurrency.description": {EN: "How many preparation, restore, and save jobs a user is waiting on may run at the same time. Replenishing and reclaiming standby worktrees runs in its own slots and is not limited by this value.", JA: "利用者が完了を待つ準備・復元・保存を、同時に何本まで実行するかです。standby worktree の補充と回収は専用の枠で動くため、この値では止まりません。"},
	"config.pool.preparation_concurrency.impact":      {EN: "A higher value finishes a queue sooner but puts more load on CPU and disk.", JA: "大きくすると待ち行列は早く片付きますが、CPU とディスクの負荷が上がります。"},

	"config.retention.hot_standby.name":        {EN: "How long an unused standby stays ready", JA: "未使用 standby を保つ期間"},
	"config.retention.hot_standby.description": {EN: "How long a prepared standby worktree that nobody used is kept before it is reclaimed.", JA: "誰も使わなかった準備済みの standby worktree を、回収するまでどれだけ保つかです。"},
	"config.retention.hot_standby.impact":      {EN: "A shorter period frees disk sooner but makes a cold start more likely. Zero turns automatic replenishment off.", JA: "短くするとディスクは早く空きますが、一から準備する起動が増えます。0 にすると自動補充を行いません。"},

	"config.retention.ended_worktree.name":        {EN: "How long a finished session's worktree is kept", JA: "終了した session の worktree を残す期間"},
	"config.retention.ended_worktree.description": {EN: "After an agent session ends, its worktree is kept for this long so that the session can be resumed exactly where it stopped.", JA: "エージェントの session が終了した後、止めた場所からそのまま再開できるよう worktree を残しておく期間です。"},
	"config.retention.ended_worktree.impact":      {EN: "A shorter period frees disk sooner, and a resume after that point has to restore from the saved snapshot instead.", JA: "短くするとディスクは早く空き、その後の再開は保存済みのスナップショットからの復元になります。"},

	"config.retention.quarantined.name":        {EN: "How long quarantined worktrees are kept", JA: "隔離した worktree を残す期間"},
	"config.retention.quarantined.description": {EN: "A worktree whose preparation or cleanup failed is quarantined instead of deleted, so that the failure can still be examined. This is how long it stays.", JA: "準備や後片付けに失敗した worktree は、原因を調べられるよう削除せず隔離します。その隔離を保つ期間です。"},
	"config.retention.quarantined.impact":      {EN: "A shorter period frees disk sooner and leaves less to diagnose a failure with.", JA: "短くするとディスクは早く空きますが、失敗の調査に使える材料は減ります。"},

	"config.retention.recovery_snapshot.name":        {EN: "How long saved work can be restored", JA: "保存した作業を復元できる期間"},
	"config.retention.recovery_snapshot.description": {EN: "How long the snapshots wx takes when a worktree is returned are kept. They are what a resume restores from.", JA: "worktree の返却時に wx が取るスナップショットを保持する期間です。再開はこのスナップショットから復元します。"},
	"config.retention.recovery_snapshot.impact":      {EN: "A shorter period frees disk sooner and shortens the window for getting old work back.", JA: "短くするとディスクは早く空きますが、古い作業を取り戻せる期間も短くなります。"},

	"config.retention.expired_session_tombstone.name":        {EN: "How long expired session IDs are remembered", JA: "期限切れ session ID を覚えておく期間"},
	"config.retention.expired_session_tombstone.description": {EN: "After a session expires, wx keeps its identifier for this long so that a later reference to it can be answered with \"expired\" instead of \"unknown\".", JA: "session が期限切れになった後も、その ID をこの期間だけ覚えておきます。後から参照されたときに「不明」ではなく「期限切れ」と答えるためです。"},
	"config.retention.expired_session_tombstone.impact":      {EN: "A shorter period makes stale references and duplicate IDs harder to explain.", JA: "短くすると、古い参照や ID の重複の原因を説明しにくくなります。"},

	"config.retention.failed_job.name":        {EN: "How long failed job records are kept", JA: "失敗したジョブの記録を残す期間"},
	"config.retention.failed_job.description": {EN: "How long the record of a daemon job that failed is kept for diagnosis.", JA: "daemon のジョブが失敗したときの記録を、調査のためにどれだけ残すかです。"},
	"config.retention.failed_job.impact":      {EN: "A shorter period keeps less failure history available.", JA: "短くすると、参照できる失敗の履歴が減ります。"},

	"config.retention.event_log.name":        {EN: "How long the event log is kept", JA: "イベント記録を残す期間"},
	"config.retention.event_log.description": {EN: "How long the daemon keeps the record of what it did, which is what wx doctor and wx status read back.", JA: "daemon が行ったことの記録を保持する期間です。wx doctor や wx status が読み取ります。"},
	"config.retention.event_log.impact":      {EN: "A shorter period leaves less history for diagnosis.", JA: "短くすると、調査に使える履歴が減ります。"},

	"config.discovery.max_depth.name":        {EN: "How deep to look for repositories", JA: "リポジトリを探す深さ"},
	"config.discovery.max_depth.description": {EN: "How many directory levels below a workspace root wx searches for Git repositories.", JA: "workspace の root から何階層下まで Git リポジトリを探すかです。"},
	"config.discovery.max_depth.impact":      {EN: "A deeper search finds more repositories and takes longer.", JA: "深くすると見つかるリポジトリは増え、走査に時間がかかります。"},

	"config.discovery.max_entries.name":        {EN: "Maximum entries to inspect", JA: "走査するエントリ数の上限"},
	"config.discovery.max_entries.description": {EN: "The maximum number of directory entries wx inspects while searching a workspace, so that a huge tree cannot stall the search.", JA: "workspace の走査中に調べるディレクトリエントリ数の上限です。巨大なツリーで走査が止まらないようにするためのものです。"},
	"config.discovery.max_entries.impact":      {EN: "A higher limit supports larger trees at the cost of a longer search.", JA: "上限を上げると大きなツリーにも対応できますが、走査は長くなります。"},

	"config.discovery.timeout.name":        {EN: "Time limit for the search", JA: "走査の時間制限"},
	"config.discovery.timeout.description": {EN: "How long the search for repositories and workspaces may take before it is given up.", JA: "リポジトリと workspace の走査を、あきらめるまでどれだけ続けてよいかです。"},
	"config.discovery.timeout.impact":      {EN: "A limit that is too short rejects large workspaces that would have been found.", JA: "短すぎると、本来見つかるはずの大きな workspace が拒否されます。"},

	"config.discovery.reconcile_interval.name":        {EN: "How often the daemon checks itself", JA: "daemon が点検する間隔"},
	"config.discovery.reconcile_interval.description": {EN: "How often the daemon compares what it records with what is on disk, and runs the maintenance that follows from the difference.", JA: "daemon が記録と実体を突き合わせ、その差から必要な保守を行う間隔です。"},
	"config.discovery.reconcile_interval.impact":      {EN: "A shorter interval notices changes sooner and runs maintenance more often.", JA: "短くすると変化に早く気づき、保守の実行回数が増えます。"},

	"config.discovery.exclude.name":        {EN: "Directories to skip while searching", JA: "走査から除外するディレクトリ"},
	"config.discovery.exclude.description": {EN: "Directory names that are not searched for repositories, such as dependency or build directories.", JA: "リポジトリを探さないディレクトリ名です。依存関係やビルド成果物のディレクトリなどを指定します。"},
	"config.discovery.exclude.impact":      {EN: "Removing an entry widens the search area and slows it down.", JA: "除外を外すと走査範囲が広がり、時間がかかります。"},

	"config.readiness.mode.name":        {EN: "When an agent may start", JA: "エージェントを開始してよい時点"},
	"config.readiness.mode.description": {EN: "Whether an agent may start as soon as the worktree is usable (early) or only after preparation is completely finished (full).", JA: "worktree が使える状態になった時点で開始してよいか（early）、準備が完全に終わってから開始するか（full）です。"},
	"config.readiness.mode.impact":      {EN: "early starts sooner and relies on the wx hooks to hold the agent back until the rest is ready; full waits for everything up front.", JA: "early は早く始まり、残りの準備が整うまで wx の hook がエージェントを待たせます。full は最初にすべてを待ちます。"},

	"config.readiness.early_paths.name":        {EN: "Files to place before an early start", JA: "early 開始の前に置くファイル"},
	"config.readiness.early_paths.description": {EN: "Extra paths copied into the worktree before an early start, for the configuration an agent reads as it comes up.", JA: "early 開始の前に worktree へ用意する追加の path です。エージェントが起動時に読む設定を置くために使います。"},
	"config.readiness.early_paths.impact":      {EN: "Makes that configuration available sooner; each path also has to be prepared before the agent may start.", JA: "その設定を早く利用できます。一方で、指定した path はエージェント開始前に準備する必要があります。"},

	"config.readiness.timeout.name":        {EN: "How long to wait for a worktree", JA: "worktree を待つ時間の上限"},
	"config.readiness.timeout.description": {EN: "How long wx waits for a worktree to become usable before it gives up.", JA: "worktree が使える状態になるのを、あきらめるまで待つ時間です。"},
	"config.readiness.timeout.impact":      {EN: "A value that is too short interrupts preparation that would have succeeded.", JA: "短すぎると、成功していたはずの準備を途中で打ち切ります。"},

	"config.readiness.progress.name":        {EN: "Show preparation progress", JA: "準備の進捗を表示する"},
	"config.readiness.progress.description": {EN: "Whether progress is printed while a worktree is being prepared on an interactive terminal.", JA: "対話的な端末で worktree を準備している間、進捗を表示するかどうかです。"},
	"config.readiness.progress.impact":      {EN: "Display only; preparation itself is unchanged.", JA: "表示だけの違いで、準備の動作は変わりません。"},

	"config.resume.auto_fresh.name":        {EN: "Resume in a fresh worktree automatically", JA: "復元できないとき新しい worktree で再開する"},
	"config.resume.auto_fresh.description": {EN: "What wx does when a resume cannot restore the original worktree: start in a fresh one automatically, or ask first.", JA: "再開時に元の worktree を復元できなかった場合に、確認せず新しい worktree で始めるか、先に尋ねるかです。"},
	"config.resume.auto_fresh.impact":      {EN: "When enabled the conversation resumes without a prompt, but the unsaved work of the original worktree is not there.", JA: "有効にすると確認なしで会話を再開できますが、元の worktree の未保存の作業はありません。"},

	"config.lease.ttl.name":        {EN: "How long a borrowed worktree is left alone", JA: "借りた worktree をそのままにする時間"},
	"config.lease.ttl.description": {EN: "How long a worktree borrowed without an agent may sit before wx saves its work and moves on.", JA: "エージェントを介さずに借りた worktree を、wx が作業を保存して先へ進めるまで放置してよい時間です。"},
	"config.lease.ttl.impact":      {EN: "Edits made after that point are not in the snapshot wx saved.", JA: "その時点より後の編集は、wx が保存したスナップショットに含まれません。"},

	"config.lease.shell.name":        {EN: "Shell to open", JA: "開くシェル"},
	"config.lease.shell.description": {EN: "The shell wx starts when it opens a shell in a worktree.", JA: "worktree でシェルを開くときに wx が起動するシェルです。"},
	"config.lease.shell.impact":      {EN: "Leave it empty to use the shell from the environment.", JA: "空にすると、環境の設定にあるシェルを使います。"},

	"config.includes.default_agent_rules.name":        {EN: "Place the standard agent instruction files", JA: "標準のエージェント指示ファイルを配置する"},
	"config.includes.default_agent_rules.description": {EN: "Whether the standard agent instruction files that Git does not track, such as CLAUDE.local.md, are copied from the source repository into the worktree. Tracked files arrive with the checkout and are unaffected.", JA: "CLAUDE.local.md のように Git が追跡しない標準のエージェント指示ファイルを、ソースリポジトリから worktree へコピーするかどうかです。追跡されているファイルは checkout で入るため、この設定の対象外です。"},
	"config.includes.default_agent_rules.impact":      {EN: "When disabled, only the paths configured explicitly are placed.", JA: "無効にすると、明示的に設定した path だけを配置します。"},

	"config.agent.add_dir.name":        {EN: "Extra directories given to the agent", JA: "エージェントへ渡す追加ディレクトリ"},
	"config.agent.add_dir.description": {EN: "When wx passes the repositories of a workspace to the agent as additional directories it may read: always, only when working in a worktree (worktree), or never (off).", JA: "workspace のリポジトリを、エージェントが読める追加ディレクトリとして渡す条件です。常に渡す（always）、worktree で作業するときだけ渡す（worktree）、渡さない（off）から選びます。"},
	"config.agent.add_dir.impact":      {EN: "Changes which repositories and files the agent can read.", JA: "エージェントが読めるリポジトリとファイルが変わります。"},

	"config.update.auto_check.name":        {EN: "Check for new versions", JA: "新しいバージョンを確認する"},
	"config.update.auto_check.description": {EN: "Whether the daemon occasionally asks GitHub whether a newer wx has been released, so that the status screen and an interactive launch can tell you about it.", JA: "新しい wx が公開されていないかを daemon が時々 GitHub に尋ねるかどうかです。状態画面と対話的な起動でお知らせするために使います。"},
	"config.update.auto_check.impact":      {EN: "When disabled, wx never contacts GitHub on its own; wx update still checks when you run it yourself.", JA: "無効にすると、wx が自分から GitHub へ接続することはなくなります。自分で実行する wx update は無効でも確認します。"},

	"config.daemon.login_shell.name":        {EN: "Start the daemon through a login shell", JA: "daemon をログインシェルから起動する"},
	"config.daemon.login_shell.description": {EN: "Whether the LaunchAgent starts the daemon through your login shell, so that the PATH your shell startup files build reaches the daemon and the commands it runs for you, such as the Git hooks of a repository.", JA: "LaunchAgent が daemon をログインシェル経由で起動するかどうかです。シェルの起動ファイルが組み立てた PATH が daemon と、その daemon が実行するリポジトリの Git hook などへ届きます。"},
	"config.daemon.login_shell.impact":      {EN: "Changing this rewrites the LaunchAgent, so run wx daemon install afterwards; when disabled, only the fixed PATH written into the LaunchAgent is available.", JA: "変更すると LaunchAgent の内容が変わるため、あとで wx daemon install を実行してください。無効にすると、LaunchAgent に書かれた固定の PATH だけが使われます。"},

	"config.logging.level.name":        {EN: "Log detail", JA: "ログの詳しさ"},
	"config.logging.level.description": {EN: "How much detail the daemon writes to its log file.", JA: "daemon がログファイルへどこまで詳しく書くかです。"},
	"config.logging.level.impact":      {EN: "A more detailed level helps diagnosis and produces much more log.", JA: "詳しくすると調査はしやすくなりますが、ログの量は大きく増えます。"},

	"config.sessions.paths.claude.sessions.name":        {EN: "Where Claude history is read from", JA: "Claude の履歴を読む場所"},
	"config.sessions.paths.claude.sessions.description": {EN: "The directories searched for Claude conversation history when you pick a conversation to resume.", JA: "再開する会話を選ぶときに、Claude の会話履歴を探すディレクトリです。"},
	"config.sessions.paths.claude.sessions.impact":      {EN: "Changes which conversations appear as candidates.", JA: "候補として表示される会話が変わります。"},

	"config.sessions.paths.codex.sessions.name":        {EN: "Where Codex history is read from", JA: "Codex の履歴を読む場所"},
	"config.sessions.paths.codex.sessions.description": {EN: "The directories searched for Codex conversation history when you pick a conversation to resume.", JA: "再開する会話を選ぶときに、Codex の会話履歴を探すディレクトリです。"},
	"config.sessions.paths.codex.sessions.impact":      {EN: "Changes which conversations appear as candidates.", JA: "候補として表示される会話が変わります。"},

	"config.worktree.name":        {EN: "Worktree policy for this workspace", JA: "この workspace の worktree 方針"},
	"config.worktree.description": {EN: "What wx does for this workspace: keep standby worktrees ready (hot), prepare one on demand (cold), or work in the repository itself (off).", JA: "この workspace で wx がどうするかです。standby を用意しておく（hot）、必要になってから準備する（cold）、リポジトリ本体で作業する（off）から選びます。"},
	"config.worktree.impact":      {EN: "Takes effect the next time an agent is launched for this workspace.", JA: "この workspace で次にエージェントを起動するときから有効になります。"},

	"config.copy.name":        {EN: "Paths copied into the worktree", JA: "worktree へコピーする path"},
	"config.copy.description": {EN: "Paths taken from a workspace root that is not a Git repository and copied into each worktree, for files the work needs but Git does not track.", JA: "Git リポジトリではない workspace root から各 worktree へコピーする path です。作業に必要だが Git が追跡していないファイルのために使います。"},
	"config.copy.impact":      {EN: "Changes what a worktree contains, so the standby worktrees prepared from the old list are rebuilt.", JA: "worktree の内容が変わるため、古い一覧で用意した standby worktree は作り直しになります。"},

	"config.link.name":        {EN: "Paths linked back to the workspace", JA: "workspace へリンクする path"},
	"config.link.description": {EN: "Paths under a workspace root that is not a Git repository which are linked back to that root instead of being copied, for large or shared content such as caches.", JA: "Git リポジトリではない workspace root の下で、コピーではなくその root へリンクする path です。キャッシュのように大きい、または共有したい内容に使います。"},
	"config.link.impact":      {EN: "Every worktree reads and writes the same content, so a change in one is seen by all of them.", JA: "どの worktree も同じ実体を読み書きするため、一方の変更が他方にも見えます。"},

	"config.reuse_standby.name":        {EN: "Reuse standbys in this workspace", JA: "この workspace で standby を再利用する"},
	"config.reuse_standby.description": {EN: "Overrides, for this workspace, whether a standby worktree may be updated to the requested commit and reused.", JA: "standby worktree を要求された commit へ更新して再利用してよいかを、この workspace だけ上書きします。"},
	"config.reuse_standby.impact":      {EN: "Takes precedence over the global setting for this workspace only.", JA: "この workspace に限り、全体の設定より優先されます。"},

	"config.fetch_default_branch.name":        {EN: "Fetch the default branch in this workspace", JA: "この workspace で既定ブランチを fetch する"},
	"config.fetch_default_branch.description": {EN: "Overrides, for this workspace, whether wx fetches origin's default branch before leases that do not specify a branch.", JA: "branch を指定しない貸出の前に origin の既定ブランチを fetch するかを、この workspace だけ上書きします。"},
	"config.fetch_default_branch.impact":      {EN: "A fast-forward origin commit can become the base of new or updated READY worktrees; explicit --branch leases are unchanged.", JA: "fast-forward した origin commit が新規または更新する READY worktree の起点になります。--branch を明示した貸出は変わりません。"},

	"config.submodules.name":        {EN: "Prepare submodules in this workspace", JA: "この workspace で submodule を準備する"},
	"config.submodules.description": {EN: "Overrides, for this workspace, whether submodules are checked out in the worktree.", JA: "worktree に submodule を用意するかを、この workspace だけ上書きします。"},
	"config.submodules.impact":      {EN: "Takes precedence over the global setting; changing it rebuilds the standby worktrees of this workspace.", JA: "全体の設定より優先されます。変更するとこの workspace の standby worktree は作り直しになります。"},

	"config.warm_count.name":        {EN: "Standby worktrees for this workspace", JA: "この workspace の standby worktree 数"},
	"config.warm_count.description": {EN: "How many prepared worktrees wait for this workspace, overriding the global count.", JA: "この workspace のために待機させる準備済み worktree の数で、全体の設定を上書きします。"},
	"config.warm_count.impact":      {EN: "More standbys mean faster launches and more disk in use. Zero turns automatic replenishment off for this workspace.", JA: "多いほど起動は速くなり、ディスクの使用量は増えます。0 にするとこの workspace の自動補充を行いません。"},

	"config.default_branch.name":        {EN: "Branch new worktrees start from", JA: "worktree の起点ブランチ"},
	"config.default_branch.description": {EN: "The explicit branch a new worktree of this repository is checked out from when no branch is given; when unset, wx resolves one from the repository's Git refs.", JA: "ブランチを指定しなかったとき、このリポジトリの新しい worktree をどのブランチから作るかを明示します。未設定なら wx がリポジトリの Git ref から自動解決します。"},
	"config.default_branch.impact":      {EN: "Changes the starting point of the work in newly prepared worktrees.", JA: "新しく準備する worktree での作業の出発点が変わります。"},

	"config.dir_name.name":        {EN: "Directory name for this repository", JA: "このリポジトリのディレクトリ名"},
	"config.dir_name.description": {EN: "Fixes the directory name this repository is given inside a worktree, instead of deriving it.", JA: "worktree の中でこのリポジトリに与えるディレクトリ名を、導出せずに固定します。"},
	"config.dir_name.impact":      {EN: "Only worktrees prepared after the change use the new name.", JA: "変更後に準備する worktree だけが新しい名前になります。"},

	"config.dir_source.name":        {EN: "Where this repository's directory name comes from", JA: "このリポジトリのディレクトリ名の決め方"},
	"config.dir_source.description": {EN: "Whether this repository's directory name is taken from its remote URL (remote) or from its directory name on disk (directory).", JA: "このリポジトリのディレクトリ名を、remote の URL から決めるか（remote）、手元のディレクトリ名から決めるか（directory）です。"},
	"config.dir_source.impact":      {EN: "Takes precedence over the global naming policy for this repository.", JA: "このリポジトリに限り、全体の命名方針より優先されます。"},

	"config.cow_min_size_kib.name":        {EN: "Smallest file to share with CoW here", JA: "このリポジトリの CoW 共有の最小サイズ"},
	"config.cow_min_size_kib.description": {EN: "Overrides, for this repository, the file size in KiB below which files are copied instead of shared.", JA: "共有せずコピーするファイルサイズの下限（KiB）を、このリポジトリだけ上書きします。"},
	"config.cow_min_size_kib.impact":      {EN: "The standby worktrees of this repository are rebuilt.", JA: "このリポジトリの standby worktree は作り直しになります。"},

	"config.prepare.command.name":        {EN: "Command run after checkout", JA: "チェックアウト後に実行するコマンド"},
	"config.prepare.command.description": {EN: "A command wx runs in this repository once the files are in place, for setup such as installing dependencies.", JA: "ファイルを配置した後に、このリポジトリで wx が実行するコマンドです。依存関係の導入などの下準備に使います。"},
	"config.prepare.command.impact":      {EN: "If the command fails, the worktree is not handed over: the preparation fails with it.", JA: "コマンドが失敗すると worktree は貸し出されず、準備ごと失敗します。"},

	"config.prepare.inputs.name":        {EN: "Paths that trigger preparation on standby updates", JA: "standby 更新で準備を再実行する path"},
	"config.prepare.inputs.description": {EN: "Repository-relative path patterns whose changes make wx rerun the preparation command while updating a standby worktree.", JA: "standby worktree の更新中に変更されると、準備コマンドを再実行する repository 相対の path pattern です。"},
	"config.prepare.inputs.impact":      {EN: "Only matching tracked or placed paths add the preparation command to standby updates; an empty list keeps the existing behavior.", JA: "一致した tracked または配置 path がある更新だけ準備コマンドを追加で実行します。空の list なら従来どおりです。"},

	"config.prepare.timeout.name":        {EN: "Time limit for that command", JA: "そのコマンドの時間制限"},
	"config.prepare.timeout.description": {EN: "How long the command run after checkout may take before it is stopped.", JA: "チェックアウト後のコマンドを、打ち切るまでどれだけ実行してよいかです。"},
	"config.prepare.timeout.impact":      {EN: "A limit that is too short fails preparation that would have succeeded.", JA: "短すぎると、成功していたはずの準備が失敗します。"},

	"config.prepare.version.name":        {EN: "Preparation version", JA: "準備のバージョン"},
	"config.prepare.version.description": {EN: "A label you change by hand when the preparation command should be treated as different, even though its text did not change.", JA: "準備コマンドの文面は変わらないが別物として扱いたいときに、手で書き換える識別子です。"},
	"config.prepare.version.impact":      {EN: "Changing it makes the existing standby worktrees unusable, so they are prepared again.", JA: "変更すると既存の standby worktree は使えなくなり、準備し直しになります。"},

	"config.repository_defaults.default_branch.name":        {EN: "Default starting branch for this workspace", JA: "この workspace の既定の起点ブランチ"},
	"config.repository_defaults.default_branch.description": {EN: "The explicit starting branch used by the repositories of this workspace that do not set one themselves; when unset, wx resolves one from each repository's Git refs.", JA: "この workspace のリポジトリのうち、自分で指定していないものが使う起点ブランチを明示します。未設定なら wx が各リポジトリの Git ref から自動解決します。"},
	"config.repository_defaults.default_branch.impact":      {EN: "Changes the starting point of newly prepared worktrees in this workspace.", JA: "この workspace で新しく準備する worktree の出発点が変わります。"},

	"config.repository_defaults.dir_source.name":        {EN: "Default directory-name source for this workspace", JA: "この workspace の既定のディレクトリ名の決め方"},
	"config.repository_defaults.dir_source.description": {EN: "How directory names are derived for the repositories of this workspace that do not set it themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもののディレクトリ名の決め方です。"},
	"config.repository_defaults.dir_source.impact":      {EN: "Only worktrees prepared after the change use the new names.", JA: "変更後に準備する worktree だけが新しい名前になります。"},

	"config.repository_defaults.cow_min_size_kib.name":        {EN: "Default CoW threshold for this workspace", JA: "この workspace の既定の CoW 最小サイズ"},
	"config.repository_defaults.cow_min_size_kib.description": {EN: "The CoW size threshold used by the repositories of this workspace that do not set one themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないものが使う CoW の下限サイズです。"},
	"config.repository_defaults.cow_min_size_kib.impact":      {EN: "The standby worktrees of the affected repositories are rebuilt.", JA: "影響を受けるリポジトリの standby worktree は作り直しになります。"},

	"config.repository_defaults.submodules.name":        {EN: "Default submodule handling for this workspace", JA: "この workspace の既定の submodule 設定"},
	"config.repository_defaults.submodules.description": {EN: "Whether submodules are prepared for the repositories of this workspace that do not set it themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので submodule を準備するかどうかです。"},
	"config.repository_defaults.submodules.impact":      {EN: "The standby worktrees of the affected repositories are rebuilt.", JA: "影響を受けるリポジトリの standby worktree は作り直しになります。"},

	"config.repository_defaults.prepare.command.name":        {EN: "Default post-checkout command for this workspace", JA: "この workspace の既定のチェックアウト後コマンド"},
	"config.repository_defaults.prepare.command.description": {EN: "The command run after checkout in the repositories of this workspace that do not set one themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので、チェックアウト後に実行するコマンドです。"},
	"config.repository_defaults.prepare.command.impact":      {EN: "If it fails, preparation of the affected repositories fails with it.", JA: "失敗すると、影響を受けるリポジトリの準備も失敗します。"},

	"config.repository_defaults.prepare.inputs.name":        {EN: "Default paths that trigger preparation on standby updates", JA: "この workspace の standby 更新で準備を再実行する既定 path"},
	"config.repository_defaults.prepare.inputs.description": {EN: "Path patterns inherited by repositories in this workspace that make wx rerun the preparation command when a standby is updated.", JA: "この workspace のリポジトリが継承し、standby 更新時に準備コマンドを再実行する契機となる path pattern です。"},
	"config.repository_defaults.prepare.inputs.impact":      {EN: "Matching changes rerun preparation for the affected worktrees before they are handed over.", JA: "一致する変更があれば、影響を受ける worktree を貸し出す前に準備を再実行します。"},

	"config.repository_defaults.prepare.timeout.name":        {EN: "Default command time limit for this workspace", JA: "この workspace の既定のコマンド時間制限"},
	"config.repository_defaults.prepare.timeout.description": {EN: "The time limit for that command in the repositories of this workspace that do not set one themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないものでの、そのコマンドの時間制限です。"},
	"config.repository_defaults.prepare.timeout.impact":      {EN: "A limit that is too short fails preparation that would have succeeded.", JA: "短すぎると、成功していたはずの準備が失敗します。"},

	"config.repository_defaults.prepare.version.name":        {EN: "Default preparation version for this workspace", JA: "この workspace の既定の準備バージョン"},
	"config.repository_defaults.prepare.version.description": {EN: "The preparation version used by the repositories of this workspace that do not set one themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないものが使う準備バージョンです。"},
	"config.repository_defaults.prepare.version.impact":      {EN: "Changing it makes the existing standby worktrees unusable, so they are prepared again.", JA: "変更すると既存の standby worktree は使えなくなり、準備し直しになります。"},

	"config.repository_defaults.includes.default_agent_rules.name":        {EN: "Default agent instruction files for this workspace", JA: "この workspace の既定のエージェント指示ファイル"},
	"config.repository_defaults.includes.default_agent_rules.description": {EN: "Whether the standard agent instruction files are placed for the repositories of this workspace that do not set it themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので、標準のエージェント指示ファイルを配置するかどうかです。"},
	"config.repository_defaults.includes.default_agent_rules.impact":      {EN: "Changes what the worktrees of the affected repositories contain.", JA: "影響を受けるリポジトリの worktree の内容が変わります。"},

	"config.repository_defaults.readiness.mode.name":        {EN: "Default start timing for this workspace", JA: "この workspace の既定の開始時点"},
	"config.repository_defaults.readiness.mode.description": {EN: "When an agent may start for the repositories of this workspace that do not set it themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので、エージェントを開始してよい時点です。"},
	"config.repository_defaults.readiness.mode.impact":      {EN: "Changes how soon an agent may start in this workspace.", JA: "この workspace でエージェントを開始できる早さが変わります。"},

	"config.repository_defaults.readiness.early_paths.name":        {EN: "Default early paths for this workspace", JA: "この workspace の既定の early 配置 path"},
	"config.repository_defaults.readiness.early_paths.description": {EN: "The paths placed before an early start in the repositories of this workspace that do not set them themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので、early 開始の前に配置する path です。"},
	"config.repository_defaults.readiness.early_paths.impact":      {EN: "Changes which configuration is available before preparation finishes.", JA: "準備の完了前に利用できる設定が変わります。"},

	"config.repository_defaults.readiness.timeout.name":        {EN: "Default wait limit for this workspace", JA: "この workspace の既定の待ち時間上限"},
	"config.repository_defaults.readiness.timeout.description": {EN: "How long to wait for a worktree in the repositories of this workspace that do not set it themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので、worktree を待つ時間の上限です。"},
	"config.repository_defaults.readiness.timeout.impact":      {EN: "A value that is too short interrupts preparation that would have succeeded.", JA: "短すぎると、成功していたはずの準備を途中で打ち切ります。"},

	"config.repository_defaults.readiness.progress.name":        {EN: "Default progress display for this workspace", JA: "この workspace の既定の進捗表示"},
	"config.repository_defaults.readiness.progress.description": {EN: "Whether preparation progress is shown for the repositories of this workspace that do not set it themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので、準備の進捗を表示するかどうかです。"},
	"config.repository_defaults.readiness.progress.impact":      {EN: "Display only; preparation itself is unchanged.", JA: "表示だけの違いで、準備の動作は変わりません。"},

	"config.repository_defaults.storage.copy_mode.name":        {EN: "Default copy method for this workspace", JA: "この workspace の既定のコピー方式"},
	"config.repository_defaults.storage.copy_mode.description": {EN: "How files are copied for the repositories of this workspace that do not set it themselves.", JA: "この workspace のリポジトリのうち、自分で指定していないもので、ファイルをどうコピーするかです。"},
	"config.repository_defaults.storage.copy_mode.impact":      {EN: "The standby worktrees of the affected repositories are rebuilt.", JA: "影響を受けるリポジトリの standby worktree は作り直しになります。"},
}
