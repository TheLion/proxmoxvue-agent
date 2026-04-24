# Changelog

All notable changes to `proxmoxvue-agent` will be documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Initial agent skeleton with `--register CODE` enrollment flow
- Config loading/saving (`/etc/proxmoxvue-agent/config.yml`, mode `0600`)
- Redacting `String()` methods on credential-bearing structs so
  `fmt.Printf("%+v", cfg)` never leaks the Supabase refresh token
- 30s status-snapshot push (`status_snapshots` INSERT via PostgREST)
- Proxmox REST adapter: `cluster/resources`, power-actions
  (start/stop/reboot/shutdown/suspend/resume) + UPID polling
- Supabase Realtime WS subscribe on `public.commands` with presence
  enabled (iOS clients can see the agent's WS-online state via
  `presence_join`/`presence_leave` — stricter than `last_seen_at`)
- Command dispatcher: atomic claim (conditional PATCH on
  `status=pending`), Proxmox dispatch, await UPID, result write-back
  with `exitstatus`. TTL-expired rows are marked `expired` so the
  iOS-UI can surface that state instead of hanging on `pending`
