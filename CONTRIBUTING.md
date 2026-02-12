# Contributing to Mache

Mache is an early-stage research project. Contributions are welcome, but expect rapid API changes.

## Getting Started

```bash
git clone https://github.com/agentic-research/mache.git
cd mache
task build
task test
```

### Prerequisites

- Go 1.24+
- [Task](https://taskfile.dev) (build runner)
- **macOS:** [fuse-t](https://www.fuse-t.org/) (`brew install --cask fuse-t`)
- **Linux:** libfuse-dev (`apt-get install libfuse-dev`)

## Development Workflow

```bash
task fmt       # Format code (gofumpt)
task vet       # Static analysis
task lint      # golangci-lint
task test      # Run tests
task check     # All of the above
```

## Submitting Changes

1. Fork the repo and create a feature branch.
2. Make your changes. Add tests for new functionality.
3. Run `task check` and ensure it passes.
4. Open a pull request with a clear description of the change.

## Code Style

- Go standard conventions apply.
- Code is formatted with [gofumpt](https://github.com/mvdan/gofumpt).
- Pre-commit hooks enforce formatting and linting — install them with `pre-commit install`.

## Reporting Issues

Open an issue on GitHub. Include steps to reproduce, expected behavior, and actual behavior.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
