<p align="center">
  <img src="https://proxmoxvue.app/app-icon.svg" width="120" alt="ProxmoxVue logo">
</p>

<h1 align="center">proxmoxvue-agent</h1>

<p align="center">
  On-host daemon for <strong><a href="https://proxmoxvue.app">ProxmoxVue</a></strong> —
  the iPad-first Proxmox VE management app.
</p>

<p align="center">
  <a href="https://proxmoxvue.app">proxmoxvue.app</a> ·
  <a href="https://proxmoxvue.app/faq/">FAQ</a> ·
  <a href="https://proxmoxvue.app/support/">Support</a>
</p>

<p align="center">
  <a href="https://apps.apple.com/app/proxmoxvue/id6762568065" target="_blank" rel="noopener">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://tools.applemediaservices.com/api/badges/download-on-the-app-store/white/en-us?size=250x83">
      <img alt="Download on the App Store" src="https://tools.applemediaservices.com/api/badges/download-on-the-app-store/black/en-us?size=250x83" height="50">
    </picture>
  </a>
</p>

---

The agent bridges your Proxmox VE cluster to the ProxmoxVue Supabase
backend so the iPad app can manage VMs, containers and snapshots from
anywhere — without opening the Proxmox web-UI to the internet. It runs
**on your Proxmox host or anywhere on the same network** (any Linux
machine that can reach the Proxmox API on port 8006).

## Where to run it

Pick whichever fits your setup — the agent is the same binary either way:

- **On the Proxmox host itself** — simplest path, default `127.0.0.1:8006`,
  one less machine to maintain.
- **On a separate Linux machine in your network** — keeps your hypervisor
  pristine, easy to audit and replace. Works great on a small always-on
  box: a NUC, a tiny VM, or a **Raspberry Pi 4 or 5** (running 64-bit
  Raspberry Pi OS / Ubuntu — we ship an `arm64` binary). Older Pi 3 / 32-bit
  ARM (`armv7`) is **not supported** out of the box; build from source if
  you need it.
- **As a Docker container** — same binary in a small image, configured via
  env vars. See [docs/DOCKER.md](docs/DOCKER.md).

## Install

### One-line install (Proxmox host or any Linux machine)

```sh
curl -sSL https://raw.githubusercontent.com/TheLion/proxmoxvue-agent/main/scripts/install.sh \
  | sudo sh -s -- <CODE>
```

Replace `<CODE>` with the **enrollment code** shown in the iPad app's
*Add host* screen. The interactive prompt asks for your Proxmox API URL
— enter `https://127.0.0.1:8006` if you're on the Proxmox host itself,
or the LAN address (e.g. `https://192.168.1.10:8006`) if you're running
the agent elsewhere.

Prefer to review the script first? See [`scripts/install.sh`](scripts/install.sh)
and [docs/INSTALL.md](docs/INSTALL.md) for a manual install.

### Docker

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

See [docs/DOCKER.md](docs/DOCKER.md) for the full env-var reference and
a `docker-compose.yml` example.

## What it does

- Authenticates outbound-only to the ProxmoxVue Supabase backend (no
  inbound ports opened on your host or firewall)
- Reports cluster status every 30 seconds so the iOS app can show live
  metrics
- Listens for VM/LXC commands (start, stop, reboot, snapshot, create,
  delete) and executes them against the Proxmox REST API

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full data-flow
and security model.

## Supported platforms

Linux with systemd (Debian, Ubuntu, Raspberry Pi OS 64-bit), built for
`amd64` and `arm64`. Tested against Proxmox VE 8.x. One agent enrollment
maps to one Proxmox host — for multiple hosts, run multiple instances
(separate machines, or one machine with multiple containers).

## Documentation

- [FAQ](https://proxmoxvue.app/faq/) — common questions about the app and agent
- [Support](https://proxmoxvue.app/support/) — get in touch
- [docs/INSTALL.md](docs/INSTALL.md) — manual install steps
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — data-flow and security model

## License

[MIT](LICENSE)
