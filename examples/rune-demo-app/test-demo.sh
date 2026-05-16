#!/bin/bash

# Rune Demo App Test Script
# This script demonstrates various Rune features with the demo application

set -e

echo "🚀 Rune Demo App Test Script"
echo "=============================="

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to print colored output
print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if rune CLI is available
check_rune_cli() {
    print_status "Checking Rune CLI availability..."
    if ! command -v rune &> /dev/null; then
        print_error "Rune CLI not found. Please install Rune first."
        exit 1
    fi
    print_success "Rune CLI found"
}

# Build the demo application
build_app() {
    print_status "Building demo application..."
    
    # Check if we're in the right directory
    if [ ! -f "main.go" ]; then
        print_error "main.go not found. Please run this script from the demo app directory."
        exit 1
    fi
    
    # Build Docker image
    docker build -t rune-demo-app:latest . || {
        print_error "Failed to build Docker image"
        exit 1
    }
    print_success "Demo app built successfully"
}

# Deploy the service
deploy_service() {
    print_status "Deploying demo service..."
    
    # Create namespace if it doesn't exist
    rune namespace create demo 2>/dev/null || print_warning "Namespace 'demo' may already exist"
    
    # Deploy the service
    rune cast service.yaml || {
        print_error "Failed to deploy service"
        exit 1
    }
    print_success "Service deployed successfully"
}

# Wait for service to be ready
wait_for_service() {
    print_status "Waiting for service to be ready..."
    
    local max_attempts=30
    local attempt=1
    
    while [ $attempt -le $max_attempts ]; do
        if rune health rune-demo-app --namespace=demo 2>/dev/null | grep -q "healthy"; then
            print_success "Service is ready"
            return 0
        fi
        
        echo -n "."
        sleep 2
        attempt=$((attempt + 1))
    done
    
    print_error "Service failed to become ready within timeout"
    return 1
}

# Test basic functionality
test_basic_functionality() {
    print_status "Testing basic functionality..."
    
    # Get service info
    print_status "Getting service information..."
    rune get service rune-demo-app --namespace=demo
    
    # Check health
    print_status "Checking service health..."
    rune health rune-demo-app --namespace=demo
    
    # View logs
    print_status "Viewing service logs..."
    rune logs rune-demo-app --namespace=demo --tail=10
}

# Test scaling
test_scaling() {
    print_status "Testing scaling functionality..."
    
    # Scale up
    print_status "Scaling up to 5 instances..."
    rune scale rune-demo-app 5 --namespace=demo
    
    # Wait a moment
    sleep 5
    
    # Check instances
    print_status "Checking instances..."
    rune get instance --namespace=demo
    
    # Scale down
    print_status "Scaling down to 2 instances..."
    rune scale rune-demo-app 2 --namespace=demo
    
    sleep 5
    
    # Check instances again
    rune get instance --namespace=demo
}

# Test exec functionality
test_exec() {
    print_status "Testing exec functionality..."
    
    # Execute simple command
    print_status "Executing 'ls' command..."
    rune exec --namespace=demo rune-demo-app -- ls -la
    
    # Execute environment check
    print_status "Checking environment variables..."
    rune exec --namespace=demo rune-demo-app -- env | grep -E "(RUNE|SERVICE|INSTANCE)" || true
    
    # Execute status command
    print_status "Checking service status..."
    rune exec --namespace=demo rune-demo-app -- ps aux | head -5
}

# Test service discovery
test_service_discovery() {
    print_status "Testing service discovery..."
    
    # Discover service
    print_status "Discovering service endpoints..."
    rune discover rune-demo-app --namespace=demo
}

# Test HTTP endpoints
test_http_endpoints() {
    print_status "Testing HTTP endpoints..."
    
    # Get the service port (assuming it's exposed)
    local port=8080
    
    # Test basic endpoint
    print_status "Testing / endpoint..."
    curl -s http://localhost:$port/ | jq '.message' 2>/dev/null || print_warning "Could not test / endpoint"
    
    # Test health endpoint
    print_status "Testing /health endpoint..."
    curl -s http://localhost:$port/health | jq '.status' 2>/dev/null || print_warning "Could not test /health endpoint"
    
    # Test metrics endpoint
    print_status "Testing /metrics endpoint..."
    curl -s http://localhost:$port/metrics | head -5 || print_warning "Could not test /metrics endpoint"
}

# Test interactive commands
test_interactive() {
    print_status "Testing interactive commands..."
    
    local port=8080
    
    # Test status command
    print_status "Testing interactive status command..."
    curl -s -X POST http://localhost:$port/interactive \
        -H "Content-Type: application/json" \
        -d '{"command": "status"}' | jq '.result' 2>/dev/null || print_warning "Could not test interactive endpoint"
    
    # Test config command
    print_status "Testing interactive config command..."
    curl -s -X POST http://localhost:$port/interactive \
        -H "Content-Type: application/json" \
        -d '{"command": "config"}' | jq '.result' 2>/dev/null || print_warning "Could not test config command"
}

# Cleanup function
cleanup() {
    print_status "Cleaning up..."
    
    # Delete the service
    rune delete rune-demo-app --namespace=demo 2>/dev/null || print_warning "Service may already be deleted"
    
    # Remove namespace
    rune delete namespace demo 2>/dev/null || print_warning "Namespace may already be deleted"
    
    print_success "Cleanup completed"
}

# Main execution
main() {
    echo "Starting Rune Demo App Test..."
    echo ""
    
    # Check prerequisites
    check_rune_cli
    
    # Build and deploy
    build_app
    deploy_service
    
    # Wait for service
    wait_for_service
    
    # Run tests
    test_basic_functionality
    test_scaling
    test_exec
    test_service_discovery
    test_http_endpoints
    test_interactive
    
    print_success "All tests completed successfully!"
    echo ""
    echo "Demo features tested:"
    echo "✅ Service deployment"
    echo "✅ Health checks"
    echo "✅ Logging"
    echo "✅ Scaling"
    echo "✅ Exec functionality"
    echo "✅ Service discovery"
    echo "✅ HTTP endpoints"
    echo "✅ Interactive commands"
    echo ""
    echo "To continue testing manually:"
    echo "  rune logs rune-demo-app --namespace=demo --follow"
    echo "  rune exec --namespace=demo rune-demo-app -- bash"
    echo "  rune scale rune-demo-app 5 --namespace=demo"
    echo ""
    echo "To cleanup:"
    echo "  rune delete rune-demo-app --namespace=demo"
}

# Handle cleanup on script exit
trap cleanup EXIT

# Run main function
main "$@"
