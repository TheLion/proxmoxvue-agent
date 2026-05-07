#!/bin/sh
# docker-entrypoint.sh — bootstrap proxmoxvue-agent inside a container.
#
# On first run (no config.yml yet): writes a skeleton config from env vars,
# then runs `--register <CODE>` to exchange the enrollment code for a
# Supabase session. The agent's --register does a non-destructive merge,
# so the proxmox block we wrote is preserved.
#
# On subsequent runs (config.yml already populated): skip enrollment and
# go straight to `--run`. Mount /etc/proxmoxvue-agent as a volume to
# persist the config across restarts.

set -eu

CONFIG_DIR="${PROXMOXVUE_CONFIG_DIR:-/etc/proxmoxvue-agent}"
CONFIG_FILE="${CONFIG_DIR}/config.yml"

require_env() {
    eval "v=\${$1:-}"
    if [ -z "$v" ]; then
        echo "error: $1 is required (set it as an environment variable)" >&2
        exit 2
    fi
}

write_skeleton_config() {
    require_env PROXMOX_API_URL
    require_env PROXMOX_API_TOKEN_ID
    require_env PROXMOX_API_TOKEN_SECRET

    verify_tls="${PROXMOX_VERIFY_TLS:-false}"
    poll_interval="${AGENT_POLL_INTERVAL_SECONDS:-30}"
    log_level="${AGENT_LOG_LEVEL:-info}"
    log_path="${AGENT_LOG_FILE_PATH:-/var/log/proxmoxvue-agent.log}"

    mkdir -p "$CONFIG_DIR"
    chmod 0700 "$CONFIG_DIR"

    umask 077
    cat > "$CONFIG_FILE" <<YAML
proxmox:
  api_url: ${PROXMOX_API_URL}
  api_token_id: ${PROXMOX_API_TOKEN_ID}
  api_token_secret: ${PROXMOX_API_TOKEN_SECRET}
  verify_tls: ${verify_tls}

agent:
  poll_interval_seconds: ${poll_interval}
  log_level: ${log_level}
  log_file_path: ${log_path}
YAML
    chmod 0600 "$CONFIG_FILE"
}

if [ ! -f "$CONFIG_FILE" ]; then
    require_env PROXMOXVUE_ENROLLMENT_CODE
    echo "no existing config — bootstrapping a fresh agent"
    write_skeleton_config

    # --register exchanges the enrollment code for a Supabase session and
    # rewrites the supabase: block. Non-TTY auto-skips the Proxmox prompt
    # (we already wrote that block above).
    proxmoxvue-agent --register \
        --config "$CONFIG_FILE" \
        "$PROXMOXVUE_ENROLLMENT_CODE"
else
    echo "existing config found at $CONFIG_FILE — skipping enrollment"
fi

exec proxmoxvue-agent --run --config "$CONFIG_FILE"
