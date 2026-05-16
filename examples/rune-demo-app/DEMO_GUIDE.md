# Rune Demo Application - Complete Feature Guide

This guide demonstrates how to use all Rune MVP features with the demo application.

## 🚀 Quick Start

### 1. Build and Deploy

```bash
# Build the application
cd examples/rune-demo-app
docker build -t rune-demo-app:latest .

# Deploy to development environment
rune cast environments/development.yaml

# Or deploy to production
rune cast environments/production.yaml
```

### 2. Verify Deployment

```bash
# Check service status
rune get service rune-demo-app --namespace=development

# Check health
rune health rune-demo-app --namespace=development

# View logs
rune logs rune-demo-app --namespace=development
```

## 📋 Feature Demonstrations

### 1. Environment Variables & Configuration

The demo app uses extensive environment variables to demonstrate Rune's configuration capabilities:

```bash
# View current configuration
rune exec --namespace=development rune-demo-app -- env | grep -E "(ENVIRONMENT|VERSION|DEBUG|FEATURE)"

# Test configuration endpoint
curl http://localhost:7863/ | jq '.config'
```

**Key Environment Variables:**
- `ENVIRONMENT`: Controls app behavior (development/production)
- `DEBUG_MODE`: Enables debug features
- `FEATURE_FLAGS`: Comma-separated feature flags
- `CUSTOM_MESSAGE`: Customizable welcome message
- `REPLICA_INDEX`: Set by Rune for multi-instance deployments

### 2. Health Checks

The app implements comprehensive health checks:

```bash
# Check health status
rune health rune-demo-app --namespace=development

# Test health endpoint directly
curl http://localhost:7863/health

# Monitor health over time
watch -n 5 'curl -s http://localhost:7863/health | jq .'
```

**Health Check Features:**
- Liveness probe: `/health` endpoint
- Readiness probe: Same endpoint with different thresholds
- Simulated failures: 5% chance of failure for testing
- Component-level checks: Database, memory, disk

### 3. Logging & Observability

```bash
# View real-time logs
rune logs rune-demo-app --namespace=development --follow

# View logs for specific instance
rune logs rune-demo-app-instance-123 --namespace=development

# View recent logs
rune logs rune-demo-app --namespace=development --tail=50
```

**Logging Features:**
- Structured logging with timestamps
- Instance identification in log messages
- Different log levels (debug, info, warn, error)
- Request counting and metrics

### 4. Exec & Debugging

```bash
# Execute simple commands
rune exec --namespace=development rune-demo-app -- ls -la

# Interactive shell
rune exec --namespace=development rune-demo-app -- bash

# Check environment variables
rune exec --namespace=development rune-demo-app -- env

# Check process status
rune exec --namespace=development rune-demo-app -- ps aux

# View application config
rune exec --namespace=development rune-demo-app -- cat config.json
```

**Exec Features:**
- Interactive TTY support
- Environment variable access
- File system access
- Process monitoring

### 5. Scaling

```bash
# Scale up to 5 instances
rune scale rune-demo-app 5 --namespace=development

# Check all instances
rune get instance --namespace=development

# View logs from all instances
rune logs rune-demo-app --namespace=development

# Scale down to 2 instances
rune scale rune-demo-app 2 --namespace=development
```

**Scaling Features:**
- Multi-instance deployment
- Instance identification
- Load balancing
- Health monitoring per instance

### 6. Service Discovery

```bash
# Discover service endpoints
rune discover rune-demo-app --namespace=development

# Test internal connectivity
curl http://rune-demo-app.development.rune:7863/health

# Check service DNS
nslookup rune-demo-app.development.rune
```

### 7. HTTP Endpoints

The demo app provides several HTTP endpoints for testing:

```bash
# Service information
curl http://localhost:7861/ | jq '.'

# Health check
curl http://localhost:7861/health | jq '.'

# Prometheus metrics
curl http://localhost:7861/metrics

# Debug information (requires DEBUG_MODE=true)
curl http://localhost:7861/debug | jq '.'

# Interactive commands
curl -X POST http://localhost:7863/interactive \
  -H "Content-Type: application/json" \
  -d '{"command": "status"}' | jq '.'
```

### 8. Resource Management

```bash
# Check resource usage
rune top services --namespace=development

# View resource allocation
rune get service rune-demo-app --namespace=development -o yaml | grep -A 10 resources
```

**Resource Features:**
- CPU requests and limits
- Memory requests and limits
- Resource monitoring
- Resource enforcement

### 9. Network Policies

The demo app includes network policies for testing:

```bash
# View network policy
rune get service rune-demo-app --namespace=development -o yaml | grep -A 20 networkPolicy

# Test network connectivity
rune exec --namespace=development rune-demo-app -- curl -I http://google.com
```

## 🔧 Advanced Testing Scenarios

### 1. Multi-Environment Testing

```bash
# Deploy to development
rune cast environments/development.yaml

# Deploy to production
rune cast environments/production.yaml

# Compare configurations
diff <(rune get service rune-demo-app --namespace=development -o yaml) \
     <(rune get service rune-demo-app --namespace=production -o yaml)
```

### 2. Health Check Failure Testing

The app simulates health check failures:

```bash
# Monitor health checks
watch -n 2 'curl -s http://localhost:7863/health | jq ".status"'

# Check Rune's health monitoring
rune health rune-demo-app --namespace=development
```

### 3. Load Testing

```bash
# Generate load
for i in {1..100}; do
  curl -s http://localhost:7863/ > /dev/null
done

# Check metrics
curl http://localhost:7863/metrics | grep requests_total
```

### 4. Instance Management

```bash
# List all instances
rune get instance --namespace=development

# Execute in specific instance
rune exec --namespace=development rune-demo-app-instance-123 -- env

# View logs from specific instance
rune logs rune-demo-app-instance-123 --namespace=development
```

## 🧪 Interactive Testing

### 1. Command Testing

```bash
# Test all available commands
for cmd in ls pwd env ps config status memory help; do
  echo "Testing command: $cmd"
  curl -s -X POST http://localhost:7863/interactive \
    -H "Content-Type: application/json" \
    -d "{\"command\": \"$cmd\"}" | jq '.result'
done
```

### 2. Configuration Testing

```bash
# Test different environment variables
rune cast service.yaml --set-env DEBUG_MODE=true
rune cast service.yaml --set-env CUSTOM_MESSAGE="Custom Test Message"
rune cast service.yaml --set-env FEATURE_FLAGS="test,debug,experimental"
```

### 3. Scaling Testing

```bash
# Test scaling up and down
for scale in 1 3 5 2 1; do
  echo "Scaling to $scale instances"
  rune scale rune-demo-app $scale --namespace=development
  sleep 10
  rune get instance --namespace=development
done
```

## 📊 Monitoring & Metrics

### 1. Prometheus Metrics

```bash
# View all metrics
curl http://localhost:7863/metrics

# Filter specific metrics
curl http://localhost:7863/metrics | grep -E "(requests_total|uptime_seconds|memory_bytes)"
```

### 2. Application Metrics

```bash
# Check request count
curl -s http://localhost:7863/metrics | grep requests_total

# Check memory usage
curl -s http://localhost:7863/metrics | grep memory_bytes

# Check uptime
curl -s http://localhost:7863/metrics | grep uptime_seconds
```

## 🛠️ Troubleshooting

### 1. Service Won't Start

```bash
# Check service status
rune get service rune-demo-app --namespace=development

# View logs
rune logs rune-demo-app --namespace=development

# Check health
rune health rune-demo-app --namespace=development
```

### 2. Health Checks Failing

```bash
# Test health endpoint directly
curl http://localhost:7863/health

# Check Rune health status
rune health rune-demo-app --namespace=development

# View health check logs
rune logs rune-demo-app --namespace=development | grep health
```

### 3. Exec Not Working

```bash
# Check if service is running
rune get instance --namespace=development

# Try exec with debug
rune exec --namespace=development rune-demo-app -- --debug ls

# Check container status
docker ps | grep rune-demo-app
```

## 🧹 Cleanup

```bash
# Delete service
rune delete rune-demo-app --namespace=development

# Delete namespace
rune delete namespace development

# Remove Docker image
docker rmi rune-demo-app:latest
```

## 📝 Summary

This demo application showcases all Rune MVP features:

✅ **Environment Variables**: Comprehensive configuration management  
✅ **Health Checks**: Liveness and readiness probes with failure simulation  
✅ **Logging**: Structured logging with instance identification  
✅ **Exec**: Interactive command execution and debugging  
✅ **Scaling**: Multi-instance deployment and management  
✅ **Service Discovery**: DNS-based service discovery  
✅ **Resource Management**: CPU and memory limits  
✅ **Network Policies**: Ingress/egress traffic control  
✅ **Metrics**: Prometheus-compatible metrics  
✅ **Multi-Environment**: Development and production configurations  

The demo app provides a complete testing environment for all Rune features and can be used for development, testing, and learning purposes.
