# Docker

Run the agent in a container instead of via systemd. Same binary, same
data flow — only the deployment surface differs. Recommended if you
already run other services in containers, or if you want the agent
isolated from the host.

> **Status:** the Dockerfile and entrypoint script live in this repo and
> can be built locally today. A multi-arch image on `ghcr.io` is on the
> roadmap — until that's published, build the image yourself with the
> `Build it yourself` instructions below.

## Quick start

### docker run

```sh
docker run -d --name proxmoxvue-agent \
  --restart unless-stopped \
  -e PROXMOXVUE_ENROLLMENT_CODE=<CODE> \
  -e PROXMOX_API_URL=https://192.168.1.10:8006 \
  -e PROXMOX_API_TOKEN_ID=root@pam!proxmoxvue-agent \
  -e PROXMOX_API_TOKEN_SECRET=00000000-0000-0000-0000-000000000000 \
  -v proxmoxvue-agent-config:/etc/proxmoxvue-agent \
  ghcr.io/thelion/proxmoxvue-agent:latest
```

### docker compose

See [`docker-compose.example.yml`](../docker-compose.example.yml) at the
repo root. Copy it, fill in the env vars, and run `docker compose up -d`.

## How it works

On first start the entrypoint:

1. Detects that `/etc/proxmoxvue-agent/config.yml` is missing
2. Writes a skeleton config from `PROXMOX_API_URL` / `PROXMOX_API_TOKEN_ID`
   / `PROXMOX_API_TOKEN_SECRET` env vars
3. Runs `proxmoxvue-agent --register $PROXMOXVUE_ENROLLMENT_CODE` —
   exchanges the code for a Supabase session, merges the supabase block
   into the config (preserving the proxmox block we just wrote)
4. Runs `proxmoxvue-agent --run` as PID 1

On subsequent starts the config already exists, so step 1-3 are skipped
and the agent starts directly. Persist `/etc/proxmoxvue-agent` as a
named volume so the registration survives container restarts.

## Environment variables

| Name | Required | Default | Notes |
|---|---|---|---|
| `PROXMOXVUE_ENROLLMENT_CODE` | first run only | — | Code shown in the iPad app's *Add host* screen. Single-use, expires after 15 minutes. |
| `PROXMOX_API_URL` | yes | — | e.g. `https://192.168.1.10:8006`. Use `https://127.0.0.1:8006` only if the container shares the Proxmox host's network namespace. |
| `PROXMOX_API_TOKEN_ID` | yes | — | Form: `user@realm!tokenid`. See [INSTALL.md → Proxmox API token](INSTALL.md#proxmox-api-token) for how to mint one. |
| `PROXMOX_API_TOKEN_SECRET` | yes | — | UUID copied once from the Proxmox web-UI. |
| `PROXMOX_VERIFY_TLS` | no | `false` | Set `true` if your Proxmox host has a trusted TLS cert. |
| `AGENT_POLL_INTERVAL_SECONDS` | no | `30` | Tick frequency for the snapshot push loop. |
| `AGENT_LOG_LEVEL` | no | `info` | One of `debug` / `info` / `warn` / `error`. |
| `AGENT_LOG_FILE_PATH` | no | `/dev/null/agent.log` | Deliberately non-writable so the agent falls back to stderr — every INFO event then shows up in `docker logs`. Override with a real path if you want file-based logging inside the container. |
| `PROXMOXVUE_CONFIG_DIR` | no | `/etc/proxmoxvue-agent` | Override only if you need a different mount path. |

The env vars are read by the entrypoint script and used to populate
`config.yml` on first run; they are not re-read on subsequent starts.
To change a value after enrollment, either edit the config inside the
volume or recreate the container with a fresh volume.

## Re-enrollment

If you've removed the host in the iPad app (Settings → Hosts →
swipe-delete) or rotated credentials, you need a fresh code:

```sh
docker compose down -v        # removes the volume → discards old config
docker compose up -d          # bootstraps from env vars again
```

Or, in a running container:

```sh
docker exec -it proxmoxvue-agent \
  proxmoxvue-agent --register --config /etc/proxmoxvue-agent/config.yml <NEW_CODE>
docker restart proxmoxvue-agent
```

## Networking

The agent only makes outbound connections — to Supabase (HTTPS + WSS to
`*.supabase.co:443`) and to your Proxmox API on port 8006. No published
ports, no inbound firewall rules. If your Docker host sits behind an
egress firewall, allow `*.supabase.co` on 443.

## Build it yourself

The image is published from CI; if you want to build locally for
testing or air-gapped deployments:

```sh
# Single-arch (your host's architecture)
docker build -t proxmoxvue-agent:local .

# Multi-arch (amd64 + arm64) using buildx
docker buildx build --platform linux/amd64,linux/arm64 \
  -t proxmoxvue-agent:local --load .
```

Note: `--load` only works for single-platform builds; for multi-arch
testing, push to a registry or use `--output type=oci`.

## Limitations

- One container = one enrolled Proxmox host (same constraint as the
  systemd install — see [README → Supported platforms](../README.md#supported-platforms)).
  Run multiple containers (different volumes, different env vars) to
  manage multiple hosts.
- `armv7` / 32-bit ARM is not part of the published multi-arch image.
  Build from source if you need it.
- Bind-mounting `/etc/proxmoxvue-agent` from the host instead of using
  a named volume works but you need to pre-create the directory with
  mode `0700` and ensure the in-container UID can read it.
