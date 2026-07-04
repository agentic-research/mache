# `find-smells` composite action

Runs the [mache](https://github.com/agentic-research/mache) structural smell
**ratchet gate** over your checkout: it fails only on findings *above* a
committed baseline (existing debt is grandfathered), and uploads all findings
to your repo's code-scanning tab as SARIF.

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

## Inputs

| Input           | Default                    | Description                                                                                                                                             |
| --------------- | -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `mache-version` | `v0.13.0`                  | Release tag for the `mache-linux-amd64` binary. >= v0.13.0 auto-provisions leyline (all 10 rules / LLO); >= v0.12.0 is SARIF + the 5 tree-sitter rules. |
| `schema`        | *(FCA infer)*              | Path to a mache topology schema.                                                                                                                        |
| `baseline`      | `docs/smell-baseline.json` | Committed ratchet floor.                                                                                                                                |
| `fail-on-new`   | `true`                     | Fail the job on new findings; `false` = advisory.                                                                                                       |
| `upload-sarif`  | `true`                     | Emit + upload SARIF to code-scanning.                                                                                                                   |
