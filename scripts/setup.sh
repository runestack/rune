#!/bin/bash
set -e

# Function to print colored output
print_step() {
  echo -e "\033[0;34m==>\033[0m \033[1m$1\033[0m"
}

print_error() {
  echo -e "\033[0;31mERROR:\033[0m \033[1m$1\033[0m"
}

print_success() {
  echo -e "\033[0;32mSUCCESS:\033[0m \033[1m$1\033[0m"
}

# Check if Go is installed
check_go() {
  print_step "Checking if Go is installed..."
  if ! command -v go &> /dev/null; then
    print_error "Go is not installed. Please install Go 1.19 or later."
    exit 1
  fi
  
  GO_VERSION=$(go version | grep -oP 'go\d+\.\d+' | grep -oP '\d+\.\d+')
  print_success "Go is installed (version $GO_VERSION)"
}

# Check if Docker is installed
check_docker() {
  print_step "Checking if Docker is installed..."
  if ! command -v docker &> /dev/null; then
    print_error "Docker is not installed. Please install Docker."
    exit 1
  fi
  
  DOCKER_VERSION=$(docker --version | grep -oP 'Docker version \K[^,]+')
  print_success "Docker is installed (version $DOCKER_VERSION)"
}

# Check if Docker Compose is installed
check_docker_compose() {
  print_step "Checking if Docker Compose is installed..."
  if ! command -v docker-compose &> /dev/null; then
    print_error "Docker Compose is not installed. Please install Docker Compose."
    exit 1
  fi
  
  COMPOSE_VERSION=$(docker-compose --version | grep -oP 'docker-compose version \K[^,]+')
  print_success "Docker Compose is installed (version $COMPOSE_VERSION)"
}

# Install Go tools
install_go_tools() {
  print_step "Installing Go tools..."
  
  # Install golangci-lint
  print_step "Installing golangci-lint..."
  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
  
  # Install mockgen
  print_step "Installing mockgen..."
  go install github.com/golang/mock/mockgen@latest
  
  # Install godoc
  print_step "Installing godoc..."
  go install golang.org/x/tools/cmd/godoc@latest

  
  print_success "Go tools installed successfully"
}

# Set up pre-commit hooks
setup_pre_commit() {
  print_step "Setting up pre-commit hooks..."
  
  # Check if .git directory exists
  if [ ! -d ".git" ]; then
    print_error "This is not a git repository. Please initialize git first."
    exit 1
  fi
  
  # Create pre-commit hook
  mkdir -p .git/hooks
  cat > .git/hooks/pre-commit << 'EOL'
#!/bin/bash
set -e

# Run gofmt
echo "Running gofmt..."
gofmt_files=$(gofmt -l $(find . -name '*.go' | grep -v vendor))
if [ -n "$gofmt_files" ]; then
  echo "Go files must be formatted with gofmt. Please run:"
  echo "  gofmt -w $(echo $gofmt_files)"
  exit 1
fi

# Run golangci-lint
echo "Running golangci-lint..."
golangci-lint run ./...

# Run tests
echo "Running tests..."
go test ./...
EOL
  
  chmod +x .git/hooks/pre-commit
  print_success "Pre-commit hooks set up successfully"
}

# Set up the project
setup_project() {
  print_step "Setting up the project..."
  
  # Initialize Go module dependencies
  print_step "Initializing Go module dependencies..."
  go mod tidy
  
  # Create bin directory
  print_step "Creating bin directory..."
  mkdir -p bin
  
  print_success "Project set up successfully"
}

# Main function
main() {
  print_step "Setting up development environment..."
  
  check_go
  check_docker
  check_docker_compose
  install_go_tools
  setup_pre_commit
  setup_project
  
  print_success "Development environment set up successfully!"
  echo ""
  echo "You can now build the project with: make build"
  echo "Start the development environment with: make dev"
  echo "Run tests with: make test"
}

# Run main function
main 