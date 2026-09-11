package main

import (
	"fmt"
	"io"
)

func topUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `Usage: wx [wx-options] <claude|codex> [agent-arguments...]
       wx <command> [options]

Global options:
  --branch <branch|repo=branch>  choose a detached base (repeatable)
  --fresh                        resume conversation from the current base
  -s, --select-worktree          select and save the workspace policy again
  -w, --worktree                 create a worktree without saving a policy
  -n, --no-worktree              run here without saving a policy
  -h, --help                     show help
  -v, --version                  show version

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
  resume <id> [agent] [args...]  restore a wx session
  discard-recovery <workspace>   discard recovery state that lost its refs
  forget <workspace-path>        forget an inactive workspace
  daemon start|stop|restart      change whether the daemon is running
  daemon install|uninstall       register or remove the LaunchAgent`)
}

func commandUsage(w io.Writer, name string) {
	switch name {
	case "status":
		_, _ = fmt.Fprintln(w, `Usage: wx status [--verbose] [--json]

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
  --json         print machine-readable JSON`)
	case "doctor":
		_, _ = fmt.Fprintln(w, `Usage: wx doctor [--probe] [--verbose] [--json]

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
  --json         print machine-readable JSON with every result regardless of --verbose`)
	case "gc":
		_, _ = fmt.Fprintln(w, `Usage: wx gc [--dry-run]

Run retention cleanup without deleting unarchived workspace data.

Options:
  --dry-run  report candidates without removing them`)
	case "prune":
		_, _ = fmt.Fprintln(w, `Usage: wx prune [--all] [--dry-run]

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
  --dry-run  report what would be deleted, changing nothing`)
	case "clear":
		_, _ = fmt.Fprintln(w, `Usage: wx clear [--all] [--standby] [--discard] [--dry-run]

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
             them, changing nothing`)
	case "retry-standby":
		_, _ = fmt.Fprintln(w, `Usage: wx retry-standby <workspace-path>
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
         configuration still replenishes`)
	case "config":
		_, _ = fmt.Fprintln(w, `Usage: wx config
       wx config <key> <value>
       wx config <key> --add <value>
       wx config <key> --remove <value>
       wx config <key> --reset
       wx config --workspace <path> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]
       wx config --repository <path> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]

Show effective configuration, or atomically update one supported scalar key or list.

--add and --remove take a list key: discovery.exclude, readiness.early_paths, or
sessions.paths.<claude|codex>.sessions. --reset takes any of those list keys or any
scalar key wx config lists; it drops the key from the config file so the built-in
default applies again.

With --workspace or --repository, show every key that scope can override with its
effective value and source, or update one of them. Both paths may be relative or a
repository subdirectory; linked worktrees resolve to the repository's main worktree.
--repository rejects a path outside a Git repository, while --workspace keeps a
non-repository directory as the workspace root. The two flags cannot be combined.

A workspace overrides worktree, copy, link, reuse_standby, submodules, warm_count,
agent.add_dir, retention.hot_standby, retention.ended_worktree, discovery.max_depth
and discovery.exclude. warm_count 0 disables replenishment, and so does
retention.hot_standby 0; reducing the count lets normal GC reclaim unused standby
slots. reuse_standby controls whether an older READY standby is updated at lease
time; the default is true, and false preserves exact-match cold-start behavior.

A repository overrides default_branch, dir_name, dir_source, cow_min_size_kib,
prepare.command, prepare.timeout, prepare.version, includes.default_agent_rules,
readiness.mode, readiness.early_paths, readiness.timeout and storage.copy_mode. A
lease leasing several repositories waits in full mode if any of them asks for it,
and uses the longest readiness.timeout among them.

A list key set on a scope replaces the global list instead of extending it. The
first --add copies the global list as it stands right then, so later changes to the
global value no longer reach that scope; --reset drops the scope list and restores
the global one. A repository readiness.early_paths does not apply to the shared
workspace root stage of a multi-repository workspace, which keeps using the global
list. --reset on any scalar key restores the inherited value.

Submodules (worktree.submodules, default true):
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

Copy mode (storage.copy_mode):
  auto  share identical checked-out files with APFS CoW; fall back to copies, but quarantine when ownership is unprovable (default)
  cow   fail preparation if CoW fails
  copy  keep normal Git checkout files
storage.cow_min_size_kib sets the smallest file CoW shares, in KiB (default 16).
Files below it keep their normal checkout copy; 0 shares every eligible file, and
a larger value trades disk savings for less per-file work. Changing it stops
reuse of READY standby worktrees prepared under the previous value.
A repository can override the limit and the copy mode with wx config --repository,
since the best values depend on the repository's file size distribution and
checkout. Only the repositories whose effective values changed lose the reuse of
their READY standby worktrees. A --config override on a single lease wins over
both the repository entry and the global setting.

Workspace root files (multi-repository workspaces):
The root itself has no checkout, so only these paths reach a slot: AGENTS.md,
AGENTS.local.md, CLAUDE.md, CLAUDE.local.md, the agent asset directories
.claude/skills, .claude/agents, .claude/commands, .claude/hooks and
.codex/prompts, and whatever the rules below add. Missing paths are skipped.
Add more with a .worktreeinclude (copied, glob patterns, no match is fine) and a
.worktreelink (symlinked back to the root, literal paths that must exist) in the
root itself, or with wx config --workspace <path> copy --add and link --add. A
copy path set in the config must exist or preparation fails. A path cannot be both copied and linked. The root manifests
apply to multi-repository workspaces only; inside a repository the same file
names keep their repository meaning.

Readiness (readiness.mode):
  early  wait for Git registration and startup files, then launch with readiness hooks (default)
  full   wait for checkout, includes, links, prepare commands, and final validation
Without readiness hooks, both modes wait for full preparation. Resume and shell/run/new always wait for full preparation.
Use full when checkout hooks or prepare commands generate or update startup settings.
readiness.early_paths adds literal repository-relative paths to the startup list;
edit it with wx config readiness.early_paths --add/--remove/--reset, or per
repository with wx config --repository <path> readiness.early_paths --add.
Directories include their descendants; no glob patterns are expanded. Only paths
already scheduled by checkout or copy/link rules are materialized. Workspace roots
use the same selection. Absolute paths, escapes, the root itself, and .git are rejected.
readiness.progress prints the preparation progress to stderr while a lease waits
(default true); set it to false to wait without any output. A non-terminal stderr
never gets the progress lines, whatever the setting says.`)
	case "resume":
		_, _ = fmt.Fprintln(w, `Usage: wx resume <wx-session-id> [claude|codex] [--fresh] [--branch <branch>] [agent-arguments...]

Restore an archived wx session into a new managed workspace.
With --fresh, keep the conversation but build the worktree from the current base.
Use --branch with --fresh to choose the detached base.`)
	case "shell":
		_, _ = fmt.Fprintln(w, `Usage: wx shell [--branch <branch|repo=branch>] [--resume <wx-session-id>]

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
  --resume <wx-session-id>       restore the worktree of an earlier wx session`)
	case "run":
		_, _ = fmt.Fprintln(w, `Usage: wx run [--branch <branch|repo=branch>] [--resume <wx-session-id>] -- <command> [arguments...]

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
  --resume <wx-session-id>       restore the worktree of an earlier wx session`)
	case "new":
		_, _ = fmt.Fprintln(w, `Usage: wx new [--branch <branch|repo=branch>] [--json]

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
  --json                         print the session id and path as JSON`)
	case "release":
		_, _ = fmt.Fprintln(w, `Usage: wx release <wx-session-id> [--discard]

Return a workspace leased by wx new without waiting for lease.ttl. Unfinished
work is saved first, and the worktree stays for retention.ended_worktree so
wx shell --resume <id> can reopen it.

With --discard the work is not saved: the return schedules the worktree for
removal instead, so nothing is left to reopen with wx shell --resume.

Only leases with no running process are returned this way. A workspace held by
wx shell or wx run is returned when that shell or command exits, and one held
by an agent when that agent exits; wx clear --all asks either of them to stop.

Options:
  --discard  return the lease without saving unfinished work, and remove the
             worktree instead of keeping it for retention.ended_worktree`)
	case "slots":
		_, _ = fmt.Fprintln(w, `Usage: wx slots [--all] [--json]

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
  --json  print machine-readable JSON`)
	case "bench":
		_, _ = fmt.Fprintln(w, `Usage: wx bench [--runs <n>] [--branch <branch|repo=branch>] [--sweep|--config <key=value,...>] [--reuse] [--json]

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
  --json                         print machine-readable JSON`)
	case "discard-recovery":
		_, _ = fmt.Fprintln(w, `Usage: wx discard-recovery <workspace-path> [--dry-run]

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
  --dry-run  list what would be discarded without changing anything`)
	case "forget":
		_, _ = fmt.Fprintln(w, `Usage: wx forget <workspace-path>

Forget an inactive workspace after all managed slots are safely archived.

A workspace whose sessions were quarantined because their recovery refs are
missing is refused until wx discard-recovery <workspace-path> discards that
state.`)
	case "daemon":
		_, _ = fmt.Fprintln(w, `Usage: wx daemon <start|stop|restart|install|uninstall> [--foreground]

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
                to start the daemon. This is how the LaunchAgent runs wx.`)
	case "setup":
		_, _ = fmt.Fprintln(w, `Usage: wx setup [--check [--json]] [--update] [--remove]

Walk through what wx needs to run on its own and apply the choices. Each item
is offered with the choices its current state allows, so running setup again
after a completed setup changes nothing.

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
  --remove  delete the configuration wx setup writes and report what was kept`)
	case "hook":
		_, _ = fmt.Fprintln(w, `Usage: wx hook <event>

Run a wx agent hook event read from stdin. Invoked by agent hook
configuration, not normally run directly.`)
	default:
		topUsage(w)
	}
}
