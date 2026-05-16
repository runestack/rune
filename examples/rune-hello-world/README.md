# Rune Hello World Application

A comprehensive hello world application designed to test various Rune platform features, including `rune exec`, environment variables, logging, and interactive debugging.

## Features

### 🚀 **Core Functionality**
- HTTP server with multiple endpoints
- Environment variable configuration
- Health checks and monitoring
- Debug information and metrics
- Interactive command execution
- Graceful shutdown handling

### 🔧 **Rune Integration Features**
- Environment variable injection for testing
- Interactive shell access via `rune exec`
- Health check endpoints for monitoring
- Debug endpoints for troubleshooting
- Comprehensive logging and metrics

## Quick Start

### 1. Build and Deploy

```bash
# Build the Docker image
docker build -t rune-hello-world:latest -f examples/rune-hello-world/Dockerfile .

# Cast to Rune
rune cast examples/rune-hello-world/service.yaml --detach
```

### 2. Test Basic Functionality

```bash
# Check service status
rune get services

# View logs
rune logs rune-hello-world

# Test health endpoint
curl http://localhost:7863/health
```

### 3. Test Environment Variables

```bash
# Exec into the service to check environment
rune exec rune-hello-world -- env

# Check specific environment variables
rune exec rune-hello-world -- bash -c "echo \$CUSTOM_MESSAGE"
rune exec rune-hello-world -- bash -c "echo \$DEBUG_MODE"
```

## API Endpoints

### `/` - Main Info Endpoint
Returns comprehensive application information including configuration, environment variables, and system details.

```bash
curl http://localhost:7863/
```

**Response:**
```json
{
  "message": "Hello from Rune Hello World!",
  "timestamp": "2024-01-01T12:00:00Z",
  "environment": "development",
  "version": "1.0.0",
  "config": {
    "port": 8080,
    "host": "0.0.0.0",
    "environment": "development",
    "version": "1.0.0",
    "log_level": "info",
    "database_url": "postgresql://localhost:5432/rune_hello_world",
    "api_key": "demo-api-key-12345",
    "debug_mode": true,
    "max_connections": 100,
    "timeout_seconds": 30,
    "feature_flags": "basic,advanced,experimental",
    "custom_message": "Hello from Rune Hello World!"
  },
  "headers": {...},
  "hostname": "container-123",
  "pid": 1,
  "uptime": "5m30s"
}
```

### `/health` - Health Check
Returns application health status and uptime information.

```bash
curl http://localhost:7863/health
```

**Response:**
```json
{
  "status": "healthy",
  "timestamp": "2024-01-01T12:00:00Z",
  "uptime": "5m30s",
  "version": "1.0.0",
  "environment": "development"
}
```

### `/debug` - Debug Information
Returns detailed debug information including memory stats, goroutines, and full configuration (only when DEBUG_MODE=true).

```bash
curl http://localhost:7863/debug
```

### `/interactive` - Interactive Commands
POST endpoint for executing commands within the container.

```bash
curl -X POST http://localhost:7863/interactive \
  -H "Content-Type: application/json" \
  -d '{"command": "ls"}'
```

## Environment Variables

The application supports extensive environment variable configuration:

### Basic Configuration
- `PORT`: Server port (default: 8080)
- `HOST`: Server host (default: 0.0.0.0)
- `ENVIRONMENT`: Environment name (default: development)
- `VERSION`: Application version (default: 1.0.0)
- `LOG_LEVEL`: Logging level (default: info)

### Database Configuration
- `DATABASE_URL`: Database connection string
- `API_KEY`: API key for authentication

### Feature Flags and Debugging
- `DEBUG_MODE`: Enable debug mode (default: false)
- `FEATURE_FLAGS`: Comma-separated feature flags
- `CUSTOM_MESSAGE`: Custom welcome message

### Performance Settings
- `MAX_CONNECTIONS`: Maximum concurrent connections
- `TIMEOUT_SECONDS`: Request timeout in seconds

## Testing Rune Features

### 1. Testing `rune exec`

```bash
# Interactive bash session
rune exec rune-hello-world -- bash

# Check environment variables
rune exec rune-hello-world -- env | grep CUSTOM_MESSAGE

# View running processes
rune exec rune-hello-world -- ps aux

# Check application files
rune exec rune-hello-world -- ls -la

# Test interactive commands
rune exec rune-hello-world -- bash -c "curl localhost:7863/health"
```

### 2. Testing Environment Variable Injection

```bash
# Check all environment variables
rune exec rune-hello-world -- env

# Test specific variables
rune exec rune-hello-world -- bash -c "echo \$DEBUG_MODE"
rune exec rune-hello-world -- bash -c "echo \$CUSTOM_MESSAGE"
rune exec rune-hello-world -- bash -c "echo \$DATABASE_URL"
```

### 3. Testing Health Checks

```bash
# Check service health
rune get services rune-hello-world

# View health check logs
rune logs rune-hello-world | grep health
```

### 4. Testing Debug Features

```bash
# Enable debug mode and check debug endpoint
rune exec rune-hello-world -- bash -c "curl localhost:7863/debug"

# Check memory usage
rune exec rune-hello-world -- bash -c "free -h"

# Monitor processes
rune exec rune-hello-world -- htop
```

## Interactive Commands

When using `rune exec rune-hello-world -- bash`, you can test these commands:

### System Information
```bash
# Check environment
env

# View processes
ps aux

# Check disk usage
df -h

# View memory usage
free -h

# Check network connections
netstat -tuln
```

### Application Testing
```bash
# Test health endpoint
curl localhost:7863/health

# Test main endpoint
curl localhost:7863/

# Test debug endpoint
curl localhost:7863/debug

# Test interactive endpoint
curl -X POST localhost:7863/interactive \
  -H "Content-Type: application/json" \
  -d '{"command": "help"}'
```

### File Operations
```bash
# List application files
ls -la

# View application binary
file rune-hello-world

# Check application logs
tail -f /var/log/application.log 2>/dev/null || echo "No log file"
```

## Troubleshooting

### Common Issues

1. **Service not starting**
   ```bash
   # Check service status
   rune get services rune-hello-world
   
   # View logs
   rune logs rune-hello-world
   ```

2. **Environment variables not set**
   ```bash
   # Check environment in container
   rune exec rune-hello-world -- env
   
   # Verify service configuration
   rune get service rune-hello-world -o yaml
   ```

3. **Health checks failing**
   ```bash
   # Test health endpoint directly
   rune exec rune-hello-world -- curl localhost:7863/health
   
   # Check if port is listening
   rune exec rune-hello-world -- netstat -tuln | grep 8080
   ```

### Debug Mode

Enable debug mode by setting `DEBUG_MODE=true` in the service configuration:

```yaml
env:
  DEBUG_MODE: "true"
```

This will:
- Enable the `/debug` endpoint
- Provide detailed memory and runtime statistics
- Show all environment variables
- Display goroutine count and system information

## Development

### Building Locally

```bash
# Build the application
go build -o rune-hello-world main.go

# Run locally
./rune-hello-world
```

### Testing Locally

```bash
# Test with environment variables
DEBUG_MODE=true CUSTOM_MESSAGE="Local Test" ./rune-hello-world

# Test endpoints
curl http://localhost:7863/health
curl http://localhost:7863/
curl http://localhost:7863/debug
```

## Configuration Examples

### Development Environment
```yaml
env:
  ENVIRONMENT: "development"
  DEBUG_MODE: "true"
  LOG_LEVEL: "debug"
  CUSTOM_MESSAGE: "Hello from Development!"
```

### Production Environment
```yaml
env:
  ENVIRONMENT: "production"
  DEBUG_MODE: "false"
  LOG_LEVEL: "warn"
  CUSTOM_MESSAGE: "Hello from Production!"
  MAX_CONNECTIONS: "1000"
  TIMEOUT_SECONDS: "60"
```

### Testing Environment
```yaml
env:
  ENVIRONMENT: "testing"
  DEBUG_MODE: "true"
  FEATURE_FLAGS: "test,experimental"
  CUSTOM_MESSAGE: "Hello from Testing!"
```

## Security Considerations

- The application exposes debug information when `DEBUG_MODE=true`
- API keys and sensitive data are logged as `[REDACTED]`
- Health checks are publicly accessible
- Debug endpoint requires `DEBUG_MODE=true`

## Performance

- Lightweight HTTP server with minimal resource usage
- Configurable connection limits and timeouts
- Graceful shutdown handling
- Memory usage monitoring via debug endpoint

This hello world application provides a comprehensive testing environment for Rune platform features, making it easy to validate functionality and troubleshoot issues.
