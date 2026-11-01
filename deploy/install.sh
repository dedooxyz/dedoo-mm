#!/usr/bin/env bash
# Install/upgrade Dedoo Multisig Manager on a systemd host. Run as root.
#
#   ./install.sh /path/to/wojak-keeper-binary [package-dir]
#
# Creates the wojakkeeper system user, installs the binary under
# /opt/wojak-keeper, installs the systemd unit and enables the service.
# An existing keeper.env or state file is never overwritten.
set -euo pipefail

BIN="${1:?usage: install.sh <wojak-keeper-binary> [package-dir]}"
PKG_DIR="${2:-$(cd "$(dirname "$0")" && pwd)}"
DEST=/opt/wojak-keeper
UNIT=/etc/systemd/system/wojak-keeper.service

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi
if [ ! -x "$BIN" ]; then
  echo "binary not found or not executable: $BIN" >&2
  exit 1
fi

if ! id wojakkeeper >/dev/null 2>&1; then
  useradd --system --home "$DEST" --shell /usr/sbin/nologin --user-group wojakkeeper
fi

install -d -m 0750 -o wojakkeeper -g wojakkeeper "$DEST"
install -m 0755 -o root -g root "$BIN" "$DEST/wojak-keeper.new"
mv -f "$DEST/wojak-keeper.new" "$DEST/wojak-keeper"

if [ ! -f "$DEST/keeper.env" ]; then
  if [ -f "$PKG_DIR/keeper.env.example" ]; then
    install -m 0600 -o wojakkeeper -g wojakkeeper "$PKG_DIR/keeper.env.example" "$DEST/keeper.env"
    echo "installed template: $DEST/keeper.env  <-- EDIT THIS before going live"
  fi
else
  chown wojakkeeper:wojakkeeper "$DEST/keeper.env"
  chmod 0600 "$DEST/keeper.env"
  echo "kept existing $DEST/keeper.env"
fi

install -m 0644 "$PKG_DIR/wojak-keeper.service" "$UNIT"
systemctl daemon-reload
systemctl enable wojak-keeper.service

echo
echo "Installed. Next steps:"
echo "  1. edit $DEST/keeper.env (WIF, vault, redeem script, peers)"
echo "  2. systemctl start wojak-keeper"
echo "  3. systemctl status wojak-keeper --no-pager"
