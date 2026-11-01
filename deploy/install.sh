#!/usr/bin/env bash
# Install/upgrade Dedoo Multisig Manager on a systemd host. Run as root.
#
#   ./install.sh /path/to/dedoo-mm [package-dir]
#
# Creates the dedoo system user, installs the binary under /opt/dedoo-mm,
# installs the systemd unit and enables the service. An existing dedoo-mm.env
# or state file is never overwritten.
set -euo pipefail

BIN="${1:?usage: install.sh <dedoo-mm-binary> [package-dir]}"
PKG_DIR="${2:-$(cd "$(dirname "$0")" && pwd)}"
DEST=/opt/dedoo-mm
UNIT=/etc/systemd/system/dedoo-mm.service
SERVICE_USER=dedoo

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi
if [ ! -x "$BIN" ]; then
  echo "binary not found or not executable: $BIN" >&2
  exit 1
fi

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home "$DEST" --shell /usr/sbin/nologin --user-group "$SERVICE_USER"
fi

install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DEST"
install -m 0755 -o root -g root "$BIN" "$DEST/dedoo-mm.new"
mv -f "$DEST/dedoo-mm.new" "$DEST/dedoo-mm"

if [ ! -f "$DEST/dedoo-mm.env" ]; then
  if [ -f "$PKG_DIR/dedoo-mm.env.example" ]; then
    install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_USER" \
      "$PKG_DIR/dedoo-mm.env.example" "$DEST/dedoo-mm.env"
    echo "installed template: $DEST/dedoo-mm.env  <-- EDIT THIS before going live"
  fi
else
  chown "$SERVICE_USER:$SERVICE_USER" "$DEST/dedoo-mm.env"
  chmod 0600 "$DEST/dedoo-mm.env"
  echo "kept existing $DEST/dedoo-mm.env"
fi

install -m 0644 "$PKG_DIR/dedoo-mm.service" "$UNIT"
systemctl daemon-reload
systemctl enable dedoo-mm.service

echo
echo "Installed. Next steps:"
echo "  1. edit $DEST/dedoo-mm.env (WIF, vault, redeem script, peers)"
echo "  2. systemctl start dedoo-mm"
echo "  3. systemctl status dedoo-mm --no-pager"
