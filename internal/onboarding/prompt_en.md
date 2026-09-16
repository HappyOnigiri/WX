# Fix this repository's wx worktree setup

wx performed its first setup check for `{{.Workspace}}` in the leased worktree `{{.SlotPath}}` and found the results below.

Do not carry out this request inside a session that wx already started in that leased worktree.
Work from each source repository's main checkout when editing `.worktreeinclude`, `.worktreelink`, hooks, or wx configuration.
Run build and test commands in a fresh wx worktree with `wx run <command>`.

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

Use `.worktreeinclude` for untracked files that each slot needs independently.
A `.worktreelink` entry shares the same object across slots and the source repository, so writes from a slot reach the source checkout.
Only propose link candidates and ask the user before adding them.

Finish by running `{{.RecheckCommand}}`. The setup is complete only when it exits successfully without problem or unchecked findings.
