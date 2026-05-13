# GitHub Actions with Rune CLI

This directory contains examples of using the Rune CLI in GitHub Actions for CI/CD workflows.

## Files

- `rune-deploy.yml` - Complete GitHub Actions workflow for deploying services with Rune
- `services/example-app.yaml` - Example service definition for testing
- `README.md` - This documentation

## Quick Start

### 1. Copy the Workflow

Copy `rune-deploy.yml` to your repository's `.github/workflows/` directory:

```bash
mkdir -p .github/workflows
cp examples/github-actions/rune-deploy.yml .github/workflows/
```

### 2. Configure Secrets

Add these secrets to your GitHub repository:

| Secret | Description |
|--------|-------------|
| `RUNE_SERVER` | Your Rune server address (e.g., `https://rune.example.com:7863`) |
| `RUNE_TOKEN` | API token for authentication |
| `RUNE_NAMESPACE` | Namespace to deploy to (optional, defaults to `system`) |

### 3. Create Services Directory

Create a `services/` directory in your repository root and add your service YAML files:

```bash
mkdir services
cp examples/github-actions/services/example-app.yaml services/
```

### 4. Customize the Workflow

Update the workflow file to match your needs:

- Change `RUNE_VERSION` to match your Rune server version
- Modify the trigger paths if your services are in a different directory
- Adjust the deployment logic for your specific requirements

## Workflow Features

- **Multi-OS Testing**: Tests on Ubuntu and macOS
- **Service Validation**: YAML syntax and Rune-specific validation
- **Flexible Deployment**: Deploy all services or specific ones
- **Manual Triggers**: Deploy specific services via GitHub Actions UI
- **Verification**: Service status checks and health monitoring
- **Cleanup**: Optional cleanup for pull request testing

## Example Usage

### Deploy All Services
The workflow automatically deploys all services when you push to `main` or `develop` branches.

### Deploy Specific Service
1. Go to **Actions → Deploy with Rune**
2. Click **Run workflow**
3. Enter the service name (e.g., `example-app`)
4. Click **Run workflow**

### Pull Request Testing
The workflow runs on pull requests to validate service definitions and can optionally deploy to a testing environment.

## Customization

### Environment-Specific Deployments

Modify the workflow to deploy to different environments based on the branch:

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
        namespace: $NAMESPACE
    EOF
```

### Service-Specific Logic

Add custom logic for different service types:

```yaml
- name: Deploy Services
  run: |
    find services/ -name "*.yaml" -type f | while read -r service_file; do
      service_name=$(basename "$service_file" .yaml)
      
      # Custom logic based on service type
      if [[ "$service_name" == *"frontend"* ]]; then
        echo "Deploying frontend service: $service_name"
        # Frontend-specific deployment logic
      elif [[ "$service_name" == *"backend"* ]]; then
        echo "Deploying backend service: $service_name"
        # Backend-specific deployment logic
      fi
      
      rune cast "$service_file"
    done
```

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

### With External Tools

```yaml
- name: Deploy with Helm
  run: |
    # Install Helm
    curl https://get.helm.sh/helm-v3.0.0-linux-amd64.tar.gz | tar xz
    
    # Deploy chart
    ./linux-amd64/helm install my-app ./charts/my-app
    
    # Use Rune for additional services
    rune cast services/external-service.yaml
```

## Troubleshooting

### Common Issues

**CLI Installation Fails**
- Check if the Rune version exists in releases
- Verify the `install-cli.sh` script is accessible
- Use the latest available version if needed

**Authentication Fails**
- Verify your `RUNE_SERVER` and `RUNE_TOKEN` secrets
- Check if the token has the required permissions
- Test the connection manually: `rune status`

**Service Deployment Fails**
- Validate your service YAML files
- Check the Rune server logs
- Verify the service definition syntax

### Debug Mode

Enable verbose output by setting environment variables:

```yaml
- name: Deploy Services
  env:
    RUNE_DEBUG: 1
  run: |
    rune cast services/my-app.yaml --verbose
```

## Best Practices

1. **Version Pinning**: Pin the Rune CLI version in production workflows
2. **Service Validation**: Always validate service definitions before deployment
3. **Rollback Strategy**: Implement rollback procedures for failed deployments
4. **Monitoring**: Add health checks and monitoring after deployment
5. **Security**: Use least-privilege tokens and rotate them regularly

## Support

For more information about using Rune CLI:
- [Rune CLI overview](https://docs.runestack.io/cli/overview/)
- [Service spec reference](https://docs.runestack.io/reference/service-spec/)
- [Installation](https://docs.runestack.io/start/installation/)
- [CI deployments](https://docs.runestack.io/guides/ci-deployments/)
