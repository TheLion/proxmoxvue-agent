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

---

The agent runs on your Proxmox VE host and bridges your cluster to
the ProxmoxVue backend, so the iPad app can manage VMs, containers
and snapshots from anywhere — without opening the Proxmox web-UI to
the internet.

## Install

On your Proxmox host:

```sh
curl -sSL https://raw.githubusercontent.com/TheLion/proxmoxvue-agent/main/scripts/install.sh \
  | sudo sh -s -- <CODE>
```

Replace `<CODE>` with the **enrollment code** shown in the iPad app's
*Add host* screen.

Prefer to review the script first? See [`scripts/install.sh`](scripts/install.sh)
and [docs/INSTALL.md](docs/INSTALL.md) for a manual install.

## What it does

- Authenticates outbound-only to the ProxmoxVue Supabase backend (no
  inbound ports opened on your host or firewall)
- Reports cluster status every 30 seconds so the iOS app can show live
  metrics
- Listens for VM/LXC commands (start, stop, reboot, snapshot, create,
  delete) and executes them against the local Proxmox REST API

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full data-flow
and security model.

## Supported platforms

Linux, built for `amd64` and `arm64`. Tested against Proxmox VE 8.x.

## Documentation

- [FAQ](https://proxmoxvue.app/faq/) — common questions about the app and agent
- [Support](https://proxmoxvue.app/support/) — get in touch
- [docs/INSTALL.md](docs/INSTALL.md) — manual install steps
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — data-flow and security model

## License

[MIT](LICENSE)
