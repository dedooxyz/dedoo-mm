#!/usr/bin/env bash
# Build the multi-platform release archives from prebuilt binaries.
#
#   ./scripts/package-release.sh v0.1.0 ./bin
#
# BIN_DIR must contain:
#   dedoo-mm-linux-amd64
#   dedoo-mm-linux-arm64
#   dedoo-mm-linux-armv7
#   dedoo-mm-darwin-amd64
#   dedoo-mm-darwin-arm64
#   dedoo-mm-windows-amd64.exe
#   dedoo-mm-windows-arm64.exe
#
# Output: release-assets/ (override with OUT_DIR).
set -euo pipefail

VERSION="${1:?usage: package-release.sh <version> <bin-dir>}"
BIN_DIR="${2:?usage: package-release.sh <version> <bin-dir>}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${OUT_DIR:-$ROOT/release-assets}"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

rm -rf "$OUT"
mkdir -p "$OUT"

copy_docs() {
  local dir="$1"
  cp "$ROOT/README.md" "$ROOT/SECURITY.md" "$dir/"
  cp "$ROOT/docs/DEPLOYMENT.md" "$ROOT/docs/DISTRIBUTION.md" "$ROOT/docs/AGENT_DEPLOYMENT.md" "$dir/"
}

package_unix() {
  local tag="$1" bin="$2" with_systemd="$3"
  local dir="$STAGE/dedoo-multisig-manager-$VERSION-$tag"
  mkdir -p "$dir"
  cp "$BIN_DIR/$bin" "$dir/dedoo-mm"
  chmod 0755 "$dir/dedoo-mm"
  cp "$ROOT/config/dedoo-mm.env.example" "$dir/"
  copy_docs "$dir"

  if [ "$with_systemd" = yes ]; then
    cp "$ROOT/deploy/dedoo-mm.service" "$ROOT/deploy/install.sh" "$dir/"
    chmod 0755 "$dir/install.sh"
  fi

  case "$tag" in
    darwin-*)
      cat > "$dir/RUN-MACOS.txt" <<'NOTE'
macOS build
===========
This binary is unsigned. If macOS blocks it after download:
  xattr -d com.apple.quarantine ./dedoo-mm
  chmod +x ./dedoo-mm

Set the variables from dedoo-mm.env.example in your shell (or a launchd plist),
then run ./dedoo-mm. The Linux systemd unit is not used on macOS.
NOTE
      ;;
  esac

  if [ "$with_systemd" = yes ]; then
    (cd "$dir" && sha256sum dedoo-mm dedoo-mm.env.example dedoo-mm.service install.sh \
      DEPLOYMENT.md DISTRIBUTION.md AGENT_DEPLOYMENT.md SECURITY.md README.md > SHA256SUMS)
  else
    (cd "$dir" && sha256sum dedoo-mm dedoo-mm.env.example \
      DEPLOYMENT.md DISTRIBUTION.md AGENT_DEPLOYMENT.md SECURITY.md README.md RUN-MACOS.txt > SHA256SUMS)
  fi

  tar -C "$STAGE" -czf "$OUT/dedoo-multisig-manager-$VERSION-$tag.tar.gz" \
    "dedoo-multisig-manager-$VERSION-$tag"
}

package_windows() {
  local tag="$1" bin="$2"
  local dir="$STAGE/dedoo-multisig-manager-$VERSION-$tag"
  mkdir -p "$dir"
  cp "$BIN_DIR/$bin" "$dir/dedoo-mm.exe"
  cp "$ROOT/config/dedoo-mm.env.example" "$dir/"
  copy_docs "$dir"
  cat > "$dir/RUN-WINDOWS.txt" <<'NOTE'
Windows build
=============
1. Copy dedoo-mm.env.example to dedoo-mm.env and edit it.
2. Set each variable in PowerShell, for example:
     $env:CHAIN="wojakcoin"
     $env:P2P_BOOTSTRAP="<relay multiaddr provided privately>"
     ...
   (Do not commit or share dedoo-mm.env.)
3. Run: .\dedoo-mm.exe

The Linux systemd unit is not used on Windows.
NOTE
  (cd "$dir" && sha256sum dedoo-mm.exe dedoo-mm.env.example \
    DEPLOYMENT.md DISTRIBUTION.md AGENT_DEPLOYMENT.md SECURITY.md README.md RUN-WINDOWS.txt > SHA256SUMS)
  (cd "$STAGE" && zip -qr "$OUT/dedoo-multisig-manager-$VERSION-$tag.zip" \
    "dedoo-multisig-manager-$VERSION-$tag")
}

package_unix linux-amd64 dedoo-mm-linux-amd64 yes
package_unix linux-arm64 dedoo-mm-linux-arm64 yes
package_unix linux-armv7 dedoo-mm-linux-armv7 yes
package_unix darwin-amd64 dedoo-mm-darwin-amd64 no
package_unix darwin-arm64 dedoo-mm-darwin-arm64 no
package_windows windows-amd64 dedoo-mm-windows-amd64.exe
package_windows windows-arm64 dedoo-mm-windows-arm64.exe

(cd "$OUT" && sha256sum ./*.tar.gz ./*.zip > SHA256SUMS)
echo "release assets written to $OUT"
ls -lh "$OUT"
