# Review this repository's wx worktree setup

wx performed its first setup check for `{{.Workspace}}` in the leased worktree `{{.SlotPath}}`.
Even when the automatic checks pass, verify the repository-specific build, test, lint, and dependency setup.

This request can be carried out inside a session started in the leased worktree above.
In that case, run build and test commands in the current worktree.
Treat tracked files, HEAD, and the index in each source repository as read-only.
However, this task authorizes you to update `.worktreelink`, `.worktreeinclude`, and that repository's `.git/info/exclude` in the main checkout.
Apply those settings automatically without asking the user whenever the repository contains enough information to decide safely.

## Findings

{{range .Findings}}

- **{{severity .Severity}} / {{.Check}}** — {{.Summary}}{{if .Target}}
  - target: `{{.Target}}`{{end}}{{if .Cause}}
  - cause: {{.Cause}}{{end}}{{if .Action}}
  - action: {{.Action}}{{end}}
{{end}}

## Repositories

{{range .Repositories}}

- `{{.RelativePath}}`: main checkout `{{.MainPath}}`, checked slot path `{{.SlotPath}}`
{{end}}

Read README, package.json, Makefile, and similar project files to determine the build, test, lint, and dependency setup this repository expects.
Make those commands work in a wx worktree.
Do not guess missing files from wx's findings alone; compare the main checkout with the leased worktree.

Prefer `.worktreelink` for required untracked paths.
A `.worktreelink` entry shares the same object across the main checkout and every slot.
Use `.worktreeinclude` only when sharing one object would cause conflicts, corruption, or unintended state propagation and each slot therefore needs its own copy.
Never put the same path in both manifests, and do not add tracked paths to either manifest.

Use `git check-ignore` to verify that each manifest and link target is ignored by Git.
If `.worktreelink` or `.worktreeinclude` itself is not ignored, add `/.worktreelink` or `/.worktreeinclude`, respectively, to that repository's `.git/info/exclude` without duplicating entries.
If a link target is not ignored, add a narrowly scoped root-relative rule for it to the same exclude file.
Resolve the file to edit with `git rev-parse --git-path info/exclude` so this also works from linked worktrees.

Past setup attempts exposed the following problems. Check that the resulting configuration avoids them:

- Sharing `node_modules`, `vendor`, virtual environments, or build caches between worktrees can let absolute-path metadata, installs, and cleanup corrupt another worktree.
  Do not link or include these directories; use the repository's existing prepare command or hook to recreate them per slot when necessary.
- An ignore rule with a trailing `/` may not match a directory link target before that path exists in the destination.
  Use a root-relative rule without the trailing slash, such as `/tmp`, and verify that `git check-ignore` succeeds for both source and slot paths.
- In a multi-repository workspace, put each repository's manifests at the root of that repository's main checkout. Manifests in nested directories inside a repository are not loaded.
- Tracked files already come from checkout and should not remain in a manifest. Past stale include entries for newly tracked files such as `AGENTS.md` caused worktree preparation failures.

After updating the settings, run `{{.RecheckCommand}}` yourself and iterate on the configuration and verification as needed.
The setup is complete only when it exits successfully without problem or unchecked findings.
