# wx

`wx` launches Claude Code or Codex with a per-workspace choice of daemon-managed, detached Git worktrees or the current directory.
When worktrees are enabled, the agent works in an isolated checkout.

## Install

```sh
make setup
make ci
make install
wx daemon install
```

The default installation path is `~/.local/bin/wx`.
The LaunchAgent starts the daemon at login and maintains one ready workspace for recently used workspaces that permit hot standby.

Upgrading from a wx that predates `wx daemon start` requires `wx daemon install` to be run again, even on a machine where the LaunchAgent is already registered.
The plist now runs `wx daemon start --foreground` where it used to run `wx daemon serve`, and the new binary rejects `serve` as an unknown action.
Until the plist is rewritten, launchd keeps restarting a daemon that exits immediately and the log fills with usage output.

A running daemon keeps executing the binary it started with, so `make install` alone does not update it.
The daemon notices the replacement within ten seconds and restarts itself once no job is running and no request is in flight.
Run `wx daemon restart` to ask for the same restart without waiting for that check.
Either way the daemon waits for an idle moment, so a restart never cuts short a request that is already running.

```sh
wx daemon stop     # exit the running daemon
wx daemon start    # run it again
wx daemon restart  # replace it with the installed binary
```

All three wait for the daemon to reach the requested state and give up after 60 seconds.
The daemon acts on the request at its next idle moment rather than straight away, so the wait is normally a second or two, and longer while a job is still running.
In a terminal the command shows a `stopping...` line for as long as it waits.
A `stop`, and a `restart` the daemon accepted, also name what the daemon was still waiting for.
`start`, and a `restart` that found nothing listening and fell back to launchd, have no such answer to report and say only that the daemon never appeared.
`wx daemon stop` leaves the LaunchAgent registered, so the next login — or the next `wx claude` — starts the daemon again.
Use `wx daemon uninstall` to stop it from coming back at all.

## Use

```sh
wx claude
wx codex
wx --branch feature/api codex
wx --branch server=feature/api --branch web=feature/ui claude
```

On the first launch in an unconfigured workspace, a terminal menu offers **Hot standby**, **Cold start**, and **No worktree**.
Use the up/down keys to choose and Enter to save the choice; Esc or Ctrl+C cancels without saving or launching an agent.
Cold start is initially selected.
Hot standby permits a ready worktree to be kept in the pool; cold start creates a new worktree for each launch without replenishing standby worktrees.
No worktree runs the agent in the current directory without starting the daemon or creating a managed session.

```sh
wx --select-worktree claude  # choose again and save
wx --no-worktree codex       # run here for this invocation only
wx --worktree claude         # create a new worktree for this invocation only
wx -s claude                 # same as --select-worktree; -n and -w are the other two
```

These three options are mutually exclusive and must precede the agent name.
`--branch` and `--fresh` require worktrees.
Arguments following `claude` or `codex` are passed through unchanged.
`wx status`, `wx doctor`, `wx sessions`, and `wx gc --dry-run` expose daemon state and diagnostics.
`wx status` prints one row per workspace followed by daemon, job, and storage summaries.
Use `wx status --verbose` (or `wx status -v`) for the complete grouped detail view, while `wx status --json` keeps the machine-readable response unchanged.
The `*` marker identifies the workspace containing the current directory, including a managed slot under any current or retired storage root.
`LAST USED` is shown only when a workspace has exactly one repository whose main path is that workspace root; otherwise it is `—`.

The user-level Claude Code and Codex hooks should call these commands only when `WX_SESSION_ID` is present:

```text
wx hook session-start
wx hook user-prompt-submit
wx hook pre-tool-use
wx hook session-end
```

The hooks bind native agent session IDs, gate prompts and tools until the workspace is ready, and send a short release RPC.
Hook configuration remains outside this repository so it can be managed with the rest of the user's agent configuration.

## Configuration

Run `wx config` to show the effective configuration and all scalar keys.
Update a scalar without expanding the sparse YAML file:

```sh
wx config retention.hot_standby 168h
```

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

Disable this default with `wx config includes.default_agent_rules false`.
Set `repositories.<path>.includes.default_agent_rules` in `~/.config/wx/config.yaml` to override the global value for one repository.
Changing the setting affects newly prepared worktrees; files are not removed from a worktree that is currently leased.

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

Recovery snapshots are stored as protected Git objects and refs in the source repository.
They can contain staged, unstaged, and non-ignored untracked content and are readable by processes with access to that repository.
A local rule file copied into a worktree is untracked content of that kind.

## Development

Run `make setup` once to install the pinned development tools, then `make ci` for the local quality gate.
Security and SBOM checks are paused in the initial phase and are not part of `make setup`, `make ci`, or the Git hooks.
They remain available as manual, explicit opt-in targets; each target installs its paused tool only when you invoke that target:

```sh
make security-local             # govulncheck, dependency, gosec, license, secrets
make workflow-security-audit    # zizmor
make sbom
```

Mutation testing was tried and dropped: it drove test design from a threshold rather than from behavior, and the survivor triage cost outweighed its value at this project's scale.
`gremlins` and the mutation targets have been removed rather than paused.

Do not re-enable these checks in CI, workflows, or hooks without the user's explicit permission.

Git hooks are not tracked in this repository.
Install them by placing an executable `pre-commit` and `pre-push` under `$(git rev-parse --git-common-dir)/hooks`.
Each hook dispatches to the matching `make hook-pre-commit` / `make hook-pre-push` target.
Keeping the hook bodies there rather than under a repository-local `core.hooksPath` leaves a user-level `core.hooksPath` dispatcher intact.
A repository-local setting would otherwise shadow that dispatcher for every hook.

The invariants this repository will not trade away, and the conventions that follow from them, are in [`AGENTS.md`](AGENTS.md).
The package layout, session lifecycle, and ownership proof are in [`docs/architecture.md`](docs/architecture.md).
