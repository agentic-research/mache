---
name: authoring-smell-rules
description: Add a find_smells rule to mache — a SQL query over the projected code graph that surfaces a code smell (duplication, complexity, dead code, drift). Use when adding or debugging a SmellRule in cmd/smell_rules.go or an external $MACHE_SMELL_RULES_DIR rule. Covers the SmellRule struct, the standalone-vs-_ast table capability split, severity/polarity, tags, and the adversarial-fixture test discipline.
allowed-tools: Read,Glob,Grep,Edit,Write,Bash(go test *),Bash(task *),Bash(./mache *),mcp__mache__find_smells,mcp__mache__search
argument-hint: <rule_id> [language] [must-hold|must-not-hold]
version: 0.1.0
author: ART Ecosystem (mache)
---

<!-- Provenance: mache beads mache-444483, mache-187b22, mache-185791, mache-c7421e (polarity), mache-96341d (allowlist), ADR-0018. Engine: cmd/smell_rules.go, cmd/smell_findings.go. -->

# /mache:authoring-smell-rules — Add a find_smells rule

A smell rule is a **SQL query over mache's projected graph tables** plus metadata. Rules live in `smellRegistry` in `cmd/smell_rules.go` (built-ins) or in `$MACHE_SMELL_RULES_DIR` (external, appended at init). Each rule is one `SmellRule`:

```go
SmellRule{
    ID:          "magic_int_in_comparison", // stable; the MCP/CLI argument
    Languages:   []string{"go"},            // match _source.language; empty = any
    Description:  "...",                     // shown in the rules listing
    Requires:    []string{"_ast", "nodes"}, // tables the query reads (capability gate)
    ScopeColumn: "lit.source_id",            // SQL expr compared to source_id; "" disables scoping
    Query: `SELECT source_id, node_id, start_byte, end_byte, start_row, start_col, <metric> AS metric
            FROM ... WHERE ... %s`,          // %s = optional injected scope clause
    DefaultMinMetric: 10,                    // threshold when caller omits min_metric (0 = return all)
    Severity:    SeverityWarn,               // off | warn | error
    Tags:        []string{"complexity"},     // free-form; CLI --tags selection (cap 3-5)
}
```

### The query contract

Return columns **in this order**: `source_id, node_id, start_byte, end_byte, start_row, start_col, metric`. The `%s` placeholder receives the scope clause (so a rule can be run against one file). `metric` drives sorting (desc) and `min_metric` filtering — use `0 AS metric` for non-metric rules.

## ⚠ The capability split (`Requires`) — get this right

What tables exist depends on who built the `.db`:

- **standalone mache** emits: `nodes`, `node_refs`, `node_defs`.
- **ley-line-open–built `.db`** additionally has: `_ast`, `_source`, `_imports`, `_lsp*`.

A rule that `Requires: ["_ast"]` silently can't run on a standalone build — 4 of the 9 built-ins (complexity, long_function, long_file, magic_int) are `_ast`-only for exactly this reason (ADR-0012). **Prefer `nodes`/`node_defs`/`node_refs` if you want the rule to run everywhere; only reach for `_ast`/`_source` when the smell genuinely needs syntax.** List every table the query reads in `Requires` so agents can tell upfront whether it'll run.

## Polarity — must-NOT-hold vs must-hold

Most rules are **must-not-hold**: a match IS the smell (duplicate def, magic int, dead code). Some are **must-hold** invariants where ABSENCE is the smell (mache-c7421e) — those are inverted: the query finds violations of a property that should always be true. Decide polarity first; it changes whether your `WHERE` selects the bad thing or the missing-good thing.

## Severity & gating (ADR-0018)

- `off` — defined but emits nothing (ship-disabled in a pack).
- `warn` — emits findings, exit 0 (observability default; the zero value).
- `error` — emits findings, exit 1 (author asserts this drift must not ship).
  The rule states *what's true*; the **gate decides whether truth is fatal** at CLI via `--fail-on` / `--tags`. Don't encode CI-vs-precommit in the rule — that emerges from `(--tags × --fail-on)` profiles.

## Procedure

1. **Find prior art.** `grep -n 'ID:' cmd/smell_rules.go` — copy the closest rule (e.g. the two newest: divergent-default dups `mache-187b22`, task-usage drift `mache-185791`).
1. **Write the query** against the minimal table set; verify column order; put the scope clause as trailing `%s`.
1. **Set metadata**: `Requires` (every table touched), `Severity`, 1–3 `Tags`, `DefaultMinMetric` if metric-bearing.
1. **TEST with adversarial fixtures** (non-negotiable, \[[feedback_happy_path_not_enough]\]): a fixture that SHOULD trip the rule AND one that should NOT (boundary). Assert counts on both. A rule with only a happy-path test is not done.
1. **Run it**: `./mache find_smells --rule <id> <db>` (or `mcp__mache__find_smells`), and `task smells` for the self-scan.
1. **Document the trade-offs** in `Description` — exclusions, why certain categories are skipped (see the dup-defs rule for the gold-standard exclusion rationale).

## Red flags

- Query reads `_ast` but `Requires` omits it → silently empty on standalone builds.
- Wrong column count/order → findings render garbage or crash `smell_findings.go`.
- Only a happy-path fixture → false confidence; add the negative/boundary case.
- A long tail of trivial low-metric rows → set `DefaultMinMetric`.
- More than ~5 tags → clippy-group explosion; trim.

## Definition of done

Rule registered, query returns the 7-column contract, `Requires` lists every table, severity/tags set, **both positive and negative fixtures pass**, and `./mache find_smells --rule <id>` produces the expected findings on a real `.db`.

> If this rule feels like it wants to be its own tool/action (SARIF, GH code-scanning), that's tracked separately — `mache-445887`.
