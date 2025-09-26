#!/usr/bin/env bash

set -euo pipefail

echo "Building CraftDocs Search with FTS5 support..."

# Build with FTS5 support
go build --tags fts5 -o craftdocs-search ./app

echo "Build complete: craftdocs-search (with FTS5 support)"
echo ""
echo "To test the search:"
echo "  ./craftdocs-search \"your search query\""