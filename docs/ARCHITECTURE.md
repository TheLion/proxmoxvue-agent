# Architecture

## What the agent does

The agent is a small Go daemon that runs on your Proxmox VE host **or
on any Linux machine in the same network** (e.g. a Raspberry Pi 4/5 on
your LAN). It bridges your Proxmox cluster to the ProxmoxVue Supabase
backend so the ProxmoxVue iPad app can manage the cluster from anywhere,
**without opening the Proxmox web-UI to the internet**.

## Data flow

```
┌───────────────┐                 ┌──────────────────┐               ┌────────────────────┐
│ iPad app      │ ─── Supabase ── │ Supabase         │ ─── WS/REST ──│ Agent              │
│ (Anonymous    │    Realtime     │ (Postgres + RLS) │    outbound   │ (Proxmox host or   │
│  Auth)        │                 │                  │   only        │  LAN machine / Pi) │
└───────────────┘                 └──────────────────┘               └──────────┬─────────┘
                                                                                │ localhost
                                                                                │ or LAN
                                                                                ▼
                                                                       ┌───────────────┐
                                                                       │ Proxmox VE    │
                                                                       │ REST API      │
                                                                       │ (api2/json)   │
                                                                       └───────────────┘
```

**The agent makes only outbound connections.** No port is opened on the
machine it runs on. When the agent runs off-host, the connection from
agent to Proxmox API stays inside your LAN over HTTPS (Proxmox's
self-signed cert by default, or a trusted cert if you've installed one).

## Command flow

Commands worden door de iOS-app (owner) in `public.commands` INSERT'ed.
De agent subscribet via Supabase Realtime op INSERT-events gefilterd op
`host_id=eq.<mine>`, claimed de rij atomair via PATCH met conditie
`status=eq.pending`, voert de bijhorende Proxmox-actie uit, en schrijft
het resultaat terug in dezelfde rij.

### Command contract

| Veld | Waarde |
|---|---|
| `kind` | `start` \| `stop` \| `reboot` \| `shutdown` \| `suspend` \| `resume` |
| `payload.guest_kind` | `qemu` \| `lxc` |
| `payload.node` | Proxmox-nodenaam |
| `payload.vmid` | Numeric VMID |

Iteratie 1 dekt alleen power-acties. Snapshot/create/delete/config-edit
volgen in latere iteraties.

### TTL (decision #196)

`expires_at` staat default op `now() + 30s`. Een command die bij het
claim-moment voorbij de expiry is, wordt als `status=expired`
afgeschreven zonder uitvoering. Dat voorkomt dat een agent die na een
lange netwerkhickup weer online komt, oude commando's alsnog uitvoert.

### Idempotency

De `ClaimCommand`-PATCH zet `status=claimed` met conditie
`status=eq.pending`. PostgREST retourneert een lege array als de
conditie niet matchte — de agent interpreteert dat als "al geclaimed".
Twee agents die tegelijk claimen krijgen dus automatisch een
winner/loser.

### Geen catch-up — presence-based gating

De agent doet géén scan bij reconnect. In plaats daarvan enabled hij
**Supabase Realtime presence** op z'n command-channel, en de iOS-app
subscribet daarop. Zolang de agent's WS verbonden is, zien iOS-clients
`presence_join` en kunnen ze enqueue-acties enablen; valt de WS weg,
dan krijgen ze binnen ~15s een `presence_leave` en disabelt de UI de
acties.

Dit is strikter dan `hosts.last_seen_at` (die is REST-based en mist
WS-only disconnects) en vangt het scenario op waarin een agent nog
status pushed maar z'n command-subscribe dood is. De 30s TTL blijft
als vangnet voor de zeldzame race (agent-WS valt precies tussen
enqueue en verwerking weg).

## Outbound connections

| Target | Protocol | Purpose |
|---|---|---|
| `<project>.supabase.co:443` | HTTPS | REST calls: auth token refresh, INSERT status snapshots, UPDATE command results |
| `<project>.supabase.co:443` | WSS (WebSocket) | Realtime subscription on the `commands` table — receives app→agent commands with <1s latency |
| Proxmox API (`localhost:8006` or LAN address `:8006`) | HTTPS (Proxmox API) | Cluster/node/VM status, action execution (start, stop, reboot, snapshot). Reaches the Proxmox host directly when the agent is on-host, or over the LAN when the agent runs on a separate machine. |

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

The agent logs to stdout/stderr (captured by systemd-journald) and a
rotated file under `/var/log/proxmoxvue-agent/`. Credentials are never
logged:

- Config structs have custom `String()` methods that render tokens as
  `[REDACTED]` when formatted with `%+v`
- A unit test (`config_test.go`) enforces this so future refactors
  can't accidentally leak tokens
- Log level defaults to `INFO`; enable `DEBUG` via `AGENT_LOG_LEVEL=debug`
  environment variable in the systemd unit (tokens remain redacted)

### Level allocation

Each level has a single intent — operators and developers should be
able to predict where to look without scanning unrelated noise.

- **ERROR** — operator action required. Examples: command dispatch
  failure (Proxmox API), command claim/complete DB-failure, Proxmox
  poll failure, Supabase push failure, session permanently revoked.
- **WARN** — transient condition that the agent recovers from
  automatically. Examples: realtime channel join rejected, WS frame
  decode skipped, transient HTTP retries.
- **INFO** — operator default — kerngebeurtenissen worth one line in
  journalctl. Examples: agent started/stopping, snapshot pushed (per
  push), command claimed/dispatched/done/expired, refresh token
  rotated.
- **DEBUG** — every observable agent activity, used for E2E
  troubleshooting and performance tuning. Includes: realtime WS
  dial/connect, channel join request + reply, presence track,
  heartbeat (every 25s), postgres_changes events (pre-filter),
  HTTP request + response (method + URL + status, no bodies),
  snapshot fetch counts/size/duration, config load (path + mtime),
  command pipeline (claim attempt, payload-validation, await-poll
  tick).

If you find yourself adding a `slog.Info` for a high-volume happy-path
event (more than once per minute), it almost certainly belongs at
DEBUG instead.
