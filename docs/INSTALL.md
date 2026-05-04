# Installation

## Requirements

- Proxmox VE 8.x (Debian 12 base)
- Architecture: `amd64` or `arm64`
- systemd
- Root access on the host (the install script uses `sudo`)

## One-line install

Run this on the Proxmox host, replacing `<CODE>` with the enrollment
code shown in the ProxmoxVue iPad app:

```sh
curl -sSL https://raw.githubusercontent.com/TheLion/proxmoxvue-agent/main/scripts/install.sh \
  | sudo sh -s -- <CODE>
```

The script:

1. Detects your architecture (`amd64` / `arm64`)
2. Downloads the matching binary + SHA256 checksum from the latest GitHub Release
3. Verifies the checksum
4. Installs the binary to `/usr/local/bin/proxmoxvue-agent`
5. Creates `/etc/proxmoxvue-agent/` (mode `0700`, owner `root`)
6. Writes a systemd unit at `/etc/systemd/system/proxmoxvue-agent.service`
7. Runs `proxmoxvue-agent --register <CODE>` — exchanges the code for
   Supabase credentials and **interactively asks for your Proxmox API
   URL, token id, token secret, and TLS preference** (see [Proxmox API
   token](#proxmox-api-token) below for how to mint a token first).
   Each field can be skipped with Enter if it is already in the config.
8. Enables and starts the service

> **Headless / non-TTY installs** (cloud-init, Ansible, scripted runs):
> the prompt is skipped automatically when no terminal is attached.
> In that case, populate the `proxmox:` section of
> `/etc/proxmoxvue-agent/config.yml` manually after the install — see
> [Proxmox API token](#proxmox-api-token) for the exact YAML — and
> then `sudo systemctl restart proxmoxvue-agent`.

## Proxmox API token

Create a dedicated API token so the agent never touches your root
password:

1. In the Proxmox web-UI: **Datacenter → Permissions → API Tokens → Add**
2. Pick the user (typically `root@pam` or a dedicated `proxmoxvue@pve`
   user), give the token an ID (e.g. `proxmoxvue-agent`), uncheck
   **Privilege Separation** for now, click **Add** — copy the UUID it
   shows once (it's not recoverable)
3. Grant the token access: **Datacenter → Permissions → Add → API Token
   Permission**, path `/`, token `user@realm!tokenid`, role
   `PVEVMAdmin` (for start/stop/reboot later) or `PVEAuditor` (read-only
   for snapshot-push only)

With the token in hand, the install script's interactive prompt asks
for:

| Prompt | Example |
|---|---|
| `api_url` | `https://127.0.0.1:8006` |
| `api_token_id` | `root@pam!proxmoxvue-agent` |
| `api_token_secret` | `00000000-0000-0000-0000-000000000000` |
| `verify_tls (y/N)` | `N` (Proxmox uses self-signed certs by default) |

If you skipped the prompt or are running headless, edit
`/etc/proxmoxvue-agent/config.yml` (mode `0600`) so the `proxmox:`
section looks like this:

```yaml
proxmox:
  api_url: https://127.0.0.1:8006
  api_token_id: root@pam!proxmoxvue-agent
  api_token_secret: 00000000-0000-0000-0000-000000000000
  verify_tls: false
```

(The `supabase:` block is filled in for you by `--register`.)

Then:

```sh
sudo systemctl restart proxmoxvue-agent
sudo journalctl -u proxmoxvue-agent -f
```

You should see `snapshot pushed (N bytes)` every 30 seconds.

## Manual install

If you prefer not to pipe a remote script through `sudo sh`:

```sh
# 1. Download + verify
ARCH=$(dpkg --print-architecture)   # amd64 or arm64
VERSION=$(curl -sSL https://api.github.com/repos/TheLion/proxmoxvue-agent/releases/latest \
          | grep '"tag_name"' | cut -d'"' -f4)
BASE="https://github.com/TheLion/proxmoxvue-agent/releases/download/${VERSION}"

curl -sSLO "${BASE}/proxmoxvue-agent-linux-${ARCH}.tar.gz"
curl -sSLO "${BASE}/proxmoxvue-agent-linux-${ARCH}.tar.gz.sha256"
sha256sum -c "proxmoxvue-agent-linux-${ARCH}.tar.gz.sha256"

# 2. Install binary
tar -xzf "proxmoxvue-agent-linux-${ARCH}.tar.gz"
sudo install -m 0755 proxmoxvue-agent /usr/local/bin/proxmoxvue-agent

# 3. Prepare config dir
sudo install -d -m 0700 -o root -g root /etc/proxmoxvue-agent

# 4. Enroll (writes /etc/proxmoxvue-agent/config.yml at mode 0600 and
#    interactively prompts for your Proxmox API URL + token)
sudo proxmoxvue-agent --register <CODE>

# 5. Install systemd unit (copy scripts/proxmoxvue-agent.service from this repo)
sudo cp proxmoxvue-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now proxmoxvue-agent
```

## Upgrade

Re-run the one-line install command with any valid enrollment code; the
script replaces the binary and restarts the service. Your config file
is preserved.

To upgrade without re-enrolling:

```sh
curl -sSL https://raw.githubusercontent.com/TheLion/proxmoxvue-agent/main/scripts/install.sh \
  | sudo sh -s -- --upgrade-only
```

## Uninstall

```sh
sudo systemctl disable --now proxmoxvue-agent
sudo rm /etc/systemd/system/proxmoxvue-agent.service
sudo rm /usr/local/bin/proxmoxvue-agent
sudo rm -rf /etc/proxmoxvue-agent
sudo systemctl daemon-reload
```

Also remove the host from the ProxmoxVue iPad app (Settings → Hosts →
swipe-delete) so the agent-user is revoked server-side.

## Troubleshooting

**Service fails to start:**
```sh
sudo journalctl -u proxmoxvue-agent -n 50
```

**Enrollment code rejected:** codes expire after 15 minutes and are
single-use. Generate a new code in the iPad app.

**Outbound connection blocked:** the agent needs HTTPS access to
`*.supabase.co`. If your Proxmox host sits behind an outbound firewall,
allow those domains on port 443.
