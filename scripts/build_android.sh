#!/usr/bin/env bash
#
# Build script for the Ollama Android app.
#
# Prerequisites:
#   - Go 1.24+
#   - Android NDK (via ANDROID_NDK_HOME or sdkmanager)
#   - gomobile (go install golang.org/x/mobile/cmd/gomobile@latest && gomobile init)
#   - Node.js / npm (for building the React SPA)
#   - Java 17+ and ANDROID_HOME set (for Gradle)
#
# Usage:
#   ./scripts/build_android.sh          # debug APK
#   ./scripts/build_android.sh release  # release APK
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ANDROID_DIR="$REPO_ROOT/android"
GO_MOBILE_PKG="$REPO_ROOT/app/cmd/android/ollama"
AAR_OUTPUT="$ANDROID_DIR/app/libs/ollama.aar"
GOMOBILE_VERSION="${GOMOBILE_VERSION:-v0.0.0-20241204233305-ce44b2716d33}"
GRADLE_BIN="${GRADLE_BIN:-gradle}"
APP_VERSION_NAME="${APP_VERSION_NAME:-${VERSION:-0.0.0}}"
APP_VERSION_CODE="${APP_VERSION_CODE:-1}"

RESTORE_MODULE_FILES=false
if command -v git &>/dev/null; then
    if git -C "$REPO_ROOT" diff --quiet -- go.mod go.sum && git -C "$REPO_ROOT" diff --cached --quiet -- go.mod go.sum; then
        RESTORE_MODULE_FILES=true
    fi
fi

cleanup_module_files() {
    if [ "$RESTORE_MODULE_FILES" = true ] && command -v git &>/dev/null; then
        git -C "$REPO_ROOT" restore --source=HEAD -- go.mod go.sum >/dev/null 2>&1 || true
    fi
}

trap cleanup_module_files EXIT

echo "=== Building Ollama Android App ==="

# ──────────────────────────────────────────────
# 1. Build the React SPA (same as desktop)
# ──────────────────────────────────────────────
echo ""
echo "--- Step 1: Build React SPA ---"
cd "$REPO_ROOT/app/ui/app"
if [ ! -d node_modules ]; then
    npm install
fi
npm run build
echo "React SPA built → app/ui/app/dist/"

# ──────────────────────────────────────────────
# 2. Generate TypeScript types from Go
# ──────────────────────────────────────────────
echo ""
echo "--- Step 2: Generate Go → TypeScript types ---"
cd "$REPO_ROOT"
go generate ./app/ui 2>/dev/null || echo "(skipping go generate — tscriptify may not be installed)"

# ──────────────────────────────────────────────
# 3. Build Go library as Android AAR via gomobile
# ──────────────────────────────────────────────
echo ""
echo "--- Step 3: gomobile bind → AAR ---"

echo "Using golang.org/x/mobile@$GOMOBILE_VERSION"
go get "golang.org/x/mobile/bind@$GOMOBILE_VERSION"
go install "golang.org/x/mobile/cmd/gomobile@$GOMOBILE_VERSION"
go install "golang.org/x/mobile/cmd/gobind@$GOMOBILE_VERSION"
export PATH="$(go env GOPATH)/bin:$PATH"
gomobile init

mkdir -p "$(dirname "$AAR_OUTPUT")"

# Target arm64 and amd64 (for emulators)
gomobile bind \
    -v \
    -target=android/arm64,android/amd64 \
    -androidapi 26 \
    -o "$AAR_OUTPUT" \
    "$GO_MOBILE_PKG"

echo "AAR built → $AAR_OUTPUT"

# ──────────────────────────────────────────────
# 4. Build Android APK via Gradle
# ──────────────────────────────────────────────
echo ""
echo "--- Step 4: Gradle build ---"

if ! command -v "$GRADLE_BIN" &>/dev/null; then
    echo "Gradle executable '$GRADLE_BIN' was not found on PATH."
    exit 1
fi

BUILD_TYPE="${1:-debug}"
if [ "$BUILD_TYPE" = "release" ]; then
    "$GRADLE_BIN" -p "$ANDROID_DIR" assembleRelease \
        -PappVersionName="$APP_VERSION_NAME" \
        -PappVersionCode="$APP_VERSION_CODE"
    APK_PATH="app/build/outputs/apk/release/app-release-unsigned.apk"
else
    "$GRADLE_BIN" -p "$ANDROID_DIR" assembleDebug \
        -PappVersionName="$APP_VERSION_NAME" \
        -PappVersionCode="$APP_VERSION_CODE"
    APK_PATH="app/build/outputs/apk/debug/app-debug.apk"
fi

echo ""
echo "=== Build complete ==="
echo "APK: $ANDROID_DIR/$APK_PATH"
