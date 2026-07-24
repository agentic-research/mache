# Contributing to Mache

Mache is an early-stage research project. Contributions are welcome, but please note that the API is still evolving rapidly.

## Getting Started

```bash
git clone https://github.com/agentic-research/mache.git
cd mache
task test
```

### Prerequisites

- Go 1.26+
- [Task](https://taskfile.dev) (build runner)
- Network access to download Mache's exact SHA-pinned
  [ley-line-open](https://github.com/agentic-research/ley-line-open) release.
  Leyline is Mache's source parser, not an optional test integration.

`task test` provisions the published Leyline artifact into
`~/.mache/bin/leyline` before running. The cache is refreshed when its version
does not match Mache's pin, even if a same-version developer build exists on
`PATH`.

## Development Workflow

```bash
task fmt       # Format code (gofumpt)
task vet       # Static analysis
task lint      # All linters: Go, docs, Actions, YAML, and structural smells
task test      # Provision the pinned Leyline release, then run all tests
task check     # Mutating development gate (formats, lints, and tests)
task ci        # Non-mutating CI-equivalent gate used by the pre-push hook
```

When adopting a new Leyline release, run `task leyline:adoption`. It downloads
the pinned platform asset, verifies its published SHA-256 and `--version`, then
runs the Leyline-facing consumer suites uncached. The network-dependent release
check is deliberately explicit; routine `task ci` blocks on the deterministic
consumer smoke without depending on GitHub release uptime.

## Submitting Changes

1. **Fork** the repo and create a feature branch.
1. **Make your changes**. Add tests for new functionality.
1. **Run `task check`** and ensure it passes.
1. **Commit your changes**. We use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) messages (e.g., `fix: ...`, `feat: ...`, `docs: ...`).
1. **Open a pull request** with a clear description of the change.

## Scope of Contributions

We welcome bug fixes, documentation improvements, and small feature additions.

For large features or architectural changes, **please open an issue first** to discuss the proposal. Drive-by PRs with thousands of lines of code may be declined if they don't align with the project roadmap.

## Code Style

- Go standard conventions apply.
- Code is formatted with [gofumpt](https://github.com/mvdan/gofumpt).
- Pre-commit hooks enforce formatting and linting — install them with `pre-commit install`.
- Pre-push runs `task ci`; unchanged Go package results remain eligible for
  Go's test cache even with `-v`.

## Reporting Issues

Open an issue on GitHub. Include steps to reproduce, expected behavior, and actual behavior.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
