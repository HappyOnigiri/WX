# wx

`wx` launches Claude Code or Codex inside daemon-managed, detached Git worktrees.
The source repository's HEAD, index, and tracked working files are never used as the agent's working directory.

## Install

```sh
make setup
make ci
make install
wx daemon install
```

The default installation path is `~/.local/bin/wx`.
The LaunchAgent starts the daemon at login and maintains one ready workspace for recently used repositories.

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

Arguments following `claude` or `codex` are passed through unchanged.
`wx status`, `wx doctor`, `wx sessions`, and `wx gc --dry-run` expose daemon state and diagnostics.

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

Complex workspace and repository overrides are edited in `~/.config/wx/config.yaml`.
Configuration is strictly decoded; an invalid reload leaves the last valid daemon configuration active.
Paths expand `$HOME` only—`~` and arbitrary environment variables are rejected.

Recovery snapshots are stored as protected Git objects and refs in the source repository.
They can contain staged, unstaged, and non-ignored untracked content and are readable by processes with access to that repository.

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
