# Multi-Context Management in Rune

Rune now supports multiple named contexts, allowing you to manage connections to different Rune servers and easily switch between them. This guide explains how to use the new context management system.

## Overview

A context in Rune represents a connection to a specific Rune server with authentication credentials and optional namespace settings. You can have multiple contexts configured and switch between them as needed.

## Context Structure

Each context contains:
- **Server**: The URL of the Rune server (e.g., `https://prod-server.com:7861`)
- **Token**: Authentication token for the server
- **Namespace**: Optional default namespace for the context

## Commands

### 1. Login Command (Enhanced)

The `login` command now supports named contexts and is a shortcut for `config set-context`:

```bash
# Login to default context
rune login --token "your-token"

# Login to a named context
rune login production --server https://prod-server.com:7861 --token "prod-token"

# Login with namespace
rune login staging --server https://staging-server.com:7861 --token "staging-token" --namespace staging
```

**Flags:**
- `--server`: Server URL (defaults to `http://localhost:7861`)
- `--token`: Authentication token
- `--token-file`: Path to file containing the token
- `--namespace`: Optional default namespace

### 2. Config Management Commands

#### View Current Configuration
```bash
rune config view
```
Shows the current context and its settings.

#### Set/Update Context
```bash
# Set default context
rune config set-context --server https://server.com:7861 --token "token123"

# Set named context
rune config set-context production --server https://prod.com:7861 --token "prod-token"

# Update existing context
rune config set-context production --namespace prod-namespace
```

#### Switch Contexts
```bash
rune config use-context production
```
Changes the current context to the specified one.

#### List All Contexts
```bash
rune config list-contexts
```
Shows all configured contexts with the current one marked with `*`.

#### Delete Context
```bash
rune config delete-context staging
```
Removes a context (cannot delete the current context).

## Usage Examples

### Scenario 1: Development and Production

```bash
# Set up development context
rune login dev --server http://localhost:7861 --token "dev-token" --namespace dev

# Set up production context
rune login prod --server https://prod.company.com:7861 --token "prod-token" --namespace production

# List contexts
rune config list-contexts

# Switch to production
rune config use-context prod

# Verify current context
rune config view

# Switch back to development
rune config use-context dev
```

### Scenario 2: Multiple Environments

```bash
# Set up staging environment
rune config set-context staging --server https://staging.company.com:7861 --token "staging-token" --namespace staging

# Set up QA environment
rune config set-context qa --server https://qa.company.com:7861 --token "qa-token" --namespace qa

# Set up production environment
rune config set-context production --server https://prod.company.com:7861 --token "prod-token" --namespace production

# List all environments
rune config list-contexts

# Deploy to staging
rune config use-context staging
rune cast service.yaml

# Deploy to production
rune config use-context production
rune cast service.yaml
```

### Scenario 3: Team Collaboration

```bash
# Set up your personal development context
rune login personal --server http://localhost:7861 --token "personal-token" --namespace personal

# Set up shared team context
rune login team --server https://team-server.com:7861 --token "team-token" --namespace team

# Work on personal project
rune config use-context personal
rune cast personal-service.yaml

# Switch to team project
rune config use-context team
rune cast team-service.yaml
```

## Configuration File

Contexts are stored in `~/.rune/config.yaml` with this structure:

```yaml
current-context: production
contexts:
  default:
    server: http://localhost:7861
    token: default-token-123
    namespace: default
  production:
    server: https://prod-server.com:7861
    token: prod-token-456
    namespace: prod
  staging:
    server: https://staging-server.com:7861
    token: staging-token-789
    namespace: staging
```

## Best Practices

1. **Use descriptive names**: Name contexts after environments (e.g., `prod`, `staging`, `dev`) or teams
2. **Namespace organization**: Use namespaces to organize resources within each context
3. **Token security**: Store sensitive tokens in files and use `--token-file` flag
4. **Regular cleanup**: Remove unused contexts to keep configuration clean
5. **Documentation**: Document context purposes for team members

## Migration from Single Context

If you have an existing single-context configuration, it will automatically be converted to the new format:

- Your existing configuration becomes the `default` context
- The `default` context becomes the current context
- All existing functionality continues to work

## Troubleshooting

### Context Not Found
```bash
Error: context 'production' does not exist. Use 'rune config set-context production' to create it first
```
**Solution**: Create the context first using `rune config set-context` or `rune login`.

### Cannot Delete Current Context
```bash
Error: cannot delete current context 'production'. Switch to a different context first
```
**Solution**: Switch to a different context first, then delete the unwanted one.

### Permission Denied
```bash
Error: failed to write config file
```
**Solution**: Check file permissions on `~/.rune/` directory or use `--config` flag to specify a different location.

## Related Commands

- `rune whoami` - Shows current context information, authenticated subject, and the connected server's version (when reachable)
- `rune version` - Shows client and server build/version info (`--client` to skip the server probe)
- `rune config view` - Detailed context information
- `rune config list-contexts` - List all available contexts

## See Also

- [Getting Started Guide](getting-started.md)
- [Service Definitions](service-definitions.md)
- [CLI Commands Reference](../cli-commands.md)
