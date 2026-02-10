# Mache

**The Universal Semantic Overlay Engine**

Mache provides content-addressed, layered storage for structured data with filesystem views for exploration and querying.

## Features

- **Content-Addressed Storage (CAS)**: Store data once, reference many times via hard links
- **Layered Overlays**: Docker-style layers for versioned, composable data views
- **Filesystem Query Interface**: Navigate data with standard Unix tools (ls, grep, find)
- **SQLite Integration**: Complex queries via virtual tables (planned)
- **Self-Organizing Learned Filesystems**: ML models that externalize representations as navigable directory structures (experimental)

## Quick Start

### Prerequisites

**macOS (Apple Silicon/Intel):**
```bash
# Install fuse-t (userspace FUSE, no kernel extensions required)
brew install --cask fuse-t

# Install Task (build tool)
brew install go-task
```

**Linux:**
```bash
# Install FUSE development headers
# Ubuntu/Debian:
sudo apt-get install libfuse-dev

# Fedora/RHEL:
sudo dnf install fuse-devel

# Install Task
brew install go-task
# or: go install github.com/go-task/task/v3/cmd/task@latest
```

### Building

```bash
# Clone the repository
git clone https://github.com/agentic-research/mache.git
cd mache

# Build (checks for fuse-t on macOS, builds and codesigns)
task build

# Run tests
task test

# See all available tasks
task --list
```

### Using Plain Go Commands

If you prefer not to use Task, you need to set CGO flags manually on macOS:

```bash
# macOS only - set CGO flags for fuse-t
export CGO_CFLAGS="-I/Library/Frameworks/fuse_t.framework/Versions/Current/Headers"
export CGO_LDFLAGS="-F/Library/Frameworks -framework fuse_t -Wl,-rpath,/Library/Frameworks"

# Then use standard go commands
go build
go test ./...
```

Or use the provided `.envrc` file with [direnv](https://direnv.net/):

```bash
# Install direnv
brew install direnv

# Allow the .envrc file
direnv allow

# Now go commands work automatically
go test ./...
```

## Usage

```bash
# Mount a filesystem (example - not yet fully implemented)
./mache /path/to/mountpoint

# With schema and data
./mache --schema vulns.yaml --data nvd-feed.json /mnt/vulns
```

## Architecture

Mache implements several key concepts:

1. **FUSE Bridge**: Uses fuse-t on macOS for userspace mounting (no kernel extensions)
2. **Declarative Schemas**: Define filesystem topology via YAML/JSON configuration
3. **BREAD Foundation**: Built on theoretical framework proving filesystem organization preserves graph properties

See [Architecture Decision Records](docs/adr/) for detailed design rationale:
- [ADR-0001: User-Space FUSE Bridge (fuse-t)](docs/adr/0001-user-space-fuse-bridge.md)
- [ADR-0002: Declarative Topology Schema](docs/adr/0002-declarative-topology-schema.md)
- [ADR-0003: Self-Organizing Learned Filesystem](docs/adr/0003-self-organizing-learned-filesystem.md)

## Use Cases

### Vulnerability Data Aggregation

Replace complex ETL pipelines and SQL databases with organized filesystem views:

```bash
# Different feeds, different layers
vunnel run nvd --output /mache/layers/nvd/$(date +%Y-%m-%d)
vunnel run github --output /mache/layers/ghsa/$(date +%Y-%m-%d)

# Mount unified view
mache mount \
  --layer nvd/2024-02-10 \
  --layer ghsa/2024-02-10 \
  /vulns

# Query with standard tools
ls /vulns/by-severity/critical/
grep -r "nginx" /vulns/by-package/
```

### ML Training Data Organization

Models organize filesystems during training, externalizing learned representations:

```bash
# Model clusters data as it learns
/learned/security-concepts/
  authentication/
    jwt/CVE-2024-1234.json
    oauth/CVE-2024-5678.json

# Training data self-organizes for better locality
# No vector database needed - inference is directory navigation
```

## Development

```bash
# Run tests with coverage
task test-coverage

# Format code
task fmt

# Run linters
task lint

# Run all checks
task check

# Clean build artifacts
task clean
```

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.

## Contributing

This is an early-stage research project. Contributions welcome, but expect rapid API changes.

## Related Work

- **BREAD Paper**: Theoretical foundation for graph→filesystem projection
- **Grype/Vunnel**: Inspiration for vulnerability data use case
- **fuse-t**: Userspace FUSE implementation for macOS
- **cgofuse**: Cross-platform FUSE binding for Go
