# Rune

Rune is a lightweight, powerful orchestration platform designed to simplify the deployment and management of services across environments. It's inspired by Kubernetes and Nomad but focuses on developer simplicity and minimal operational complexity.

## Features

- **Simple deployment**: Deploy services with a single command
- **Multi-service deployments**: Runesets for complex application stacks with templating
- **Multi-environment**: Seamlessly run services across development, testing, and production
- **Lightweight**: Minimal resource footprint
- **Container-native**: First-class support for containerized applications
- **Process-aware**: Run and manage local processes when containers aren't needed
- **Dependency-aware**: Automatically manage service dependencies
- **Multi-node**: Scale across multiple nodes
- **Built-in security**: Authentication, authorization, and encryption
- **Interactive debugging**: Exec into running services for real-time debugging
- **Health monitoring**: Built-in health checks and probes
- **Rollback support**: Version history and instant rollbacks
- **Structured logging**: Comprehensive logging with multiple outputs

## Quick Start

### Prerequisites

- Go 1.19 or later
- Docker (for container-based services)

### Installation

#### Quick Install (Recommended)

```bash
# Install CLI only (for developers)
curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install-cli.sh | bash

# Install complete server environment (recommended for most users)
curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install-server.sh | sudo bash -s -- --version v0.1.0

# Install binary-only (assumes Docker already configured)
curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install.sh | sudo bash -s -- --version v0.1.0

# For automated deployment (cloud-init, CI/CD) - runs as root automatically
curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install-server.sh | bash -s -- --version v0.1.0
```

#### From Source

```bash
# Clone and build
git clone https://github.com/runestack/rune.git
cd rune
make setup
make build

# Install from source
go install github.com/runestack/rune/cmd/rune@latest

# Verify installation
rune version
```

### Upgrade runed

#### Scripted upgrade (recommended)
```bash
# Upgrade runed on the server to v0.0.1-dev.10 (keeps config/data)
curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install-server.sh \
| sudo bash -s -- --version v0.0.1-dev.10 --skip-docker
```

#### Manual binary swap
```bash
VER=v0.0.1-dev.10
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) echo "Unsupported arch"; exit 1 ;; esac

sudo systemctl stop runed
curl -L -o /tmp/rune.tgz "https://github.com/runestack/rune/releases/download/$VER/rune_linux_${ARCH}.tar.gz"
sudo tar -C /usr/local/bin -xzf /tmp/rune.tgz rune runed
sudo systemctl start runed
```

#### Verify
```bash
runed --version
sudo systemctl status runed --no-pager | cat
```

## First Run & Bootstrap

After installing the Rune server (`runed`), you need to bootstrap the system and create your first user. Here are the essential steps:

### 1. Start the Server

```bash
# Start the server (will auto-create runefile.yaml if none exists)
runed

# Or with custom config
runed --config=/path/to/runefile.yaml
```

### 2. Bootstrap the System

From another terminal, bootstrap the system to create the initial admin user:

```bash
# Bootstrap the system and save the token
rune admin bootstrap > ~/rune_bootstrap_token.txt

# This creates the server-admin user and generates a bootstrap token
```

### 3. Login as Server Admin

Use the bootstrap token to authenticate as the server admin:

```bash
# Login using the bootstrap token
rune login server-admin --token-file ~/rune_bootstrap_token.txt

# Verify you're logged in
rune whoami
# Should show: server-admin
```

### 4. Create Additional Users

Now you can create additional users with appropriate policies:

```bash
# Create a regular user with admin policy
rune admin token create --name github-actions --policy readwrite --out-file github_actions_token.txt
rune admin token create --name user123 --policy admin --out-file user123_token.txt

# Create a user with limited permissions
rune admin user create developer --password=devpass
rune admin policy create developer-policy.yaml
rune admin token create --name developer --policy developer-policy --out-file dev_token.txt
```

### 5. Switch to Regular User (Optional)

```bash
# Login as the new user
rune login developer --password=devpass

# Or use the token
rune login developer --token-file dev_token.txt

# Verify the switch
rune whoami
# Should show: developer
```

### Bootstrap Complete!

You now have a fully configured Rune system with:
- ✅ Server running and accessible
- ✅ Initial admin user (server-admin) created
- ✅ Bootstrap token for initial access
- ✅ Additional users and policies configured
- ✅ Ready for service deployment

**Important Security Notes:**
- Keep bootstrap tokens secure - they have full admin access
- Delete bootstrap tokens after creating regular users
- Implement least-privilege policies for production use

## Core Concepts

### Services
Services are the basic unit of deployment in Rune. They can be containers, processes, or any executable application.

```yaml
# service.yaml
service:
  name: "api"
  image: "my-api:latest"
  ports:
    - 8080:80
  scale: 3
  health:
    liveness:
      type: http
      path: /healthz
      port: 8080
```

### Runesets
Runesets are multi-service deployment packages that allow you to deploy complex application stacks as a single unit.

```
runeset/
├── runeset.yaml          # Main manifest
├── casts/                # Service definitions
│   ├── api.yaml         # API service
│   ├── database.yaml    # Database service
│   └── cache.yaml       # Cache service
├── values/               # Environment values
│   ├── dev.yaml         # Development
│   └── prod.yaml        # Production
└── templates/            # Template files
```

### Namespaces
Rune supports namespaces for environment isolation and multi-tenancy.

```bash
# Deploy to specific namespace
rune cast service.yaml -n production

# List services in namespace
rune get services -n staging
```

## Usage Examples

### Basic Service Management

```bash
# Deploy a service
rune cast service.yaml

# Scale a service
rune scale api 5

# Check service status
rune status api

# View logs
rune logs api --follow

# Delete a service
rune delete api
```

### Multi-Service Deployments

```bash
# Deploy entire runeset
rune cast runeset/

# Deploy with specific values
rune cast runeset/ --values=production

# Preview rendered YAML
rune cast runeset/ --render-only
```

### Interactive Debugging

```bash
# Execute bash in running service
rune exec api bash

# Run one-off command
rune exec api ls -la /app

# Set working directory and environment
rune exec api --workdir=/app --env=DEBUG=true python debug.py
```

### Service Operations

```bash
# Restart service (bounce instances)
rune restart api

# Stop service (scale to 0)
rune stop api

# Check health status
rune health api

# Rollback to previous version
rune rollback api
```

### Validation and Quality

```bash
# Validate YAML files
rune lint service.yaml
rune lint runeset/

# Check service dependencies
rune deps api
```

## Authentication & Security

Rune provides built-in authentication and security features.

### User Management

```bash
# Create admin user
rune admin user create admin --password=secret

# Create regular user
rune admin user create developer --password=devpass

# List users
rune admin user list
```

### Policy Management

```yaml
# policy.yaml
name: "developer-policy"
description: "Developer access policy"
rules:
  - resource: "services"
    actions: ["get", "logs", "exec"]
    namespaces: ["dev", "staging"]
```

```bash
# Create policy
rune admin policy create policy.yaml

# List policies
rune admin policy list
```

### API Tokens

```bash
# Generate token for user
rune admin token generate --user=developer

# List tokens
rune admin token list
```

### Authentication Flow

```bash
# Login interactively
rune login

# Login with credentials
rune login --username=developer --password=devpass

# Check current user
rune whoami

# Use token for API access
export RUNE_API_TOKEN="your-token-here"
rune get services
```

## Configuration

### Server Configuration (`runefile.yaml`)

The server configuration file contains registry authentication, Docker settings, and server options:

```bash
# Run with built-in defaults (auto-creates runefile.yaml)
runed

# Use custom server configuration
runed --config=/path/to/runefile.yaml
```

**Configuration locations (in order of precedence):**
1. **`--config` flag** - Explicitly specified file
2. **`./runefile.yaml`** - Local development override
3. **`/etc/rune/runefile.yaml`** - System-wide production config

**Example server configuration:**
```yaml
# runefile.yaml
server:
  grpc_address: ":7863"
  http_address: ":7861"

docker:
  registries:
    - name: "ecr-prod"
      registry: "123456789012.dkr.ecr.us-west-2.amazonaws.com"
      auth:
        type: "ecr"
        region: "us-west-2"

log:
  level: "info"
  format: "json"
  outputs:
    - "console"
    - "file:/var/log/rune.log"
```

### Client Configuration (`$HOME/.rune/config.yaml`)

Client configuration manages connections to Rune servers:

```yaml
# $HOME/.rune/config.yaml
current-context: production
contexts:
  production:
    server: "https://rune.company.com:7863"
    token: "your-auth-token"
    defaultNamespace: "production"
  development:
    server: "localhost:7863"
    token: "dev-token"
    defaultNamespace: "dev"
```

**Client config locations:**
- **`$HOME/.rune/config.yaml`** - User's home directory
- **`$RUNE_CLI_CONFIG`** - Environment variable override

### CLI Configuration

Manage CLI settings and preferences:

```bash
# Set API server
rune config set api-server localhost:8080

# Get configuration
rune config get api-server

# List all settings
rune config list
```

## CI/CD with GitHub Actions

Deploy services automatically using the Rune CLI in GitHub Actions:

```yaml
# .github/workflows/rune-deploy.yml
name: Deploy to Rune
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - name: Deploy to Rune
        run: |
          rune cast runeset/ --values=production
```

See [CI/CD Guide](docs/guides/ci-cd-github-actions.md) for complete setup instructions.

## Development

### Setup Development Environment

```bash
# Clone the repository
git clone https://github.com/runestack/rune.git
cd rune

# Install development dependencies
make setup

# Build the project
make build

# Run tests
make test

# Run specific test types
make test-unit          # Unit tests only
make test-integration   # Integration tests only

# Code quality
make lint               # Run linting
make coverage-summary   # Test coverage report
```

### Key Development Commands

```bash
# Build binaries
make build              # Build rune and runed
make install            # Install to $GOPATH/bin

# Development environment
make dev                # Start development environment
make proto              # Generate protobuf files

# Testing
make test               # All tests
make test-unit          # Unit tests
make test-integration   # Integration tests
make coverage-summary   # Coverage report

# Code quality
make lint               # Linting
make fmt                # Format code
```

### Project Structure

```
rune/
├── cmd/                # Main binaries
│   ├── rune/          # CLI tool
│   └── runed/         # Server daemon
├── pkg/                # Core packages
│   ├── cli/           # CLI framework and commands
│   ├── api/            # API server and client
│   ├── orchestrator/   # Service orchestration
│   ├── runner/         # Service runners (Docker/Process)
│   ├── store/          # State persistence
│   ├── types/          # Core types and YAML parsing
│   ├── crypto/         # Security and encryption
│   ├── log/            # Structured logging
│   └── worker/         # Task execution framework
├── examples/            # Example services and runesets
├── docs/                # Documentation
└── test/                # Integration tests
```

## Architecture

Rune follows a client-server architecture:

- **Control Plane** (`runed`): Manages state, orchestrates services, provides APIs
- **CLI** (`rune`): User interface for service management operations
- **Runners**: Execute services (Docker containers or local processes)
- **Store**: Persistent state using BadgerDB with future etcd support
- **Authentication**: Built-in user management and API security

### Data Flow
```
CLI → Client → API → Orchestrator → Store + Runner
```

## Documentation

For more detailed documentation:

- [User Guide](docs/guides/README.md)
- [API Documentation](docs/api/README.md)
- [Examples](examples/README.md)
- [Development Guide](.cursor/rules/development-guide.mdc)
- [Architecture Guide](.cursor/rules/architecture-guide.mdc)
- [CLI Commands](.cursor/rules/cli-commands.mdc)

## Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for details on:

- Setting up your development environment
- Code style and conventions
- Testing guidelines
- Pull request process

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details. 