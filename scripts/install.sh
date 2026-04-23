#!/bin/sh
# proxmoxvue-agent installer
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/TheLion/proxmoxvue-agent/main/scripts/install.sh \
#     | sudo sh -s -- <ENROLLMENT-CODE>
#
#   # Upgrade without re-enrolling:
#   curl -sSL ... | sudo sh -s -- --upgrade-only
#
# This script:
#   1. Detects arch (amd64 / arm64)
#   2. Downloads the latest release binary + SHA256 from GitHub Releases
#   3. Verifies checksum
#   4. Installs to /usr/local/bin/proxmoxvue-agent
#   5. Creates /etc/proxmoxvue-agent/ (0700, root)
#   6. Writes /etc/systemd/system/proxmoxvue-agent.service
#   7. Runs `proxmoxvue-agent --register <CODE>` (unless --upgrade-only)
#   8. systemctl enable --now proxmoxvue-agent

set -eu

REPO="TheLion/proxmoxvue-agent"
BINARY="/usr/local/bin/proxmoxvue-agent"
CONFIG_DIR="/etc/proxmoxvue-agent"
UNIT="/etc/systemd/system/proxmoxvue-agent.service"

UPGRADE_ONLY=0
ENROLLMENT_CODE=""

for arg in "$@"; do
  case "$arg" in
    --upgrade-only) UPGRADE_ONLY=1 ;;
    -h|--help)
      sed -n '2,20p' "$0"
      exit 0 ;;
    *) ENROLLMENT_CODE="$arg" ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "error: must be run as root (use sudo)" >&2
  exit 1
fi

if [ "$UPGRADE_ONLY" -eq 0 ] && [ -z "$ENROLLMENT_CODE" ]; then
  echo "error: enrollment code required" >&2
  echo "usage: install.sh <CODE>   (or --upgrade-only)" >&2
  exit 2
fi

# ── 1. detect arch ────────────────────────────────────────────────────
case "$(uname -m)" in
  x86_64)          ARCH="amd64" ;;
  aarch64|arm64)   ARCH="arm64" ;;
  *) echo "error: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

# ── 2. find latest release ───────────────────────────────────────────
echo "fetching latest release info..."
LATEST=$(curl -sSL "https://api.github.com/repos/${REPO}/releases/latest" \
         | grep '"tag_name":' | head -1 | cut -d'"' -f4)
if [ -z "$LATEST" ]; then
  echo "error: could not determine latest release" >&2
  exit 1
fi
echo "latest version: $LATEST"

TARBALL="proxmoxvue-agent-linux-${ARCH}.tar.gz"
CHECKSUM="${TARBALL}.sha256"
BASE="https://github.com/${REPO}/releases/download/${LATEST}"

TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT

# ── 3. download + verify ─────────────────────────────────────────────
echo "downloading $TARBALL..."
curl -sSL -o "$TMP/$TARBALL"  "$BASE/$TARBALL"
curl -sSL -o "$TMP/$CHECKSUM" "$BASE/$CHECKSUM"

echo "verifying checksum..."
(cd "$TMP" && sha256sum -c "$CHECKSUM") || {
  echo "error: checksum verification failed" >&2
  exit 1
}

# ── 4. install binary ────────────────────────────────────────────────
tar -xzf "$TMP/$TARBALL" -C "$TMP"
install -m 0755 "$TMP/proxmoxvue-agent" "$BINARY"
echo "installed $BINARY"

# ── 5. config dir ────────────────────────────────────────────────────
install -d -m 0700 -o root -g root "$CONFIG_DIR"

# ── 6. systemd unit ──────────────────────────────────────────────────
cat > "$UNIT" <<'EOF'
[Unit]
Description=ProxmoxVue Agent
Documentation=https://github.com/TheLion/proxmoxvue-agent
After=network-online.target pve-cluster.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/proxmoxvue-agent run
Restart=on-failure
RestartSec=5
RestartPreventExitStatus=78
User=root
Group=root
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/etc/proxmoxvue-agent
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$UNIT"
systemctl daemon-reload

# ── 7. enroll (skipped on --upgrade-only) ────────────────────────────
if [ "$UPGRADE_ONLY" -eq 0 ]; then
  echo "enrolling host..."
  "$BINARY" --register "$ENROLLMENT_CODE"
fi

# ── 8. start service ─────────────────────────────────────────────────
systemctl enable proxmoxvue-agent
systemctl restart proxmoxvue-agent
echo ""
echo "proxmoxvue-agent installed and running."
echo "check status:  systemctl status proxmoxvue-agent"
echo "view logs:     journalctl -u proxmoxvue-agent -f"
