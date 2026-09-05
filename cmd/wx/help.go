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
  --fresh                        resume conversation without wx recovery state
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
  clean [--all] [--standby]      delete managed worktrees now
  sessions [--all] [--json]      list managed sessions
  config [<key> <value>]         show or update configuration
  resume <id> <agent> [args...]  restore a wx session
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
  --json  print machine-readable JSON`)
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
	case "clean":
		_, _ = fmt.Fprintln(w, `Usage: wx clean [--all] [--standby] [--dry-run]

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
the daemon, and running it again rejoins the clean already in progress. While
a clean runs, new sessions and resumes are refused.

Exit status is 0 when every target succeeded or there was nothing to do, 1
when something failed, was quarantined, or did not finish, and 2 for an
argument error. Keeping sessions in use or standby worktrees is not a failure.

Options:
  --all      ask sessions in use to stop, then delete what stopped, standby
             worktrees included
  --standby  delete standby worktrees too
  --dry-run  report the targets and the reasons wx cannot process some of
             them, changing nothing`)
	case "config":
		_, _ = fmt.Fprintln(w, `Usage: wx config [<key> <value>]

Show effective configuration, or atomically update one supported scalar key.`)
	case "resume":
		_, _ = fmt.Fprintln(w, `Usage: wx resume <wx-session-id> <claude|codex> [agent-arguments...]

Restore an archived wx session into a new managed workspace.`)
	case "sessions":
		_, _ = fmt.Fprintln(w, `Usage: wx sessions [--all] [--json]

List managed wx sessions and their recovery state.

Options:
  --all   include expired sessions
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
