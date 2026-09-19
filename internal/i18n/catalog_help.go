package i18n

// catalog_help.go は wx 全体の help と、workspace を貸し出すコマンドの help 本文を持つ。
// 桁揃えと改行位置そのものが表示契約なので、行や語句へ分割せずコマンド単位の
// 長文を英日 2 本で置き、描画側は ID を引くだけにする。

// 本文中のコマンド名・オプション名・path などの機械識別子は訳さない。

var helpCatalogSession = map[string]Entry{
	"help.top": {
		EN: `Usage: wx [wx-options] <claude|codex> [agent-arguments...]
       wx <command> [options]

Global options:
  --branch <branch|repo=branch>  choose a detached base (repeatable)
  --fresh                        resume conversation from the current base
  -s, --select-worktree          select and save the workspace policy again
  -w, --worktree                 create a worktree without saving a policy
  -n, --no-worktree              run here without saving a policy
  -h, --help                     show help
  -v, --version                  show version

The --branch option may also appear after claude or codex; arguments after -- are passed unchanged.

Commands:
  claude [arguments...]          launch Claude Code in a wx workspace
  codex [arguments...]           launch Codex in a wx workspace
  shell [--resume <id>]          open a shell in a wx workspace
  run [--resume <id>] -- <cmd>   run one command in a wx workspace
  new [--json]                   lease a workspace and print its path
  release <id> [--discard]       return a leased workspace now
  status [--verbose] [--json]    show daemon and pool state
  doctor [--probe] [--json]      check configuration, dependencies, worktrees
  gc [--dry-run]                 run retention cleanup
  prune [--all] [--dry-run]      delete recovery refs the database cannot explain
  clear [--all] [--standby]      delete managed worktrees now
  retry-standby [--all] [<path>] resume standby replenishment after it stopped
  slots [--all] [--json]         list managed wx slots and their disk usage
  bench [--runs <n>] [--json]    measure how long a workspace takes to prepare
  config [<key> ...]             show or update configuration
  setup [--check] [--remove]     review and complete, or remove, the wx setup
  setup-check [<workspace>]      validate one workspace without retiring standby slots
  resume <id> [agent] [args...]  restore a wx session
  discard-recovery <workspace>   discard recovery state that lost its refs
  forget <workspace-path>        stop managing a workspace
  update [--apply]               check for a newer wx and install it
  daemon start|stop|restart      change whether the daemon is running
  daemon install|uninstall       register or remove the LaunchAgent`,
		JA: `使い方: wx [wx-options] <claude|codex> [agent-arguments...]
       wx <command> [options]

全体オプション:
  --branch <branch|repo=branch>  detached base を選択（複数指定可）
  --fresh                        現在の base から会話を再開
  -s, --select-worktree          workspace 方針を再選択して保存
  -w, --worktree                 方針を保存せず worktree を作成
  -n, --no-worktree              方針を保存せずここで実行
  -h, --help                     ヘルプを表示
  -v, --version                  バージョンを表示

--branch は claude または codex の後ろにも置けます。-- より後ろの引数はそのまま渡します。

コマンド:
  claude [arguments...]          wx workspace で Claude Code を起動
  codex [arguments...]           wx workspace で Codex を起動
  shell [--resume <id>]          wx workspace で shell を開く
  run [--resume <id>] -- <cmd>   wx workspace でコマンドを1つ実行
  new [--json]                   workspace を貸し出し、その path を表示
  release <id> [--discard]       貸出中の workspace を直ちに返却
  status [--verbose] [--json]    daemon と pool の状態を表示
  doctor [--probe] [--json]      設定・依存関係・worktree を検査
  gc [--dry-run]                 保持期間に基づく整理を実行
  prune [--all] [--dry-run]      database が説明できない復旧 ref を削除
  clear [--all] [--standby]      管理 worktree を直ちに削除
  retry-standby [--all] [<path>] 停止した standby 補充を再開
  slots [--all] [--json]         管理 wx slot とディスク使用量を一覧表示
  bench [--runs <n>] [--json]    workspace の準備にかかる時間を計測
  config [<key> ...]             設定を表示または更新
  setup [--check] [--remove]     wx setup を確認して完了、または削除
  setup-check [<workspace>]      standby を退役させず workspace 1 件を検証
  resume <id> [agent] [args...]  wx session を復元
  discard-recovery <workspace>   ref を失った復旧状態を破棄
  forget <workspace-path>        workspace の管理を解除
  update [--apply]               新しい wx を確認して導入
  daemon start|stop|restart      daemon の稼働状態を変更
  daemon install|uninstall       LaunchAgent を登録または削除`,
	},
	"help.command.setup-check": {
		EN: `Usage: wx setup-check [<workspace>]

Lease one workspace normally and run the focused worktree setup checks against it.
Unlike wx doctor --probe, this does not retire standby slots or discard the lease.`,
		JA: `使い方: wx setup-check [<workspace>]

workspace 1 件を通常どおり貸し出し、worktree のセットアップ検査を実行します。
wx doctor --probe と異なり、standby slot の退役や貸出の破棄は行いません。`,
	},
	"help.command.resume": {
		EN: `Usage: wx resume <wx-session-id> [claude|codex] [--fresh] [--branch <branch>] [agent-arguments...]

Restore an archived wx session into a new managed workspace.
With --fresh, keep the conversation but build the worktree from the current base.
Use --branch with --fresh to choose the detached base.`,
		JA: `使い方: wx resume <wx-session-id> [claude|codex] [--fresh] [--branch <branch>] [agent-arguments...]

アーカイブ済み wx session を新しい管理 workspace へ復元します。
--fresh では会話を保持し、現在の base から worktree を作成します。
--fresh と --branch で detached base を選択します。`,
	},
	"help.command.shell": {
		EN: `Usage: wx shell [--branch <branch|repo=branch>] [--resume <wx-session-id>]

Open a shell in a managed wx workspace. This is the way to work in an isolated
worktree yourself, without launching an agent.

The workspace is prepared the same way it is for wx claude and wx codex, and it
is returned when the shell exits: unfinished work is saved first, and the
worktree stays for retention.ended_worktree so wx shell --resume <id> brings it
back. wx clear --all can also ask the shell to stop.

--resume only accepts sessions leased by wx shell, wx run, or wx new. Resume a
session started by an agent with wx resume <wx-session-id> [claude|codex].

The worktree is detached, as it is for every wx workspace; --branch only
chooses the base commit, and creating or pushing a branch is left to you. The
shell comes from lease.shell, then $SHELL, then /bin/sh.

Options:
  --branch <branch|repo=branch>  choose a detached base (repeatable)
  --resume <wx-session-id>       restore the worktree of an earlier wx session`,
		JA: `使い方: wx shell [--branch <branch|repo=branch>] [--resume <wx-session-id>]

管理された wx workspace で shell を開きます。agent を起動せず隔離された
worktree で自分で作業するためのコマンドです。

workspace は wx claude / wx codex と同じ方法で準備され、shell 終了時に返却されます。未完了の作業は先に保存され、worktree は retention.ended_worktree の間残るため wx shell --resume <id> で戻せます。wx clear --all は shell に停止を求めることもできます。

--resume は wx shell、wx run、wx new が貸し出した session だけを受け付けます。agent が開始した session は wx resume <wx-session-id> [claude|codex] で再開します。

worktree は全 wx workspace と同じく detached です。--branch は base commit だけを選び、branch の作成や push は利用者が行います。shell は lease.shell、次に $SHELL、最後に /bin/sh から選びます。

オプション:
  --branch <branch|repo=branch>  detached base を選択（複数指定可）
  --resume <wx-session-id>       以前の wx session の worktree を復元`,
	},
	"help.command.run": {
		EN: `Usage: wx run [--branch <branch|repo=branch>] [--resume <wx-session-id>] -- <command> [arguments...]

Run one command in a managed wx workspace and exit with its status. Use it to
run a build or a test suite against an isolated worktree without disturbing the
current one.

The workspace is returned when the command exits, and the same saving and
retention as wx shell apply, so wx shell --resume <id> can reopen what the
command left behind.

--resume only accepts sessions leased by wx shell, wx run, or wx new. Resume a
session started by an agent with wx resume <wx-session-id> [claude|codex].

The command runs at the top of the workspace even when wx run is invoked from a
subdirectory; the original directory is passed as WX_SOURCE_CWD.

Options:
  --branch <branch|repo=branch>  choose a detached base (repeatable)
  --resume <wx-session-id>       restore the worktree of an earlier wx session`,
		JA: `使い方: wx run [--branch <branch|repo=branch>] [--resume <wx-session-id>] -- <command> [arguments...]

管理された wx workspace でコマンドを1つ実行し、その終了 status で終了します。
隔離された worktree で build や test suite を、現在の状態を乱さず実行します。

command 終了時に workspace を返却し、保存と retention は wx shell と同じです。wx shell --resume <id> で command が残した内容を再開できます。

--resume は wx shell、wx run、wx new が貸し出した session だけを受け付けます。agent が開始した session は wx resume <wx-session-id> [claude|codex] で再開します。

wx run を subdirectory から呼んでも command は workspace の top で実行し、元の directory は WX_SOURCE_CWD として渡します。

オプション:
  --branch <branch|repo=branch>  detached base を選択（複数指定可）
  --resume <wx-session-id>       以前の wx session の worktree を復元`,
	},
	"help.command.new": {
		EN: `Usage: wx new [--branch <branch|repo=branch>] [--json]

Lease a managed wx workspace and print its path on one line. Nothing is
started in it, so this is how an agent prepares a worktree to hand to a
SubAgent.

The lease does NOT follow the process that asked for it: wx new sends no
heartbeat, so the workspace is not reclaimed when the caller exits. It is
returned when the wx session that ran wx new ends, when wx release <id> is
run, or when lease.ttl has passed, whichever comes first.
Without one of the first two, the workspace stays leased until that deadline.

Expiry saves before it returns the lease, and nothing edits the worktree
afterwards, so work written after the deadline is not saved. Return the lease
with wx release <id> when the SubAgent is done rather than relying on the
deadline.

The session id to pass to wx release and wx shell --resume is shown by wx
slots, or by --json here.

Options:
  --branch <branch|repo=branch>  choose a detached base (repeatable)
  --json                         print the session id and path as JSON`,
		JA: `使い方: wx new [--branch <branch|repo=branch>] [--json]

管理された wx workspace を貸し出し、path を1行で表示します。何も
起動しないため、agent が SubAgent に渡す worktree を準備する方法です。
SubAgent。

lease は要求した process には追従しません。wx new は heartbeat を送らないため caller 終了時には回収されません。wx new を実行した wx session の終了、wx release <id>、lease.ttl 経過のうち早い時点で返却されます。最初の2つがなければ期限まで貸出中です。

期限切れでは lease を返す前に保存し、その後 worktree は編集しません。期限後に書いた作業は保存されないため、期限に頼らず SubAgent 完了時に wx release <id> で返却してください。

wx release と wx shell --resume に渡す session id は wx slots または
ここで --json を指定すると表示されます。

オプション:
  --branch <branch|repo=branch>  detached base を選択（複数指定可）
  --json                         session id と path を JSON で表示`,
	},
	"help.command.release": {
		EN: `Usage: wx release <wx-session-id> [--discard] [--wait] [--json]

Return a workspace leased by wx new without waiting for lease.ttl. Unfinished
work is saved first, and the worktree stays for retention.ended_worktree so
wx shell --resume <id> can reopen it.

With --discard the work is not saved: the return schedules the worktree for
removal instead, so nothing is left to reopen with wx shell --resume.

Only leases with no running process are returned this way. A workspace held by
wx shell or wx run is returned when that shell or command exits, and one held
by an agent when that agent exits; wx clear --all asks either of them to stop.

By default this command reports that the return was accepted while the save or
removal runs in the background. --wait waits for that job to finish and exits 1
if it fails or quarantines the slot. Interrupting --wait leaves the daemon job
running; use wx doctor to check it.

Exit status is 0 when the request succeeds, 1 for an RPC or waited-job failure,
and 2 for invalid arguments.

Options:
  --discard  return the lease without saving unfinished work, and remove the
             worktree instead of keeping it for retention.ended_worktree
  --wait     wait for the snapshot or removal job and report its final state
  --json     print one machine-readable JSON result line`,
		JA: `使い方: wx release <wx-session-id> [--discard] [--wait] [--json]

wx new で貸し出した workspace を lease.ttl を待たずに返却します。未完了の
作業は先に保存し、worktree は retention.ended_worktree の間残るため、
wx shell --resume <id> で再開できます。

--discard では作業を保存せず、返却時に worktree を
削除対象にするため、wx shell --resume で再開するものは残りません。

実行中 process がない lease だけをこの方法で返却します。workspace を保持する
wx shell / wx run は shell または command の終了時、保持する
agent は agent 終了時に返却されます。wx clear --all はいずれにも停止を求めます。

既定では返却を受け付けたことだけを表示し、保存または削除は daemon がバックグラウンドで続けます。--wait はその job の完了まで待ち、失敗または slot の隔離で終了コード 1 を返します。--wait を中断しても daemon の job は続くため、wx doctor で状態を確認してください。

終了コードは、要求成功が 0、RPC または待機した job の失敗が 1、引数不正が 2 です。

オプション:
  --discard  未保存の作業を保存せず貸出を返却し、
             worktree を retention.ended_worktree に残さず削除
  --wait     snapshot または削除 job の完了を待ち、最終状態を表示
  --json     機械可読な JSON の結果を 1 行で表示`,
	},
	"help.command.slots": {
		EN: `Usage: wx slots [--all] [--json]

List managed wx slots with their repository, session, copy mode, and disk usage.
AGENT names what holds the slot: claude or codex for an agent, and wx-shell,
wx-run, or wx-path for a workspace leased by wx shell, wx run, or wx new.
Every slot that still occupies disk is listed, so the SIZE(MB) column covers the
same slots as the Disk line of wx status. REPO names the source repositories the
slot was prepared from; a multi-repo workspace lists them separated by commas.

SIZE(MB) is what the slot occupies on its own, rounded up: blocks it still
shares with the main worktree are excluded, so the column sums without double
counting. COPY reports what the slot looks like now: cow once any file still
shares blocks with the main worktree, copy otherwise.

The daemon measures a slot when its preparation finishes and re-measures every
root periodically; wx slots only reads those results, never measures on demand.
Rows still waiting for the first measurement show pending, and platforms that
cannot compare blocks show unsupported. --json carries the measurement time.

--json adds the lease kind, the time the lease expires, and the wx session
that asked for a wx new lease.

Options:
  --all   also list sessions that no longer hold a slot
  --json  print machine-readable JSON`,
		JA: `使い方: wx slots [--all] [--json]

管理 wx slot の repository、session、copy mode、ディスク使用量を一覧表示します。AGENT は slot の保持者を示し、agent なら claude/codex、wx shell・wx run・wx new の貸出なら wx-shell/wx-run/wx-path です。disk を占有する全 slot を一覧するため SIZE(MB) は wx status の Disk 行と同じ対象です。REPO は準備元の source repository で、multi-repo workspace は comma 区切りです。

SIZE(MB) は slot 単独の占有量を切り上げた値です。main worktree と共有する block は除外するため二重計上せず合計できます。COPY は現在の状態を示し、1 file でも block を共有していれば cow、そうでなければ copy です。

daemon は準備完了時に slot を計測し、その後も root ごとに定期計測します。wx slots は結果を読むだけで、要求時の計測はしません。初回計測待ちの行は pending、block を比較できない platform は unsupported と表示します。--json には計測時刻を含めます。

--json では lease kind、lease の期限、wx new の貸出を要求した wx session も追加します。

オプション:
  --all   slot を保持していない session も一覧表示
  --json  機械可読 JSON を表示`,
	},
}
