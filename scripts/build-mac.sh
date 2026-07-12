#!/bin/bash
# build-mac.sh — Build Commit as a signed, notarized DMG installer for macOS
#
# Modes (auto-detected):
#   1. Distribution mode:
#      DEVELOPER_ID="Developer ID Application: ..." NOTARY_PROFILE="commit-notary" ./scripts/build-mac.sh
#
#   2. Local/dev mode (no env vars):
#      Ad-hoc signed — will trip Gatekeeper on other Macs
#
# Options:
#   --skip-build       Reuse existing binaries
#   --skip-notarize    Sign but skip notarization
set -e

VERSION="1.4.1"
APP_NAME="Commit"
BUNDLE_ID="com.msfoundry.commit"
DMG_NAME="Commit-${VERSION}.dmg"
STAGING_DIR="/tmp/commit-dmg-staging"
APP_BUNDLE="${STAGING_DIR}/${APP_NAME}.app"
ENTITLEMENTS="config/entitlements.plist"

SKIP_BUILD=0
SKIP_NOTARIZE=0
for arg in "$@"; do
    case "$arg" in
        --skip-build)    SKIP_BUILD=1 ;;
        --skip-notarize) SKIP_NOTARIZE=1 ;;
    esac
done

if [[ -n "${DEVELOPER_ID:-}" ]]; then
    SIGN_MODE="distribution"
    SIGN_IDENTITY="${DEVELOPER_ID}"
else
    SIGN_MODE="adhoc"
    SIGN_IDENTITY="-"
    echo "WARNING: DEVELOPER_ID not set — ad-hoc signing only."
    echo "  This DMG will trip Gatekeeper on other Macs."
    echo ""
fi

echo "Building ${APP_NAME} v${VERSION} (${SIGN_MODE})"
echo ""

# Step 1: Build universal binary (arm64 + amd64)
if [[ "${SKIP_BUILD}" == "0" ]]; then
    echo "Compiling universal binary..."
    CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o /tmp/commit-arm64 .
    CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/commit-amd64 .
    lipo -create /tmp/commit-arm64 /tmp/commit-amd64 -output /tmp/commit-universal
    rm /tmp/commit-arm64 /tmp/commit-amd64
    echo "  Done"
    echo ""
fi

# Step 2: Create .app bundle
echo "Creating app bundle..."
rm -rf "${STAGING_DIR}"
mkdir -p "${APP_BUNDLE}/Contents/MacOS"
mkdir -p "${APP_BUNDLE}/Contents/Resources"

cp /tmp/commit-universal "${APP_BUNDLE}/Contents/MacOS/${APP_NAME}"
chmod +x "${APP_BUNDLE}/Contents/MacOS/${APP_NAME}"

cat > "${APP_BUNDLE}/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleName</key>
    <string>${APP_NAME}</string>
    <key>CFBundleDisplayName</key>
    <string>${APP_NAME}</string>
    <key>CFBundleIdentifier</key>
    <string>${BUNDLE_ID}</string>
    <key>CFBundleVersion</key>
    <string>${VERSION}</string>
    <key>CFBundleShortVersionString</key>
    <string>${VERSION}</string>
    <key>CFBundleExecutable</key>
    <string>${APP_NAME}</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleIconFile</key>
    <string>AppIcon</string>
    <key>LSMinimumSystemVersion</key>
    <string>12.0</string>
    <key>LSUIElement</key>
    <true/>
    <key>NSHighResolutionCapable</key>
    <true/>
    <key>NSHumanReadableCopyright</key>
    <string>Copyright 2026 MS Foundry. All rights reserved.</string>
</dict>
</plist>
PLIST

# Copy icon if it exists
if [[ -f "config/AppIcon.icns" ]]; then
    cp "config/AppIcon.icns" "${APP_BUNDLE}/Contents/Resources/"
fi

echo "  Done"
echo ""

# Step 3: Codesign
echo "Signing app bundle..."
if [[ "${SIGN_MODE}" == "distribution" ]]; then
    if [[ ! -f "${ENTITLEMENTS}" ]]; then
        echo "ERROR: Entitlements file missing at ${ENTITLEMENTS}"
        exit 1
    fi
    codesign --force --deep \
        --sign "${SIGN_IDENTITY}" \
        --identifier "${BUNDLE_ID}" \
        --options runtime \
        --entitlements "${ENTITLEMENTS}" \
        --timestamp \
        "${APP_BUNDLE}"
    echo "  Signed with: ${SIGN_IDENTITY}"

    echo "Verifying signature..."
    codesign --verify --strict --verbose=2 "${APP_BUNDLE}"
    echo "  Verified"
else
    codesign --force --sign - --identifier "${BUNDLE_ID}" "${APP_BUNDLE}/Contents/MacOS/${APP_NAME}"
    echo "  Ad-hoc signed (not for distribution)"
fi
echo ""

# Step 4: Notarize (distribution mode only)
if [[ "${SIGN_MODE}" == "distribution" && "${SKIP_NOTARIZE}" == "0" ]]; then
    if [[ -z "${NOTARY_PROFILE:-}" ]]; then
        echo "WARNING: NOTARY_PROFILE not set — skipping notarization."
        echo "  To notarize, run:"
        echo "    xcrun notarytool store-credentials \"commit-notary\" \\"
        echo "      --apple-id <your-apple-id> --team-id <TEAMID> --password <app-specific-pw>"
        echo ""
    else
        echo "Notarizing app (this takes 1-10 minutes)..."
        APP_ZIP="${STAGING_DIR}/${APP_NAME}-notarize.zip"
        ditto -c -k --keepParent "${APP_BUNDLE}" "${APP_ZIP}"
        xcrun notarytool submit "${APP_ZIP}" \
            --keychain-profile "${NOTARY_PROFILE}" \
            --wait
        rm -f "${APP_ZIP}"
        echo "  Notarized"

        echo "Stapling ticket..."
        xcrun stapler staple "${APP_BUNDLE}"
        xcrun stapler validate "${APP_BUNDLE}"
        echo "  Stapled"
        echo ""
    fi
fi

# Step 5: Create DMG
echo "Creating DMG..."
rm -f "${DMG_NAME}"

ln -sf /Applications "${STAGING_DIR}/Applications"

hdiutil create \
    -volname "${APP_NAME} ${VERSION}" \
    -srcfolder "${STAGING_DIR}" \
    -ov \
    -format UDZO \
    "${DMG_NAME}"

echo "  Created: ${DMG_NAME}"
echo ""

# Step 6: Sign + notarize the DMG (distribution mode only)
if [[ "${SIGN_MODE}" == "distribution" ]]; then
    echo "Signing DMG..."
    codesign --force --sign "${SIGN_IDENTITY}" --timestamp "${DMG_NAME}"
    echo "  Signed"

    if [[ "${SKIP_NOTARIZE}" == "0" && -n "${NOTARY_PROFILE:-}" ]]; then
        echo "Notarizing DMG..."
        xcrun notarytool submit "${DMG_NAME}" \
            --keychain-profile "${NOTARY_PROFILE}" \
            --wait
        echo "  Notarized"

        echo "Stapling DMG..."
        xcrun stapler staple "${DMG_NAME}"
        xcrun stapler validate "${DMG_NAME}"
        echo "  Stapled"
    fi
    echo ""
fi

# Cleanup
rm -rf "${STAGING_DIR}"
rm -f /tmp/commit-universal

DMG_SIZE=$(du -h "${DMG_NAME}" | cut -f1)
echo "========================================="
echo "Commit v${VERSION} — macOS (${SIGN_MODE})"
echo "  File: ${DMG_NAME}"
echo "  Size: ${DMG_SIZE}"
echo ""
echo "To install:"
echo "  1. Open ${DMG_NAME}"
echo "  2. Drag Commit to Applications"
echo "  3. Run: /Applications/Commit.app/Contents/MacOS/Commit"
echo "  4. Open http://localhost:9384 in your browser"
echo "========================================="
