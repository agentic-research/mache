# Git hook templates

These files contain the **rsry-managed shell logic** that `rsry hooks install`
splices into the repository's `post-push` and `post-merge` hooks.

They are deliberately **not standalone runnable scripts** — they contain
just the body content. `rsry hooks install` wraps each block in a
`#!/bin/sh` shebang plus the marker comments below before writing it to the
real hooks dir.

The hooks dir is resolved at install time via `git rev-parse --git-path
hooks`, so worktrees, submodules, and `core.hooksPath` overrides all route
to the right place — it is **not** always `.git/hooks/`.

## Layout

- `post-push` — body shell logic that runs after `git push`. Pushes the
  local Dolt beads DB to the configured Dolt remote, best-effort.
- `post-merge` — body shell logic that runs after `git pull` / merge.
  Pulls the latest Dolt beads from the configured Dolt remote, best-effort.

## Marker lines (literal)

The install command wraps the body with these exact lines:

```
# >>> rsry-managed (do not edit between these markers; `rsry hooks install` regenerates) >>>
# <<< rsry-managed <<<
```

On re-install, only the content between the markers is regenerated — any
user-written hook content outside the markers is preserved. This means
rsry's hooks coexist with custom team hooks without clobbering them. A
unit test (`readme_documents_actual_marker_lines`) keeps this section in
sync with the constants in `src/main.rs`.

To customize: edit the installed hook (find the path via `rsry hooks
status`) and put your custom logic **outside** the marker block.
Reinstalling will only touch the marked section.

To inspect: `rsry hooks status` reports whether each hook is installed,
whether it carries the rsry markers, and where the canonical hooks
directory lives for this repo.
