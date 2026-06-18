#!/usr/bin/env bash
# dist.sh — produce universal (arm64+x86_64) downloadable artifacts for Homebrew:
#   dist/BrokerMenuBar-<version>.zip   (the menu-bar app; bundles a universal broker)
#   dist/brokerctl-<version>.tar.gz    (the CLI)
# Prints the sha256 of each for the cask/formula.
#
#   scripts/dist.sh [version]
#
# Signing & notarization (optional; needed for a clean download on any Mac):
#   CODESIGN_ID   — a "Developer ID Application: NAME (TEAMID)" identity. When set,
#                   the app + nested broker + CLI are signed with Hardened Runtime
#                   + a secure timestamp. Without it, artifacts are ad-hoc signed
#                   (fine for a Homebrew cask, which strips quarantine on install).
#   To also NOTARIZE + staple the app (removes the Gatekeeper alert everywhere),
#   provide notarytool credentials — either:
#     NOTARY_PROFILE                       — a `notarytool store-credentials` profile, OR
#     APPLE_ID + APPLE_TEAM_ID + NOTARY_PASSWORD  (app-specific password)
#   Notarization requires CODESIGN_ID (you can't notarize an ad-hoc build).
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0)}"
VERSION="${VERSION#v}"
MODULE="github.com/papanton/bazel-broker"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-X $MODULE/internal/version.Version=$VERSION -X $MODULE/internal/version.Commit=$COMMIT -X $MODULE/internal/version.Date=$DATE"
SIGN="${CODESIGN_ID:--}"   # "-" = ad-hoc

# Notarytool auth args (empty array if no creds provided).
notary_auth=()
if [ -n "${NOTARY_PROFILE:-}" ]; then
  notary_auth=(--keychain-profile "$NOTARY_PROFILE")
elif [ -n "${APPLE_ID:-}" ] && [ -n "${APPLE_TEAM_ID:-}" ] && [ -n "${NOTARY_PASSWORD:-}" ]; then
  notary_auth=(--apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$NOTARY_PASSWORD")
fi

DIST="dist"; rm -rf "$DIST"; mkdir -p "$DIST" bin

echo "==> universal Go binaries (arm64 + x86_64)"
for b in broker brokerctl; do
  GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -ldflags "$LDFLAGS" -o "/tmp/$b-arm64" "./cmd/$b"
  GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -ldflags "$LDFLAGS" -o "/tmp/$b-amd64" "./cmd/$b"
  lipo -create "/tmp/$b-arm64" "/tmp/$b-amd64" -output "bin/$b"
  lipo -info "bin/$b"
done

echo "==> universal menu-bar app (Release)"
( cd apps/MenuBar && xcodegen generate >/dev/null )
xcodebuild -project apps/MenuBar/BrokerMenuBar.xcodeproj -scheme BrokerMenuBar \
  -configuration Release -derivedDataPath apps/MenuBar/build/dd \
  ARCHS="arm64 x86_64" ONLY_ACTIVE_ARCH=NO \
  CODE_SIGN_IDENTITY="$SIGN" $( [ "$SIGN" = "-" ] || echo "OTHER_CODE_SIGN_FLAGS=--timestamp --options=runtime" ) \
  build >/tmp/dist-xcodebuild.log 2>&1 || { tail -20 /tmp/dist-xcodebuild.log; exit 1; }
APP="apps/MenuBar/build/dd/Build/Products/Release/BrokerMenuBar.app"

# Sign inside-out with Hardened Runtime: the bundled broker (a Mach-O in Resources)
# first, then re-seal the app — otherwise notarization fails on an unsigned nested
# executable. Also Developer-ID-sign the standalone CLI.
if [ "$SIGN" != "-" ]; then
  echo "==> Developer ID sign (Hardened Runtime) — $SIGN"
  codesign --force --timestamp --options runtime -s "$SIGN" "$APP/Contents/Resources/broker"
  codesign --force --timestamp --options runtime -s "$SIGN" "$APP"
  codesign --force --timestamp --options runtime -s "$SIGN" "bin/brokerctl"
  codesign --verify --deep --strict "$APP" && echo "  codesign verify ok"
fi

echo "==> stage app"
cp -R "$APP" "$DIST/"
APP_OUT="$DIST/BrokerMenuBar.app"

# Notarize + staple the app (so it launches with no Gatekeeper alert on any Mac).
if [ "$SIGN" != "-" ] && [ "${#notary_auth[@]}" -gt 0 ]; then
  echo "==> notarize (notarytool submit --wait) — this can take a few minutes"
  ditto -c -k --keepParent "$APP_OUT" "$DIST/_notarize.zip"
  xcrun notarytool submit "$DIST/_notarize.zip" "${notary_auth[@]}" --wait   # set -e: aborts on Invalid/rejected
  rm -f "$DIST/_notarize.zip"
  echo "==> staple"
  xcrun stapler staple "$APP_OUT"
  xcrun stapler validate "$APP_OUT" && echo "  stapled ✓"
  NOTARY_STATE="notarized + stapled"
elif [ "$SIGN" != "-" ]; then
  NOTARY_STATE="Developer ID signed (NOT notarized — no notarytool creds)"
else
  NOTARY_STATE="ad-hoc (unsigned)"
fi

echo "==> package"
( cd "$DIST" && ditto -c -k --sequesterRsrc --keepParent BrokerMenuBar.app "BrokerMenuBar-$VERSION.zip" && rm -rf BrokerMenuBar.app )
tar -czf "$DIST/brokerctl-$VERSION.tar.gz" -C bin brokerctl

echo
echo "==> artifacts (sha256 for the cask/formula):"
for f in "$DIST/BrokerMenuBar-$VERSION.zip" "$DIST/brokerctl-$VERSION.tar.gz"; do
  printf '  %s  %s\n' "$(shasum -a 256 "$f" | cut -d' ' -f1)" "$(basename "$f")"
done
echo "==> version: $VERSION  ($NOTARY_STATE)"
