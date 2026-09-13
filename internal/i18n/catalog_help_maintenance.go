package i18n

// catalog_help_maintenance.go は状態確認・診断・回収コマンドの help 本文を持つ。
// 桁揃えと改行位置そのものが表示契約なので、行や語句へ分割せずコマンド単位の
// 長文を英日 2 本で置き、描画側は ID を引くだけにする。

// 本文中のコマンド名・オプション名・path などの機械識別子は訳さない。

var helpCatalogMaintenance = map[string]Entry{
	"help.command.status": {
		EN: `Usage: wx status [--verbose] [--json]

Show a workspace summary, or all daemon, pool, session, retention, and disk
details with --verbose (-v). The JSON shape is unchanged by either display.

The summary lists the workspaces whose POLICY creates worktrees (HOT or COLD)
and any workspace that still holds one. A workspace that neither uses nor holds
a worktree is left to --verbose and --json.

Disk reports what the managed slots and snapshots occupy on their own: blocks
they still share with the main worktrees are excluded, so it sums with the
SIZE(MB) column of wx slots and stays below what du reports. Paths outside the
database are listed separately as Unmanaged. --verbose adds the full allocated
size and the shared part behind it.

Options:
  --verbose, -v  show detailed status instead of the summary
  --json         print machine-readable JSON`,
		JA: `使い方: wx status [--verbose] [--json]

workspace の要約、または daemon・pool・session・retention・disk の全詳細を表示
詳細は --verbose (-v) で表示します。どちらの表示でも JSON の形状は変わりません。

要約には POLICY が worktree（HOT または COLD）を作成する workspace
と、worktree を保持している workspace を一覧表示します。どちらも該当しない workspace は
--verbose と --json の場合だけ表示します。

Disk は管理 slot と snapshot が単独で占める容量を示します。
main worktree と共有している block は除外するため、
SIZE(MB) wx slots の列と合計でき、du より小さくなります。
database 外の path は Unmanaged として別に表示します。--verbose では割当全体と
共有部分も追加表示します。

オプション:
  --verbose, -v  要約の代わりに詳細な状態を表示
  --json         機械可読 JSON を表示`,
	},
	"help.command.doctor": {
		EN: `Usage: wx doctor [--probe] [--verbose] [--json]

Run read-only checks for configuration, storage, Git, launchd, hooks, and recovery data.
Print one line when nothing is wrong; otherwise report each problem with its target, cause, and action.
Exit 0 when only passing and informational results remain, 1 when a problem or an unfinished check remains.

Without --probe no worktree is created, so a workspace that prepares a worktree
wx cannot actually work in is not detected. With --probe wx leases one workspace
at a time, waits for EARLY READY and then FULL READY, and inspects what it got:
submodules the index records but whose directory stayed empty, tracked files
already modified in a worktree wx just prepared, output a post-checkout hook
wrote while still exiting 0, and whether the copy shared blocks with the main
worktrees. It also reports how long each workspace took and what the prepared
slot occupies per repository. Every probed workspace is returned without saving
it, as wx release --discard does.

--probe first retires the standby worktrees waiting for each workspace so what
it measures is a cold start. They are reclaimed by normal GC and replenished
afterwards, so starting a session in a probed workspace is slower until then.
It runs one workspace at a time and takes as long as preparing every registered
workspace does. Interrupting it skips the return of the workspace being probed,
which then stays leased until the wx session that ran the command ends, or until
lease.ttl.

Options:
  --probe        prepare a worktree in every registered workspace and check it
  --verbose, -v  show passing checks, informational results, and extra diagnostics
  --json         print machine-readable JSON with every result regardless of --verbose`,
		JA: `使い方: wx doctor [--probe] [--verbose] [--json]

設定、storage、Git、launchd、hook、復旧データを読み取り専用で検査します。
問題がなければ1行、問題があれば対象・原因・対処とともに報告します。
正常または情報だけなら終了コード0、問題または未完了の検査があれば1で終了します。

--probe なしでは worktree を作成しないため、worktree の準備が必要な workspace が
実際には動作できないことを検出できません。--probe では workspace を1つずつ貸し出し、
EARLY READY、続いて FULL READY まで待ち、取得した内容を検査します。
index が記録しているのにディレクトリが空のままの submodule、wx が準備した worktree ですでに変更された tracked file、終了コード0のまま post-checkout hook が書き込んだ出力、copy が main worktree と block を共有したかを検査します。workspace ごとの所要時間と、準備した slot が repository ごとに占める容量も表示します。検査した workspace は wx release --discard と同じく保存せず返却します。

--probe は各 workspace の待機中 standby worktree を先に退役させるため、
cold start を計測します。これらは通常の GC で回収され、その後補充されます。
その後に補充されるため、検査した workspace で session を始めると補充が終わるまで遅くなります。1 workspace ずつ実行し、登録済み全 workspace の準備と同じだけ時間がかかります。中断すると検査中の workspace は返却されず、コマンドを実行した wx session の終了または lease.ttl まで貸出中のままです。

オプション:
  --probe        登録済み各 workspace で worktree を準備して検査
  --verbose, -v  成功した検査、情報、追加の診断を表示
  --json         機械可読 JSON を表示 --verbose に関係なく全結果を`,
	},
	"help.command.gc": {
		EN: `Usage: wx gc [--dry-run]

Run retention cleanup without deleting unarchived workspace data.

Options:
  --dry-run  report candidates without removing them`,
		JA: `使い方: wx gc [--dry-run]

未アーカイブの workspace データを削除せず保持期間の整理を実行します。

オプション:
  --dry-run  削除せず候補を報告`,
	},
	"help.command.prune": {
		EN: `Usage: wx prune [--all] [--dry-run]

Delete the recovery refs that the current database no longer explains. These
are left behind when the state database is rebuilt, and the daemon reports
them once per repository.

By default wx deletes only the refs it can prove are safe to lose: every
object they reach is also reachable from another ref, so nothing is lost. A
ref that keeps objects of its own is left alone and reported with the number
of objects that would become unreachable.

With --all those refs are deleted too, and the work saved in them is lost for
good. Refs the current database still expects are never touched, with or
without --all.

Exit status is 0 when the run finished, 1 when something failed, and 2 for an
argument error. Keeping refs that could not be proven safe is not a failure.

Options:
  --all      delete refs whose contents cannot be proven safe to lose,
             discarding the work they hold
  --dry-run  report what would be deleted, changing nothing`,
		JA: `使い方: wx prune [--all] [--dry-run]

現在の database が説明できない復旧 ref を削除します。 state database の再作成時に残され、daemon が repository ごとに一度報告する ref です。

既定では、失って安全だと証明できる ref だけを wx が削除します。ref が到達する全 object が別の ref からも到達できるため、作業は失われません。固有の object を保持する ref は残し、到達不能になる object 数を報告します。

--all ではそれらの ref も削除され、保存された作業は完全に失われます。現在の database が必要とする ref は --all の有無にかかわらず変更しません。

実行完了時は終了コード0、失敗時は1、引数エラー時は2です。安全に削除できると証明できない ref を残しても失敗ではありません。

オプション:
  --all      失って安全だと証明できない ref を削除し、
             保持している作業を破棄
  --dry-run  削除対象を報告し、何も変更しない`,
	},
	"help.command.clear": {
		EN: `Usage: wx clear [--all] [--standby] [--discard] [--dry-run]

Delete the worktrees wx manages without waiting for their retention period.
Work is saved first: recovery data, session history, and workspace
registrations stay, and their retention follows the existing settings.

Standby worktrees waiting for the next session are kept unless --standby or
--all is given. Once they are deleted, wx does not replenish them until the
affected workspace is used again.

Without --all, sessions that are in use are left alone. With --all, wx asks
those sessions to stop, waits up to 30s for each of them, and deletes only the
ones that stopped; nothing is killed.

Quarantined slots are deleted in every mode, without waiting out
retention.quarantined. Database registration authorizes deletion, including slots with
missing identity records or changed markers, locks, and HEAD. Unregistered
directories are left alone. With --discard, unfinished work is deleted without
requiring a successful snapshot. Sessions in use still require --all.

The command waits for every target to finish. Interrupting it does not stop
the daemon, and running it again rejoins the clear already in progress. While
a clear runs, new sessions and resumes are refused.

Exit status is 0 when every target succeeded or there was nothing to do, 1
when something failed, was quarantined, or did not finish, and 2 for an
argument error. Keeping sessions in use or standby worktrees is not a failure.

Options:
  --all      ask sessions in use to stop, then delete what stopped, standby
             worktrees included
  --discard  delete selected worktrees without saving unfinished work
  --standby  delete standby worktrees too
  --dry-run  report the targets and the reasons wx cannot process some of
             them, changing nothing`,
		JA: `使い方: wx clear [--all] [--standby] [--discard] [--dry-run]

保持期間を待たずに wx の管理 worktree を削除します。
最初に作業を保存します。復旧データ、session 履歴、workspace 登録は残り、保持期間は既存設定に従います。

次の session を待つ standby worktree は --standby または --all を指定しない限り残します。削除すると、その workspace が再び使われるまで wx は補充しません。

--all なしでは使用中の session を残します。--all では session に停止を求め、各 session を最大30秒待って、停止したものだけを削除します。強制終了はしません。

隔離された slot は全モードで retention.quarantined を待たずに削除します。database の登録が削除を認可するため、identity record、marker、lock、HEAD が変わった slot や欠落した slot も対象です。未登録の directory は残します。--discard では snapshot 成功を待たず未完了の作業を削除します。使用中 session には引き続き --all が必要です。

全対象の完了を待ちます。中断しても daemon は停止せず、再実行すると進行中の clear に再参加します。clear 中は新しい session と resume を拒否します。

全対象が成功または対象なしなら終了コード0、失敗・隔離・未完了があれば1、引数エラーなら2です。使用中 session や standby worktree を残しても失敗ではありません。

オプション:
  --all      使用中 session に停止を求め、停止したものを削除（standby
             worktree を含む）
  --discard  未完了の作業を保存せず選択した worktree を削除
  --standby  standby worktree も削除
  --dry-run  対象と、wx が処理できない理由を報告し、
             何も変更しない`,
	},
	"help.command.retry-standby": {
		EN: `Usage: wx retry-standby <workspace-path>
       wx retry-standby --all

Resume automatic standby worktree replenishment for a workspace after wx
stopped it, then retry replenishment in the current generation. Replenishment
also resumes on its own once wx claude or wx codex succeeds for the workspace.
Standby worktrees that failed to prepare still fill the warm count, so
retry-standby schedules them for removal before it replenishes.
Quarantined worktrees are left untouched; wx gc deletes them once
retention.quarantined has passed, and wx clear deletes them right away.

The workspace path is shown by wx status when standby replenishment is stopped.

Options:
  --all  resume every workspace whose replenishment stopped and whose
         configuration still replenishes`,
		JA: `使い方: wx retry-standby <workspace-path>
       wx retry-standby --all

wx が停止した workspace の standby worktree 自動補充を再開し、
現在の世代で補充を再試行します。 wx claude または wx codex が workspace で成功すると補充も自動的に再開します。準備に失敗した standby worktree も warm count を占めるため、retry-standby は補充前に削除を予約します。隔離された worktree は変更せず、retention.quarantined 経過後は wx gc が、直ちに削除する場合は wx clear が処理します。

standby 補充が停止した場合、workspace path は wx status で確認できます。

オプション:
  --all  補充が停止し、設定上も補充対象である全 workspace を再開`,
	},
	"help.command.discard-recovery": {
		EN: `Usage: wx discard-recovery <workspace-path> [--dry-run]

Discard the recovery state of the sessions whose recovery refs are gone from
the source repository, so that workspace stops being reported by wx doctor and
can be forgotten.

A recovery ref disappears when the source repository is deleted and recreated
at the same path. wx then quarantines the snapshots that recorded those refs,
and they can no longer restore anything. This command deletes those snapshot
records, ends their sessions, and deletes the worktrees of the slots they
quarantined, unsaved work included. Sessions of other workspaces, and every
session that still has its refs, are left alone. Run --dry-run first to see
the sessions and worktree paths that would go.

Options:
  --dry-run  list what would be discarded without changing anything`,
		JA: `使い方: wx discard-recovery <workspace-path> [--dry-run]

復旧 ref が source repository から消えた session の復旧状態を破棄し、
wx doctor の報告対象から workspace を外して
管理を解除できるようにします。

source repository を同じ path で削除・再作成すると recovery ref が消えます。wx はその ref を記録した snapshot を隔離し、復元できなくします。この command は snapshot record を削除し、session を終了し、隔離した slot の worktree（未保存の作業を含む）を削除します。他 workspace の session と、ref が残る session はそのままです。先に --dry-run で対象 session と worktree path を確認してください。

オプション:
  --dry-run  変更せず破棄対象を一覧表示`,
	},
	"help.command.forget": {
		EN: `Usage: wx forget <workspace-path> [--discard-recovery]

Stop managing a workspace, reclaiming the worktrees wx still owns for it.

Standby worktrees are reclaimed on the way out: they hold no work of yours.
A workspace that is still in use is refused, whatever the flags: end its
sessions, or run wx release <id>, and run wx forget again.

Recovery state is kept, and the workspace is refused until you say to throw it
away. That state is the released sessions wx can still restore, their snapshots
and the recovery refs in the source repository, and the worktrees of the
sessions that were quarantined because their refs are gone. The refusal counts
what is there. --discard-recovery deletes all of it, unsaved work included, and
then forgets the workspace.

Options:
  --discard-recovery  discard the recovery state of this workspace instead of
                      refusing to forget it`,
		JA: `使い方: wx forget <workspace-path> [--discard-recovery]

workspace の管理を解除し、wx が保持している worktree を回収します。

standby worktree は利用者の作業を持たないため、解除の過程で回収します。使用中の workspace は flag にかかわらず拒否するので、session を終了するか wx release <id> を実行してから wx forget をもう一度実行してください。

復旧状態は保持し、破棄を指示するまで workspace の解除を拒否します。復旧状態とは、wx がまだ復元できる返却済み session と、その snapshot および source repository 内の recovery ref、さらに ref が消えたため隔離された session の worktree です。拒否の際にはその件数を表示します。--discard-recovery はそれらを未保存の作業ごと削除してから workspace の管理を解除します。

オプション:
  --discard-recovery  解除を拒否する代わりに、この workspace の
                      復旧状態を破棄する`,
	},
	"help.command.daemon": {
		EN: `Usage: wx daemon <start|stop|restart|install|uninstall> [--foreground]

Change whether the daemon is running, or install and remove the per-user
LaunchAgent that starts it at login.

  start      run the daemon, or report that it is already running
  stop       exit the running daemon, leaving the LaunchAgent registered
  restart    replace the running daemon so a new wx binary takes effect
  install    write the LaunchAgent plist and load it
  uninstall  unload the LaunchAgent and remove its plist

start, stop, and restart wait for the daemon to reach the requested state and
give up after 60s. The daemon acts only once it is idle, so none of them cuts
short a request or a job that is already running.

Options:
  --foreground  with start, serve in this process instead of asking launchd
                to start the daemon. This is how the LaunchAgent runs wx.`,
		JA: `使い方: wx daemon <start|stop|restart|install|uninstall> [--foreground]

daemon の稼働状態を変更するか、login 時に起動するユーザー単位の LaunchAgent を登録・解除します。

  start      daemon を起動（起動中ならその旨を表示）
  stop       稼働中の daemon を終了（LaunchAgent は登録したまま）
  restart    稼働中の daemon を置き換え、新しい wx binary を有効化
  install    LaunchAgent plist を書き込み読み込む
  uninstall  LaunchAgent を解除し plist を削除

start、stop、restart は daemon が要求状態になるまで待ち、60秒で打ち切ります。daemon は idle になってから動作するため、実行中の request や job を途中で切りません。

オプション:
  --foreground  start 時に launchd へ依頼せず、この process で daemon を提供
                LaunchAgent が wx を実行する方式`,
	},
	"help.command.bench": {
		EN: `Usage: wx bench [--runs <n>] [--branch <branch|repo=branch>] [--sweep|--config <key=value,...>] [--reuse] [--json]

Measure how long the current workspace takes to become usable, and where that
time goes. Each run leases a workspace the way wx new does, waits for EARLY
READY and then for FULL READY, prints the breakdown the daemon recorded for
that preparation, and returns the lease without saving it.

EARLY READY is the point an agent can start: Git registration and the startup
files are in place. FULL READY adds the remaining checkout, the includes and
links, the prepare command, CoW sharing, and the final validation. Both are
measured from the lease request, so they include the time the request waited
for a job slot.

By default the standby worktrees waiting for the current workspace are retired
first, so what is measured is a cold start. They are reclaimed by normal GC and
replenished afterwards. With --reuse nothing is retired and the run measures
whatever the pool returns, which is reported as warm when a prepared slot was
handed over with no preparation to measure.

The breakdown comes from the running daemon and is not persisted, so restarting
the daemon between the preparation and the report loses it. Phase names that
contain a dot, such as cow.compare, are totals across the parallel workers of
that phase and can exceed the wall-clock time of the phase above them.

With --runs, wx waits for the saving, removal, and replenishment jobs of the
previous run to finish before measuring the next one, and prints the minimum,
median, and maximum at the end. A single successful run has no distribution to
summarize, so its measured time is printed on its own, both in that summary and
in the comparison table.

--sweep measures the settings a search for the best CoW threshold usually
compares: copy_mode=copy as the baseline without CoW sharing, then
cow_min_size_kib of 0, 4, 8, 16, 32, 64, and 128, each with the copy mode the
daemon is running with. It is the same measurement as passing those eight
values as --config by hand, so the two options are rejected together.

Repeat --config to compare preparation settings of your own. Each --config is
one configuration to measure, written as copy_mode=<auto|cow|copy> and
cow_min_size_kib=<n> separated by commas; the keys you leave out keep the value
the daemon is running with.

Every configuration is measured --runs times, and the comparison table adds the
disk each configuration left behind: the exclusive size is what the slot
occupies after CoW sharing is discounted, taken from the same measurement wx
slots reports. A configuration whose usage is not measured before the lease is
returned is reported as - and keeps its timings. A single run per configuration
is one sample of a machine under changing load, so settings whose min and max
overlap are separated by measuring again with a larger --runs. The settings
travel with the lease request and apply only to the slot prepared for it: wx
neither reads nor writes your configuration file, and the daemon keeps
preparing every other workspace with its own settings. Interrupting the command
therefore leaves no measurement setting behind. Slots prepared for a
configuration are not handed to later leases or kept as standby, so each
configuration is always measured as a cold start, and both --sweep and --config
are rejected together with --reuse.

The measured workspace is returned without saving it, as wx release --discard
does. Interrupting wx bench skips that return, and the workspace then stays
leased until the wx session that ran the command ends, or until lease.ttl.

Exit status is 0 when every run finished, 1 when one of them failed, and 2 for
an argument error.

Options:
  --runs <n>                     measure this many leases (default 1)
  --branch <branch|repo=branch>  choose a detached base (repeatable)
  --sweep                        compare copy_mode=copy and cow_min_size_kib
                                 0/4/8/16/32/64/128
  --config <key=value,...>       compare this preparation setting; keys are
                                 copy_mode and cow_min_size_kib (repeatable)
  --reuse                        keep standby worktrees and measure what the
                                 pool returns
  --json                         print machine-readable JSON`,
		JA: `使い方: wx bench [--runs <n>] [--branch <branch|repo=branch>] [--sweep|--config <key=value,...>] [--reuse] [--json]

現在の workspace が使えるようになるまでの時間と、その内訳を計測します。
内訳を計測します。各 run は wx new と同じように workspace を貸し出し、EARLY
READY、続いて FULL READY を待ち、daemon が記録した準備の内訳を
表示して保存せず lease を返却します。

EARLY READY は agent を開始できる時点で、Git 登録と startup file が揃っています。FULL READY では残りの checkout、include/link、prepare command、CoW sharing、最終検証も完了します。どちらも lease 要求から計測するため、job slot 待ち時間を含みます。

既定では現在の workspace を待つ standby worktree を先に退役させ、cold start を計測します。通常の GC で回収した後に補充します。--reuse では退役させず pool が返したものを計測し、準備なしで渡された slot は warm と報告します。

内訳は稼働中 daemon から取得し永続化しないため、準備後・表示前に daemon を再起動すると失われます。cow.compare のように dot を含む phase 名は並列 worker の合計で、上位 phase の経過時間を超えることがあります。

--runs では前の run の保存・削除・補充 job が終わってから次を計測し、最後に最小・中央値・最大を表示します。成功 run が1件だけなら分布を作らず、その計測値を概要と比較表に単独表示します。

--sweep は最適な CoW threshold の探索で比較する設定を計測します。CoW なしの copy_mode=copy を基準に、cow_min_size_kib の 0、4、8、16、32、64、128 を daemon の copy mode ごとに測ります。8値を --config で指定する場合と同じため、2つの option は併用できません。

--config を繰り返して独自の準備設定を比較できます。各 --config は1つの設定で、copy_mode=<auto|cow|copy> と cow_min_size_kib=<n> を comma 区切りで書きます。省略した key は daemon の実効値を使います。

各設定を --runs 回計測し、比較表には設定後の disk も追加します。exclusive size は CoW sharing を差し引いた slot 占有量で、wx slots と同じ計測値です。lease 返却前に usage を計測できなかった設定は - と表示し、時間は残します。設定ごとに1 run は負荷変動下の1サンプルなので、min/max が重なる設定は --runs を増やして再計測します。設定は lease 要求とともに送られ、その slot にだけ適用します。wx は設定ファイルを読み書きせず、他 workspace は独自設定で準備します。中断しても計測設定は残りません。設定用 slot は後続 lease や standby に回さないため常に cold start を測り、--sweep と --config は --reuse と併用できません。

計測した workspace は wx release --discard と同じく保存せず返却します。wx bench を中断すると返却を省略し、コマンドを実行した wx session の終了または lease.ttl まで貸出中のままです。

全 run 完了時は終了コード0、1件でも失敗すれば1、引数エラーなら2です。

オプション:
  --runs <n>                     この回数だけ lease を計測（既定 1）
  --branch <branch|repo=branch>  detached base を選択（複数指定可）
  --sweep                        copy_mode=copy と cow_min_size_kib の
                                 0/4/8/16/32/64/128 を比較
  --config <key=value,...>       準備設定を比較（key は copy_mode と
                                 cow_min_size_kib、複数指定可）
  --reuse                        standby worktree を保持し、pool の返却を計測
  --json                         機械可読 JSON を表示`,
	},
}
