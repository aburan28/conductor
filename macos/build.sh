#!/usr/bin/env bash
# Build Conductor.app with the Command Line Tools alone: a universal (arm64 + x86_64) app
# holding the Go commands and the PostgreSQL bundle it runs.
#
#   macos/build.sh                       macos/build/Conductor.app, universal, signed ad hoc
#   macos/build.sh --arch arm64          one architecture, for a quicker local build
#   macos/build.sh --version 1.2.3       stamp a version into Info.plist (default 0.0.0)
#   macos/build.sh --postgres DIR        bundle a PostgreSQL already fetched into DIR
#   macos/build.sh --no-postgres         bundle none: the app can then only attach to a database
#   macos/build.sh --go-only             cross-compile the Go commands only (works on Linux)
#   macos/build.sh --open                and launch it
#
# Steps: the Go commands (conductor, conductord, conductor-mcp) cross-compiled for darwin
# on both architectures and joined with lipo into Contents/Resources/bin; one `swift build
# --triple` per architecture joined with lipo into Contents/MacOS/Conductor; Sparkle's
# framework copied with ditto into Contents/Frameworks; PostgreSQL (fetch-postgres.sh) into
# Contents/Resources/postgres; then packaging/sign.sh, which signs with
# DEVELOPER_ID_APPLICATION when it is set and ad hoc otherwise, saying which.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
OUT="$HERE/build"
APP="$OUT/Conductor.app"

die() { echo "build: $*" >&2; exit 1; }

ARCHS=(arm64 x86_64)
VERSION=""
PG_DIR=""
NO_POSTGRES=0 GO_ONLY=0 OPEN=0
while [ $# -gt 0 ]; do
    case "$1" in
        --arch)        [ $# -ge 2 ] || die "--arch needs a value"; ARCHS=("$2"); shift 2 ;;
        --version)     [ $# -ge 2 ] || die "--version needs a value"; VERSION="${2#v}"; shift 2 ;;
        --postgres)    [ $# -ge 2 ] || die "--postgres needs a value"; PG_DIR="$2"; shift 2 ;;
        --no-postgres) NO_POSTGRES=1; shift ;;
        --go-only)     GO_ONLY=1; shift ;;
        --open)        OPEN=1; shift ;;
        -h|--help)     sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
done
for arch in "${ARCHS[@]}"; do
    case "$arch" in arm64|x86_64) ;; *) die "--arch must be arm64 or x86_64, not $arch" ;; esac
done

# -- the Go commands ------------------------------------------------------------------
#
# Pure Go (no cgo), so they cross-compile from any machine. The version is stamped the way
# the Makefile stamps it, so `conductor version` and `conductor doctor` can tell the app's
# commands from a stale install.
command -v go >/dev/null 2>&1 || die "go is required to build the conductor commands"
MODULE="$(cd "$REPO" && go list -m)"
BUILD_VERSION="${BUILD_VERSION:-$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo devel)}"
BUILD_COMMIT="${BUILD_COMMIT:-$(git -C "$REPO" rev-parse HEAD 2>/dev/null || true)}"
LDFLAGS="-s -w -X $MODULE/internal/version.version=$BUILD_VERSION -X $MODULE/internal/version.commit=$BUILD_COMMIT"
GO_BINS=(conductor conductord conductor-mcp)
for arch in "${ARCHS[@]}"; do
    goarch="$arch"
    [ "$arch" = x86_64 ] && goarch=amd64
    for bin in "${GO_BINS[@]}"; do
        echo "build: go $bin (darwin/$goarch)"
        (cd "$REPO" && CGO_ENABLED=0 GOOS=darwin GOARCH="$goarch" \
            go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/go/$arch/$bin" "./cmd/$bin")
    done
done
if [ "$GO_ONLY" -eq 1 ]; then
    echo "build: Go commands in $OUT/go/{$(IFS=,; echo "${ARCHS[*]}")}"
    exit 0
fi
[ "$(uname -s)" = "Darwin" ] || die "the app needs macOS to build (swift build for Apple targets, lipo, codesign); --go-only works anywhere"

# -- the app ----------------------------------------------------------------------------
#
# One `swift build` per architecture, joined with lipo, rather than `--arch arm64 --arch
# x86_64`: that form needs Xcode's build system, and this must work with the Command Line
# Tools alone.
cd "$HERE"
SLICES=()
SPARKLE=""
for arch in "${ARCHS[@]}"; do
    triple="$arch-apple-macosx13.0"
    echo "build: swift ($triple)"
    rc=0
    swift build -c release --triple "$triple" 2>&1 | { grep -v '^\[' || true; } || rc=$?
    [ "$rc" -eq 0 ] || die "swift build failed for $arch (exit $rc); nothing was packaged"
    bindir="$(swift build -c release --triple "$triple" --show-bin-path)"
    [ -x "$bindir/Conductor" ] || die "swift build left no $bindir/Conductor"
    SLICES+=("$bindir/Conductor")
    # Sparkle ships as one framework holding both architectures.
    SPARKLE="$bindir/Sparkle.framework"
done
[ -d "$SPARKLE" ] || die "no Sparkle.framework beside the binary in $(dirname "$SPARKLE")"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin" "$APP/Contents/Frameworks"
if [ "${#SLICES[@]}" -eq 1 ]; then
    cp "${SLICES[0]}" "$APP/Contents/MacOS/Conductor"
else
    lipo -create "${SLICES[@]}" -output "$APP/Contents/MacOS/Conductor"
fi
cp "$HERE/Info.plist" "$APP/Contents/Info.plist"
if [ -n "$VERSION" ]; then
    plutil -replace CFBundleShortVersionString -string "$VERSION" "$APP/Contents/Info.plist"
    plutil -replace CFBundleVersion -string "$VERSION" "$APP/Contents/Info.plist"
fi
plutil -lint "$APP/Contents/Info.plist" >/dev/null
# ditto, not cp -R: the framework's Versions/Current links must stay links, or its
# signature no longer matches its layout.
ditto "$SPARKLE" "$APP/Contents/Frameworks/Sparkle.framework"

for bin in "${GO_BINS[@]}"; do
    inputs=()
    for arch in "${ARCHS[@]}"; do inputs+=("$OUT/go/$arch/$bin"); done
    if [ "${#inputs[@]}" -eq 1 ]; then
        cp "${inputs[0]}" "$APP/Contents/Resources/bin/$bin"
    else
        lipo -create "${inputs[@]}" -output "$APP/Contents/Resources/bin/$bin"
    fi
    chmod 0755 "$APP/Contents/Resources/bin/$bin"
done

if [ "$NO_POSTGRES" -eq 0 ]; then
    [ -n "$PG_DIR" ] || PG_DIR="$OUT/postgres"
    [ -x "$PG_DIR/bin/postgres" ] || "$HERE/fetch-postgres.sh" --out "$PG_DIR"
    ditto "$PG_DIR" "$APP/Contents/Resources/postgres"
else
    echo "build: no PostgreSQL bundled (--no-postgres); the app will ask for a database to attach to"
fi

"$HERE/packaging/sign.sh" "$APP"
echo "build: built $APP ($(lipo -archs "$APP/Contents/MacOS/Conductor"))"
if [ "$OPEN" -eq 1 ]; then
    open "$APP"
fi
