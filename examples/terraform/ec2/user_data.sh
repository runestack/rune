#!/bin/bash
set -euo pipefail

# Rune EC2 User Data Script - Using Official Installer
# This script installs Rune Server using the official install-server.sh installer
# Compatible with Ubuntu 22.04+, Debian 11+, and Amazon Linux 2023

RUNE_VERSION="${rune_version:-}"
GIT_BRANCH="${git_branch:-master}"

# Log all output to both console and file
exec > /var/log/user-data.log 2>&1

echo "Starting Rune installation via official installer..."

# Ensure we have curl
if ! command -v curl >/dev/null 2>&1; then
    echo "Installing curl..."
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -y
        apt-get install -y curl
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y curl
    elif command -v yum >/dev/null 2>&1; then
        yum install -y curl
    fi
fi

# Install Rune using the official installer
if [ -n "$RUNE_VERSION" ]; then
    echo "Installing Rune version: $RUNE_VERSION"
    curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install-server.sh | bash -s -- --version "$RUNE_VERSION"
else
    echo "Installing Rune from source (branch: $GIT_BRANCH)"
    curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install-server.sh | bash -s -- --from-source --branch "$GIT_BRANCH"
fi

# Wait for installer to complete
echo "Waiting for installer to complete..."
sleep 5

# Display Terraform-specific completion message
echo ""
echo "🎉 Rune installation via Terraform completed!"
echo ""
echo "The installer has automatically set up CLI access for the primary user."
echo "You can now run: rune status"
echo ""
echo "Installation log: /var/log/user-data.log"
