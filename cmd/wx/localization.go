package main

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// commandContext は直接サブコマンド関数を呼ぶテストと、run からの通常起動の
// どちらにも設定言語を明示する。壊れた設定は config.LoadLanguage の英語 fallback を使う。
func commandContext(ctx context.Context) context.Context {
	if i18n.HasLanguage(ctx) {
		return ctx
	}
	return i18n.WithLanguage(ctx, config.LoadLanguage())
}

func commandLocalizer() *i18n.Localizer {
	return i18n.New(config.LoadLanguage())
}

// translateHelp は既存の長い help 本文を機械識別子を変えずに日本語へ寄せる。
// 英語本文は既存契約をそのまま維持し、未翻訳の長文は安全に英語を残す。
func translateHelp(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	replacements := []struct{ en, ja string }{
		{"Usage:", "使い方:"},
		{"Global options:", "全体オプション:"},
		{"Commands:", "コマンド:"},
		{"Options:", "オプション:"},
		{"without changing anything", "変更せずに"},
		{"print machine-readable JSON", "機械可読 JSON を表示"},
		{"show detailed status instead of the summary", "要約の代わりに詳細な状態を表示"},
		{"show passing checks and extra diagnostics", "成功した検査と追加の診断を表示"},
		{"report the current state without changing anything", "変更せずに現在の状態を表示"},
		{"delete the configuration wx setup writes, leaving the shell startup file alone", "wx setup が書き込んだ設定を削除し、shell の起動ファイルは残す"},
		{"show version", "バージョンを表示"},
		{"show help", "ヘルプを表示"},
		{"already running", "すでに起動しています"},
		{"already stopped", "すでに停止しています"},
		{"start the daemon", "daemon を起動"},
		{"stop the daemon", "daemon を停止"},
		{"restart the daemon", "daemon を再起動"},
		{"run retention cleanup", "保持期間に基づく整理を実行"},
		{"machine-readable", "機械可読"},
		{"choose a detached base (repeatable)", "detached base を選択（複数指定可）"},
		{"resume conversation from the current base", "現在の base から会話を再開"},
		{"select and save the workspace policy again", "workspace 方針を再選択して保存"},
		{"create a worktree without saving a policy", "方針を保存せず worktree を作成"},
		{"run here without saving a policy", "方針を保存せずここで実行"},
		{"launch Claude Code in a wx workspace", "wx workspace で Claude Code を起動"},
		{"launch Codex in a wx workspace", "wx workspace で Codex を起動"},
		{"open a shell in a wx workspace", "wx workspace で shell を開く"},
		{"run one command in a wx workspace", "wx workspace でコマンドを1つ実行"},
		{"lease a workspace and print its path", "workspace を貸し出し、その path を表示"},
		{"return a leased workspace now", "貸出中の workspace を直ちに返却"},
		{"show daemon and pool state", "daemon と pool の状態を表示"},
		{"check configuration, dependencies, worktrees", "設定・依存関係・worktree を検査"},
		{"delete recovery refs the database cannot explain", "database が説明できない復旧 ref を削除"},
		{"delete managed worktrees now", "管理 worktree を直ちに削除"},
		{"resume standby replenishment after it stopped", "停止した standby 補充を再開"},
		{"list managed wx slots and their disk usage", "管理 wx slot とディスク使用量を一覧表示"},
		{"measure how long a workspace takes to prepare", "workspace の準備にかかる時間を計測"},
		{"show or update configuration", "設定を表示または更新"},
		{"review and complete, or remove, the wx setup", "wx setup を確認して完了、または削除"},
		{"restore a wx session", "wx session を復元"},
		{"discard recovery state that lost its refs", "ref を失った復旧状態を破棄"},
		{"forget an inactive workspace", "非アクティブな workspace の管理を解除"},
		{"change whether the daemon is running", "daemon の稼働状態を変更"},
		{"Show a workspace summary, or all daemon, pool, session, retention, and disk", "workspace の要約、または daemon・pool・session・retention・disk の全詳細を表示"},
		{"details with --verbose (-v). The JSON shape is unchanged by either display.", "詳細は --verbose (-v) で表示します。どちらの表示でも JSON の形状は変わりません。"},
		{"The summary lists the workspaces whose POLICY creates worktrees (HOT or COLD)", "要約には POLICY が worktree（HOT または COLD）を作成する workspace"},
		{"and any workspace that still holds one. A workspace that neither uses nor holds", "と、worktree を保持している workspace を一覧表示します。どちらも該当しない workspace は"},
		{"a worktree is left to --verbose and --json.", "--verbose と --json の場合だけ表示します。"},
		{"Disk reports what the managed slots and snapshots occupy on their own: blocks", "Disk は管理 slot と snapshot が単独で占める容量を示します。"},
		{"they still share with the main worktrees are excluded, so it sums with the", "main worktree と共有している block は除外するため、"},
		{"column of wx slots and stays below what du reports. Paths outside the", "wx slots の列と合計でき、du より小さくなります。"},
		{"database are listed separately as Unmanaged. --verbose adds the full allocated", "database 外の path は Unmanaged として別に表示します。--verbose では割当全体と"},
		{"size and the shared part behind it.", "共有部分も追加表示します。"},
		{"Run read-only checks for configuration, storage, Git, launchd, hooks, and recovery data.", "設定、storage、Git、launchd、hook、復旧データを読み取り専用で検査します。"},
		{"Print one line when nothing is wrong; otherwise report each problem with its target, cause, and action.", "問題がなければ1行、問題があれば対象・原因・対処とともに報告します。"},
		{"Exit 0 when only passing and informational results remain, 1 when a problem or an unfinished check remains.", "正常または情報だけなら終了コード0、問題または未完了の検査があれば1で終了します。"},
		{"Without --probe no worktree is created, so a workspace that prepares a worktree", "--probe なしでは worktree を作成しないため、worktree の準備が必要な workspace が"},
		{"wx cannot actually work in is not detected. With --probe wx leases one workspace", "実際には動作できないことを検出できません。--probe では workspace を1つずつ貸し出し、"},
		{"at a time, waits for EARLY READY and then FULL READY, and inspects what it got:", "EARLY READY、続いて FULL READY まで待ち、取得した内容を検査します。"},
		{"--probe first retires the standby worktrees waiting for each workspace so what", "--probe は各 workspace の待機中 standby worktree を先に退役させるため、"},
		{"it measures is a cold start. They are reclaimed by normal GC and replenished", "cold start を計測します。これらは通常の GC で回収され、その後補充されます。"},
		{"Options:", "オプション:"},
		{"report candidates without removing them", "削除せず候補を報告"},
		{"report what would be deleted, changing nothing", "削除対象を報告し、何も変更しない"},
		{"report the targets and the reasons wx cannot process some of", "対象と、wx が処理できない理由を報告し、"},
		{"them, changing nothing", "何も変更しない"},
		{"delete refs whose contents cannot be proven safe to lose,", "失って安全だと証明できない ref を削除し、"},
		{"discarding the work they hold", "保持している作業を破棄"},
		{"ask sessions in use to stop, then delete what stopped, standby", "使用中 session に停止を求め、停止したものを削除（standby"},
		{"worktrees included", "worktree を含む）"},
		{"delete standby worktrees too", "standby worktree も削除"},
		{"return the lease without saving unfinished work, and remove the", "未保存の作業を保存せず貸出を返却し、"},
		{"worktree instead of keeping it for retention.ended_worktree", "worktree を retention.ended_worktree に残さず削除"},
		{"print the session id and path as JSON", "session id と path を JSON で表示"},
		{"print machine-readable JSON with every result regardless of --verbose", "--verbose に関係なく全結果を機械可読 JSON で表示"},
		{"show detailed status instead of the summary", "要約の代わりに詳細な状態を表示"},
		{"show passing checks, informational results, and extra diagnostics", "成功した検査、情報、追加の診断を表示"},
		{"prepare a worktree in every registered workspace and check it", "登録済み各 workspace で worktree を準備して検査"},
		{"run the daemon, or report that it is already running", "daemon を起動（すでに起動中ならその旨を表示）"},
		{"exit the running daemon, leaving the LaunchAgent registered", "稼働中の daemon を終了（LaunchAgent は登録したまま）"},
		{"replace the running daemon so a new wx binary takes effect", "稼働中の daemon を置き換え、新しい wx binary を有効化"},
		{"write the LaunchAgent plist and load it", "LaunchAgent plist を書き込み読み込む"},
		{"unload the LaunchAgent and remove its plist", "LaunchAgent を解除し plist を削除"},
		{"Run retention cleanup without deleting unarchived workspace data.", "未アーカイブの workspace データを削除せず保持期間の整理を実行します。"},
		{"Delete the recovery refs that the current database no longer explains.", "現在の database が説明できない復旧 ref を削除します。"},
		{"Delete the worktrees wx manages without waiting for their retention period.", "保持期間を待たずに wx の管理 worktree を削除します。"},
		{"Resume automatic standby worktree replenishment for a workspace after wx", "wx が停止した workspace の standby worktree 自動補充を再開し、"},
		{"stopped it, then retry replenishment in the current generation.", "現在の世代で補充を再試行します。"},
		{"Show effective configuration, or atomically update one supported scalar key or list.", "実効設定を表示するか、対応する scalar key または list を1件 atomically 更新します。"},
		{"Use --describe to show a key's type, scopes, choices, purpose, and impact.", "--describe で key の型、scope、選択肢、目的、影響を表示します。"},
		{"Restore an archived wx session into a new managed workspace.", "アーカイブ済み wx session を新しい管理 workspace へ復元します。"},
		{"With --fresh, keep the conversation but build the worktree from the current base.", "--fresh では会話を保持し、現在の base から worktree を作成します。"},
		{"Use --branch with --fresh to choose the detached base.", "--fresh と --branch で detached base を選択します。"},
		{"Open a shell in a managed wx workspace. This is the way to work in an isolated", "管理された wx workspace で shell を開きます。agent を起動せず隔離された"},
		{"worktree yourself, without launching an agent.", "worktree で自分で作業するためのコマンドです。"},
		{"The workspace is prepared the same way it is for wx claude and wx codex, and it", "workspace は wx claude / wx codex と同じ方法で準備され、"},
		{"is returned when the shell exits: unfinished work is saved first, and the", "shell 終了時に返却されます。未完了の作業は先に保存され、"},
		{"Run one command in a managed wx workspace and exit with its status. Use it to", "管理された wx workspace でコマンドを1つ実行し、その終了 status で終了します。"},
		{"run a build or a test suite against an isolated", "隔離された workspace で build や test suite を実行するために使います。"},
		{"Lease a managed wx workspace and print its path on one line. Nothing is", "管理された wx workspace を貸し出し、path を1行で表示します。何も"},
		{"started in it, so this is how an agent prepares a worktree to hand to a", "起動しないため、agent が SubAgent に渡す worktree を準備する方法です。"},
		{"The lease does NOT follow the process that asked for it: wx new sends no", "lease は要求元の process には追従しません。wx new は heartbeat を送らないため、"},
		{"heartbeat, so the workspace is not reclaimed when the caller exits. It is", "caller 終了時に workspace は回収されません。"},
		{"returned when the wx session that ran wx new ends, when wx release <id> is\nrun, or when lease.ttl has passed, whichever comes first.", "wx new を実行した wx session の終了、wx release <id> の実行、lease.ttl の経過のいずれかで返却されます。"},
		{"Without one of the first two, the workspace stays leased until that deadline.", "前の2つを行わない場合、workspace はその期限まで貸出中のままです。"},
		{"afterwards, so work written after the deadline is not saved. Return the lease\nwith wx release <id> when the SubAgent is done rather than relying on the\ndeadline.", "期限後に書き込んだ作業は保存されません。期限に頼らず、SubAgent が終わったら wx release <id> で返却してください。"},
		{"SubAgent.", "SubAgent。"},
		{"Expiry saves before it returns the lease, and nothing edits the worktree", "期限切れでは lease を返す前に保存し、その後 worktree は編集しません。"},
		{"The session id to pass to wx release and wx shell --resume is shown by wx", "wx release と wx shell --resume に渡す session id は wx slots または"},
		{"slots, or by --json here.", "ここで --json を指定すると表示されます。"},
		{"Return a workspace leased by wx new without waiting for lease.ttl. Unfinished", "wx new で貸し出した workspace を lease.ttl を待たずに返却します。未完了の"},
		{"work is saved first, and the worktree stays for retention.ended_worktree so", "作業は先に保存し、worktree は retention.ended_worktree の間残るため、"},
		{"List managed wx slots with their repository, session, copy mode, and disk usage.", "管理 wx slot の repository、session、copy mode、ディスク使用量を一覧表示します。"},
		{"Measure how long the current workspace takes to become usable, and where that", "現在の workspace が使えるようになるまでの時間と、その内訳を計測します。"},
		{"Discard the recovery state of the sessions whose recovery refs are gone from", "復旧 ref が source repository から消えた session の復旧状態を破棄し、"},
		{"the source repository, so that workspace stops being reported by wx doctor and", "wx doctor の報告対象から workspace を外して"},
		{"can be forgotten.", "管理を解除できるようにします。"},
		{"Forget an inactive workspace after all managed slots are safely archived.", "管理 slot をすべて安全にアーカイブした後、非アクティブな workspace の管理を解除します。"},
		{"Change whether the daemon is running, or install and remove the per-user", "daemon の稼働状態を変更するか、ユーザー単位の"},
		{"LaunchAgent that starts it at login.", "login 時に起動する LaunchAgent をインストール・削除します。"},
		{"Walk through what wx needs to run on its own and apply the choices. Each item", "wx が単独で動くために必要な項目を確認し、選択を適用します。各項目は"},
		{"is offered with the choices its current state allows, so running setup again", "現在の状態で選べる操作だけを提示するため、setup を再実行しても"},
		{"after a completed setup changes nothing.", "完了済みの設定は変わりません。"},
		{"Run a wx agent hook event read from stdin. Invoked by agent hook", "stdin から wx agent hook event を読み取って実行します。agent hook の"},
		{"configuration, not normally run directly.", "設定から呼び出され、通常は直接実行しません。"},
	}
	// 上の置換で分割された段落の残りも、機械的な識別子を変えずに翻訳する。
	// help 本文は既存の改行を契約として保つため、行をまとめた長めの断片を使う。
	remainder := []struct{ en, ja string }{
		// 一部の help は呼出し元が先頭の空白を整形するため、段落全体ではなく
		// 見出し・オプション行を単独でも置き換える。
		{"Agent directories (agent.add_dir):", "Agent directory（agent.add_dir）:"},
		{"pass every repository directory under the agent's working directory to --add-dir (default)", "agent の working directory 配下にある全 repository directory を --add-dir に渡す（既定）"},
		{"pass them only when the agent runs in a wx worktree", "agent が wx worktree で動く場合だけ渡す"},
		{"never pass them", "渡さない"},
		{"A workspace with several repositories puts the agent's working directory at the\nparent of those repositories, so their .claude/skills and other agent assets are\nonly loaded when the directories are passed with --add-dir. A workspace that is a\nsingle repository has nothing to pass. Directories you pass yourself are kept.", "複数 repository の workspace では agent の working directory がそれらの親になるため、--add-dir で渡した場合だけ .claude/skills などを読み込みます。単一 repository の workspace には渡すものがありません。自分で渡した directory は保持します。"},
		{"The workspace path is shown by wx status when standby replenishment is stopped.", "standby 補充が停止した場合、workspace path は wx status で確認できます。"},
		{"run a build or a test suite against an isolated worktree without disturbing the\ncurrent one.", "隔離された worktree で build や test suite を、現在の状態を乱さず実行します。"},
		{"wx shell --resume <id> can reopen it.", "wx shell --resume <id> で再開できます。"},
		{"time goes. Each run leases a workspace the way wx new does, waits for EARLY\nREADY and then for FULL READY, prints the breakdown the daemon recorded for that\npreparation, and returns the lease without saving it.", "内訳を計測します。各 run は wx new と同じように workspace を貸し出し、EARLY READY、続いて FULL READY を待ち、daemon が記録した準備の内訳を表示して保存せず返却します。"},
		{"  --runs <n>                     measure this many leases (default 1)", "  --runs <n>                     この回数だけ lease を計測（既定 1）"},
		{"  --branch <branch|repo=branch>  choose a detached base (repeatable)", "  --branch <branch|repo=branch>  detached base を選択（複数指定可）"},
		{"  --sweep                        compare copy_mode=copy and cow_min_size_kib\n                                 0/4/8/16/32/64/128", "  --sweep                        copy_mode=copy と cow_min_size_kib の\n                                 0/4/8/16/32/64/128 を比較"},
		{"  --config <key=value,...>       compare this preparation setting; keys are\n                                 copy_mode and cow_min_size_kib (repeatable)", "  --config <key=value,...>       準備設定を比較（key は copy_mode と\n                                 cow_min_size_kib、複数指定可）"},
		{"  --reuse                        keep standby worktrees and measure what the\n                                 pool returns", "  --reuse                        standby worktree を保持し、pool の返却を計測"},
		{"  --all   also list sessions that no longer hold a slot", "  --all   slot を保持していない session も一覧表示"},
		{"  --dry-run  list what would be discarded without changing anything", "  --dry-run  変更せず破棄対象を一覧表示"},
		{"submodules the index records but whose directory stayed empty, tracked files\nalready modified in a worktree wx just prepared, output a post-checkout hook\nwrote while still exiting 0, and whether the copy shared blocks with the main\nworktrees. It also reports how long each workspace took and what the prepared\nslot occupies per repository. Every probed workspace is returned without saving\nit, as wx release --discard does.", "index が記録しているのにディレクトリが空のままの submodule、wx が準備した worktree ですでに変更された tracked file、終了コード0のまま post-checkout hook が書き込んだ出力、copy が main worktree と block を共有したかを検査します。workspace ごとの所要時間と、準備した slot が repository ごとに占める容量も表示します。検査した workspace は wx release --discard と同じく保存せず返却します。"},
		{"afterwards, so starting a session in a probed workspace is slower until then.\nIt runs one workspace at a time and takes as long as preparing every registered\nworkspace does. Interrupting it skips the return of the workspace being probed,\nwhich then stays leased until the wx session that ran the command ends, or until\nlease.ttl.", "その後に補充されるため、検査した workspace で session を始めると補充が終わるまで遅くなります。1 workspace ずつ実行し、登録済み全 workspace の準備と同じだけ時間がかかります。中断すると検査中の workspace は返却されず、コマンドを実行した wx session の終了または lease.ttl まで貸出中のままです。"},
		{"with every result regardless of --verbose", "--verbose に関係なく全結果を"},
		{"These\nare left behind when the state database is rebuilt, and the daemon reports\nthem once per repository.", "state database の再作成時に残され、daemon が repository ごとに一度報告する ref です。"},
		{"By default wx deletes only the refs it can prove are safe to lose: every\nobject they reach is also reachable from another ref, so nothing is lost. A\nref that keeps objects of its own is left alone and reported with the number\nof objects that would become unreachable.", "既定では、失って安全だと証明できる ref だけを wx が削除します。ref が到達する全 object が別の ref からも到達できるため、作業は失われません。固有の object を保持する ref は残し、到達不能になる object 数を報告します。"},
		{"With --all those refs are deleted too, and the work saved in them is lost for\ngood. Refs the current database still expects are never touched, with or\nwithout --all.", "--all ではそれらの ref も削除され、保存された作業は完全に失われます。現在の database が必要とする ref は --all の有無にかかわらず変更しません。"},
		{"Exit status is 0 when the run finished, 1 when something failed, and 2 for an\nargument error. Keeping refs that could not be proven safe is not a failure.", "実行完了時は終了コード0、失敗時は1、引数エラー時は2です。安全に削除できると証明できない ref を残しても失敗ではありません。"},
		{"Work is saved first: recovery data, session history, and workspace\nregistrations stay, and their retention follows the existing settings.", "最初に作業を保存します。復旧データ、session 履歴、workspace 登録は残り、保持期間は既存設定に従います。"},
		{"Standby worktrees waiting for the next session are kept unless --standby or\n--all is given. Once they are deleted, wx does not replenish them until the\naffected workspace is used again.", "次の session を待つ standby worktree は --standby または --all を指定しない限り残します。削除すると、その workspace が再び使われるまで wx は補充しません。"},
		{"Without --all, sessions that are in use are left alone. With --all, wx asks\nthose sessions to stop, waits up to 30s for each of them, and deletes only the\nones that stopped; nothing is killed.", "--all なしでは使用中の session を残します。--all では session に停止を求め、各 session を最大30秒待って、停止したものだけを削除します。強制終了はしません。"},
		{"Quarantined slots are deleted in every mode, without waiting out\nretention.quarantined. Database registration authorizes deletion, including slots with\nmissing identity records or changed markers, locks, and HEAD. Unregistered\ndirectories are left alone. With --discard, unfinished work is deleted without\nrequiring a successful snapshot. Sessions in use still require --all.", "隔離された slot は全モードで retention.quarantined を待たずに削除します。database の登録が削除を認可するため、identity record、marker、lock、HEAD が変わった slot や欠落した slot も対象です。未登録の directory は残します。--discard では snapshot 成功を待たず未完了の作業を削除します。使用中 session には引き続き --all が必要です。"},
		{"The command waits for every target to finish. Interrupting it does not stop\nthe daemon, and running it again rejoins the clear already in progress. While\na clear runs, new sessions and resumes are refused.", "全対象の完了を待ちます。中断しても daemon は停止せず、再実行すると進行中の clear に再参加します。clear 中は新しい session と resume を拒否します。"},
		{"Exit status is 0 when every target succeeded or there was nothing to do, 1\nwhen something failed, was quarantined, or did not finish, and 2 for an\nargument error. Keeping sessions in use or standby worktrees is not a failure.", "全対象が成功または対象なしなら終了コード0、失敗・隔離・未完了があれば1、引数エラーなら2です。使用中 session や standby worktree を残しても失敗ではありません。"},
		{"Replenishment\nalso resumes on its own once wx claude or wx codex succeeds for the workspace.\nStandby worktrees that failed to prepare still fill the warm count, so\nretry-standby schedules them for removal before it replenishes.\nQuarantined worktrees are left untouched; wx gc deletes them once\nretention.quarantined has passed, and wx clear deletes them right away.", "wx claude または wx codex が workspace で成功すると補充も自動的に再開します。準備に失敗した standby worktree も warm count を占めるため、retry-standby は補充前に削除を予約します。隔離された worktree は変更せず、retention.quarantined 経過後は wx gc が、直ちに削除する場合は wx clear が処理します。"},
		{"--all  resume every workspace whose replenishment stopped and whose\n         configuration still replenishes", "--all  補充が停止し、設定上も補充対象である全 workspace を再開"},
		{"--add and --remove take a list key: discovery.exclude, readiness.early_paths, or\nsessions.paths.<claude|codex>.sessions. --reset takes any of those list keys or any\nscalar key wx config lists; it drops the key from the config file so the built-in\ndefault applies again.", "--add と --remove は list key（discovery.exclude、readiness.early_paths、sessions.paths.<claude|codex>.sessions）を受け取ります。--reset はこれらの list key または wx config が一覧する scalar key に使え、設定ファイルから key を削除して組み込みの既定値へ戻します。"},
		{"With --workspace or --repository, show every key that scope can override with its\neffective value and source, or update one of them. Both paths may be relative or a\nrepository subdirectory; linked worktrees resolve to the repository's main worktree.\n--repository rejects a path outside a Git repository, while --workspace keeps a\nnon-repository directory as the workspace root. The two flags cannot be combined.", "--workspace または --repository では、その scope が上書きできる全 key の実効値と出典を表示するか、1件を更新します。path は相対指定や repository の subdirectory でもよく、linked worktree は repository の main worktree に解決します。--repository は Git repository 外を拒否し、--workspace は非 repository directory も workspace root として扱います。2つの flag は併用できません。"},
		{"A workspace overrides worktree, copy, link, reuse_standby, submodules, warm_count,\nagent.add_dir, retention.hot_standby, retention.ended_worktree, discovery.max_depth\nand discovery.exclude. warm_count 0 disables replenishment, and so does\nretention.hot_standby 0; reducing the count lets normal GC reclaim unused standby\nslots. reuse_standby controls whether an older READY standby is updated at lease\ntime; the default is true, and false preserves exact-match cold-start behavior.", "workspace では worktree、copy、link、reuse_standby、submodules、warm_count、agent.add_dir、retention.hot_standby、retention.ended_worktree、discovery.max_depth、discovery.exclude を上書きできます。warm_count 0 または retention.hot_standby 0 では補充を無効にし、count を減らすと通常の GC が未使用 standby slot を回収できます。reuse_standby は貸出時に古い READY standby を更新するかを制御します。既定値は true、false では完全一致の cold-start 動作を保ちます。"},
		{"A repository overrides default_branch, dir_name, dir_source, cow_min_size_kib,\nprepare.command, prepare.timeout, prepare.version, includes.default_agent_rules,\nreadiness.mode, readiness.early_paths, readiness.timeout and storage.copy_mode. A\nlease leasing several repositories waits in full mode if any of them asks for it,\nand uses the longest readiness.timeout among them.", "repository では default_branch、dir_name、dir_source、cow_min_size_kib、prepare.command、prepare.timeout、prepare.version、includes.default_agent_rules、readiness.mode、readiness.early_paths、readiness.timeout、storage.copy_mode を上書きできます。複数 repository の貸出では、1つでも full を要求すれば full mode で待ち、最長の readiness.timeout を使います。"},
		{"A list key set on a scope replaces the global list instead of extending it. The\nfirst --add copies the global list as it stands right then, so later changes to the\nglobal value no longer reach that scope; --reset drops the scope list and restores\nthe global one. A repository readiness.early_paths does not apply to the shared\nworkspace root stage of a multi-repository workspace, which keeps using the global\nlist. --reset on any scalar key restores the inherited value.", "scope で設定した list key は global list を拡張せず置き換えます。最初の --add はその時点の global list をコピーするため、以後の global 変更は scope に届きません。--reset は scope list を解除して global に戻します。repository の readiness.early_paths は複数 repository workspace の共有 workspace root 段階には適用せず、global list を使います。scalar key の --reset は継承値に戻します。"},
		{"Submodules (worktree.submodules, default true):\nLinked worktrees resolve a submodule's gitdir per worktree, so they cannot reuse\nthe main repository's .git/modules/<name>. wx therefore clones each submodule\nfrom that local module, which stays offline and shares objects as hardlinks. A\nsubmodule is skipped with a warning, leaving the empty gitlink directory, when\nthe local module is absent, when it lacks the gitlink commit, or when no\nupstream url is available; preparation still succeeds. After the clone wx\nrestores the submodule's origin to the upstream url so git push does not write\ninto the main repository's .git/modules. Changing this stops reuse of READY\nstandby worktrees prepared under the previous value.\nCommits made inside a worktree's submodule are lost when the slot is deleted,\nbecause snapshots can only record the gitlink. Push them before releasing.", "Submodules（worktree.submodules、既定 true）:\nlinked worktree は worktree ごとに submodule の gitdir を解決するため、main repository の .git/modules/<name> を再利用できません。wx は各 submodule をその local module から clone し、offline のまま object を hardlink で共有します。local module がない、gitlink commit がない、upstream url がない場合は warning を出して空の gitlink directory を残し、準備は成功します。clone 後は submodule の origin を upstream url に戻すため、git push が main repository の .git/modules へ書き込みません。この設定を変えると、以前の値で準備した READY standby は再利用されません。\nworktree 内の submodule で作った commit は、snapshot が gitlink しか記録できないため slot 削除時に失われます。返却前に push してください。"},
		{"Agent directories (agent.add_dir):\n  always     pass every repository directory under the agent's working directory to --add-dir (default)\n  worktree   pass them only when the agent runs in a wx worktree\n  off        never pass them\nA workspace with several repositories puts the agent's working directory at the\nparent of those repositories, so their .claude/skills and other agent assets are\nonly loaded when the directories are passed with --add-dir. A workspace that is a\nsingle repository has nothing to pass. Directories you pass yourself are kept.", "Agent directory（agent.add_dir）:\n  always     agent の working directory 配下の全 repository directory を --add-dir に渡す（既定）\n  worktree   agent が wx worktree で動く場合だけ渡す\n  off        渡さない\n複数 repository の workspace では agent の working directory がそれらの親になるため、--add-dir で渡した場合だけ .claude/skills などを読み込みます。単一 repository の workspace には渡すものがありません。自分で渡した directory は保持します。"},
		{"Copy mode (storage.copy_mode):\n  auto  share identical checked-out files with APFS CoW; fall back to copies, but quarantine when ownership is unprovable (default)\n  cow   fail preparation if CoW fails\n  copy  keep normal Git checkout files\nstorage.cow_min_size_kib sets the smallest file CoW shares, in KiB (default 16).\nFiles below it keep their normal checkout copy; 0 shares every eligible file, and\na larger value trades disk savings for less per-file work. Changing it stops\nreuse of READY standby worktrees prepared under the previous value.\nA repository can override the limit and the copy mode with wx config --repository,\nsince the best values depend on the repository's file size distribution and\ncheckout. Only the repositories whose effective values changed lose the reuse of\ntheir READY standby worktrees. A --config override on a single lease wins over\nboth the repository entry and the global setting.", "Copy mode（storage.copy_mode）:\n  auto  同一 checkout file を APFS CoW で共有し、失敗時は copy へ戻す。ただし所有権を証明できない場合は隔離する（既定）\n  cow   CoW 失敗時は準備を失敗にする\n  copy  通常の Git checkout file を保持\nstorage.cow_min_size_kib は CoW で共有する最小 file サイズ（KiB、既定16）です。これより小さい file は通常の copy のままです。0 では対象 file をすべて共有し、大きくすると file ごとの処理を減らす代わりに容量を使います。変更すると以前の値で準備した READY standby は再利用されません。\nrepository は wx config --repository で limit と copy mode を上書きできます。最適値は repository の file 分布と checkout に依存します。実効値が変わった repository の READY standby だけが再利用されなくなります。1回の lease の --config 上書きは repository と global の両方より優先します。"},
		{"Workspace root files (multi-repository workspaces):\nThe root itself has no checkout, so only these paths reach a slot: AGENTS.md,\nAGENTS.local.md, CLAUDE.md, CLAUDE.local.md, the agent asset directories\n.claude/skills, .claude/agents, .claude/commands, .claude/hooks and\n.codex/prompts, and whatever the rules below add. Missing paths are skipped.\nAdd more with a .worktreeinclude (copied, glob patterns, no match is fine) and a\n.worktreelink (symlinked back to the root, literal paths that must exist) in the\nroot itself, or with wx config --workspace <path> copy --add and link --add. A\ncopy path set in the config must exist or preparation fails. A path cannot be both\ncopied and linked. The root manifests apply to multi-repository workspaces only;\ninside a repository the same file names keep their repository meaning.", "Workspace root files（multi-repository workspace）:\nroot 自体には checkout がないため、slot へ届くのは AGENTS.md、AGENTS.local.md、CLAUDE.md、CLAUDE.local.md、agent asset directory（.claude/skills、.claude/agents、.claude/commands、.claude/hooks、.codex/prompts）と、下記の規則が追加する path だけです。存在しない path は省略します。\nroot の .worktreeinclude（copy、glob は no match 可）と .worktreelink（root へ symlink、存在必須の literal path）、または wx config --workspace <path> copy --add / link --add で追加できます。設定した copy path は存在しないと準備に失敗します。1つの path を copy と link の両方にはできません。root manifest は multi-repository workspace にだけ適用し、repository 内では同じ file 名が repository 本来の意味を保ちます。"},
		{"Readiness (readiness.mode):\n  early  wait for Git registration and startup files, then launch with readiness hooks (default)\n  full   wait for checkout, includes, links, prepare commands, and final validation\nWithout readiness hooks, both modes wait for full preparation. Resume and shell/run/new always wait for full preparation.\nUse full when checkout hooks or prepare commands generate or update startup settings.\nreadiness.early_paths adds literal repository-relative paths to the startup list;\nedit it with wx config readiness.early_paths --add/--remove/--reset, or per\nrepository with wx config --repository <path> readiness.early_paths --add.\nDirectories include their descendants; no glob patterns are expanded. Only paths\nalready scheduled by checkout or copy/link rules are materialized. Workspace roots\nuse the same selection. Absolute paths, escapes, the root itself, and .git are rejected.\nreadiness.progress prints the preparation progress to stderr while a lease waits\n(default true); set it to false to wait without any output. A non-terminal stderr\nnever gets the progress lines, whatever the setting says.", "Readiness（readiness.mode）:\n  early  Git 登録と startup file を待ち、readiness hook で起動（既定）\n  full   checkout、include、link、prepare command、最終検証まで待つ\nreadiness hook がない場合は両 mode とも full preparation を待ちます。resume と shell/run/new は常に full preparation を待ちます。checkout hook や prepare command が startup 設定を生成・更新する場合は full を使います。\nreadiness.early_paths は repository 相対の literal path を startup list に追加します。wx config readiness.early_paths --add/--remove/--reset、または repository ごとの wx config --repository <path> readiness.early_paths --add で編集します。\ndirectory は子孫を含み、glob は展開しません。checkout または copy/link 規則ですでに予定された path だけが materialize されます。workspace root も同じ選択を使います。absolute path、escape、root 自体、.git は拒否します。\nreadiness.progress は lease 待機中の準備進捗を stderr に表示します（既定 true）。false にすると無表示で待ちます。non-terminal stderr には設定にかかわらず進捗行を出しません。"},
		{"Restore an archived wx session into a new managed workspace.\nWith --fresh, keep the conversation but build the worktree from the current base.\nUse --branch with --fresh to choose the detached base.", "アーカイブ済み wx session を新しい管理 workspace へ復元します。\n--fresh では会話を保持し、現在の base から worktree を作成します。\n--fresh と --branch で detached base を選択します。"},
		{"The workspace is prepared the same way it is for wx claude and wx codex, and it\nis returned when the shell exits: unfinished work is saved first, and the\nworktree stays for retention.ended_worktree so wx shell --resume <id> brings it\nback. wx clear --all can also ask the shell to stop.", "workspace は wx claude / wx codex と同じ方法で準備され、shell 終了時に返却されます。未完了の作業は先に保存され、worktree は retention.ended_worktree の間残るため wx shell --resume <id> で戻せます。wx clear --all は shell に停止を求めることもできます。"},
		{"--resume only accepts sessions leased by wx shell, wx run, or wx new. Resume a\nsession started by an agent with wx resume <wx-session-id> [claude|codex].", "--resume は wx shell、wx run、wx new が貸し出した session だけを受け付けます。agent が開始した session は wx resume <wx-session-id> [claude|codex] で再開します。"},
		{"The worktree is detached, as it is for every wx workspace; --branch only\nchooses the base commit, and creating or pushing a branch is left to you. The\nshell comes from lease.shell, then $SHELL, then /bin/sh.", "worktree は全 wx workspace と同じく detached です。--branch は base commit だけを選び、branch の作成や push は利用者が行います。shell は lease.shell、次に $SHELL、最後に /bin/sh から選びます。"},
		{"  --branch <branch|repo=branch>  choose a detached base (repeatable)", "  --branch <branch|repo=branch>  detached base を選択（複数指定可）"},
		{"  --resume <wx-session-id>       restore the worktree of an earlier wx session", "  --resume <wx-session-id>       以前の wx session の worktree を復元"},
		{"The workspace is returned when the command exits, and the same saving and\nretention as wx shell apply, so wx shell --resume <id> can reopen what the\ncommand left behind.", "command 終了時に workspace を返却し、保存と retention は wx shell と同じです。wx shell --resume <id> で command が残した内容を再開できます。"},
		{"--resume only accepts sessions leased by wx shell, wx run, or wx new. Resume a\nsession started by an agent with wx resume <wx-session-id> [claude|codex].", "--resume は wx shell、wx run、wx new が貸し出した session だけを受け付けます。agent が開始した session は wx resume <wx-session-id> [claude|codex] で再開します。"},
		{"The command runs at the top of the workspace even when wx run is invoked from a\nsubdirectory; the original directory is passed as WX_SOURCE_CWD.", "wx run を subdirectory から呼んでも command は workspace の top で実行し、元の directory は WX_SOURCE_CWD として渡します。"},
		{"The lease does NOT follow the process that asked for it: wx new sends no\nheartbeat, so the workspace is not reclaimed when the caller exits. It is\nreturned when the wx session that ran wx new ends, when wx release <id> is\nrun, or when lease.ttl has passed, whichever comes first.\nWithout one of the first two, the workspace stays leased until that deadline.", "lease は要求した process には追従しません。wx new は heartbeat を送らないため caller 終了時には回収されません。wx new を実行した wx session の終了、wx release <id>、lease.ttl 経過のうち早い時点で返却されます。最初の2つがなければ期限まで貸出中です。"},
		{"Expiry saves before it returns the lease, and nothing edits the worktree\nafterwards, so work written after the deadline is not saved. Return the lease\nwith wx release <id> when the SubAgent is done rather than relying on the\ndeadline.", "期限切れでは lease を返す前に保存し、その後 worktree は編集しません。期限後に書いた作業は保存されないため、期限に頼らず SubAgent 完了時に wx release <id> で返却してください。"},
		{"List managed wx slots with their repository, session, copy mode, and disk usage.\nAGENT names what holds the slot: claude or codex for an agent, and wx-shell,\nwx-run, or wx-path for a workspace leased by wx shell, wx run, or wx new.\nEvery slot that still occupies disk is listed, so the SIZE(MB) column covers the\nsame slots as the Disk line of wx status. REPO names the source repositories the\nslot was prepared from; a multi-repo workspace lists them separated by commas.", "管理 wx slot の repository、session、copy mode、ディスク使用量を一覧表示します。AGENT は slot の保持者を示し、agent なら claude/codex、wx shell・wx run・wx new の貸出なら wx-shell/wx-run/wx-path です。disk を占有する全 slot を一覧するため SIZE(MB) は wx status の Disk 行と同じ対象です。REPO は準備元の source repository で、multi-repo workspace は comma 区切りです。"},
		{"SIZE(MB) is what the slot occupies on its own, rounded up: blocks it still\nshares with the main worktree are excluded, so the column sums without double\ncounting. COPY reports what the slot looks like now: cow once any file still\nshares blocks with the main worktree, copy otherwise.", "SIZE(MB) は slot 単独の占有量を切り上げた値です。main worktree と共有する block は除外するため二重計上せず合計できます。COPY は現在の状態を示し、1 file でも block を共有していれば cow、そうでなければ copy です。"},
		{"The daemon measures a slot when its preparation finishes and re-measures every\nroot periodically; wx slots only reads those results, never measures on demand.\nRows still waiting for the first measurement show pending, and platforms that\ncannot compare blocks show unsupported. --json carries the measurement time.\n\n--json adds the lease kind, the time the lease expires, and the wx session\nthat asked for a wx new lease.", "daemon は準備完了時に slot を計測し、その後も root ごとに定期計測します。wx slots は結果を読むだけで、要求時の計測はしません。初回計測待ちの行は pending、block を比較できない platform は unsupported と表示します。--json には計測時刻を含めます。\n\n--json では lease kind、lease の期限、wx new の貸出を要求した wx session も追加します。"},
		{"Measure how long the current workspace takes to become usable, and where that\ntime goes. Each run leases a workspace the way wx new does, waits for EARLY\nREADY and then for FULL READY, prints the breakdown the daemon recorded for that\npreparation, and returns the lease without saving it.", "現在の workspace が使えるまでの時間と内訳を計測します。各 run は wx new と同じように workspace を貸し出し、EARLY READY、続いて FULL READY を待ち、daemon が記録した準備の内訳を表示して保存せず返却します。"},
		{"EARLY READY is the point an agent can start: Git registration and the startup\nfiles are in place. FULL READY adds the remaining checkout, the includes and\nlinks, the prepare command, CoW sharing, and the final validation. Both are\nmeasured from the lease request, so they include the time the request waited\nfor a job slot.", "EARLY READY は agent を開始できる時点で、Git 登録と startup file が揃っています。FULL READY では残りの checkout、include/link、prepare command、CoW sharing、最終検証も完了します。どちらも lease 要求から計測するため、job slot 待ち時間を含みます。"},
		{"By default the standby worktrees waiting for the current workspace are retired\nfirst, so what is measured is a cold start. They are reclaimed by normal GC and\nreplenished afterwards. With --reuse nothing is retired and the run measures\nwhatever the pool returns, which is reported as warm when a prepared slot was\nhanded over with no preparation to measure.", "既定では現在の workspace を待つ standby worktree を先に退役させ、cold start を計測します。通常の GC で回収した後に補充します。--reuse では退役させず pool が返したものを計測し、準備なしで渡された slot は warm と報告します。"},
		{"The breakdown comes from the running daemon and is not persisted, so restarting\nthe daemon between the preparation and the report loses it. Phase names that\ncontain a dot, such as cow.compare, are totals across the parallel workers of\nthat phase and can exceed the wall-clock time of the phase above them.", "内訳は稼働中 daemon から取得し永続化しないため、準備後・表示前に daemon を再起動すると失われます。cow.compare のように dot を含む phase 名は並列 worker の合計で、上位 phase の経過時間を超えることがあります。"},
		{"With --runs, wx waits for the saving, removal, and replenishment jobs of the\nprevious run to finish before measuring the next one, and prints the minimum,\nmedian, and maximum at the end. A single successful run has no distribution to\nsummarize, so its measured time is printed on its own, both in that summary and\nin the comparison table.", "--runs では前の run の保存・削除・補充 job が終わってから次を計測し、最後に最小・中央値・最大を表示します。成功 run が1件だけなら分布を作らず、その計測値を概要と比較表に単独表示します。"},
		{"--sweep measures the settings a search for the best CoW threshold usually\ncompares: copy_mode=copy as the baseline without CoW sharing, then\ncow_min_size_kib of 0, 4, 8, 16, 32, 64, and 128, each with the copy mode the\ndaemon is running with. It is the same measurement as passing those eight\nvalues as --config by hand, so the two options are rejected together.", "--sweep は最適な CoW threshold の探索で比較する設定を計測します。CoW なしの copy_mode=copy を基準に、cow_min_size_kib の 0、4、8、16、32、64、128 を daemon の copy mode ごとに測ります。8値を --config で指定する場合と同じため、2つの option は併用できません。"},
		{"Repeat --config to compare preparation settings of your own. Each --config is\none configuration to measure, written as copy_mode=<auto|cow|copy> and\ncow_min_size_kib=<n> separated by commas; the keys you leave out keep the value\nthe daemon is running with.", "--config を繰り返して独自の準備設定を比較できます。各 --config は1つの設定で、copy_mode=<auto|cow|copy> と cow_min_size_kib=<n> を comma 区切りで書きます。省略した key は daemon の実効値を使います。"},
		{"Every configuration is measured --runs times, and the comparison table adds the\ndisk each configuration left behind: the exclusive size is what the slot\noccupies after CoW sharing is discounted, taken from the same measurement wx\nslots reports. A configuration whose usage is not measured before the lease is\nreturned is reported as - and keeps its timings. A single run per configuration\nis one sample of a machine under changing load, so settings whose min and max\noverlap are separated by measuring again with a larger --runs. The settings\ntravel with the lease request and apply only to the slot prepared for it: wx\nneither reads nor writes your configuration file, and the daemon keeps\npreparing every other workspace with its own settings. Interrupting the command\ntherefore leaves no measurement setting behind. Slots prepared for a\nconfiguration are not handed to later leases or kept as standby, so each\nconfiguration is always measured as a cold start, and both --sweep and --config\nare rejected together with --reuse.", "各設定を --runs 回計測し、比較表には設定後の disk も追加します。exclusive size は CoW sharing を差し引いた slot 占有量で、wx slots と同じ計測値です。lease 返却前に usage を計測できなかった設定は - と表示し、時間は残します。設定ごとに1 run は負荷変動下の1サンプルなので、min/max が重なる設定は --runs を増やして再計測します。設定は lease 要求とともに送られ、その slot にだけ適用します。wx は設定ファイルを読み書きせず、他 workspace は独自設定で準備します。中断しても計測設定は残りません。設定用 slot は後続 lease や standby に回さないため常に cold start を測り、--sweep と --config は --reuse と併用できません。"},
		{"The measured workspace is returned without saving it, as wx release --discard\ndoes. Interrupting wx bench skips that return, and the workspace then stays\nleased until the wx session that ran the command ends, or until lease.ttl.", "計測した workspace は wx release --discard と同じく保存せず返却します。wx bench を中断すると返却を省略し、コマンドを実行した wx session の終了または lease.ttl まで貸出中のままです。"},
		{"Exit status is 0 when every run finished, 1 when one of them failed, and 2 for\nan argument error.", "全 run 完了時は終了コード0、1件でも失敗すれば1、引数エラーなら2です。"},
		{"A recovery ref disappears when the source repository is deleted and recreated\nat the same path. wx then quarantines the snapshots that recorded those refs,\nand they can no longer restore anything. This command deletes those snapshot\nrecords, ends their sessions, and deletes the worktrees of the slots they\nquarantined, unsaved work included. Sessions of other workspaces, and every\nsession that still has its refs, are left alone. Run --dry-run first to see\nthe sessions and worktree paths that would go.", "source repository を同じ path で削除・再作成すると recovery ref が消えます。wx はその ref を記録した snapshot を隔離し、復元できなくします。この command は snapshot record を削除し、session を終了し、隔離した slot の worktree（未保存の作業を含む）を削除します。他 workspace の session と、ref が残る session はそのままです。先に --dry-run で対象 session と worktree path を確認してください。"},
		{"A workspace whose sessions were quarantined because their recovery refs are\nmissing is refused until wx discard-recovery <workspace-path> discards that\nstate.", "recovery ref がないため session を隔離した workspace は、wx discard-recovery <workspace-path> でその状態を破棄するまで拒否されます。"},
		{"Change whether the daemon is running, or install and remove the per-user\nLaunchAgent that starts it at login.", "daemon の稼働状態を変更するか、login 時に起動するユーザー単位の LaunchAgent を登録・解除します。"},
		{"  start      run the daemon, or report that it is already running\n  stop       exit the running daemon, leaving the LaunchAgent registered\n  restart    replace the running daemon so a new wx binary takes effect\n  install    write the LaunchAgent plist and load it\n  uninstall  unload the LaunchAgent and remove its plist", "  start      daemon を起動（起動中ならその旨を表示）\n  stop       稼働中の daemon を終了（LaunchAgent は登録したまま）\n  restart    稼働中の daemon を置き換え、新しい wx binary を有効化\n  install    LaunchAgent plist を書き込み読み込む\n  uninstall  LaunchAgent を解除し plist を削除"},
		{"start, stop, and restart wait for the daemon to reach the requested state and\ngive up after 60s. The daemon acts only once it is idle, so none of them cuts\nshort a request or a job that is already running.", "start、stop、restart は daemon が要求状態になるまで待ち、60秒で打ち切ります。daemon は idle になってから動作するため、実行中の request や job を途中で切りません。"},
		{"  --foreground  with start, serve in this process instead of asking launchd\n                to start the daemon. This is how the LaunchAgent runs wx.", "  --foreground  start 時に launchd へ依頼せず、この process で daemon を提供\n                LaunchAgent が wx を実行する方式"},
		{"The items are the prerequisites, storage.worktree_root, the shell PATH entry,\nthe LaunchAgent, the Claude and Codex hook entries, and the daemon. wx owns the\nwx entries of the agent hook configuration: it writes a dedicated group\nper event and never touches the rest of the file.", "項目は prerequisite、storage.worktree_root、shell PATH entry、LaunchAgent、Claude/Codex hook entry、daemon です。wx は agent hook 設定の wx entry だけを管理し、event ごとに専用 group を書き、ファイルの他の部分には触れません。"},
		{"Cancelling stops the walk without undoing what was already applied; every item\nis idempotent, so running wx setup again finishes the rest.", "キャンセルすると適用済みを戻さず walk を停止します。各項目は冪等なので wx setup を再実行すれば残りを完了できます。"},
		{"--remove deletes what wx setup writes instead of asking: the wx hook entries,\nthe LaunchAgent, and the configuration file. Removing the LaunchAgent boots the\ndaemon out, so run anything that needs the daemon, such as wx clear, first. The\nshell startup file is left alone: wx cannot tell its own PATH line apart from\none you wrote or one your dotfile manager owns, so remove that line yourself.\nThe state database, the log directory, and the worktree root are kept too\nbecause they hold saved work and records; each is printed on a line starting\nwith \"leftover\" so a script can act on them. The uninstaller runs this.", "--remove は質問せず wx setup の書き込み（wx hook entry、LaunchAgent、設定ファイル）を削除します。LaunchAgent を解除すると daemon が停止するため、wx clear など daemon が必要な操作を先に実行してください。shell startup file は残します。自分の PATH 行や dotfile manager の行と区別できないため、利用者が削除してください。state database、log directory、worktree root も保存済み作業と記録を含むため残し、script が扱えるよう各 path を `leftover` で始まる行に表示します。uninstaller はこれを実行します。"},
		{"Exit status is 0 when the walk finished, 1 when an item could not be applied,\nthe walk was cancelled, or no terminal is attached, and 2 for an argument\nerror. --check reports differences with status 0. --update does not use the\nexit status to report a missing terminal: it names the items that need\nattention on stderr and exits 0. --remove needs no terminal, keeps going after\na failed item, and exits 1 when any item failed.", "walk 完了時の終了コードは0、適用できない項目・キャンセル・端末なしは1、引数エラーは2です。--check は差分があっても0です。--update は端末なしを終了コードで示さず、対応が必要な項目を stderr に出して0で終了します。--remove は端末不要で、失敗後も続行し、失敗があれば1で終了します。"},
		{"  --check   report the current state and change nothing; needs no terminal\n  --json    with --check, print machine-readable JSON\n  --update  offer only the items that no longer match what wx would write, and\n            print nothing when there are none. The installer runs this.\n  --remove  delete the configuration wx setup writes and report what was kept\n  --item    configure only this item (for example hooks.claude)\n  --action  apply one action offered for --item by wx setup --check\n  --value   value used by the manual action", "  --check   現在の状態を表示し、変更しない（端末不要）\n  --json    --check と併用し、機械可読 JSON を表示\n  --update  wx の書き込み内容と一致しない項目だけ提示し、なければ無表示。installer が実行\n  --remove  wx setup が書き込んだ設定を削除し、残したものを報告\n  --item    この項目だけを設定（例: hooks.claude）\n  --action  wx setup --check が提示した --item の action を適用\n  --value   manual action に使う値"},
		{"Run a wx agent hook event read from stdin. Invoked by agent hook\nconfiguration, not normally run directly.", "stdin から wx agent hook event を読み取って実行します。agent hook configuration から呼び出され、通常は直接実行しません。"},
		{"register or remove the LaunchAgent", "LaunchAgent を登録または削除"},
		{"delete selected worktrees without saving unfinished work", "未完了の作業を保存せず選択した worktree を削除"},
		{"delete selected worktrees without saving unfinished work", "未完了の作業を保存せず選択した worktree を削除"},
		{"worktree without disturbing the", "worktree を現在の"},
		{"can reopen it.", "再開できます。"},
		{"can reopen what the", "で残した内容を再開"},
		{"with every result regardless of --verbose", "--verbose に関係なく全結果を表示"},
	}
	for _, replacement := range remainder {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	for _, replacement := range replacements {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	// help の raw string はコマンドごとに改行位置が少し異なる。長い段落の
	// 一致に失敗しても、行単位で利用者向けの英文を残さないよう最後に補う。
	for _, replacement := range []struct{ en, ja string }{
		{"A workspace with several repositories puts the agent's working directory at the", "複数 repository の workspace では agent の working directory がそれらの親になります。"},
		{"parent of those repositories, so their .claude/skills and other agent assets are", "そのため .claude/skills などの agent asset は"},
		{"only loaded when the directories are passed with --add-dir. A workspace that is", "--add-dir で directory を渡した場合だけ読み込まれます。workspace が"},
		{"a single repository has nothing to pass. Directories you pass yourself are kept.", "単一 repository なら渡すものはありません。自分で渡した directory は保持します。"},
		{"Workspace root files (multi-repository workspaces):", "Workspace root files（multi-repository workspace）:"},
		{"The root itself has no checkout, so only these paths reach a slot: AGENTS.md,", "root 自体には checkout がないため、slot へ届く path は AGENTS.md、"},
		{"AGENTS.local.md, CLAUDE.md, CLAUDE.local.md, the agent asset directories", "AGENTS.local.md、CLAUDE.md、CLAUDE.local.md、agent asset directory"},
		{"the agent asset directories", "agent asset directory"},
		{".claude/skills, .claude/agents, .claude/commands, .claude/hooks and", ".claude/skills、.claude/agents、.claude/commands、.claude/hooks と"},
		{".codex/prompts, and whatever the rules below add. Missing paths are skipped.", ".codex/prompts と、下記の規則が追加する path です。存在しない path は省略します。"},
		{"Add more with a .worktreeinclude (copied, glob patterns, no match is fine) and a", ".worktreeinclude（copy、glob は no match 可）と"},
		{".worktreelink (symlinked back to the root, literal paths that must exist) in the", ".worktreelink（root へ symlink、存在必須の literal path）を"},
		{"root itself, or with wx config --workspace <path> copy --add and link --add. A", "root 自体に置くか、wx config --workspace <path> copy --add / link --add で追加します。"},
		{"copy path set in the config must exist or preparation fails. A path cannot be both copied and linked. The root manifests", "設定した copy path は存在しないと準備に失敗します。path は copy と link の両方にはできません。root manifest は"},
		{"apply to multi-repository workspaces only; inside a repository the same file", "multi-repository workspace にだけ適用し、repository 内では同じ file"},
		{"names keep their repository meaning.", "名が repository 本来の意味を保ちます。"},
		{"time goes. Each run leases a workspace the way wx new does, waits for EARLY", "内訳を計測します。各 run は wx new と同じように workspace を貸し出し、EARLY"},
		{"READY and then for FULL READY, prints the breakdown the daemon recorded for", "READY、続いて FULL READY を待ち、daemon が記録した準備の内訳を"},
		{"that preparation, and returns the lease without saving it.", "表示して保存せず lease を返却します。"},
		{"With --discard the work is not saved: the return schedules the worktree for", "--discard では作業を保存せず、返却時に worktree を"},
		{"removal instead, so nothing is left to reopen with wx shell --resume.", "削除対象にするため、wx shell --resume で再開するものは残りません。"},
		{"Only leases with no running process are returned this way. A workspace held by", "実行中 process がない lease だけをこの方法で返却します。workspace を保持する"},
		{"wx shell or wx run is returned when that shell or command exits, and one held", "wx shell / wx run は shell または command の終了時、保持する"},
		{"by an agent when that agent exits; wx clear --all asks either of them to stop.", "agent は agent 終了時に返却されます。wx clear --all はいずれにも停止を求めます。"},
	} {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	return text
}

func writeUsage(w io.Writer, render func(io.Writer), lang i18n.Language) {
	if lang != i18n.Japanese {
		render(w)
		return
	}
	var out bytes.Buffer
	render(&out)
	_, _ = io.WriteString(w, translateHelp(out.String(), lang))
}

// translateHumanOutput は固定ラベルだけを置き換える軽量な表示層である。
// payload の path・ID・状態値・外部コマンドの原文は変更しないため、JSON と
// 診断の可変値を同じ renderer から安全に再利用できる。
func translateHumanOutput(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	replacements := []struct{ en, ja string }{
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
			for _, replacement := range replacements {
				prefix = strings.ReplaceAll(prefix, replacement.en, replacement.ja)
			}
			lines[index] = prefix + suffix + ending
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

func localizeDaemonText(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	for _, replacement := range []struct{ en, ja string }{
		{"cancelled the pending stop of", "保留中の停止要求をキャンセルしました:"},
		{"stop was already requested; waiting for the daemon to exit", "停止要求は送信済みです。daemon の終了を待っています"},
		{"run wx daemon install to register the LaunchAgent first", "LaunchAgent を登録するには wx daemon install を実行してください"},
		{"stop it with wx daemon stop and start it again with wx daemon start", "wx daemon stop で停止し、wx daemon start で再起動してください"},
		{"the daemon is not managed by launchd", "daemon は launchd に管理されていません"},
		{"accepted the restart request but was not replaced within", "再起動要求を受理しましたが、次の daemon に置き換わらないまま期限を超えました"},
		{"accepted the stop request but did not exit within", "停止要求を受理しましたが、終了しないまま期限を超えました"},
		{"launchd was asked to start", "launchd に起動を依頼しました"},
		{"but no daemon answered", "が daemon から応答を受け取れませんでした"},
	} {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	return text
}

// localizeErrorText は設定検証などの固定エラーだけを翻訳し、path・key・外部エラーの値は残す。
func localizeErrorText(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	return strings.ReplaceAll(text, "language must be en or ja", i18n.New(string(lang)).Localize("config.language.invalid", nil))
}

func localizeError(err error, lang i18n.Language) string {
	if err == nil {
		return ""
	}
	return localizeErrorText(err.Error(), lang)
}

func localizeSetupError(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	for _, replacement := range []struct{ en, ja string }{
		{"setup item ", "setup 項目 "},
		{" does not offer action ", " は操作 "},
		{"; available actions: ", "。利用可能な操作: "},
		{" action manual requires --value", " の manual 操作には --value が必要です"},
	} {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	return text
}
