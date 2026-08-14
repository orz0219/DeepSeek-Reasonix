#!/usr/bin/env bash
# One-shot macOS desktop build for personal use: runs the standard pipeline
# (scripts/desktop-build.sh) and then keeps only the runnable Reasonix.app in
# dist/ — no .zip/.dmg archives. The .app is ad-hoc signed (no Apple Developer
# ID), so first launch may need `xattr -dr com.apple.quarantine` (see
# desktop/README.md).
#
# Usage: scripts/build-mac-desktop.sh [version] [arch]
#   version  defaults to the newest entry in release-notes/releases.json
#   arch     defaults to arm64; pass "universal" for Apple Silicon + Intel
#
# Requirements: same as scripts/desktop-build.sh (Go, Node, pnpm 10, Wails CLI).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# v1.24.1 -> v1.24.2; any -prerelease suffix is stripped before bumping the
# patch number (v1.24.2-beta.1 -> v1.24.3).
next_version() {
	python3 -c '
import re, sys
m = re.match(r"v?(\d+)\.(\d+)\.(\d+)", sys.argv[1])
if not m:
	sys.exit(1)
print("v%d.%d.%d" % (int(m.group(1)), int(m.group(2)), int(m.group(3)) + 1))
' "$1"
}

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
	# Default: newest entry in release-notes/releases.json, bumped by one
	# patch version. When stdin is a TTY, prompt to confirm or override;
	# in non-TTY (CI) runs the bumped version is used without prompting.
	CURRENT_VERSION="$(python3 -c 'import json;print(json.load(open("release-notes/releases.json"))["releases"][0]["version"])' 2>/dev/null || true)"
	[ -n "$CURRENT_VERSION" ] && CURRENT_VERSION="v${CURRENT_VERSION#v}"
	if [ -z "$CURRENT_VERSION" ]; then
		echo "cannot determine version from release-notes/releases.json; pass it explicitly: $0 <version> [arch]" >&2
		exit 1
	fi
	NEXT_VERSION="$(next_version "$CURRENT_VERSION")" || {
		echo "cannot bump version: $CURRENT_VERSION" >&2
		exit 1
	}
	if [ -t 0 ]; then
		echo "current release: $CURRENT_VERSION"
		read -r -p "version to build [default: $NEXT_VERSION]: " VERSION
		VERSION="${VERSION:-$NEXT_VERSION}"
	else
		VERSION="$NEXT_VERSION"
	fi
fi
ARCH="${2:-arm64}"
case "$ARCH" in
arm64 | amd64 | universal) ;;
*) echo "unsupported arch: $ARCH (use arm64|amd64|universal)" >&2; exit 1 ;;
esac
# VERSION flows into go -ldflags via desktop-build.sh; reject anything that
# could smuggle extra flags (spaces, quotes, -X syntax).
if ! printf '%s' "$VERSION" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; then
	echo "invalid version: $VERSION (want e.g. v1.24.2)" >&2
	exit 1
fi

command -v python3 >/dev/null 2>&1 || { echo "python3 missing" >&2; exit 1; }
if ! command -v wails >/dev/null 2>&1; then
	# `make wails-install` installs into GOBIN, which may not be on PATH.
	GOBIN="$(go env GOBIN 2>/dev/null || true)"
	[ -n "$GOBIN" ] || GOBIN="$(go env GOPATH)/bin"
	if [ -x "$GOBIN/wails" ]; then
		PATH="$GOBIN:$PATH"
	else
		echo "wails CLI missing; run 'make wails-install' first" >&2
		exit 1
	fi
fi

# desktop-build.sh rewrites the productVersion inside desktop/wails.json; restore
# it afterwards unless the user had their own uncommitted changes there.
wails_json_was_clean=1
git diff --quiet -- desktop/wails.json || wails_json_was_clean=0
staging=""
restore_side_effects() {
	if [ "$wails_json_was_clean" = "1" ]; then
		git checkout -- desktop/wails.json || echo "warning: could not restore desktop/wails.json" >&2
	fi
	[ -n "$staging" ] && rm -rf "$staging"
}
trap restore_side_effects EXIT

echo "==> desktop-build.sh darwin/$ARCH $VERSION"
DESKTOP_BUILD_SKIP_DMG=1 scripts/desktop-build.sh "darwin/$ARCH" "$VERSION"

# Reassemble the bundle the way desktop-build.sh's staging does, then keep only
# the runnable .app in dist/ instead of the .zip archive.
staging="$(mktemp -d)"
app="$staging/Reasonix.app"
cp -R desktop/build/bin/reasonix-desktop.app "$app"
cp desktop/build/bin/reasonix "$app/Contents/MacOS/reasonix"

bundle_executable=$(/usr/libexec/PlistBuddy -c "Print :CFBundleExecutable" "$app/Contents/Info.plist")
[ "$bundle_executable" = "reasonix-desktop" ] || { echo "macOS bundle executable is $bundle_executable, want reasonix-desktop" >&2; exit 1; }

bundle_icon=$(/usr/libexec/PlistBuddy -c "Print :CFBundleIconFile" "$app/Contents/Info.plist")
case "$bundle_icon" in
*.icns) ;;
*) bundle_icon="$bundle_icon.icns" ;;
esac
[ -s "desktop/build/darwin/icon.icns" ] || { echo "macOS source icon is missing: desktop/build/darwin/icon.icns" >&2; exit 1; }
cp "desktop/build/darwin/icon.icns" "$app/Contents/Resources/$bundle_icon"

codesign --force --deep -s - "$app"

mkdir -p dist
rm -rf dist/Reasonix.app
rm -f dist/Reasonix-darwin-*.zip
cp -R "$app" dist/Reasonix.app

echo "==> done: dist/Reasonix.app (${ARCH}, $VERSION, ad-hoc signed)"
