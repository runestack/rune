# Rune Demo Application

A comprehensive demo application that showcases Rune's MVP features including environment variables, health checks, logging, exec capabilities, scaling, and more.

## Features Demonstrated

### ✅ MVP Features (Rune Core)
- **Environment Variables**: Comprehensive configuration via environment variables
- **Health Checks**: HTTP-based liveness and readiness probes
- **Logging**: Structured logging with different levels
- **Exec Support**: Interactive command execution via HTTP endpoints
- **Scaling**: Multi-instance deployment with replica identification
- **Resource Management**: CPU and memory limits
- **Network Policies**: Ingress/egress traffic control
- **Service Discovery**: Load-balanced service endpoints
- **Metrics**: Prometheus-style metrics endpoint

### 🔧 Application Features
- **Multi-instance Support**: Each instance has unique ID and replica index
- **Health Monitoring**: Simulated health checks with occasional failures
- **Debug Mode**: Enhanced debugging information when enabled
- **Interactive Commands**: Shell-like command execution
- **Metrics Exposition**: Prometheus-compatible metrics
- **Graceful Shutdown**: Proper signal handling and cleanup

## Quick Start

### 1. Build the Application

```bash
# Navigate to the demo app directory
cd examples/rune-demo-app

# Build the Docker image
docker build -t rune-demo-app:latest .
```

### 2. Deploy with Rune

```bash
# Deploy the service
rune cast service.yaml

# Check service status
rune get service rune-demo-app --namespace=demo

# View service logs
rune logs rune-demo-app --namespace=demo

# Check health status
rune health rune-demo-app --namespace=demo
```

### 3. Test the Application

```bash
# Get service info
curl http://localhost:7863/

# Check health
curl http://localhost:7863/health

# View metrics (Prometheus format)
curl http://localhost:7863/metrics

# Debug information (requires DEBUG_MODE=true)
curl http://localhost:7863/debug

# Interactive commands
curl -X POST http://localhost:7863/interactive \
  -H "Content-Type: application/json" \
  -d '{"command": "status"}'
```

## Rune CLI Commands Demo

### Service Management

```bash
# Deploy the service
rune cast service.yaml

# List services
rune get service --namespace=demo

# Get specific service details
rune get service rune-demo-app --namespace=demo -o yaml

# Scale the service
rune scale rune-demo-app 5 --namespace=demo

# Check service health
rune health rune-demo-app --namespace=demo
```

### Logs and Monitoring

```bash
# View real-time logs
rune logs rune-demo-app --namespace=demo

# Follow logs
rune logs rune-demo-app --namespace=demo --follow

# View logs for specific instance
rune logs rune-demo-app-instance-123 --namespace=demo
```

### Exec and Debugging

```bash
# Execute command in a service instance
rune exec --namespace=demo rune-demo-app -- ls -la

# Interactive shell
rune exec --namespace=demo rune-demo-app -- bash

# Execute in specific instance
rune exec --namespace=demo rune-demo-app-instance-123 -- ps aux

# Check environment variables
rune exec --namespace=demo rune-demo-app -- env | grep RUNE
```

### Service Discovery

```bash
# Discover service endpoints
rune discover rune-demo-app --namespace=demo

# Test connectivity
curl http://rune-demo-app.demo.rune:7863/health
```

## Environment Variables

The application supports extensive configuration via environment variables:

### Basic Configuration
- `PORT`: HTTP server port (default: 8080)
- `HOST`: HTTP server host (default: 0.0.0.0)
- `ENVIRONMENT`: Application environment (default: development)
- `VERSION`: Application version (default: 1.0.0)
- `LOG_LEVEL`: Logging level (default: info)

### Service Identification
- `SERVICE_NAME`: Service name for identification
- `INSTANCE_ID`: Unique instance identifier (auto-generated if empty)
- `REPLICA_INDEX`: Replica index (set by Rune)

### Feature Flags
- `DEBUG_MODE`: Enable debug mode (default: false)
- `FEATURE_FLAGS`: Comma-separated feature flags
- `CUSTOM_MESSAGE`: Custom welcome message

### Performance
- `MAX_CONNECTIONS`: Maximum concurrent connections
- `TIMEOUT_SECONDS`: Request timeout in seconds

## API Endpoints

### `/` - Service Information
Returns comprehensive service information including configuration, instance details, and runtime stats.

### `/health` - Health Check
Returns health status with detailed component checks. Used by Rune for liveness and readiness probes.

### `/metrics` - Prometheus Metrics
Exposes Prometheus-compatible metrics including:
- Request count
- Uptime
- Memory usage
- Goroutine count

### `/debug` - Debug Information
Returns detailed debug information when `DEBUG_MODE=true`.

### `/interactive` - Command Execution
POST endpoint for executing commands:
```bash
curl -X POST http://localhost:7863/interactive \
  -H "Content-Type: application/json" \
  -d '{"command": "status"}'
```

Available commands: `ls`, `pwd`, `env`, `ps`, `config`, `status`, `memory`, `help`

## Health Checks

The application implements comprehensive health checks:

### Liveness Probe
- **Path**: `/health`
- **Interval**: 30s
- **Timeout**: 5s
- **Failure Threshold**: 3
- **Success Threshold**: 1

### Readiness Probe
- **Path**: `/health`
- **Interval**: 10s
- **Timeout**: 3s
- **Failure Threshold**: 3
- **Success Threshold**: 1

### Simulated Failures
The application occasionally simulates health check failures (5% chance) to test Rune's health monitoring capabilities.

## Scaling Demo

```bash
# Scale up to 5 instances
rune scale rune-demo-app 5 --namespace=demo

# Check all instances
rune get instance --namespace=demo

# View logs from all instances
rune logs rune-demo-app --namespace=demo

# Scale down to 2 instances
rune scale rune-demo-app 2 --namespace=demo
```

## Resource Management

The service is configured with resource limits:

```yaml
resources:
  cpu:
    request: "100m"
    limit: "500m"
  memory:
    request: "128Mi"
    limit: "256Mi"
```

## Network Policy

The service includes a network policy that:
- Allows incoming HTTP traffic on port 8080
- Allows outgoing traffic to common ports (80, 443, 5432)

## Monitoring and Observability

### Metrics
The application exposes Prometheus-compatible metrics at `/metrics`:
- Request count per instance
- Uptime in seconds
- Memory usage (allocated and system)
- Goroutine count

### Logging
- Structured logging with timestamps
- Instance identification in log messages
- Different log levels based on configuration

### Health Monitoring
- Real-time health status via `/health` endpoint
- Component-level health checks
- Integration with Rune's health monitoring

## Troubleshooting

### Common Issues

1. **Service won't start**
   ```bash
   # Check service status
   rune get service rune-demo-app --namespace=demo
   
   # View logs
   rune logs rune-demo-app --namespace=demo
   ```

2. **Health checks failing**
   ```bash
   # Check health status
   rune health rune-demo-app --namespace=demo
   
   # Test health endpoint directly
   curl http://localhost:7863/health
   ```

3. **Exec not working**
   ```bash
   # Check if service is running
   rune get instance --namespace=demo
   
   # Try exec with verbose output
   rune exec --namespace=demo rune-demo-app -- --debug
   ```

### Debug Mode

Enable debug mode to get more detailed information:

```bash
# Update service with debug mode
rune cast service.yaml --set-env DEBUG_MODE=true

# Check debug endpoint
curl http://localhost:7863/debug
```

## Development

### Local Development

```bash
# Run locally
go run main.go

# Build locally
go build -o rune-demo-app main.go

# Test endpoints
curl http://localhost:7863/health
```

### Docker Development

```bash
# Build image
docker build -t rune-demo-app:latest .

# Run container
docker run -p 8080:7863 rune-demo-app:latest

# Test container
curl http://localhost:7863/
```

## Cleanup

```bash
# Delete the service
rune delete rune-demo-app --namespace=demo

# Remove namespace
rune delete namespace demo
```

This demo application provides a comprehensive showcase of Rune's MVP features and can be used for testing, development, and learning purposes.
