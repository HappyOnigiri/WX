# wx

English | [日本語](README.ja.md) | [简体中文](README.zh-CN.md)

Run Claude Code or Codex with a per-workspace choice of a daemon-managed, detached Git worktree or the current directory.
When worktrees are enabled, the agent works in an isolated checkout.
`wx` prepares and manages workspaces on macOS, so you can hand off a task without switching branches in your source repository.

## Features

- **Separate workspaces** — Agents can work in detached Git worktrees, keeping your source checkout's HEAD, index, and tracked files untouched.
- **Per-workspace policy** — Choose hot standby, cold start, or no worktree interactively, or configure a default policy.
- **Ready when you need them** — A background daemon keeps workspaces ready for repositories that permit hot standby.
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

On the first launch in an unconfigured workspace, a terminal menu offers **Hot standby**, **Cold start**, and **No worktree**.
Use the up/down keys to choose and Enter to save the choice.
Esc or Ctrl+C cancels without saving or launching an agent.
Cold start is initially selected.
Hot standby permits a ready worktree to be kept in the pool.
Cold start creates a new worktree for each launch without replenishing standby worktrees.
No worktree runs the agent in the current directory without starting the daemon or creating a managed session.

```sh
wx --select-worktree claude  # choose again and save
wx --no-worktree codex       # run here for this invocation only
wx --worktree claude         # create a new worktree for this invocation only
wx -s claude                 # same as --select-worktree; -n and -w are the other two
```

These three options are mutually exclusive and must precede the agent name.
`--branch` and `--fresh` require worktrees.
Arguments after `claude` or `codex` are passed through unchanged.
To choose a starting branch, place the wx option before the agent name:

```sh
wx --branch feature/api codex
```

## More options

- **Status and diagnostics:** `wx status`, `wx doctor`.
- **Sessions and cleanup:** `wx sessions`, `wx resume`, `wx gc --dry-run`.
- **Configuration:** `wx config` shows settings and can update individual values.
- **Agent integration:** Global agent hooks enable native session resume and workspace readiness checks.
  Configure them to call `wx hook session-start`, `wx hook user-prompt-submit`, `wx hook pre-tool-use`, and `wx hook session-end` only when `WX_SESSION_ID` is set.
  Hook configuration is managed separately from wx.

`wx status` prints one row per workspace followed by daemon, job, and storage summaries.
Use `wx status --verbose` (or `wx status -v`) for the complete grouped detail view, while `wx status --json` keeps the machine-readable response unchanged.
The `*` marker identifies the workspace containing the current directory, including a managed slot under any current or retired storage root.
`LAST USED` is shown only when a workspace has exactly one repository whose main path is that workspace root.

See `wx --help` and `wx <command> --help` for commands and options:

```sh
wx --help
wx config --help
wx daemon --help
```

## Configuration

The default `worktree.undefined` policy is `ask`.
Without an interactive input and output terminal, an undefined workspace fails with instructions to choose an explicit override.
Set the behavior for undefined workspaces without prompting:

```sh
wx config worktree.undefined cold  # always create on demand
wx config worktree.undefined off   # always run in the current directory
wx config worktree.undefined hot   # always create and permit hot standby
wx config worktree.undefined ask   # restore the interactive default
```

Saved workspace choices take precedence over this default.
A choice is stored as `workspaces.<absolute-path>.worktree`, with the value `hot`, `cold`, or `off`.
For a Git repository, the key is the main worktree root, shared by its subdirectories and linked worktrees.
Outside a Git repository, the key is the current directory, which can contain multiple repositories.
Paths are matched exactly after canonicalization; policies do not implicitly apply to descendant workspaces.
Existing copy/link entries and unrelated settings are preserved when saving a choice.
An existing workspace entry without `worktree` remains undefined, as does a workspace known only to the daemon database.
Changing a policy stops future standby replenishment; existing slots follow their normal retention and recovery lifecycle.
`wx resume <id> <agent>` explicitly restores a managed session independently of the current directory's policy.
Agent-native resume arguments follow the current workspace policy; in worktree mode the selected conversation is restored through the existing wx resume hooks.

Complex workspace and repository overrides are edited in `~/.config/wx/config.yaml`.
Worktrees are created under `storage.worktree_root` (default `$HOME/wx`) as `<workspace-id>/<slot-id>/<RepoName>`.
Both IDs are six lowercase base36 characters, and `RepoName` comes from the repository's `origin` URL.
Set `storage.repo_dir_source` to `directory` to use the main worktree's own directory name instead, or pin one repository with a `dir_name` / `dir_source` entry under `repositories`.
Changing `storage.worktree_root` leaves existing sessions where they are and creates only new slots under the new root.
Configuration is strictly decoded; an invalid reload leaves the last valid daemon configuration active.
Paths expand `$HOME` only—`~` and arbitrary environment variables are rejected.

Agent rule files that convention keeps out of version control are copied into every worktree without a manifest entry, because Git does not carry them there itself:

- Claude Code: `CLAUDE.local.md`, `.claudeignore`
- Codex and the other AGENTS.md readers: `AGENTS.local.md`, `AGENTS.override.md`
- Gemini CLI: `GEMINI.local.md`, `.geminiignore`, `.aiexclude`
- Cursor: `.cursorrules`, `.cursorignore`
- Windsurf: `.windsurfrules`, `.codeiumignore`
- Cline, Roo Code, and Kilo Code: `.clinerules`, `.roorules`, `.kilocoderules`
- MCP servers, shared by several agents: `.mcp.json`
- Aider: `.aider.conf.yml`

Only untracked regular files are copied, and a tracked path is left to the checkout.
A directory or symlink under one of those names stays the job of an explicit `.worktreeinclude` or `.worktreelink` entry.
List anything else a worktree needs in `.worktreeinclude` (copied) or `.worktreelink` (symlinked to the main worktree, and required to be Git-ignored).

## Contributing

Contributions are welcome!
Share bug reports and ideas through [Issues](https://github.com/HappyOnigiri/WX/issues), or send a [pull request](https://github.com/HappyOnigiri/WX/pulls).
Documentation improvements and translations are welcome too.
