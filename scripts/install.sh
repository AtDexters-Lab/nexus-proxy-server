#!/usr/bin/env bash
set -euo pipefail

# Nexus Proxy Server installer (HTTP-01 ACME ready)
# Usage: curl -fsSL https://raw.githubusercontent.com/OWNER/REPO/main/scripts/install.sh | bash

REPO="AtDexters-Lab/nexus-proxy-server"
BIN_NAME="nexus-proxy-server"
INSTALL_DIR="/usr/local/bin"
ETC_DIR="/etc/nexus-proxy-server"
DATA_DIR="/var/lib/nexus-proxy-server"
ACME_DIR="$DATA_DIR/acme_certs"
SERVICE_NAME="nexus-proxy-server"
SYSTEMD_UNIT="/etc/systemd/system/${SERVICE_NAME}.service"

require_root() {
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    echo "This installer must run as root. Re-run with sudo." >&2
    exit 1
  fi
}

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "Missing dependency: $1" >&2; exit 1; }
}

detect_arch() {
  local m
  m=$(uname -m)
  case "$m" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) echo "Unsupported architecture: $m" >&2; exit 1 ;;
  esac
}

latest_asset_url() {
  local arch="$1"
  # Use GitHub's latest release endpoint for asset name
  echo "https://github.com/${REPO}/releases/latest/download/${BIN_NAME}_linux_${arch}.tar.gz"
}

gen_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

prompt() {
  local msg="$1" def="${2:-}"
  if [[ -n "$def" ]]; then
    read -rp "$msg [$def]: " ans
    echo "${ans:-$def}"
  else
    read -rp "$msg: " ans
    echo "$ans"
  fi
}

normalize_host() {
  # lower-case, strip trailing dot
  local h="$1"
  h="${h%%.}"
  echo "${h,,}"
}

get_public_ip() {
  if command -v dig >/dev/null 2>&1; then
    dig +short -4 myip.opendns.com @resolver1.opendns.com || true
  fi
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL https://api.ipify.org || true
  fi
}

resolve_a() {
  local host="$1"
  if command -v dig >/dev/null 2>&1; then
    dig +short -4 "$host" || true
  else
    getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u || true
  fi
}

write_config() {
  local cfg="$1" host="$2" secret="$3" idle="$4"
  mkdir -p "$(dirname "$cfg")"
  cat > "$cfg" <<YAML
backendListenAddress: ":8443"
peerListenAddress: ":8444"
hubPublicHostname: "$host"
relayPorts:
  - 443
  - 80
idleTimeoutSeconds: $idle
backendsJWTSecret: "$secret"
peers: []
peerAuthentication:
  trustedDomainSuffixes: []
acmeCacheDir: "$ACME_DIR"
YAML
}

write_systemd() {
  local unit="$1" user="$2"
  cat > "$unit" <<UNIT
[Unit]
Description=Nexus Proxy Server
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=$user
Group=$user
ExecStart=$INSTALL_DIR/$BIN_NAME -config $ETC_DIR/config.yaml
WorkingDirectory=$DATA_DIR
Restart=on-failure
RestartSec=2s
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT
}

main() {
  require_root
  need curl
  need tar
  need systemctl

  local arch
  arch=$(detect_arch)

  echo "==> Creating user and directories"
  id -u nexus >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin nexus
  mkdir -p "$INSTALL_DIR" "$ETC_DIR" "$DATA_DIR" "$ACME_DIR"
  chown -R nexus:nexus "$ETC_DIR" "$DATA_DIR"

  echo "==> Downloading ${BIN_NAME} for linux_${arch}"
  local url
  url=$(latest_asset_url "$arch")
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT
  curl -fL "$url" -o "$tmpdir/${BIN_NAME}.tar.gz"
  tar -xzf "$tmpdir/${BIN_NAME}.tar.gz" -C "$tmpdir"
  install -m 0755 "$tmpdir/${BIN_NAME}" "$INSTALL_DIR/${BIN_NAME}"

  if command -v setcap >/dev/null 2>&1; then
    echo "==> Granting CAP_NET_BIND_SERVICE to binary"
    setcap cap_net_bind_service=+ep "$INSTALL_DIR/${BIN_NAME}" || true
  fi

  echo "==> Gathering configuration"
  local host idle
  host=$(prompt "Enter the FQDN for Nexus (for Let's Encrypt HTTP-01)" "nexus.example.com")
  host=$(normalize_host "$host")
  idle=$(prompt "Idle timeout (seconds)" "60")
  local secret
  secret=$(gen_secret)

  echo "==> Please create/point DNS A record for $host to this server's public IP"
  local myip aips
  myip=$(get_public_ip)
  echo "Detected public IP: ${myip:-unknown}"
  while true; do
    read -rp "Press Enter after updating DNS (or type 'skip' to continue): " ans
    if [[ "${ans:-}" == "skip" ]]; then
      break
    fi
    aips=$(resolve_a "$host")
    echo "Current $host A records: ${aips:-none}"
    if [[ -n "$myip" && -n "$aips" && "$aips" == *"$myip"* ]]; then
      echo "DNS appears to point to this server."
      break
    fi
  done

  echo "==> Writing configuration to $ETC_DIR/config.yaml"
  write_config "$ETC_DIR/config.yaml" "$host" "$secret" "$idle"
  chown nexus:nexus "$ETC_DIR/config.yaml"
  chmod 0640 "$ETC_DIR/config.yaml"

  echo "==> Installing systemd service"
  write_systemd "$SYSTEMD_UNIT" nexus
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME"
  systemctl restart "$SERVICE_NAME"

  echo "\nInstallation complete. Details:"
  echo "  Binary: $INSTALL_DIR/$BIN_NAME"
  echo "  Config: $ETC_DIR/config.yaml"
  echo "  Data:   $DATA_DIR (ACME cache: $ACME_DIR)"
  echo "  Service: systemctl status $SERVICE_NAME"
  echo "\nBackend JWT secret (share with your backend clients):\n  $secret\n"
}

main "$@"

