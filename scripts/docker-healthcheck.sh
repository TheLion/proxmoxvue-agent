#!/bin/sh
# docker-healthcheck.sh — verify the agent's poll loop is still ticking.
#
# Reads agent.log_file_path from config.yml so the check matches whatever
# path the operator (or the agent's compiled-in default) has chosen. The
# agent writes an INFO line every poll (default 30s); a stale mtime
# (>2 min) means the loop has frozen even if the process is still up.
set -u

CONFIG_FILE="${PROXMOXVUE_CONFIG_DIR:-/etc/proxmoxvue-agent}/config.yml"
DEFAULT_LOG="/var/log/proxmoxvue-agent.log"

LOG_PATH=$(awk '
    /^[[:space:]]*log_file_path:/ {
        sub(/^[^:]*:[[:space:]]*/, "")
        gsub(/^["'\''[:space:]]+|["'\''[:space:]]+$/, "")
        print
        exit
    }
' "$CONFIG_FILE" 2>/dev/null)

[ -n "$LOG_PATH" ] || LOG_PATH="$DEFAULT_LOG"

test -n "$(find "$LOG_PATH" -mmin -2 2>/dev/null)" || exit 1
