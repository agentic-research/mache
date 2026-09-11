# Golden projections

`<fixture-id>.tsv` is the normalized, sorted dump of a fixture's projection db —
every row of `nodes`, `node_refs`, `node_defs`, `file_index` and
`_index_coverage`, minus the per-build columns (mtime, mod_time, indexed_at),
with the fixture's absolute path rewritten to `$FIXTURE` and content columns
Go-quoted onto one line. `TestGoldenProjection` in
`internal/testfixtures/golden_test.go` fails on any byte that differs
(mache-c0537f gate 1) and prints the rows that vanished / appeared. The dump
format is `DumpProjection` in `internal/testfixtures/golden.go`.

Regenerate — and READ the printed added/removed summary; commit the new golden
in the same PR as the change that moved it:

```
go test ./internal/testfixtures -run TestGoldenProjection -update -v
```

## small-go-golden

A hand-written Go corpus at `testdata/snapshots/small-go-golden/`. It is NOT a
snapshot of a real repo — every construct is there because it exercises one
projection path:

| File                  | Exercises                                                                                            |
| --------------------- | ---------------------------------------------------------------------------------------------------- |
| `main.go`             | package funcs, std + intra-module imports, cross-package calls, `&x` address refs                    |
| `generics.go`         | methods on generic receivers, one with two type parameters (`mache-51571b`)                          |
| `tool.go`             | `init()` contested with `tool/main.go` (see below), consts, vars, package-level helper               |
| `tool/main.go`        | same package name in a subdirectory — dedup suffix depends on projection ORDER                       |
| `store/store.go`      | struct + interface types, pointer- and value-receiver methods, `init()`                              |
| `store/index.go`      | second `init()` in the same package (`.from_<file>` suffix), method → package-func call              |
| `scripts/gen.py`      | a language the Go preset has no nodes for → routed to `_project_files`                               |
| `README.md`, `go.mod` | non-source files → `_project_files` (the README is one line on purpose: its bytes are in the golden) |

`tool.go` and `tool/main.go` both declare `init()` in package `main`. Which one
gets the bare id `main/functions/init` depends on which is projected first:
lexical order over the full path puts `tool.go` before `tool/main.go`, WalkDir's
per-directory order puts `tool/` first. The golden pins the lexical answer
(`ingestSourceTree`'s explicit sort), so a change that silently renames nodes
trips the gate. `TestProjectionInvariants` pins the same facts by name, as a
backstop against a regeneration that quietly accepted a wrong golden.

### Known defects the golden currently pins

A golden pins what the projection DOES, not what it should do. Two rows in the
current golden are bugs, filed on the first run of the gate; their fix PRs must
regenerate the golden and show exactly those rows leaving:

- `store/methods/string.Add` — a phantom method node keyed by the type of
  `Add`'s *parameter*: the ASTWalker drops the `receiver:` field label
  (mache-91d903; 445 such nodes on mache-self).
- `main/imports/"golden/store"` has `parent_id = main/imports/"golden`, which is
  not a node: the slash in the import path splits the id (mache-94f571; 1939
  such nodes on mache-self).
