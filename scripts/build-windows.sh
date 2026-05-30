#!/bin/bash
# build-windows.sh — Cross-compile Commit for Windows and package as .zip
set -e

VERSION="1.1.1"
APP_NAME="Commit"
ZIP_NAME="Commit-${VERSION}-windows-amd64.zip"
STAGING_DIR="/tmp/commit-windows-staging"

echo "Building ${APP_NAME} v${VERSION} for Windows..."
echo ""

# Build
echo "Compiling Windows amd64 binary..."
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/commit.exe .
echo "  Done"
echo ""

# Package
echo "Creating zip..."
rm -rf "${STAGING_DIR}"
mkdir -p "${STAGING_DIR}/${APP_NAME}"

cp /tmp/commit.exe "${STAGING_DIR}/${APP_NAME}/Commit.exe"

cat > "${STAGING_DIR}/${APP_NAME}/README.txt" <<'README'
Commit — WhatsApp Commitment Tracker
=====================================

Quick Start:
  1. Double-click Commit.exe to start
  2. Open http://localhost:9384 in your browser
  3. Follow the setup wizard (passcode, API key, WhatsApp QR)

Requirements:
  - A Claude API key from https://console.anthropic.com
  - WhatsApp on your phone (for QR linking)

Data is stored in: %USERPROFILE%\.commit\

To stop Commit, close the terminal window or press Ctrl+C.

For more info: https://github.com/mitensampat/commit
README

rm -f "${ZIP_NAME}"
cd "${STAGING_DIR}"
zip -r "${OLDPWD}/${ZIP_NAME}" "${APP_NAME}/"
cd "${OLDPWD}"

rm -rf "${STAGING_DIR}"
rm -f /tmp/commit.exe

ZIP_SIZE=$(du -h "${ZIP_NAME}" | cut -f1)
echo ""
echo "========================================="
echo "Commit v${VERSION} — Windows amd64"
echo "  File: ${ZIP_NAME}"
echo "  Size: ${ZIP_SIZE}"
echo "========================================="
