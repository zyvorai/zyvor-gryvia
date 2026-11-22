#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

set -euo pipefail

echo "Building Gryvia CLI..."

# Clean previous builds
cargo clean

# Build release binary
cargo build --release

# Get version
VERSION=$(cargo metadata --no-deps --format-version 1 | jq -r '.packages[0].version')

echo ""
echo "✓ Build complete!"
echo "  Version: $VERSION"
echo "  Binary: target/release/gryvia"
echo ""
echo "To install:"
echo "  sudo cp target/release/gryvia /usr/local/bin/"
echo ""
echo "To test:"
echo "  ./target/release/gryvia --help"
