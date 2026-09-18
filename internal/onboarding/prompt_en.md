# Review this repository's wx worktree setup

wx performed its first setup check for `{{.Workspace}}` in the leased worktree `{{.SlotPath}}`.
Even when the automatic checks pass, verify the repository-specific build, test, lint, and dependency setup.

This request can be carried out inside a session started in the leased worktree above.
In that case, run build and test commands in the current worktree and treat each source repository's main checkout as read-only.
If `.worktreeinclude`, `.worktreelink`, hooks, or wx configuration must change on the source side, present the proposed changes and ask the user first.

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

If source-side changes are needed, run `{{.RecheckCommand}}` after the user applies them.
The setup is complete only when it exits successfully without problem or unchecked findings.
