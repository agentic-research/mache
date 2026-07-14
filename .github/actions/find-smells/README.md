# `find-smells` composite action

Runs the [mache](https://github.com/agentic-research/mache) structural smell
**ratchet gate** over your checkout: it fails only on findings *above* a
committed baseline (existing debt is grandfathered), and uploads all findings
to your repo's code-scanning tab as SARIF.

New to mache smell rules entirely? Start with
[`examples/smell-rules/README.md`](../../../examples/smell-rules/README.md) —
it explains what a rule is, how the baseline/ratchet model works, and how to
wire this action, in one read.

## Usage

```yaml
permissions:
  contents: read
  security-events: write   # required for upload-sarif

jobs:
  smells:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@<sha>
      - uses: agentic-research/mache/.github/actions/find-smells@<sha>
        with:
          baseline: docs/smell-baseline.json
```

## First run — bootstrap the baseline

The gate needs a committed baseline. Generate one once and commit it:

```bash
mache build . smells.db
mache find-smells --db smells.db --rule '*' --limit 100000 \
  --baseline-root "$PWD" --write-baseline docs/smell-baseline.json
```

Without it the action fails with the exact command above.

`docs/smell-baseline.json` is the single documented default baseline path —
the action input, this README, and mache's own Taskfile `smells` gate all
agree on it. If you keep your baseline elsewhere, pass the `baseline` input.

## Inputs

| Input           | Default                    | Description                                                                                                                                                                                                          |
| --------------- | -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `mache-version` | `v0.16.2`                  | Release tag for the `mache-linux-amd64` binary. >= v0.13.0 auto-provisions leyline, so the `_ast`-backed rules run too; >= v0.12.0 is SARIF + the cross-reference rules only. Absent-table rules skip automatically. |
| `schema`        | *(FCA infer)*              | Path to a mache topology schema. Setting it forces `--backend tree-sitter` (leyline ignores build-time schemas).                                                                                                     |
| `baseline`      | `docs/smell-baseline.json` | Committed ratchet floor (see above).                                                                                                                                                                                 |
| `fail-on-new`   | `true`                     | Fail the job on new findings; `false` = advisory.                                                                                                                                                                    |
| `upload-sarif`  | `true`                     | Emit + upload SARIF to code-scanning.                                                                                                                                                                                |

## Contract with mache's own gate

The action's core invocation
(`mache build`, then `find-smells --rule '*' --limit 100000 --baseline-root "$PWD" --baseline <path>`) deliberately mirrors the `smells`
target in mache's own [Taskfile](../../../Taskfile.yml) — the gate a mache PR
runs is the gate you consume. `cmd/find_smells_action_test.go`
(`TestFindSmellsAction_TaskfileParity`) asserts the two invocation shapes and
the default baseline path stay in sync, so drift fails mache's CI rather than
surprising a consuming repo.

Two intentional differences from mache's own gate (mache-608a3c
consolidated it onto a single leyline build + single baseline, so the
builds now match):

1. mache's Taskfile gate scopes to `--tags=gate` — the ratchet rule set,
   excluding the min-0 firehose rules (`cyclomatic_complexity`,
   `magic_int_in_comparison`). The action runs `--rule '*'` because its
   pinned release predates the `gate` retag of the embedded rules;
   `--tags=gate` will be adopted when the default `mache-version` is
   bumped to a release that ships them (the parity test encodes this
   reconvergence condition).
1. mache's gate builds from the tracked tree (`git archive HEAD`); the
   action builds from the checkout, which is equivalent on a clean CI
   checkout.

Bootstrap the baseline with the same mache version the action runs and
the two always agree.
