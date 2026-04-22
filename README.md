# proxmoxvue-agent

On-host daemon for the **[ProxmoxVue iOS app](https://proxmoxvue.app)**.
Runs on your Proxmox VE host and bridges your cluster to the ProxmoxVue
backend so the iPad app can manage it from anywhere — without opening
the Proxmox web-UI to the internet.

## Install

On your Proxmox host:

```sh
curl -sSL https://raw.githubusercontent.com/TheLion/proxmoxvue-agent/main/scripts/install.sh | sudo sh -s -- <ENROLLMENT-CODE>
```

You get the enrollment code from the "Add host" screen inside the
ProxmoxVue iPad app.

Prefer to review the script first? See [`scripts/install.sh`](scripts/install.sh)
and [docs/INSTALL.md](docs/INSTALL.md) for a manual install.

## What it does

- Authenticates outbound-only to the ProxmoxVue Supabase backend (no
  inbound ports opened on your host)
- Reports cluster status every 30 seconds so the iOS app can show live
  metrics
- Listens for VM/LXC commands (start, stop, reboot, snapshot) and
  executes them against the local Proxmox REST API

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full data-flow
and security model.

## Supported platforms

Linux, built for `amd64` and `arm64`. Tested against Proxmox VE 8.x.

## License

[MIT](LICENSE)
