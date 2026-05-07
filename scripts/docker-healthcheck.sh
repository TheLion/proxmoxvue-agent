#!/bin/sh
# docker-healthcheck.sh — verify the agent's poll loop is still ticking.
#
# The agent touches ${PROXMOXVUE_CONFIG_DIR}/.last-poll on every poll
# attempt regardless of log level. A stale mtime (>2 min) means the
# loop has frozen even if the process is still up.
set -u

HEARTBEAT="${PROXMOXVUE_CONFIG_DIR:-/etc/proxmoxvue-agent}/.last-poll"

test -n "$(find "$HEARTBEAT" -mmin -2 2>/dev/null)" || exit 1
