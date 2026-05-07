# Changelog

All notable changes to `proxmoxvue-agent` will be documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- Docker `HEALTHCHECK` now reads `agent.log_file_path` from `config.yml`
  via a new `scripts/docker-healthcheck.sh` instead of hard-coding
  `${PROXMOXVUE_CONFIG_DIR}/agent.log`. Operators whose config uses the
  agent's compiled-in default (`/var/log/proxmoxvue-agent.log`) or any
  custom path no longer go `unhealthy` despite a working poll loop.

### Added
- HPKE keypair auto-heal at agent startup — when `supabase.private_key`
  is missing in config (e.g. installs from before #1476), the agent
  generates a fresh X25519 keypair, persists the private key to
  config.yml, and uploads the matching public key to
  `clusters.public_key`. Replaces the previous WARN log + manual
  re-enrollment workaround. Idempotent on retry; upload failure is
  best-effort and re-attempted on next restart.
- New `--rotate-key` command — generates a fresh HPKE keypair and
  uploads the matching public key without re-running enrollment.
  Useful for explicit key rotation or recovering from a missing
  private_key when Supabase enrollment fields are still valid.
- New `internal/keysync` package consolidates keypair lifecycle
  (`EnsurePrivateKey`, `RotateKey`, `UploadPublicKey`). Reused by
  `--register`, `--run` (auto-heal), and `--rotate-key`.
- E2E-encryptie van LXC create-passwords via HPKE (Curve25519 + ChaCha20Poly1305,
  RFC 9180) — `--register` genereert een X25519 keypair, uploadt de public key
  naar `clusters.public_key`, en de dispatcher decrypteert `password_enc`
  met de in `config.yml` opgeslagen private key. Plaintext-`password`
  blijft 1 release als fallback met WARN-log voor pre-#1476 iOS-builds (#1476).
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
