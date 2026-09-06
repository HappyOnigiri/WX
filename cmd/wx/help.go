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
  doctor [--json]                check configuration and dependencies
  gc [--dry-run]                 run retention cleanup
  prune [--all] [--dry-run]      delete recovery refs the database cannot explain
  clear [--all] [--standby]      delete managed worktrees now
  retry-standby <workspace>      resume standby replenishment after it stopped
  slots [--all] [--json]         list managed wx slots and their disk usage
  config [<key> ...]             show or update configuration
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

Options:
  --verbose, -v  show detailed status instead of the summary
  --json         print machine-readable JSON`)
	case "doctor":
		_, _ = fmt.Fprintln(w, `Usage: wx doctor [--json]

Run read-only checks for configuration, storage, Git, launchd, and hooks.

Options:
  --json  print machine-readable JSON`)
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
		_, _ = fmt.Fprintln(w, `Usage: wx clear [--all] [--standby] [--dry-run]

Delete the worktrees wx manages without waiting for their retention period.
Work is saved first: recovery data, session history, and workspace
registrations stay, and their retention follows the existing settings.

Standby worktrees waiting for the next session are kept unless --standby or
--all is given. Once they are deleted, wx does not replenish them until the
affected workspace is used again.

Without --all, sessions that are in use are left alone. With --all, wx asks
those sessions to stop, waits up to 30s for each of them, and deletes only the
ones that stopped; nothing is killed. Quarantined slots are always kept.

The command waits for every target to finish. Interrupting it does not stop
the daemon, and running it again rejoins the clear already in progress. While
a clear runs, new sessions and resumes are refused.

Exit status is 0 when every target succeeded or there was nothing to do, 1
when something failed, was quarantined, or did not finish, and 2 for an
argument error. Keeping sessions in use or standby worktrees is not a failure.

Options:
  --all      ask sessions in use to stop, then delete what stopped, standby
             worktrees included
  --standby  delete standby worktrees too
  --dry-run  report the targets and the reasons wx cannot process some of
             them, changing nothing`)
	case "retry-standby":
		_, _ = fmt.Fprintln(w, `Usage: wx retry-standby <workspace-path>

Resume automatic standby worktree replenishment for a workspace after wx
stopped it, then retry replenishment in the current generation. Replenishment
also resumes on its own once wx claude or wx codex succeeds for the workspace.
Quarantined worktrees are left untouched; wx gc deletes them once
retention.quarantined has passed.

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

List managed wx slots with their session, copy mode, and disk usage.
By default, only READY and LEASED slots are listed.

Disk usage is measured periodically by the daemon, not on demand, so the
numbers carry the measurement time shown by --json. copy_mode reports what
the slot looks like now: cow once any file still shares blocks with the main
worktree, copy otherwise.

Options:
  --all   include failed, quarantined, and released slots
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
	case "hook":
		_, _ = fmt.Fprintln(w, `Usage: wx hook <event>

Run a wx agent hook event read from stdin. Invoked by agent hook
configuration, not normally run directly.`)
	default:
		topUsage(w)
	}
}
