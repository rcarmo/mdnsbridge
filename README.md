# mdnsbridge

DNS → mDNS bridge for `.local` hostnames. It answers normal DNS queries by asking Avahi (via `avahi-resolve`).

This is handy when you want Bonjour names to work over Tailscale using **split-horizon DNS**.

## What it does

Tailscale clients can’t see mDNS broadcasts on your LAN. Run `mdnsbridge` on an exit node (or subnet router) that *can* see the LAN, and point Tailscale’s split DNS for `local` at it.

```plain
┌─────────────────┐     DNS query      ┌─────────────────┐    mDNS query    ┌─────────────────┐
│ Tailscale       │ ──────────────────▶│ mdnsbridge      │ ────────────────▶│ LAN device      │
│ client          │    printer.local   │ (exit node)     │   printer.local  │ (printer/NAS)   │
│                 │ ◀──────────────────│                 │ ◀────────────────│                 │
└─────────────────┘     192.168.1.50   └─────────────────┘    192.168.1.50  └─────────────────┘
```

## Build

```bash
make help
make build
make build-all
```

Cross-compiled binaries land in `dist/`.

## Install (systemd)

```bash
sudo make install
sudo systemctl status mdnsbridge
```

`make install` builds amd64 + armv7, detects the local architecture, and installs the right binary to `/usr/local/bin/mdnsbridge`.

## Configure Tailscale DNS (split DNS)

In the Tailscale admin console:

1. Open **Admin Console → DNS**.
2. Under **Nameservers**, choose **Split DNS**.
3. Add a rule:
	- **Domain:** `local`
	- **Nameserver:** the *Tailscale IP* of the machine running `mdnsbridge` (e.g. `100.64.0.5`)
4. Save. That’s it.

On clients, ensure they accept DNS from Tailscale:

```bash
tailscale set --accept-dns=true
```

Test from a client:

```bash
dig @100.64.0.5 printer.local +short
ping printer.local
```

## Notes

- Needs `avahi-daemon` and `avahi-resolve` (`avahi-tools` / `avahi-utils`).
- Listens on `:53` by default (UDP + TCP). Use `-addr` if you want another port.
- Caches results briefly (positive 5s, negative 2s).

## License

MIT
