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
  -h, --help                     show help
  -v, --version                  show version

Commands:
  claude [arguments...]           launch Claude Code in a wx workspace
  codex [arguments...]            launch Codex in a wx workspace
  status [--json]                show daemon and pool state
  doctor [--json]                check configuration and dependencies
  gc [--dry-run]                 run retention cleanup
  sessions [--all] [--json]     list managed sessions
  config [<key> <value>]         show or update configuration
  resume <id> <agent> [args...] restore a wx session
  forget <workspace-path>        forget an inactive workspace
  daemon install|uninstall       manage the LaunchAgent`)
}

func commandUsage(w io.Writer, name string) {
	switch name {
	case "status":
		_, _ = fmt.Fprintln(w, `Usage: wx status [--json]

Show daemon health, pool state, active sessions, retention, and disk usage.

Options:
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
		_, _ = fmt.Fprintln(w, `Usage: wx daemon <serve|install|uninstall>

Serve internally, or install and remove the per-user LaunchAgent.`)
	case "hook":
		_, _ = fmt.Fprintln(w, `Usage: wx hook <event>

Run a wx agent hook event read from stdin. Invoked by agent hook
configuration, not normally run directly.`)
	default:
		topUsage(w)
	}
}
