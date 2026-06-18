#!/usr/bin/env bash
# dist.sh — produce universal (arm64+x86_64) downloadable artifacts for Homebrew:
#   dist/BrokerMenuBar-<version>.zip   (the menu-bar app; bundles a universal broker)
#   dist/brokerctl-<version>.tar.gz    (the CLI)
# Prints the sha256 of each for the cask/formula.
#
#   scripts/dist.sh [version]
#
# Signing (optional, for public download): set CODESIGN_ID to a "Developer ID
# Application: …" identity to sign + hardened-runtime; otherwise the artifacts are
# ad-hoc signed (fine for a Homebrew cask, which strips quarantine on install).
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0)}"
VERSION="${VERSION#v}"
MODULE="github.com/antoniospapantoniou/bazel-broker"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-X $MODULE/internal/version.Version=$VERSION -X $MODULE/internal/version.Commit=$COMMIT -X $MODULE/internal/version.Date=$DATE"
SIGN="${CODESIGN_ID:--}"   # "-" = ad-hoc

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

# Sign the bundled broker (a Mach-O in Resources) with the app identity so a
# notarized build doesn't fail on an unsigned nested executable.
if [ "$SIGN" != "-" ]; then
  codesign --force --timestamp --options runtime -s "$SIGN" "$APP/Contents/Resources/broker" || true
  codesign --force --timestamp --options runtime -s "$SIGN" "$APP" || true
fi

echo "==> package"
cp -R "$APP" "$DIST/"
( cd "$DIST" && ditto -c -k --sequesterRsrc --keepParent BrokerMenuBar.app "BrokerMenuBar-$VERSION.zip" && rm -rf BrokerMenuBar.app )
tar -czf "$DIST/brokerctl-$VERSION.tar.gz" -C bin brokerctl

echo
echo "==> artifacts (sha256 for the cask/formula):"
for f in "$DIST/BrokerMenuBar-$VERSION.zip" "$DIST/brokerctl-$VERSION.tar.gz"; do
  printf '  %s  %s\n' "$(shasum -a 256 "$f" | cut -d' ' -f1)" "$(basename "$f")"
done
echo "==> version: $VERSION  (signing: $([ "$SIGN" = "-" ] && echo ad-hoc || echo "$SIGN"))"
