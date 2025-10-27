#!/bin/bash

set -e

SCRIPT_DIR=$(dirname "$0")
PROJECT_ROOT=$(dirname "$SCRIPT_DIR")
PROTO_DIR="$PROJECT_ROOT/pkg/api/proto"
GENERATED_DIR="$PROJECT_ROOT/pkg/api/generated"

echo "Generating Go code from Protocol Buffer definitions..."

# Create the generated directory if it doesn't exist
mkdir -p $GENERATED_DIR

# Find all proto files
PROTO_FILES=$(find $PROTO_DIR -name "*.proto")

# Clean up the existing generated files
rm -rf $GENERATED_DIR/*

# Generate Go code
protoc \
  --proto_path=$PROJECT_ROOT \
  --proto_path=$PROJECT_ROOT/pkg/api/proto \
  --go_out=$PROJECT_ROOT \
  --go_opt=module=github.com/runestack/rune \
  --go-grpc_out=$PROJECT_ROOT \
  --go-grpc_opt=module=github.com/runestack/rune \
  --grpc-gateway_out=$PROJECT_ROOT \
  --grpc-gateway_opt module=github.com/runestack/rune \
  --grpc-gateway_opt generate_unbound_methods=true \
  $PROTO_FILES

# Move generated files to the GENERATED_DIR
find $PROJECT_ROOT/pkg/api/proto -name "*.pb.go" -o -name "*.pb.gw.go" | while read file; do
  mv "$file" "$GENERATED_DIR/"
done

echo "Protocol Buffer code generation complete" 