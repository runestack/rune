#!/bin/bash
set -e

# Define colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

COVERAGE_FILE="coverage_unit.out"

# Print header
echo -e "${GREEN}=========================================${NC}"
echo -e "${GREEN}         Rune Unit Tests Runner         ${NC}"
echo -e "${GREEN}=========================================${NC}"

# Display testing environment
echo -e "${YELLOW}Testing environment:${NC}"
echo -e "  - Go version: $(go version)"

# Get list of packages excluding test/integration
UNIT_PACKAGES=$(go list ./... | grep -v 'test/integration')

# Run unit tests and generate coverage report
echo -e "${YELLOW}Running unit tests...${NC}"
# Skip docker package integration-like tests unless explicitly enabled
export SKIP_DOCKER_TESTS=${SKIP_DOCKER_TESTS:-1}

# Check if race detection is enabled
RACE_FLAG=""
if [ "${RACE_DETECTION:-}" = "1" ]; then
    RACE_FLAG="-race"
    echo -e "${YELLOW}Race detection enabled${NC}"
fi

# Run tests on each package individually to handle packages without tests
FAILED_PACKAGES=()
for pkg in $UNIT_PACKAGES; do
    echo -e "${YELLOW}Testing $pkg...${NC}"
    if go test -tags=unit $RACE_FLAG $pkg -v 2>&1; then
        echo -e "${GREEN}✓ $pkg passed${NC}"
    else
        echo -e "${RED}✗ $pkg failed${NC}"
        FAILED_PACKAGES+=("$pkg")
    fi
done

# Check if any packages failed
if [ ${#FAILED_PACKAGES[@]} -gt 0 ]; then
    echo -e "${RED}The following packages failed:${NC}"
    for pkg in "${FAILED_PACKAGES[@]}"; do
        echo -e "${RED}  - $pkg${NC}"
    done
    TEST_EXIT_CODE=1
else
    TEST_EXIT_CODE=0
fi

if [ $TEST_EXIT_CODE -eq 0 ]; then
    echo -e "${GREEN}All unit tests passed!${NC}"
    echo -e "${YELLOW}Coverage report written to ${COVERAGE_FILE}${NC}"
else
    echo -e "${RED}Some unit tests failed.${NC}"
fi

exit $TEST_EXIT_CODE
