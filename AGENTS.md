# Agent Instructions

This project uses **rsry** for bead tracking.

## Quick Reference

```bash
rsry bead --repo . list --status ready
rsry bead --repo . review <id>
rsry bead --repo . comment add <id> "progress note"
rsry bead --repo . close <id>
rsry bead --repo . export --status all --jsonl -o .beads/beads.jsonl
```

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation prompts.

Shell commands like `cp`, `mv`, and `rm` may be aliased to include `-i` (interactive) mode on some systems, causing the agent to hang indefinitely waiting for y/n input.

**Use these forms instead:**

```bash
# Force overwrite without prompting
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file

# For recursive operations
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

**Other commands that may prompt:**

- `scp` - use `-o BatchMode=yes` for non-interactive
- `ssh` - use `-o BatchMode=yes` to fail instead of prompting
- `apt-get` - use `-y` flag
- `brew` - use `HOMEBREW_NO_AUTO_UPDATE=1` env var

<!-- BEGIN BEADS INTEGRATION -->

## Issue Tracking with rsry Beads

**IMPORTANT**: This project uses **rsry beads** for ALL issue tracking. Do NOT
use markdown TODOs, task lists, or external issue trackers.

### Why rsry?

- Dependency-aware: Track blockers and relationships between issues
- Version-controlled: bead content is shared through `.beads/beads.jsonl`
- Agent-optimized: JSON output, ready work detection, discovered-from links
- Prevents duplicate tracking systems and confusion

### Quick Start

**Check for ready work:**

```bash
rsry bead --repo . list --status ready
```

**Create new issues:**

```bash
rsry bead --repo . create "Issue title" -d "Detailed context" -t bug|feature|task -p 0
```

**Review and update:**

```bash
rsry bead --repo . review <id>
rsry bead --repo . comment add <id> "What changed / what remains"
```

**Complete work:**

```bash
rsry bead --repo . close <id>
```

### Issue Types

- `bug` - Something broken
- `feature` - New functionality
- `task` - Work item (tests, docs, refactoring)
- `epic` - Large feature with subtasks
- `chore` - Maintenance (dependencies, tooling)

### Priorities

- `0` - Critical (security, data loss, broken builds)
- `1` - High (major features, important bugs)
- `2` - Medium (default, nice-to-have)
- `3` - Low (polish, optimization)
- `4` - Backlog (future ideas)

### Workflow for AI Agents

1. **Check ready work**: `rsry bead --repo . list --status ready` shows unblocked issues
1. **Review your task**: `rsry bead --repo . review <id>` gives bead context
1. **Work on it**: Implement, test, document
1. **Discover new work?** Create linked issue:
   - `rsry bead --repo . create "Found bug" -d "Details about what was found" -p 1`
1. **Complete**: `rsry bead --repo . close <id>`

### Auto-Sync

Bead content is shared through git:

- The live local store is SQLite (`.beads/beads.db`) and is ignored.
- The shared artifact is `.beads/beads.jsonl`.
- After mutating beads, refresh the export:
  `rsry bead --repo . export --status all --jsonl -o .beads/beads.jsonl`.

### Important Rules

- ✅ Use `rsry bead` for ALL task tracking
- ✅ Keep `.beads/beads.jsonl` refreshed after bead mutations
- ✅ Add progress comments for context that should survive sessions
- ✅ Check ready beads before asking "what should I work on?"
- ❌ Do NOT create markdown TODO lists
- ❌ Do NOT use external issue trackers
- ❌ Do NOT duplicate tracking systems

## Landing the Plane (Session Completion)

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
1. **Run quality gates** (if code changed) - Tests, linters, builds
1. **Update issue status** - Close finished work, update in-progress items
1. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   rsry bead --repo . export --status all --jsonl -o .beads/beads.jsonl
   git push
   git status  # MUST show "up to date with origin"
   ```
1. **Clean up** - Clear stashes, prune remote branches
1. **Verify** - All changes committed AND pushed
1. **Hand off** - Provide context for next session

**CRITICAL RULES:**

- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds

<!-- END BEADS INTEGRATION -->
