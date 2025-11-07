# CI/CD with Rune CLI in GitHub Actions

This guide shows how to use the Rune CLI in GitHub Actions to deploy services automatically.

## Overview

The Rune CLI can be installed in GitHub Actions runners and used to:
- Deploy services from service definitions
- Validate service configurations
- Manage service lifecycle
- Integrate with your CI/CD pipeline

## Quick Start

### 1. Add the Workflow

Copy the `examples/github-actions/rune-deploy.yml` file to your repository's `.github/workflows/` directory:

```bash
mkdir -p .github/workflows
cp examples/github-actions/rune-deploy.yml .github/workflows/
```

### 2. Configure Secrets

In your GitHub repository, go to **Settings → Secrets and variables → Actions** and add:

| Secret Name | Description | Example |
|-------------|-------------|---------|
| `RUNE_SERVER` | Your Rune server address | `https://rune.example.com:7863` |
| `RUNE_TOKEN` | API token for authentication | `eyJhbGciOiJIUzI1NiIs...` |
| `RUNE_NAMESPACE` | Namespace to deploy to | `production` |

### 3. Create Service Definitions

Place your service YAML files in a `services/` directory:

```yaml
# services/my-app.yaml
name: my-app
image: my-app:latest
ports:
  - 3000:3000
environment:
  - NODE_ENV=production
```

### 4. Trigger Deployment

The workflow automatically runs when:
- You push to `main` or `develop` branches
- You modify files in the `services/` directory
- You manually trigger it via GitHub Actions UI

## Workflow Features

### Multi-OS Testing
The workflow tests on both Ubuntu and macOS to ensure CLI compatibility.

### Service Validation
- YAML syntax validation
- Rune-specific validation
- Service definition checks

### Flexible Deployment
- Deploy all services automatically
- Deploy specific service via manual trigger
- Pull request testing with cleanup

### Verification
- Service status checks
- Deployment verification
- Health endpoint testing

## Manual Deployment

You can manually trigger deployments for specific services:

1. Go to **Actions → Deploy with Rune**
2. Click **Run workflow**
3. Enter the service name (e.g., `my-app`)
4. Click **Run workflow**

## Service Definition Structure

Your service YAML files should follow this structure:

```yaml
name: service-name
image: docker-image:tag
ports:
  - host_port:container_port
environment:
  - KEY=value
volumes:
  - host_path:container_path
health_check:
  type: http
  path: /health
  port: 80
labels:
  app: app-name
  environment: production
```

## Advanced Configuration

### Custom Rune Version

Update the workflow to use a specific Rune version:

```yaml
env:
  RUNE_VERSION: v0.2.0  # Change this as needed
```

### Conditional Deployment

Modify the workflow to deploy only on specific conditions:

```yaml
- name: Deploy Services
  if: github.ref == 'refs/heads/main'
  run: |
    # Deployment logic
```

### Environment-Specific Configs

Use different configurations per environment:

```yaml
- name: Setup Rune Context
  run: |
    # Use different namespace per branch
    NAMESPACE="staging"
    if [ "${{ github.ref }}" = "refs/heads/main" ]; then
      NAMESPACE="production"
    fi
    
    cat > ~/.rune/config.yaml <<EOF
    current-context: github-actions
    contexts:
      github-actions:
        server: ${{ secrets.RUNE_SERVER }}
        token: ${{ secrets.RUNE_TOKEN }}
        defaultNamespace: $NAMESPACE
    EOF
```

## Troubleshooting

### Common Issues

**CLI Installation Fails**
```bash
# Check if the version exists
curl -I https://github.com/runestack/rune/releases/download/v0.1.0/rune-cli_linux_amd64.tar.gz

# Use latest available version
curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install-cli.sh | bash
```

**Authentication Fails**
```bash
# Test connection manually
rune status

# Check token validity
curl -H "Authorization: Bearer $TOKEN" "$SERVER/health"
```

**Service Deployment Fails**
```bash
# Check service definition
rune validate services/my-app.yaml

# Check server logs
rune logs my-app
```

### Debug Mode

Enable verbose output in the workflow:

```yaml
- name: Deploy Services
  run: |
    # Enable debug mode
    export RUNE_DEBUG=1
    
    # Deploy with verbose output
    rune cast services/my-app.yaml --verbose
```

## Best Practices

### 1. Service Naming
- Use descriptive names
- Include environment prefix if needed
- Avoid special characters

### 2. Version Management
- Pin Rune CLI version in production
- Test new versions in staging
- Use semantic versioning

### 3. Security
- Use least-privilege tokens
- Rotate tokens regularly
- Monitor access logs

### 4. Testing
- Test deployments in staging first
- Use pull requests for validation
- Implement rollback procedures

## Integration Examples

### With Docker Build

```yaml
- name: Build and Deploy
  run: |
    # Build Docker image
    docker build -t my-app:${{ github.sha }} .
    
    # Update service definition
    sed -i "s|image: my-app:latest|image: my-app:${{ github.sha }}|" services/my-app.yaml
    
    # Deploy with Rune
    rune cast services/my-app.yaml
```

### With Helm Charts

```yaml
- name: Deploy Helm Chart
  run: |
    # Install Helm
    curl https://get.helm.sh/helm-v3.0.0-linux-amd64.tar.gz | tar xz
    
    # Deploy chart
    ./linux-amd64/helm install my-app ./charts/my-app
    
    # Or use Rune for service management
    rune cast services/my-app.yaml
```

### With Kubernetes

```yaml
- name: Deploy to K8s
  run: |
    # Apply K8s manifests
    kubectl apply -f k8s/
    
    # Use Rune for additional services
    rune cast services/external-service.yaml
```

## Monitoring and Observability

### Service Health

```yaml
- name: Monitor Services
  run: |
    # Check all service statuses
    rune get services
    
    # Monitor specific service
    rune status my-app
    
    # View logs
    rune logs my-app --follow=false
```

### Metrics Collection

```yaml
- name: Collect Metrics
  run: |
    # Service metrics
    rune metrics my-app
    
    # System metrics
    rune system metrics
```

## Conclusion

Using Rune CLI in GitHub Actions provides a powerful, declarative way to manage service deployments. The workflow handles installation, configuration, deployment, and verification automatically, making your CI/CD pipeline more reliable and maintainable.

For more information, see:
- [Rune CLI Commands](../cli-commands.md)
- [Service Definitions](../service-definitions.md)
- [Installation Guide](../install-ec2.md)


