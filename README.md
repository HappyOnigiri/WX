# wx

English | [日本語](README.ja.md) | [简体中文](README.zh-CN.md)

Run Claude Code or Codex in a separate Git worktree with a single command.
`wx` prepares and manages workspaces on macOS, so you can hand off a task without switching branches in your source repository.

## Features

- **Separate workspaces** — Agents work in detached Git worktrees, keeping your source checkout's HEAD, index, and tracked files untouched.
- **Ready when you need them** — A background daemon keeps a workspace ready for recently used repositories.
- **Session recovery** — List sessions and restore archived work with `wx sessions` and `wx resume`.
- **Familiar commands** — Use Claude Code or Codex with their usual arguments, and optionally choose a starting branch.

## Installation

Requires **macOS**, **Git**, **Go 1.26.6 or later**, and **Claude Code or Codex** installed and available on your `PATH`.

```sh
git clone https://github.com/HappyOnigiri/WX.git
cd WX
make install
export PATH="$HOME/.local/bin:$PATH"
wx daemon install
```

The binary is installed to `~/.local/bin/wx`.
Add the `export PATH` line to your shell configuration (for example, `~/.zshrc`) to keep it available in new terminals.
`wx daemon install` registers a LaunchAgent that starts the daemon at login.

To update, run `git pull --ff-only` and `make install` in the cloned WX directory, then `wx daemon install` and `wx daemon restart`.

## Quick start

From the repository you want to work on:

```sh
wx claude
wx codex
```

Each command launches the chosen agent in a managed worktree.
Arguments after `claude` or `codex` are passed through unchanged.
To choose a starting branch, place the wx option before the agent name:

```sh
wx --branch feature/api codex
```

## More options

- **Status and diagnostics:** `wx status`, `wx doctor`.
- **Sessions and cleanup:** `wx sessions`, `wx resume`, `wx gc --dry-run`, `wx clean`.
- **Configuration:** `wx config` shows settings and can update individual values.
- **Agent integration:** Global agent hooks enable native session resume and workspace readiness checks.
  Configure them to call `wx hook session-start`, `wx hook user-prompt-submit`, `wx hook pre-tool-use`, and `wx hook session-end` only when `WX_SESSION_ID` is set.
  Hook configuration is managed separately from wx.

See `wx --help` and `wx <command> --help` for commands and options:

```sh
wx --help
wx config --help
wx daemon --help
```

## Contributing

Contributions are welcome!
Share bug reports and ideas through [Issues](https://github.com/HappyOnigiri/WX/issues), or send a [pull request](https://github.com/HappyOnigiri/WX/pulls).
Documentation improvements and translations are welcome too.
