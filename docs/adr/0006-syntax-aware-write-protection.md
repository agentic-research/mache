# 6. Syntax-Aware Write Protection (Strict Mode)

Date: 2026-02-13

## Status

Proposed

## Context

When AI Agents (or humans) refactor code via Mache, they modify the virtual `source` files exposed by the filesystem. Mache splices these edits back into the original source code.

However, Agents often introduce syntax errors (e.g., missing braces, invalid keywords).
Currently, Mache accepts these writes blindly. The file on disk becomes syntactically invalid. The user only discovers this when they run the compiler/interpreter later.

For an autonomous agent loop, this delayed feedback is fatal. The Agent thinks it succeeded ("I wrote the file"), but it actually broke the build.

## Decision

We will implement a **Syntax-Aware Write Protection** mechanism, optionally enabled via a `--strict` flag.

1.  **Intercept Write:** When a `Write` (or `Release`/`Close`) operation occurs on a `source` file, Mache buffers the new content.
2.  **Incremental Parse:** Mache invokes Tree-sitter to parse the *proposed* new content.
3.  **Error Check:** We traverse the resulting AST for `ERROR` or `MISSING` nodes.
4.  **Gate:**
    *   **If Valid:** The write is committed to disk (spliced).
    *   **If Invalid:** The write is **rejected**. The filesystem operation returns an error code (e.g., `EIO` or `EINVAL`).

## Consequences

### Positive
*   **Immediate Feedback:** The Agent receives an error *during the write operation*. It knows immediately that its code is broken and can self-correct ("I failed to write, let me fix the syntax").
*   **Repo Integrity:** The codebase remains syntactically valid (mostly) at all times.
*   **Trust:** Users trust Mache not to corrupt their files.

### Negative
*   **Performance:** Parsing on every write adds latency (though Tree-sitter is fast).
*   **Editor Compatibility:** Some editors save intermediate (broken) states. If Mache rejects them, the editor might complain "Unable to save".
    *   *Mitigation:* Strict Mode should be optional or default-off for humans, default-on for Agents.
*   **Error Reporting:** Standard POSIX file I/O doesn't allow returning a complex error message ("Missing brace at line 10"). It only returns an integer error code.
    *   *Mitigation:* Mache could write the error details to a sidecar file (e.g., `/mnt/Calculate/error.log`) that the Agent can read if the write fails.

## Implementation Strategy

1.  Update `MacheFS.Write` / `Release` to trigger validation.
2.  Use `sitter.Parser.Parse` on the buffer.
3.  Walk tree to check for `n.HasError()`.
4.  Return `fuse.EIO` if error found and strict mode is on.
