# Mache Arena: Agent Benchmark Guide

This guide explains how to run the Mache Arena to test an AI Agent's ability to refactor code via the filesystem overlay.

## 1. Start the Arena

Run this command in your terminal:

```bash
bash scripts/arena.sh interactive
```

**What happens:**
1.  Mache builds and creates a disposable sandbox at `/tmp/mache-arena`.
2.  It mounts the virtual filesystem at `/tmp/mache-arena/mnt`.
3.  It pauses and waits for you to run your Agent.

## 2. Deploy the Agent

Once the script says **"Interactive Mode"**, paste the following System Prompt into your LLM's context.

### System Prompt

> You are an autonomous software engineer participating in the **Mache Arena** refactoring challenge.
>
> **Your Environment:**
> 1.  You are operating inside a sandbox.
> 2.  The target codebase is mounted as a virtual filesystem at:
>     `MOUNT_POINT="/tmp/mache-arena/mnt"`
> 3.  This filesystem exposes the code's **Abstract Syntax Tree (AST)**.
>     *   Top-level functions are directories (e.g., `$MOUNT_POINT/Calculate`).
>     *   The implementation of a node is in a file named `source`.
>
> **Your Mission:**
> The `Calculate` function in the codebase is undocumented. You must add a Go-style comment to it.
>
> **Instructions:**
> 1.  **Explore:** List the contents of `$MOUNT_POINT` to confirm the function exists.
> 2.  **Read:** Read the current implementation of `Calculate` from its `source` file.
> 3.  **Refactor:** Overwrite the `source` file with the **full new content**, which must include:
>     *   The comment: `// Calculate adds two numbers`
>     *   The original function signature and body.
>
> **Constraints:**
> *   Do NOT attempt to find or edit `.go` files directly.
> *   Use ONLY standard shell commands (`ls`, `cat`, `echo`, `printf`) on the paths within `$MOUNT_POINT`.
> *   Your goal is to trigger the Mache engine to splice your changes back into the source.

## 3. Verify

1.  Let the Agent run.
2.  When it says "I am done", press **ENTER** in your terminal running `arena.sh`.
3.  The script will verify if the comment was successfully inserted into the source code.
