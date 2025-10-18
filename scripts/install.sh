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

asset_filename() {
  local arch="$1"
  echo "${BIN_NAME}_linux_${arch}.tar.gz"
}

asset_url() {
  local arch="$1" version="${NEXUS_VERSION:-}"
  local base
  base=$(asset_filename "$arch")
  if [[ -n "$version" ]]; then
    echo "https://github.com/${REPO}/releases/download/${version}/${base}"
  else
    echo "https://github.com/${REPO}/releases/latest/download/${base}"
  fi
}

sums_url() {
  local version="${NEXUS_VERSION:-}"
  if [[ -n "$version" ]]; then
    echo "https://github.com/${REPO}/releases/download/${version}/SHA256SUMS"
  else
    echo "https://github.com/${REPO}/releases/latest/download/SHA256SUMS"
  fi
}

read_config_value() {
  local key="$1" default_value="${2:-}"
  local file="$ETC_DIR/config.yaml"
  if [[ -f "$file" ]]; then
    local value
    value=$(awk -v key="$key" '
      $0 ~ "^[[:space:]]*" key ":" {
        sub("^[[:space:]]*" key ":[[:space:]]*", "", $0)
        sub(/[[:space:]]+#.*/, "", $0)
        gsub(/^"/, "", $0)
        gsub(/"$/, "", $0)
        sub(/[[:space:]]+$/, "", $0)
        print
        exit
      }
    ' "$file")
    if [[ -n "${value:-}" ]]; then
      echo "$value"
      return
    fi
  fi
  echo "$default_value"
}

gen_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

TTY_DEVICE="/dev/tty"

prompt() {
  local msg="$1" def="${2:-}"
  local ans=""
  if [[ -r "$TTY_DEVICE" ]]; then
    if [[ -n "$def" ]]; then
      read -r -p "$msg [$def]: " ans < "$TTY_DEVICE" || true
      echo "${ans:-$def}"
    else
      read -r -p "$msg: " ans < "$TTY_DEVICE" || true
      echo "$ans"
    fi
  else
    # Non-interactive: honor defaults or env vars
    echo "$def"
  fi
}

normalize_host() {
  # lower-case, strip trailing dot
  local h="$1"
  h="${h%%.}"
  echo "${h,,}"
}

get_public_ip() {
  local ip=""
  if command -v dig >/dev/null 2>&1; then
    ip=$(dig +short -4 myip.opendns.com @resolver1.opendns.com 2>/dev/null | head -n1 || true)
  fi
  if [[ -z "$ip" ]] && command -v curl >/dev/null 2>&1; then
    ip=$(curl -fsSL https://api.ipify.org 2>/dev/null || true)
  fi
  echo -n "$ip"
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

existing_install() {
  [[ -f "$ETC_DIR/config.yaml" ]] || [[ -f "$SYSTEMD_UNIT" ]]
}

prompt_choice() {
  local msg="$1" def="$2"
  local ans=""
  if [[ -r "$TTY_DEVICE" ]]; then
    read -r -p "$msg [$def]: " ans < "$TTY_DEVICE" || true
    echo "${ans:-$def}"
  else
    echo "$def"
  fi
}

main() {
  require_root
  need curl
  need tar
  need systemctl
  # Prefer sha256sum, fallback to shasum if present
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    echo "WARN: sha256sum/shasum not found; release checksum verification will be skipped" >&2
  fi

  local arch
  arch=$(detect_arch)

  echo "==> Creating user and directories"
  id -u nexus >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin nexus
  mkdir -p "$INSTALL_DIR" "$ETC_DIR" "$DATA_DIR" "$ACME_DIR"
  chown -R nexus:nexus "$ETC_DIR" "$DATA_DIR"

  echo "==> Downloading ${BIN_NAME} for linux_${arch}"
  local url sumsurl asset sumsfile final_url resolved_redirect installed_version
  url=$(asset_url "$arch")
  sumsurl=$(sums_url)
  asset=$(asset_filename "$arch")
  sumsfile="SHA256SUMS"
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT
  installed_version="${NEXUS_VERSION:-}"
  echo "    -> $url"
  if [[ -z "$installed_version" ]]; then
    resolved_redirect=$(curl -fsIL "$url" | awk 'tolower($1) == "location:" {print $2; exit}' | tr -d '\r') || true
    if [[ -n "$resolved_redirect" ]]; then
      installed_version=$(basename "$(dirname "$resolved_redirect")")
      echo "    -> release tag $installed_version"
    fi
  fi
  final_url=$(curl -fsSL -w '%{url_effective}' -o "$tmpdir/${asset}" "$url")
  final_url=${final_url//$'\r'/}
  if [[ -z "$installed_version" ]]; then
    if [[ -n "$resolved_redirect" ]]; then
      installed_version=$(basename "$(dirname "$resolved_redirect")")
    else
      installed_version=$(basename "$(dirname "${final_url}")")
    fi
  fi
  if [[ -n "$final_url" && "$final_url" != "$url" ]]; then
    echo "    -> resolved to $final_url"
  fi
  echo "    -> $sumsurl"
  curl -fL "$sumsurl" -o "$tmpdir/${sumsfile}" || true

  # Verify checksum if tools and sums are present
  if [[ -s "$tmpdir/${sumsfile}" ]]; then
    echo "==> Verifying artifact checksum"
    local expected actual
    expected=$(awk -v f="$asset" '$2==f {print $1}' "$tmpdir/${sumsfile}" | head -n1 || true)
    if [[ -n "$expected" ]]; then
      if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$tmpdir/${asset}" | awk '{print $1}')
      else
        actual=$(shasum -a 256 "$tmpdir/${asset}" | awk '{print $1}')
      fi
      if [[ "$expected" != "$actual" ]]; then
        echo "ERROR: checksum mismatch for $asset" >&2
        echo "  expected: $expected" >&2
        echo "  actual:   $actual" >&2
        exit 1
      fi
    else
      echo "WARN: Could not find $asset in SHA256SUMS; skipping verification" >&2
    fi
  else
    echo "WARN: SHA256SUMS not downloaded; skipping verification" >&2
  fi

  tar -xzf "$tmpdir/${asset}" -C "$tmpdir"
  install -m 0755 "$tmpdir/${BIN_NAME}" "$INSTALL_DIR/${BIN_NAME}"

  if command -v setcap >/dev/null 2>&1; then
    echo "==> Granting CAP_NET_BIND_SERVICE to binary"
    setcap cap_net_bind_service=+ep "$INSTALL_DIR/${BIN_NAME}" || true
  fi

  local reconfig_mode="new"
  if existing_install; then
    echo "==> Existing Nexus installation detected"
    local choice
    choice=$(prompt_choice "Choose action: [U]pgrade binary only, [R]econfigure, [A]bort" "U")
    case "${choice^^}" in
      U) reconfig_mode="upgrade" ;;
      R) reconfig_mode="reconfigure" ;;
      A) echo "Aborting per selection."; exit 0 ;;
      *) reconfig_mode="upgrade" ;;
    esac
  fi

  local host idle secret
  idle=${NEXUS_IDLE_TIMEOUT:-60}
  if [[ "$reconfig_mode" == "reconfigure" || "$reconfig_mode" == "new" ]]; then
    echo "==> Gathering configuration"
    # Allow non-interactive override via env var NEXUS_HOST
    host=${NEXUS_HOST:-$(prompt "Enter the FQDN for Nexus (for Let's Encrypt HTTP-01)" "nexus.example.com")}
    host=$(normalize_host "$host")
    secret=$(gen_secret)

    echo "==> Please create/point DNS A record for $host to this server's public IP"
    local myip aips
    myip=$(get_public_ip)
    echo "Detected public IP: ${myip:-unknown}"
    while true; do
      local ans=""
      if [[ -r "$TTY_DEVICE" ]]; then
        read -r -p "Press Enter after updating DNS (or type 'skip' to continue): " ans < "$TTY_DEVICE" || true
      else
        # Non-interactive environment: honor NEXUS_SKIP_DNS or continue once
        ans=${NEXUS_SKIP_DNS:-skip}
      fi
      if [[ "${ans:-}" == "skip" ]]; then
        break
      fi
      aips=$(resolve_a "$host")
      echo "Current $host A records: ${aips:-none}"
      if [[ -n "$myip" && -n "$aips" ]] && echo "$aips" | grep -Fxq "$myip"; then
        echo "DNS appears to point to this server."
        break
      fi
    done

    printf "==> Writing configuration to %s\n" "$ETC_DIR/config.yaml"
    # Backup existing config if present
    if [[ -f "$ETC_DIR/config.yaml" ]]; then
      cp -f "$ETC_DIR/config.yaml" "$ETC_DIR/config.yaml.bak-$(date +%s)"
    fi
    write_config "$ETC_DIR/config.yaml" "$host" "$secret" "$idle"
    chown nexus:nexus "$ETC_DIR/config.yaml"
    chmod 0640 "$ETC_DIR/config.yaml"
  else
    # Upgrade path: keep existing config and secret
    printf "==> Keeping existing configuration at %s\n" "$ETC_DIR/config.yaml"
    if [[ -f "$ETC_DIR/config.yaml" ]]; then
      chown nexus:nexus "$ETC_DIR/config.yaml"
      chmod 0640 "$ETC_DIR/config.yaml"
    fi
  fi

  echo "==> Installing systemd service"
  if [[ ! -f "$SYSTEMD_UNIT" ]]; then
    write_systemd "$SYSTEMD_UNIT" nexus
  fi
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
  systemctl restart "$SERVICE_NAME" || systemctl start "$SERVICE_NAME" || true

  local hub_host_value="${host:-}" backend_addr_value backend_port backend_endpoint=""
  if [[ -z "$hub_host_value" ]]; then
    hub_host_value=$(read_config_value "hubPublicHostname")
  fi
  backend_addr_value=$(read_config_value "backendListenAddress" ":8443")
  backend_addr_value=${backend_addr_value//[[:space:]]/}
  if [[ -n "$backend_addr_value" ]]; then
    backend_port=${backend_addr_value##*:}
    if [[ "$backend_port" == "$backend_addr_value" || -z "$backend_port" ]]; then
      backend_port="8443"
    fi
  else
    backend_port="8443"
  fi
  if [[ -n "$hub_host_value" ]]; then
    backend_endpoint="wss://${hub_host_value}:${backend_port}/connect"
  fi

  printf "\nInstallation complete. Details:\n"
  printf "  Binary: %s\n" "$INSTALL_DIR/$BIN_NAME"
  printf "  Version: %s\n" "${installed_version:-unknown}"
  printf "  Config: %s\n" "$ETC_DIR/config.yaml"
  printf "  Data:   %s (ACME cache: %s)\n" "$DATA_DIR" "$ACME_DIR"
  printf "  Service: systemctl status %s\n" "$SERVICE_NAME"
  if [[ -n "$backend_endpoint" ]]; then
    printf "  Backend WSS endpoint: %s\n" "$backend_endpoint"
  else
    printf "  Backend WSS endpoint: set hubPublicHostname in %s\n" "$ETC_DIR/config.yaml"
  fi

  if [[ "$reconfig_mode" == "reconfigure" || "$reconfig_mode" == "new" ]]; then
    printf "\nBackend JWT secret (share with your backend clients):\n  %s\n\n" "$secret"
  fi
}

main "$@"
