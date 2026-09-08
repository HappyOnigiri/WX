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
  status [--verbose] [--json]    show daemon and pool state
  doctor [--verbose] [--json]    check configuration and dependencies
  gc [--dry-run]                 run retention cleanup
  prune [--all] [--dry-run]      delete recovery refs the database cannot explain
  clear [--all] [--standby]      delete managed worktrees now
  retry-standby <workspace>      resume standby replenishment after it stopped
  slots [--all] [--json]         list managed wx slots and their disk usage
  config [<key> ...]             show or update configuration
  setup [--check] [--remove]     review and complete, or remove, the wx setup
  resume <id> [agent] [args...]  restore a wx session
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
		_, _ = fmt.Fprintln(w, `Usage: wx doctor [--verbose] [--json]

Run read-only checks for configuration, storage, Git, launchd, hooks, and recovery data.
Print one line when nothing is wrong; otherwise report each problem with its target, cause, and action.
Exit 0 when only passing and informational results remain, 1 when a problem or an unfinished check remains.

Options:
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

Resume automatic standby worktree replenishment for a workspace after wx
stopped it, then retry replenishment in the current generation. Replenishment
also resumes on its own once wx claude or wx codex succeeds for the workspace.
Quarantined worktrees are left untouched; wx gc deletes them once
retention.quarantined has passed, and wx clear deletes them right away.

The workspace path is shown by wx status when standby replenishment is stopped.`)
	case "config":
		_, _ = fmt.Fprintln(w, `Usage: wx config
       wx config <key> <value>
       wx config <key> --add <value>
       wx config <key> --remove <value>
       wx config <key> --reset

Show effective configuration, or atomically update one supported scalar key or list.

Copy mode (storage.copy_mode):
  auto  share identical checked-out files with APFS CoW; fall back to copies, but quarantine when ownership is unprovable (default)
  cow   fail preparation if CoW fails
  copy  keep normal Git checkout files`)
	case "resume":
		_, _ = fmt.Fprintln(w, `Usage: wx resume <wx-session-id> [claude|codex] [--fresh] [--branch <branch>] [agent-arguments...]

Restore an archived wx session into a new managed workspace.
With --fresh, keep the conversation but build the worktree from the current base.
Use --branch with --fresh to choose the detached base.`)
	case "slots":
		_, _ = fmt.Fprintln(w, `Usage: wx slots [--all] [--json]

List managed wx slots with their repository, session, copy mode, and disk usage.
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

Options:
  --all   also list sessions that no longer hold a slot
  --json  print machine-readable JSON`)
	case "forget":
		_, _ = fmt.Fprintln(w, `Usage: wx forget <workspace-path>

Forget an inactive workspace after all managed slots are safely archived.`)
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
