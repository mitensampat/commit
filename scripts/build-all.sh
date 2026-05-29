#!/bin/bash
# build-all.sh — Build Commit for all platforms
#
# For distribution builds, set env vars:
#   DEVELOPER_ID="Developer ID Application: ..." \
#   NOTARY_PROFILE="commit-notary" \
#   ./scripts/build-all.sh
set -e

echo "Building Commit for all platforms"
echo "================================="
echo ""

./scripts/build-mac.sh "$@"
echo ""
./scripts/build-windows.sh
echo ""

echo "All builds complete."
ls -lh Commit-*.dmg Commit-*.zip 2>/dev/null
