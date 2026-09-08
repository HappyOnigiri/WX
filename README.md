# wx

English | [日本語](README.ja.md) | [简体中文](README.zh-CN.md)

Run Claude Code or Codex in a separate Git worktree with a single command.
`wx` prepares and manages workspaces on macOS, so you can hand off a task without switching branches in your source repository.

## Features

- **Separate workspaces** — Agents work in detached Git worktrees, keeping your source checkout's HEAD, index, and tracked files untouched.
- **Ready when you need them** — A background daemon keeps a workspace ready for recently used repositories.
- **Save disk space** — APFS Copy on Write shares data for files with identical contents in the source checkout, reducing worktree disk usage.
- **Familiar commands** — Use Claude Code or Codex with their usual arguments, and optionally choose a starting branch.

## Installation

Requires **macOS on Apple Silicon**, **Git**, and **Claude Code or Codex**.

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/install.sh | bash
```

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

Claude Code and Codex associate session logs with the paths they run from, so their standard continue / resume commands can have trouble finding past sessions when the worktree changes.
wx wraps these commands with a UI for selecting and resuming past sessions.

```sh
wx claude --resume
wx codex resume
```

## More options

- **Status and diagnostics:** `wx status`, `wx doctor`.
- **Sessions and cleanup:** `wx slots`, `wx resume`, `wx gc --dry-run`, `wx clear`.
- **Configuration:** `wx config` shows settings and can update individual values.

See `wx --help` and `wx <command> --help` for commands and options.

## Uninstallation

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/uninstall.sh | bash
```

Removes wx-managed worktrees (including unsaved work), hook entries, the LaunchAgent, configuration files, and the executable.

## Contributing

Contributions are welcome!
Share bug reports and ideas through [Issues](https://github.com/HappyOnigiri/WX/issues), or send a [pull request](https://github.com/HappyOnigiri/WX/pulls).
Documentation improvements and translations are welcome too.
