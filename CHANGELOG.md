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

### Not yet implemented
- Runtime loop (Supabase WebSocket subscribe on `commands`,
  status-snapshot push every 30s)
- Proxmox REST adapter
- Command whitelist + payload validation
