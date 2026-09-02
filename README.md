# wx

`wx` launches Claude Code or Codex inside daemon-managed, detached Git
worktrees. The source repository's HEAD, index, and tracked working files are
never used as the agent's working directory.

## Install

```sh
make setup
make ci
make install
wx daemon install
```

The default installation path is `~/.local/bin/wx`. The LaunchAgent starts the
daemon at login and maintains one ready workspace for recently used
repositories.

## Use

```sh
wx claude
wx codex
wx --branch feature/api codex
wx --branch server=feature/api --branch web=feature/ui claude
```

Arguments following `claude` or `codex` are passed through unchanged.
`wx status`, `wx doctor`, `wx sessions`, and `wx gc --dry-run` expose daemon
state and diagnostics.

The user-level Claude Code and Codex hooks should call these commands only when
`WX_SESSION_ID` is present:

```text
wx hook session-start
wx hook user-prompt-submit
wx hook pre-tool-use
wx hook session-end
```

The hooks bind native agent session IDs, gate prompts and tools until the
workspace is ready, and send a short release RPC. Hook configuration remains
outside this repository so it can be managed with the rest of the user's agent
configuration.

## Configuration

Run `wx config` to show the effective configuration and all scalar keys. Update
a scalar without expanding the sparse YAML file:

```sh
wx config retention.hot_standby 168h
```

Complex workspace and repository overrides are edited in
`~/.config/wx/config.yaml`. Configuration is strictly decoded; an invalid
reload leaves the last valid daemon configuration active. Paths expand `$HOME`
only—`~` and arbitrary environment variables are rejected.

Recovery snapshots are stored as protected Git objects and refs in the source
repository. They can contain staged, unstaged, and non-ignored untracked
content and are readable by processes with access to that repository.

## Development

Run `make setup` once to install the pinned development tools, then `make ci`
for the local quality gate. Security, SBOM, and mutation checks are paused in
the initial phase and are not part of `make setup`, `make ci`, or the Git hooks.
They remain available as manual, explicit opt-in targets; each target installs
its paused tool only when you invoke that target:

```sh
make security-local             # govulncheck, dependency, gosec, license, secrets
make workflow-security-audit    # zizmor
make sbom
make mutation-check             # changed core packages
make mutation-full-check        # all core packages
```

Do not re-enable these checks in CI, workflows, or hooks without the user's
explicit permission. Run `make hooks-install` to enable the bounded
pre-commit and pre-push checks.
