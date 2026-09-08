# wx

English | [日本語](README.ja.md) | [简体中文](README.zh-CN.md)

Run Claude Code or Codex in a separate Git worktree with a single command.
`wx` prepares and manages workspaces on macOS, so you can hand off a task without switching branches in your source repository.

## Features

- **Separate workspaces** — Agents work in detached Git worktrees, keeping your source checkout's HEAD, index, and tracked files untouched.
- **Ready when you need them** — A background daemon keeps a workspace ready for recently used repositories.
- **Session recovery** — Use `wx slots` to list managed slots and `wx resume` to restore archived work.
- **Familiar commands** — Use Claude Code or Codex with their usual arguments, and optionally choose a starting branch.

## Installation

Requires **macOS on Apple Silicon**, **Git**, and **Claude Code or Codex** available on your `PATH`.
Install the latest release without cloning this repository or installing Go:

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/install.sh | bash
```

The installer verifies and installs `~/.local/bin/wx`, registers the daemon as a LaunchAgent, and starts it.
Run the same command to update to the latest release and restart the daemon.
Then make `wx` available in your current terminal:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Add that line to your shell configuration (for example, `~/.zshrc`) for new terminals.
Then finish the setup:

```sh
wx setup
```

`wx setup` asks about each item it manages — the worktree root, the shell PATH entry, the LaunchAgent, the agent hooks, and the daemon — and applies your choices.
Running it again after a completed setup changes nothing.
See [release and source-build details](docs/release.md) for more information.

## Uninstallation

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/uninstall.sh | bash
```

The uninstaller lists the worktrees it is about to delete and asks before doing anything, then removes them, the wx hook entries, the LaunchAgent, the configuration, and `~/.local/bin/wx`.
Pass `--yes` to skip the question.
Because deletion runs through the daemon, keep it running until the uninstaller has finished.

Three things are left for you, and the uninstaller prints the exact command for each.
Those are the `PATH` line in your shell configuration, the state database and log directory, and the worktree root.
The last two hold the work wx saved for you.
Repositories you ran wx from may also hold `refs/wx/recovery/*` refs for those snapshots.

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
- **Sessions and cleanup:** `wx slots`, `wx resume`, `wx gc --dry-run`, `wx clear`.
- **Configuration:** `wx config` shows settings and can update individual values.
- **Agent integration:** Global agent hooks check workspace readiness, bind agent sessions to wx, and release them when work ends.
  `wx setup` registers them, and wx owns its own entries in the Claude and Codex hook configuration; everything else in those files is left untouched.
  Claude `--resume` and Codex `resume` keep their usual arguments.

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
