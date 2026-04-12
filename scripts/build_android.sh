#!/usr/bin/env bash
#
# Build script for the Ollama Android app.
#
# Prerequisites:
#   - Go 1.25+
#   - Android NDK (via ANDROID_NDK_HOME or sdkmanager)
#   - Node.js / npm (for building the React SPA)
#   - Java 17+ and ANDROID_HOME set (for Gradle)
#
# Usage:
#   ./scripts/build_android.sh          # debug APK
#   ./scripts/build_android.sh release  # release APK + AAB
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ANDROID_DIR="$REPO_ROOT/android"
ANDROID_APP_DIR="$ANDROID_DIR/app"
GOBIND_DIR="$ANDROID_DIR/gobind"
JNI_LIB_DIR="$ANDROID_APP_DIR/src/main/jniLibs"
GRADLE_BIN="${GRADLE_BIN:-gradle}"
APP_VERSION_NAME="${APP_VERSION_NAME:-${VERSION:-0.0.0}}"
APP_VERSION_CODE="${APP_VERSION_CODE:-1}"

export GO111MODULE=on

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
# 3. Build JNI libraries from committed gobind sources
# ──────────────────────────────────────────────
echo ""
echo "--- Step 3: Build JNI libraries ---"

if [ -n "${ANDROID_NDK_HOME:-}" ]; then
    host_tag="linux-x86_64"
    case "$(uname -s)" in
        Darwin)
            if [ "$(uname -m)" = "arm64" ]; then
                host_tag="darwin-arm64"
            else
                host_tag="darwin-x86_64"
            fi
            ;;
        Linux)
            host_tag="linux-x86_64"
            ;;
    esac

    ndk_bin="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/$host_tag/bin"
else
    echo "ANDROID_NDK_HOME is required to build the Android JNI libraries."
    exit 1
fi

build_gojni() {
    local goarch="$1"
    local abi="$2"
    local clang_triple="$3"
    local work_dir="$ANDROID_DIR/.build/gobind-$goarch"
    local out_dir="$JNI_LIB_DIR/$abi"
    local cc="$ndk_bin/${clang_triple}26-clang"
    local cxx="$ndk_bin/${clang_triple}26-clang++"

    rm -rf "$work_dir"
    mkdir -p "$work_dir/src" "$out_dir"
    cp -R "$GOBIND_DIR/src/gobind" "$work_dir/src/"

    cat > "$work_dir/go.mod" <<EOF
module gobind

go 1.26.0

require (
    github.com/ollama/ollama v0.0.0
    golang.org/x/mobile v0.0.0-20260410095206-2cfb76559b7b
)

replace github.com/ollama/ollama => $REPO_ROOT
EOF

    (
        cd "$work_dir"
        GOOS=android GOARCH="$goarch" CGO_ENABLED=1 CC="$cc" CXX="$cxx" go mod tidy
        cd src/gobind
        GOOS=android GOARCH="$goarch" CGO_ENABLED=1 CC="$cc" CXX="$cxx" \
            go build -buildvcs=false -trimpath -buildmode=c-shared -o libgojni.so
    )

    cp "$work_dir/src/gobind/libgojni.so" "$out_dir/libgojni.so"
}

find_release_apk() {
    local release_dir="$ANDROID_DIR/app/build/outputs/apk/release"

    for candidate in app-release.apk app-release-unsigned.apk; do
        if [ -f "$release_dir/$candidate" ]; then
            printf '%s\n' "app/build/outputs/apk/release/$candidate"
            return 0
        fi
    done

    echo "Release APK not found under $release_dir" >&2
    return 1
}

find_release_bundle() {
    local bundle_dir="$ANDROID_DIR/app/build/outputs/bundle/release"

    if [ -f "$bundle_dir/app-release.aab" ]; then
        printf '%s\n' "app/build/outputs/bundle/release/app-release.aab"
        return 0
    fi

    echo "Release AAB not found under $bundle_dir" >&2
    return 1
}

rm -rf "$JNI_LIB_DIR" "$ANDROID_DIR/.build"
build_gojni arm64 arm64-v8a aarch64-linux-android
build_gojni amd64 x86_64 x86_64-linux-android

echo "JNI libraries built → $JNI_LIB_DIR"

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
APK_PATH=""
BUNDLE_PATH=""
if [ "$BUILD_TYPE" = "release" ]; then
    "$GRADLE_BIN" -p "$ANDROID_DIR" assembleRelease bundleRelease \
        -PappVersionName="$APP_VERSION_NAME" \
        -PappVersionCode="$APP_VERSION_CODE"
    APK_PATH="$(find_release_apk)"
    BUNDLE_PATH="$(find_release_bundle)"
else
    "$GRADLE_BIN" -p "$ANDROID_DIR" assembleDebug \
        -PappVersionName="$APP_VERSION_NAME" \
        -PappVersionCode="$APP_VERSION_CODE"
    APK_PATH="app/build/outputs/apk/debug/app-debug.apk"
fi

echo ""
echo "=== Build complete ==="
echo "APK: $ANDROID_DIR/$APK_PATH"
if [ -n "$BUNDLE_PATH" ]; then
    echo "AAB: $ANDROID_DIR/$BUNDLE_PATH"
fi
