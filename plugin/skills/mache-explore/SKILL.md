---
name: mache-explore
description: Mount a mache FUSE filesystem inside the project directory so agents can explore structured data without per-file permission prompts.
---

# Mache Explore - FUSE Mount for Agent Access

Mount a mache data source inside the project directory so all file reads are auto-approved by Claude Code's permission model. Use this when you need direct filesystem access to mache-projected data (e.g., for Grep, Glob across the mount).

For most tasks, prefer the MCP tools (`list_directory`, `read_file`, `search`, `find_callers`, `find_callees`, `get_communities`) which are available automatically via the mache plugin. Use this FUSE skill only when you need bulk filesystem access.

## Arguments

$ARGUMENTS

Arguments format: `<schema> <data-source> [mount-name]`

- **schema**: Path to a mache schema JSON file (e.g., `examples/go-schema.json`)
- **data-source**: Path to data (directory, `.db` file, etc.)
- **mount-name**: Optional name for the mount point (default: `default`)

Examples:
```
/mache:mache-explore examples/go-schema.json .
/mache:mache-explore examples/nvd-schema.json ~/.agentic-research/venturi/nvd/results/results.db nvd
```

## Workflow

### Phase 1: Parse Arguments

Extract from `$ARGUMENTS`:
- `SCHEMA` = first argument (required)
- `DATA_SOURCE` = second argument (required)
- `MOUNT_NAME` = third argument, or `default` if not provided

If fewer than 2 arguments, ask the user for the missing values.

### Phase 2: Validate Inputs

```bash
MACHE_BIN=$(which mache 2>/dev/null) && echo "Found: $MACHE_BIN" || echo "NOT_FOUND"
```

If not found, ask the user for the full binary path. Then learn the CLI:

```bash
"$MACHE_BIN" --help 2>&1 | head -20
```

Use the `--help` output to determine correct flags. Do NOT assume flag names.

Validate schema and data source exist:
```bash
ls -la "$SCHEMA"
ls -la "$DATA_SOURCE"
```

### Phase 3: Mount

```bash
mkdir -p .mache-mount/$MOUNT_NAME
grep -q '^\.mache-mount/' .gitignore 2>/dev/null || echo '.mache-mount/' >> .gitignore
"$MACHE_BIN" <flags from --help> .mache-mount/$MOUNT_NAME &
echo $! > .mache-mount/.pid
echo "mache PID: $(cat .mache-mount/.pid)"
```

Wait for readiness (up to 10 seconds):
```bash
for i in $(seq 1 10); do
  if ls .mache-mount/$MOUNT_NAME/ 2>/dev/null | head -1; then
    echo "Mount ready"
    break
  fi
  sleep 1
done
```

### Phase 4: Explore

Data is mounted. Use `ls`, `Read`, `Grep`, `Glob` freely — no permission prompts.

## Unmounting

```bash
if [ -f .mache-mount/.pid ]; then
  kill "$(cat .mache-mount/.pid)" 2>/dev/null
  rm .mache-mount/.pid
fi
umount .mache-mount/$MOUNT_NAME 2>/dev/null || diskutil unmount .mache-mount/$MOUNT_NAME 2>/dev/null || fusermount -u .mache-mount/$MOUNT_NAME 2>/dev/null
```

## Notes

- Mount lives inside project dir = auto-approved reads
- `.gitignore` keeps mount artifacts out of version control
- Multiple mounts supported via different `mount-name` values
- On macOS uses fuse-t; on Linux uses FUSE3
- Self-mount (data source = `.`) is safe — mache skips dot-prefixed directories
