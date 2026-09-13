package i18n

// catalog_help_config.go は設定と導入に関わるコマンドの help 本文を持つ。
// 桁揃えと改行位置そのものが表示契約なので、行や語句へ分割せずコマンド単位の
// 長文を英日 2 本で置き、描画側は ID を引くだけにする。

// 本文中のコマンド名・オプション名・path などの機械識別子は訳さない。

var helpCatalogConfig = map[string]Entry{
	"help.command.config": {
		EN: `Usage: wx config
       wx config --describe <key>
       wx config --system [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --repository-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace <root> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace <root> --repository-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace <root> --repository <relative-path> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]

Show effective configuration, or atomically update one supported scalar key or list.
Use --describe to show a key's type, scopes, choices, purpose, and impact.

--add and --remove take a list key such as discovery.exclude, readiness.early_paths,
workspace copy/link, or sessions.paths.<claude|codex>.sessions, depending on scope.
--reset takes any list or scalar key wx config lists; it drops the key from the file
so the inherited or built-in default applies again.

Config v2 uses explicit scopes: --system, --workspace-defaults,
--repository-defaults, --workspace <root>, or --workspace <root> together with
--repository-defaults / --repository <relative-path>. Repository paths are
workspace-relative membership keys; absolute paths, escapes, and a repository
scope for a single-repository workspace are rejected. --repository cannot be
used without --workspace.

With --workspace, show every Workspace key that scope can override with its
effective value and source. --workspace <root> --repository-defaults edits the
shared repository defaults for that Workspace. A multi-repository Workspace can
also edit one membership with --repository <relative-path>. The old flat syntax
is accepted only for a legacy version-1 file.

A workspace overrides worktree, copy, link, reuse_standby, warm_count, agent.add_dir,
retention.hot_standby, retention.ended_worktree, discovery.max_depth and
discovery.exclude. warm_count 0 disables replenishment, and so does
retention.hot_standby 0; reducing the count lets normal GC reclaim unused standby
slots. reuse_standby controls whether an older READY standby is updated at lease
time; the default is true, and false preserves exact-match cold-start behavior.

A repository overrides default_branch, dir_name, dir_source, cow_min_size_kib,
prepare.command, prepare.timeout, prepare.version, includes.default_agent_rules,
readiness.mode, readiness.early_paths, readiness.timeout, readiness.progress and storage.copy_mode. A
lease leasing several repositories waits in full mode if any of them asks for it,
and uses the longest readiness.timeout among them.

A list key set on a scope replaces the inherited list instead of extending it. The
first --add copies the parent list as it stands right then, so later changes no longer
reach that scope; --reset drops the scope list and restores its parent. A repository
readiness.early_paths does not apply to the shared workspace root stage of a
multi-repository workspace, which keeps using the workspace list.

Submodules (repository_defaults.submodules, default true):
Linked worktrees resolve a submodule's gitdir per worktree, so they cannot reuse
the main repository's .git/modules/<name>. wx therefore clones each submodule
from that local module, which stays offline and shares objects as hardlinks. A
submodule is skipped with a warning, leaving the empty gitlink directory, when
the local module is absent, when it lacks the gitlink commit, or when no
upstream url is available; preparation still succeeds. After the clone wx
restores the submodule's origin to the upstream url so git push does not write
into the main repository's .git/modules. Changing this stops reuse of READY
standby worktrees prepared under the previous value.
Commits made inside a worktree's submodule are lost when the slot is deleted,
because snapshots can only record the gitlink. Push them before releasing.

Agent directories (agent.add_dir):
  always     pass every repository directory under the agent's working directory to --add-dir (default)
  worktree   pass them only when the agent runs in a wx worktree
  off        never pass them
A workspace with several repositories puts the agent's working directory at the
parent of those repositories, so their .claude/skills and other agent assets are
only loaded when the directories are passed with --add-dir. A workspace that is
a single repository has nothing to pass. Directories you pass yourself are kept.

Copy mode (repository_defaults.storage.copy_mode):
  auto  share identical checked-out files with APFS CoW; fall back to copies, but quarantine when ownership is unprovable (default)
  cow   fail preparation if CoW fails
  copy  keep normal Git checkout files
repository_defaults.cow_min_size_kib sets the smallest file CoW shares, in KiB (default 16).
Files below it keep their normal checkout copy; 0 shares every eligible file, and
a larger value trades disk savings for less per-file work. Changing it stops
reuse of READY standby worktrees prepared under the previous value.
A repository can override the limit and the copy mode with wx config --workspace
<root> --repository <relative-path>, since the best values depend on the
repository's file size distribution and checkout. Only the repositories whose
effective values changed lose the reuse of their READY standby worktrees. A
--config override on a single lease wins over both the repository entry and the
global setting.

Workspace root files (multi-repository workspaces):
The root itself has no checkout, so only these paths reach a slot: AGENTS.md,
AGENTS.local.md, CLAUDE.md, CLAUDE.local.md, the agent asset directories
.claude/skills, .claude/agents, .claude/commands, .claude/hooks and
.codex/prompts, and whatever the rules below add. Missing paths are skipped.
Add more with a .worktreeinclude (copied, glob patterns, no match is fine) and a
.worktreelink (symlinked back to the root, literal paths that must exist) in the
root itself, or with wx config --workspace <root> copy --add and link --add. A
copy path set in the config must exist or preparation fails. A path cannot be both copied and linked. The root manifests
apply to multi-repository workspaces only; inside a repository the same file
names keep their repository meaning.

Readiness (readiness.mode):
  early  wait for Git registration and startup files, then launch with readiness hooks (default)
  full   wait for checkout, includes, links, prepare commands, and final validation
Without readiness hooks, both modes wait for full preparation. Resume and shell/run/new always wait for full preparation.
Use full when checkout hooks or prepare commands generate or update startup settings.
readiness.early_paths adds literal repository-relative paths to the startup list;
edit it with wx config --repository-defaults readiness.early_paths
--add/--remove/--reset, or per repository with wx config --workspace <root>
--repository <relative-path> readiness.early_paths --add.
Directories include their descendants; no glob patterns are expanded. Only paths
already scheduled by checkout or copy/link rules are materialized. Workspace roots
use the same selection. Absolute paths, escapes, the root itself, and .git are rejected.
readiness.progress prints the preparation progress to stderr while a lease waits
(default true); set it to false to wait without any output. A non-terminal stderr
never gets the progress lines, whatever the setting says.`,
		JA: `使い方: wx config
       wx config --describe <key>
       wx config --system [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --repository-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace <root> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace <root> --repository-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --workspace <root> --repository <relative-path> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]

実効設定を表示するか、対応する scalar key または list を1件 atomically 更新します。
--describe で key の型、scope、選択肢、目的、影響を表示します。

--add and --remove take a list key such as discovery.exclude, readiness.early_paths,
workspace copy/link, or sessions.paths.<claude|codex>.sessions, depending on scope.
--reset takes any list or scalar key wx config lists; it drops the key from the file
so the inherited or built-in default applies again.

Config v2 uses explicit scopes: --system, --workspace-defaults,
--repository-defaults, --workspace <root>, or --workspace <root> together with
--repository-defaults / --repository <relative-path>. Repository paths are
workspace-relative membership keys; absolute paths, escapes, and a repository
scope for a single-repository workspace are rejected. --repository cannot be
used without --workspace.

With --workspace, show every Workspace key that scope can override with its
effective value and source. --workspace <root> --repository-defaults edits the
shared repository defaults for that Workspace. A multi-repository Workspace can
also edit one membership with --repository <relative-path>. The old flat syntax
is accepted only for a legacy version-1 file.

A workspace overrides worktree, copy, link, reuse_standby, warm_count, agent.add_dir,
retention.hot_standby, retention.ended_worktree, discovery.max_depth and
discovery.exclude. warm_count 0 disables replenishment, and so does
retention.hot_standby 0; reducing the count lets normal GC reclaim unused standby
slots. reuse_standby controls whether an older READY standby is updated at lease
time; the default is true, and false preserves exact-match cold-start behavior.

A repository overrides default_branch, dir_name, dir_source, cow_min_size_kib,
prepare.command, prepare.timeout, prepare.version, includes.default_agent_rules,
readiness.mode, readiness.early_paths, readiness.timeout, readiness.progress and storage.copy_mode. A
lease leasing several repositories waits in full mode if any of them asks for it,
and uses the longest readiness.timeout among them.

A list key set on a scope replaces the inherited list instead of extending it. The
first --add copies the parent list as it stands right then, so later changes no longer
reach that scope; --reset drops the scope list and restores its parent. A repository
readiness.early_paths does not apply to the shared workspace root stage of a
multi-repository workspace, which keeps using the workspace list.

Submodules (repository_defaults.submodules, default true):
Linked worktrees resolve a submodule's gitdir per worktree, so they cannot reuse
the main repository's .git/modules/<name>. wx therefore clones each submodule
from that local module, which stays offline and shares objects as hardlinks. A
submodule is skipped with a warning, leaving the empty gitlink directory, when
the local module is absent, when it lacks the gitlink commit, or when no
upstream url is available; preparation still succeeds. After the clone wx
restores the submodule's origin to the upstream url so git push does not write
into the main repository's .git/modules. Changing this stops reuse of READY
standby worktrees prepared under the previous value.
Commits made inside a worktree's submodule are lost when the slot is deleted,
because snapshots can only record the gitlink. Push them before releasing.

Agent directory（agent.add_dir）:
  always     agent の working directory 配下にある全 repository directory を --add-dir に渡す（既定）
  worktree   agent が wx worktree で動く場合だけ渡す
  off        渡さない
複数 repository の workspace では agent の working directory がそれらの親になります。
そのため .claude/skills などの agent asset は
--add-dir で directory を渡した場合だけ読み込まれます。workspace が
単一 repository なら渡すものはありません。自分で渡した directory は保持します。

Copy mode (repository_defaults.storage.copy_mode):
  auto  share identical checked-out files with APFS CoW; fall back to copies, but quarantine when ownership is unprovable (default)
  cow   fail preparation if CoW fails
  copy  keep normal Git checkout files
repository_defaults.cow_min_size_kib sets the smallest file CoW shares, in KiB (default 16).
Files below it keep their normal checkout copy; 0 shares every eligible file, and
a larger value trades disk savings for less per-file work. Changing it stops
reuse of READY standby worktrees prepared under the previous value.
A repository can override the limit and the copy mode with wx config --workspace
<root> --repository <relative-path>, since the best values depend on the
repository's file size distribution and checkout. Only the repositories whose
effective values changed lose the reuse of their READY standby worktrees. A
--config override on a single lease wins over both the repository entry and the
global setting.

Workspace root files（multi-repository workspace）:
root 自体には checkout がないため、slot へ届く path は AGENTS.md、
AGENTS.local.md、CLAUDE.md、CLAUDE.local.md、agent asset directory
.claude/skills、.claude/agents、.claude/commands、.claude/hooks と
.codex/prompts と、下記の規則が追加する path です。存在しない path は省略します。
.worktreeinclude（copy、glob は no match 可）と
.worktreelink（root へ symlink、存在必須の literal path）を
root itself, or with wx config --workspace <root> copy --add and link --add. A
設定した copy path は存在しないと準備に失敗します。path は copy と link の両方にはできません。root manifest は
multi-repository workspace にだけ適用し、repository 内では同じ file
名が repository 本来の意味を保ちます。

Readiness (readiness.mode):
  early  wait for Git registration and startup files, then launch with readiness hooks (default)
  full   wait for checkout, includes, links, prepare commands, and final validation
Without readiness hooks, both modes wait for full preparation. Resume and shell/run/new always wait for full preparation.
Use full when checkout hooks or prepare commands generate or update startup settings.
readiness.early_paths adds literal repository-relative paths to the startup list;
edit it with wx config --repository-defaults readiness.early_paths
--add/--remove/--reset, or per repository with wx config --workspace <root>
--repository <relative-path> readiness.early_paths --add.
Directories include their descendants; no glob patterns are expanded. Only paths
already scheduled by checkout or copy/link rules are materialized. Workspace roots
use the same selection. Absolute paths, escapes, the root itself, and .git are rejected.
readiness.progress prints the preparation progress to stderr while a lease waits
(default true); set it to false to wait without any output. A non-terminal stderr
never gets the progress lines, whatever the setting says.`,
	},
	"help.command.setup": {
		EN: `Usage: wx setup [--check [--json]] [--update] [--remove]
       wx setup --item <id> --action <action> [--value <value>]

Walk through what wx needs to run on its own and apply the choices. Each item
is offered with the choices its current state allows, so running setup again
after a completed setup changes nothing.

The first question chooses how to continue. The recommended settings apply the
suggested action for every item and ask nothing else; the detailed walk offers
each item one by one. --update, --check, --item and --remove never ask it.

The items are the prerequisites, storage.worktree_root, the shell PATH entry,
the LaunchAgent, the Claude and Codex hook entries, and the daemon. wx owns
the wx entries of the agent hook configuration: it writes a dedicated group
per event and never touches the rest of the file.

Cancelling stops the walk without undoing what was already applied; every item
is idempotent, so running wx setup again finishes the rest.

--remove deletes what wx setup writes instead of asking: the wx hook entries,
the LaunchAgent, and the configuration file. Removing the LaunchAgent boots the
daemon out, so run anything that needs the daemon, such as wx clear, first. The
shell startup file is left alone: wx cannot tell its own PATH line apart from
one you wrote or one your dotfile manager owns, so remove that line yourself.
The state database, the log directory, and the worktree root are kept too
because they hold saved work and records; each is printed on a line starting
with "leftover" so a script can act on them. The uninstaller runs this.

Exit status is 0 when the walk finished, 1 when an item could not be applied,
the walk was cancelled, or no terminal is attached, and 2 for an argument
error. --check reports differences with status 0. --update does not use the
exit status to report a missing terminal: it names the items that need
attention on stderr and exits 0. --remove needs no terminal, keeps going after
a failed item, and exits 1 when any item failed.

Options:
  --check   report the current state and change nothing; needs no terminal
  --json    with --check, print machine-readable JSON
  --update  offer only the items that no longer match what wx would write, and
            print nothing when there are none. The installer runs this.
  --remove  delete the configuration wx setup writes and report what was kept
  --item    configure only this item (for example hooks.claude)
  --action  apply one action offered for --item by wx setup --check
  --value   value used by the manual action`,
		JA: `使い方: wx setup [--check [--json]] [--update] [--remove]
       wx setup --item <id> --action <action> [--value <value>]

wx が単独で動くために必要な項目を確認し、選択を適用します。各項目は
現在の状態で選べる操作だけを提示するため、setup を再実行しても
完了済みの設定は変わりません。

最初の質問で進め方を選びます。おすすめ設定は全項目に推奨操作を適用し、以降は質問しません。詳細設定は項目を1つずつ提示します。--update・--check・--item・--remove では質問しません。

The items are the prerequisites, storage.worktree_root, the shell PATH entry,
the LaunchAgent, the Claude and Codex hook entries, and the daemon. wx owns
the wx entries of the agent hook configuration: it writes a dedicated group
per event and never touches the rest of the file.

キャンセルすると適用済みを戻さず walk を停止します。各項目は冪等なので wx setup を再実行すれば残りを完了できます。

--remove は質問せず wx setup の書き込み（wx hook entry、LaunchAgent、設定ファイル）を削除します。LaunchAgent を解除すると daemon が停止するため、wx clear など daemon が必要な操作を先に実行してください。shell startup file は残します。自分の PATH 行や dotfile manager の行と区別できないため、利用者が削除してください。state database、log directory、worktree root も保存済み作業と記録を含むため残し、script が扱えるよう各 path を ` + "`" + `leftover` + "`" + ` で始まる行に表示します。uninstaller はこれを実行します。

walk 完了時の終了コードは0、適用できない項目・キャンセル・端末なしは1、引数エラーは2です。--check は差分があっても0です。--update は端末なしを終了コードで示さず、対応が必要な項目を stderr に出して0で終了します。--remove は端末不要で、失敗後も続行し、失敗があれば1で終了します。

オプション:
  --check   現在の状態を表示し、変更しない（端末不要）
  --json    --check と併用し、機械可読 JSON を表示
  --update  wx の書き込み内容と一致しない項目だけ提示し、なければ無表示。installer が実行
  --remove  wx setup が書き込んだ設定を削除し、残したものを報告
  --item    この項目だけを設定（例: hooks.claude）
  --action  wx setup --check が提示した --item の action を適用
  --value   manual action に使う値`,
	},
	"help.command.hook": {
		EN: `Usage: wx hook <event>

Run a wx agent hook event read from stdin. Invoked by agent hook
configuration, not normally run directly.`,
		JA: `使い方: wx hook <event>

stdin から wx agent hook event を読み取って実行します。agent hook configuration から呼び出され、通常は直接実行しません。`,
	},
}
