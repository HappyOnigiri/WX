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
		_, _ = fmt.Fprintln(w, "Usage: wx status [--json]")
	case "doctor":
		_, _ = fmt.Fprintln(w, "Usage: wx doctor [--json]")
	case "gc":
		_, _ = fmt.Fprintln(w, "Usage: wx gc [--dry-run]")
	case "config":
		_, _ = fmt.Fprintln(w, "Usage: wx config [<key> <value>]")
	case "resume":
		_, _ = fmt.Fprintln(w, "Usage: wx resume <wx-session-id> <claude|codex> [agent-arguments...]")
	case "sessions":
		_, _ = fmt.Fprintln(w, "Usage: wx sessions [--all] [--json]")
	case "forget":
		_, _ = fmt.Fprintln(w, "Usage: wx forget <workspace-path>")
	case "daemon":
		_, _ = fmt.Fprintln(w, "Usage: wx daemon <serve|install|uninstall>")
	default:
		topUsage(w)
	}
}
