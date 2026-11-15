# snoopy-hub

`snoopy-hub` is an optional web app that lists every snoopy desktop on the
network and shows their screens: all of them as a live grid, one screen as
large as the browser allows, or a chosen set together on a bookmarkable
"wall". Desktops that also export the [RTSP stream](../README.md#watch-in-vlc)
get an `rtsp://` link to open in VLC.

It finds desktops by browsing mDNS (`_snoopy._tcp`, through avahi-daemon) and,
where mDNS does not reach, from a static list. Browsers only ever talk to the
hub: it keeps one connection to each desktop's snoopy stream, caches the
newest frame and relays it to any number of viewers, so a wall of screens and
many viewers do not add load on the desktops, and pages served over HTTPS
never load plain-HTTP content.

> [!WARNING]
> The hub aggregates **every screen it can find** in one place. Anyone who
> can open it can watch them all. Set `SNOOPY_HUB_USER` and
> `SNOOPY_HUB_PASSWORD`, or put it behind a reverse proxy with single
> sign-on, and limit who can reach it. Without credentials it logs a warning
> at every start.

## Where to run it

Any always-on Linux host on the same LAN (the same multicast segment) as the
desktops: a small server, a NAS, a Raspberry Pi, or one of the desktops. It
needs:

- systemd (it runs as a system service with a dynamic, unprivileged user),
- `avahi-daemon` running, for mDNS discovery (`sudo apt install
  avahi-daemon` or `sudo dnf install avahi`). Without it the hub still runs,
  lists the static entries and keeps retrying discovery,
- TCP access to each desktop's snoopy port (8900 by default).

It does not need a GNOME session, ffmpeg, root, or a container runtime. In a
container it would need the host's system D-Bus socket (for Avahi) or
`SNOOPY_HUB_STATIC`; no container image is provided.

## Install and enable

The snoopy packages install the hub alongside the recorder (see the
[README](../README.md#install-the-server)) but do not enable it:

| Path | What it is |
| --- | --- |
| `/usr/bin/snoopy-hub` | The hub binary |
| `/usr/lib/systemd/system/snoopy-hub.service` | System unit, installed but **not enabled** |
| `/etc/snoopy/hub.conf` | Configuration you create from `/usr/share/doc/snoopy/hub.conf.example` |
| `/var/lib/snoopy-hub/` | Generated self-signed certificate (`cert.pem`, `key.pem` 0600) |

```bash
sudo install -m 0600 /usr/share/doc/snoopy/hub.conf.example /etc/snoopy/hub.conf
sudo editor /etc/snoopy/hub.conf          # at least SNOOPY_HUB_USER / _PASSWORD
sudo systemctl enable --now snoopy-hub.service
journalctl -u snoopy-hub.service -f      # shows the certificate fingerprint and each desktop
```

Then open `https://<hub-host>/`. Package upgrades restart the hub only if it
is running; removing the package disables it.

From the tarball, install `snoopy-hub` to `/usr/bin/`, `snoopy-hub.service`
to `/usr/lib/systemd/system/` and run `sudo systemctl daemon-reload` first.

## What the desktops need

- snoopy exposed on the LAN: `SNOOPY_ADDR=0.0.0.0` in
  `/etc/snoopy/snoopy.conf` (see [Expose on the LAN](../README.md#expose-on-the-lan)).
  Only then does it advertise `_snoopy._tcp` over mDNS (with Avahi running on
  the desktop).
- Optionally the RTSP stream (`SNOOPY_RTSP=1`, ideally with
  `SNOOPY_RTSP_USER`/`SNOOPY_RTSP_PASSWORD`): the hub then shows an RTSP link
  for that desktop (from the `rtsp=` TXT record).
- Their firewall must let the hub reach 8900/tcp.

Desktops on other subnets or behind a VPN, where mDNS does not reach, go in
`SNOOPY_HUB_STATIC`, e.g.
`SNOOPY_HUB_STATIC=alice=alice-pc.example.net:8900;rtsp=8554,bob=10.0.5.12`.

## Configuration

Each setting is a flag with an environment variable (`SNOOPY_HUB_` plus the
upper-cased flag name, `-` becoming `_`). Precedence: flag, then environment,
then default; `snoopy-hub -help` shows the effective defaults. The unit reads
`/etc/snoopy/hub.conf` (`EnvironmentFile=` syntax). Empty values count as
unset; invalid values exit with status 2.

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-addr` | `SNOOPY_HUB_ADDR` | `0.0.0.0` | Bind address of the web listeners |
| `-https-port` | `SNOOPY_HUB_HTTPS_PORT` | `443` | HTTPS port |
| `-http-port` | `SNOOPY_HUB_HTTP_PORT` | `80` | HTTP port: redirects to HTTPS; `0` disables it. With `-plain-http` it serves the app |
| `-tls-cert` | `SNOOPY_HUB_TLS_CERT` | | PEM certificate chain; with `-tls-key`. Reloaded when the files change and on SIGHUP (`systemctl reload snoopy-hub`). Empty: self-signed certificate |
| `-tls-key` | `SNOOPY_HUB_TLS_KEY` | | PEM private key for `-tls-cert` |
| `-plain-http` | `SNOOPY_HUB_PLAIN_HTTP` | `false` | Serve the app over plain HTTP on `-http-port`, behind a TLS-terminating reverse proxy |
| `-user` | `SNOOPY_HUB_USER` | | HTTP Basic user name; with `-password`. Empty: no authentication (logged as a warning) |
| `-password` | `SNOOPY_HUB_PASSWORD` | | HTTP Basic password; never logged or shown by `-help` (prefer the variable: command lines are visible to local users) |
| `-static` | `SNOOPY_HUB_STATIC` | | Desktops to list without mDNS: `name=host[:port][;rtsp=PORT],...` (port 8900 by default; IPv6 in brackets) |
| `-probe-interval` | `SNOOPY_HUB_PROBE_INTERVAL` | `15s` | Health-check interval per desktop (minimum `1s`) |
| `-state-dir` | `SNOOPY_HUB_STATE_DIR` | `/var/lib/snoopy-hub` | Where the self-signed certificate is kept |
| `-version` | | | Print version information and exit |

Exit status: `0` on SIGINT/SIGTERM, `1` on runtime errors (for example a port
that cannot be bound; `Restart=on-failure` retries), `2` on invalid settings.

## TLS

**Self-signed (default).** Without `-tls-cert`/`-tls-key` the hub creates an
ECDSA P-256 certificate for the host name, `<hostname>.local`, `localhost`
and the host's addresses, valid for 397 days and renewed automatically 30
days before expiry. It is kept in `/var/lib/snoopy-hub/` and reused across
restarts; delete both files and restart to make a new one (for example after
renaming the host). Browsers warn until you trust it: compare the SHA-256
fingerprint in the log (or `sudo openssl x509 -in
/var/lib/snoopy-hub/cert.pem -noout -fingerprint -sha256`) with the one the
browser shows, then accept it or import `cert.pem` into the viewers' trust
stores. HSTS is not sent with a self-signed certificate.

**Your own certificate** (internal CA, or ACME, e.g. certbot or lego with a
DNS challenge for a LAN-only name). The service runs as a dynamic user, which
cannot read a root-only key, so hand the files to it with systemd
credentials (`sudo systemctl edit snoopy-hub.service`, systemd 250 or later):

```ini
[Service]
LoadCredential=cert.pem:/etc/letsencrypt/live/hub.example.net/fullchain.pem
LoadCredential=key.pem:/etc/letsencrypt/live/hub.example.net/privkey.pem
Environment=SNOOPY_HUB_TLS_CERT=%d/cert.pem SNOOPY_HUB_TLS_KEY=%d/key.pem
```

Credentials are copied when the service starts, so restart it after a
renewal (certbot: `--deploy-hook "systemctl restart snoopy-hub"`). Files the
service can read directly are instead reloaded when they change or on
`systemctl reload snoopy-hub`. With your own certificate the hub sends
`Strict-Transport-Security`.

**Reverse proxy.** To put the hub behind nginx, Caddy, Traefik or an SSO
proxy, set `SNOOPY_HUB_PLAIN_HTTP=1`, `SNOOPY_HUB_ADDR=127.0.0.1` and
`SNOOPY_HUB_HTTP_PORT=8080` (any port), and proxy to it. Disable response
buffering for `/api/events` and `/s/*/sse` (nginx: `proxy_buffering off;`,
the hub also sends `X-Accel-Buffering: no`) and allow long-lived responses
(`proxy_read_timeout 1h;`; the hub sends a keep-alive comment every 25 s).

## Using it

- **Index** (`/`): every online screen as a live tile with its name, host,
  the age of the last frame and an **RTSP** link plus **Copy** button if it
  exports a stream. Offline desktops are listed underneath. Tiles update
  without reloading.
- **One screen** (`/view/<id>`): the screen as large as the window allows,
  with name, host, version, frame age, **Fullscreen** (or `f`), and
  **Previous**/**Next** between online screens (or the arrow keys).
- **A set together**: press **Select** on the index, tick screens and press
  **View selected**. The wall (`/wall?s=<id>,<id>,...`) is a URL you can
  bookmark; it arranges the screens to fill the window (1, 2, 2x2, 3x2,
  3x3, ...) and has a fullscreen button. `/wall` alone shows every online
  screen.
- **RTSP links** use the desktop's `.local` host name, which works from
  machines that resolve mDNS names (macOS, iOS, Linux with nss-mdns,
  Windows 10 and later in most cases); the single view also offers the IP
  form. Links never contain credentials: VLC asks for the RTSP user and
  password if the desktop requires them. The stream goes straight from the
  viewer to the desktop, not through the hub.

Ids are derived from the instance name and host, so bookmarks keep working
while the desktop keeps its name.

### HTTP API

| Endpoint | Description |
| --- | --- |
| `GET /api/services` | JSON list: `id`, `name`, `host`, `addresses`, `port`, `source` (`mdns`/`static`), `online`, `version`, `has_rtsp`, `rtsp_url`, `rtsp_url_ip`, `first_seen`, `last_seen`, `last_frame_at`, `image` |
| `GET /api/events` | SSE: `snapshot` (`{now, services}`) first, then `added`, `removed`, `online`, `offline`, `updated` (the service) and `frame` (`{id, path, at}`) |
| `GET /s/<id>/sse` | That desktop's frame stream in snoopy's format (`data: /s/<id>/images/<file>.jpg`), from the hub's single upstream connection |
| `GET /s/<id>/images/<file>.jpg` | A frame (the newest from memory, older ones fetched from the desktop) |
| `GET /s/<id>/latest.jpg` | The newest frame the hub has seen (or the desktop's waiting image) |
| `GET /healthz` | `ok` |

Every endpoint, including `/healthz`, requires the Basic credentials when
they are set. Like snoopy's own, this API may change before 1.0.

## Firewall

| From | To | Port | Why |
| --- | --- | --- | --- |
| Viewers | Hub | 443/tcp (and 80/tcp for the redirect) | The web app |
| Hub | Desktops | 8900/tcp | Frames, health checks |
| Hub, desktops | Multicast 224.0.0.251 / ff02::fb | 5353/udp | mDNS |
| Viewers | Desktops | 8554/tcp | RTSP links (only if used) |

For example with firewalld on the hub `sudo firewall-cmd --permanent
--add-service=https --add-service=http --add-service=mdns`, and on a desktop
`sudo firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source
address=<hub-ip>/32 port port=8900 protocol=tcp accept'` (then
`--reload`).

## Security and privacy

- Treat the hub as sensitive as all the screens together: set credentials
  (or SSO in front), keep it on the LAN, and prefer a trusted certificate so
  browsers can detect interception.
- Basic credentials are compared in constant time; the password is never
  logged. Use HTTPS (the default): over plain HTTP they travel in clear text.
- The hub only connects to addresses it discovered or was configured with;
  requests name a service by id, never a host, and image names are checked
  strictly, so it cannot be used to reach other hosts. It does not follow
  redirects from desktops.
- Instance names, host names and TXT records come from the network and are
  treated as untrusted: escaped in HTML and JSON, never used in ids as is.
  Anyone on the LAN can advertise a `_snoopy._tcp` service; a fake one shows
  up as an extra tile but cannot affect the others.
- Pages send a strict Content-Security-Policy (only the hub's own scripts,
  styles and images; no framing), `X-Content-Type-Options: nosniff` and
  `Referrer-Policy: no-referrer`, and load nothing from other sites.
- Desktops still serve their frames over plain HTTP on the LAN to anyone who
  can reach 8900/tcp; the hub does not change that (restrict that port to the
  hub with the desktops' firewall).

## Troubleshooting

- **No desktops appear.** `avahi-browse -rt _snoopy._tcp` on the hub host
  should list them. If not: check `SNOOPY_ADDR=0.0.0.0` on the desktop
  (loopback binds are not advertised), Avahi on both sides, mDNS allowed by
  the firewalls, and that they share a network segment. The hub logs
  `mDNS discovery unavailable` when it cannot talk to avahi-daemon.
- **A desktop is listed but offline.** The hub probes
  `http://<address>:8900/images/waiting.jpg`; try `curl` from the hub host.
  Withdrawn mDNS entries stay listed as offline for an hour.
- **Binding port 443 fails** (`permission denied`) when started outside the
  unit: the unit grants `CAP_NET_BIND_SERVICE`; elsewhere use
  `-https-port 8443` or run it through the unit.
- **Browser warning.** Expected with the self-signed certificate; see
  [TLS](#tls).
- **Wall or tiles stop updating behind a proxy.** Turn off response buffering
  for the event streams (see [Reverse proxy](#tls)).
