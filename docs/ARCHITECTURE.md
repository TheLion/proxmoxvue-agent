# Architecture

## What the agent does

The agent is a small Go daemon that runs on your Proxmox VE host. It
bridges your local Proxmox cluster to the ProxmoxVue Supabase backend
so the ProxmoxVue iPad app can manage the cluster from anywhere,
**without opening the Proxmox web-UI to the internet**.

## Data flow

```
┌───────────────┐                 ┌──────────────────┐               ┌──────────────┐
│ iPad app      │ ─── Supabase ── │ Supabase         │ ─── WS/REST ──│ Agent        │
│ (Anonymous    │    Realtime     │ (Postgres + RLS) │    outbound   │ (your host)  │
│  Auth)        │                 │                  │   only        │              │
└───────────────┘                 └──────────────────┘               └──────┬───────┘
                                                                            │ localhost
                                                                            ▼
                                                                   ┌───────────────┐
                                                                   │ Proxmox VE    │
                                                                   │ REST API      │
                                                                   │ (api2/json)   │
                                                                   └───────────────┘
```

**The agent makes only outbound connections.** No port is opened on
your host.

## Outbound connections

| Target | Protocol | Purpose |
|---|---|---|
| `<project>.supabase.co:443` | HTTPS | REST calls: auth token refresh, INSERT status snapshots, UPDATE command results |
| `<project>.supabase.co:443` | WSS (WebSocket) | Realtime subscription on the `commands` table — receives app→agent commands with <1s latency |
| `localhost:8006` | HTTPS (Proxmox API) | Cluster/node/VM status, action execution (start, stop, reboot, snapshot) |

## On-host files

| Path | Mode | Owner | Contents |
|---|---|---|---|
| `/usr/local/bin/proxmoxvue-agent` | `0755` | `root:root` | Binary |
| `/etc/proxmoxvue-agent/` | `0700` | `root:root` | Config directory |
| `/etc/proxmoxvue-agent/config.yml` | `0600` | `root:root` | Supabase refresh token + Proxmox API token |
| `/etc/systemd/system/proxmoxvue-agent.service` | `0644` | `root:root` | Systemd unit |

## Credentials

The agent holds **two** credentials, both scoped to the single host it
runs on:

1. **Supabase refresh token** — identifies this agent as the unique
   user linked to one `hosts` row via `agent_user_id`. RLS enforces
   that this user can only INSERT into `status_snapshots` and UPDATE
   `commands` rows where `host_id` matches its JWT claim.

2. **Proxmox VE API token** — a dedicated Proxmox token (not a root
   password) that you create during setup. The agent uses it for all
   local Proxmox calls. You control which actions it can perform
   through Proxmox's own ACL system.

Neither credential is hardcoded in the binary. Both live in
`/etc/proxmoxvue-agent/config.yml` at mode `0600`, readable only by
root.

## Token rotation

Supabase refresh tokens rotate on every refresh (roughly every hour
while the agent is running). The previous token is invalidated after
a 10-second reuse window. This means a backup of the config file
taken more than a few minutes ago contains a token that no longer
works — Supabase's "detect and revoke compromised refresh tokens"
feature triggers and revokes the entire session chain if an old token
is replayed.

## Revocation

To revoke an agent from the cloud side:

1. Remove the host in the iPad app (Settings → Hosts → delete)
2. The app marks `hosts.agent_user_id = null` and deletes the
   corresponding `auth.users` row
3. The agent's next token-refresh attempt fails with
   `refresh_token_not_found` and the service exits; it will need to
   be re-enrolled with a fresh code

## What does NOT leave your host

- Proxmox root credentials (the agent never sees them)
- Proxmox VM contents (disk images, backups)
- Guest OS details beyond what the Proxmox API exposes (CPU/memory
  usage, running state)
- Log files from inside your VMs

Cluster status snapshots sent to Supabase contain node names, VM/LXC
IDs, CPU/memory usage, and storage-pool utilization — the same
information the Proxmox web-UI displays on its dashboard.

## Logging

The agent logs to stdout/stderr (captured by systemd-journald).
Credentials are never logged:

- Config structs have custom `String()` methods that render tokens as
  `[REDACTED]` when formatted with `%+v`
- A unit test (`config_test.go`) enforces this so future refactors
  can't accidentally leak tokens
- Log level defaults to `INFO`; enable `DEBUG` via `AGENT_LOG_LEVEL=debug`
  environment variable in the systemd unit (tokens remain redacted)
