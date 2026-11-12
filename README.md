<p align="center">
  <img src="assets/snoopy-500.jpg" width="300" alt="Snoopy logo" />
</p>

# Snoopy

Snoopy continuously records the screen of a GNOME desktop and streams
near-live snapshots of it to browsers and phones on your local network.

It has two parts:

- **Server** (`server/`, Go): a user-session service that records the screen in
  overlapping segments through GNOME Shell's screencast D-Bus API, extracts a
  JPEG frame every few seconds, serves them over HTTP with Server-Sent Events
  (SSE), and advertises itself over mDNS (Avahi) as `_snoopy._tcp`.
  Optionally it also broadcasts those frames as an RTSP stream that VLC
  finds on the local network ([Watch in VLC](#watch-in-vlc)).
- **Mobile app** (`mobile/`, Flutter): discovers Snoopy servers on the LAN and
  shows the live stream, with on-device history, cleanup and save-to-gallery.

Typical uses: activity logs, monitoring a kiosk or unattended machine, or
capturing long-running screen activity without manual effort.

## Security

> [!WARNING]
> **Security: the web interface has no authentication.** By default it binds
> `127.0.0.1:8900` (this machine only) and is not announced over mDNS, so the
> mobile app cannot find it. Once
> [exposed on the LAN](#expose-on-the-lan) (`SNOOPY_ADDR=0.0.0.0`, required
> for the app's mDNS discovery) it serves
> live screenshots to anyone who can reach port 8900, sends permissive CORS
> headers (`Access-Control-Allow-Origin: *`) and announces itself over mDNS:
> do that only on a trusted LAN, or keep loopback and use an SSH tunnel or an
> authenticating reverse proxy. Recordings on disk are as sensitive as your
> screen.
> The [RTSP stream](#watch-in-vlc) is off by default; once enabled it
> broadcasts the screen to the LAN on `0.0.0.0:8554` (and over mDNS) unless
> `SNOOPY_RTSP_USER` and `SNOOPY_RTSP_PASSWORD` are set (digest login; the
> video is not encrypted).

## Requirements

**Server**

- Linux with a GNOME Shell session (Wayland or X11) that exposes
  `org.gnome.Shell.Screencast` on the session bus.
- systemd (the server runs as a systemd **user** service).
- Avahi daemon, optional: advertises the server over mDNS once it is
  [exposed on the LAN](#expose-on-the-lan), so the mobile app can find it.
- `ffmpeg`, optional: needed to extract live frames (and for the RTSP
  stream). Without it recording still works and the web page shows a static
  "waiting" image.
  Install with `sudo dnf install ffmpeg` or `sudo apt install ffmpeg`.

**Mobile app**

- Android (APK attached to each GitHub release), or iOS/macOS built from
  source. The web build is not supported.

## Install the server

Download the package for your architecture from the
[GitHub releases](https://github.com/goodtune/snoopy/releases). Examples for
`v0.1.0` on x86_64 (on 64-bit ARM use `aarch64` in the rpm name and `arm64`
in the deb and tarball names):

```bash
# Fedora / RHEL
sudo dnf install https://github.com/goodtune/snoopy/releases/download/v0.1.0/snoopy-0.1.0-1.x86_64.rpm

# Debian / Ubuntu
wget https://github.com/goodtune/snoopy/releases/download/v0.1.0/snoopy_0.1.0_amd64.deb
sudo apt install ./snoopy_0.1.0_amd64.deb
```

The packages install `/usr/bin/snoopy`, the user unit
`/usr/lib/systemd/user/snoopy.service` and
`/usr/share/doc/snoopy/snoopy.conf.example`, and recommend `ffmpeg`. They
also install the optional [snoopy-attest](#supervised-desktops-snoopy-attest)
attestor (`/usr/bin/snoopy-attest`, its system unit, `attest.conf.example`
and `attestation.md` under `/usr/share/doc/snoopy/`). They do
**not** enable either service; that is the administrator's or user's choice. The
unit is `WantedBy=` and `PartOf=graphical-session.target`, so once enabled it
starts and stops with your GNOME session (not on SSH or TTY logins).

```bash
systemctl --user enable --now snoopy.service   # your user, now (not as root)
sudo systemctl --global enable snoopy.service  # or every user, at next login
journalctl --user -u snoopy.service -f         # logs
```

**Tarball** (any distribution):

```bash
wget https://github.com/goodtune/snoopy/releases/download/v0.1.0/snoopy_0.1.0_Linux_x86_64.tar.gz
tar -xzf snoopy_0.1.0_Linux_x86_64.tar.gz
sudo install -m 0755 snoopy /usr/bin/snoopy
sudo install -m 0644 snoopy.service /usr/lib/systemd/user/snoopy.service
sudo install -Dm 0644 snoopy.conf.example /usr/share/doc/snoopy/snoopy.conf.example
systemctl --user daemon-reload
systemctl --user enable --now snoopy.service
snoopy -version
```

### Expose on the LAN

By default the server listens on `127.0.0.1` only: other machines and the
mobile app cannot reach it, and it is not advertised over mDNS. To expose it
on the LAN (read [Security](#security) first), for your user:

```bash
mkdir -p ~/.config/snoopy
echo 'SNOOPY_ADDR=0.0.0.0' >> ~/.config/snoopy/snoopy.conf
systemctl --user restart snoopy.service
```

For every user on the machine, put the same line in `/etc/snoopy/snoopy.conf`
(`sudo mkdir -p /etc/snoopy && echo 'SNOOPY_ADDR=0.0.0.0' | sudo tee -a
/etc/snoopy/snoopy.conf`); it applies from each user's next service start.

### How recording works

Each segment is written by GNOME Shell to `-out` using `-template`. GNOME
Shell allows only one screencast per D-Bus client, so the server opens two
session-bus connections and alternates between them: at each rotation it
starts the next segment on the idle connection, then stops the current one,
so consecutive files briefly overlap and there is no gap. `-pause` is only a
settle time after the stop. If the second connection cannot start a
recording, the server falls back to stop, pause, start on the same
connection, which leaves a gap of roughly `-pause`.

**Lock, suspend and stalls.** While the screen is locked GNOME Shell ends
every screencast and refuses new ones. The server follows
`org.gnome.ScreenSaver.ActiveChanged`, the screencast `Error` signal and
logind's `PrepareForSleep`: it waits while locked (also at startup) and
starts a fresh segment as soon as the screen is unlocked or the machine
resumes, retrying with a short backoff until the shell accepts it. As a
fallback, a segment file that has not grown for `-stall-timeout` while
unlocked is restarted (at most once per timeout); a static screen grows
slowly, so an occasional extra segment on an idle desktop is expected.

### Exit status and restarts

Exit status is `0` when stopped by SIGINT/SIGTERM (e.g. `systemctl --user
stop`), `1` on errors including loss of the D-Bus session connection, and `2`
on invalid flags or `SNOOPY_*` values. The server always stops the active
screencast and withdraws its mDNS record before exiting. A periodic health
check (`org.freedesktop.DBus.Peer.Ping`) notices a lost session bus; the
server then exits with status 1 and `Restart=on-failure` starts a fresh
instance (up to 10 restarts in 5 minutes).

## Configuration

Each flag has an environment variable (`SNOOPY_` plus the upper-cased flag
name, `-` becoming `_`). Precedence: command-line flag, then environment
variable, then built-in default; `snoopy -help` shows the effective defaults.
Empty variables count as unset; an invalid value exits with status 2. The
service reads optional files `/etc/snoopy/snoopy.conf` (all users), then
`~/.config/snoopy/snoopy.conf` (your user, overriding), unless
[managed mode](#managed-mode) is on; start from
`/usr/share/doc/snoopy/snoopy.conf.example` and restart the service.

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-out` | `SNOOPY_OUT` | `~/.cache/snoopy/video` | Directory for recorded video segments |
| `-segment` | `SNOOPY_SEGMENT` | `30m` | Duration of each recording segment |
| `-pause` | `SNOOPY_PAUSE` | `1s` | Settle time after stopping the previous segment (see [How recording works](#how-recording-works)) |
| `-template` | `SNOOPY_TEMPLATE` | `screen-%d-%t.webm` | Filename template passed to GNOME Shell |
| `-health-check` | `SNOOPY_HEALTH_CHECK` | `15s` | Interval between D-Bus connection health checks (and stall checks) |
| `-stall-timeout` | `SNOOPY_STALL_TIMEOUT` | `5m` | Restart the segment if its file has not grown for this long while unlocked; `0` disables |
| `-addr` | `SNOOPY_ADDR` | `127.0.0.1` | HTTP bind address; a loopback address (`127.0.0.1`, `::1`, `localhost`) also skips the mDNS advertisement (see [Security](#security)) |
| `-port` | `SNOOPY_PORT` | `8900` | HTTP port |
| `-image-interval` | `SNOOPY_IMAGE_INTERVAL` | `5s` | Interval between frames extracted for streaming |
| `-image-cache-size` | `SNOOPY_IMAGE_CACHE_SIZE` | `100` | Maximum number of JPEG frames kept in `~/.cache/snoopy/images` |
| `-rtsp` | `SNOOPY_RTSP` | `false` | Broadcast the frames as an RTSP stream ([Watch in VLC](#watch-in-vlc)); accepts `1`/`true`/`yes`, `0`/`false`/`no` |
| `-rtsp-addr` | `SNOOPY_RTSP_ADDR` | `0.0.0.0` | RTSP bind address, independent of `-addr`; loopback also skips its mDNS advertisement |
| `-rtsp-port` | `SNOOPY_RTSP_PORT` | `8554` | RTSP port (TCP) |
| `-rtsp-codec` | `SNOOPY_RTSP_CODEC` | `auto` | `auto` (MPEG-4 Part 2 if ffmpeg has the `mpeg4` encoder, else H.264), `mpeg4` or `h264` |
| `-rtsp-user` | `SNOOPY_RTSP_USER` | | RTSP user name; set together with the password to require authentication |
| `-rtsp-password` | `SNOOPY_RTSP_PASSWORD` | | RTSP password; never logged or shown by `-help` (prefer the variable: command lines are visible to local users) |
| `-rtsp-max-height` | `SNOOPY_RTSP_MAX_HEIGHT` | `1080` | Scale taller frames down to this height (aspect kept, even sizes); `0` keeps the source size |
| `-version` | | | Print version information and exit |

Recorded segments are never deleted by Snoopy; prune `~/.cache/snoopy/video`
yourself (for example with a systemd timer or `find -mtime`).

### Managed mode

On a supervised desktop the per-user file and flags would let the user
override the administrator. `SNOOPY_MANAGED=1` in `/etc/snoopy/snoopy.conf`
makes that file final: Snoopy reads it itself and uses only its `SNOOPY_*`
keys plus built-in defaults, ignoring `~/.config/snoopy/snoopy.conf`, other
`SNOOPY_*` variables and flags (it logs the names it ignored). The file must
be owned by root and not group- or world-writable, or Snoopy warns and runs
unmanaged; invalid values exit with status 2.

```bash
printf 'SNOOPY_MANAGED=1\nSNOOPY_OUT=/srv/snoopy/alice\n' | sudo tee /etc/snoopy/snoopy.conf
sudo chown root:root /etc/snoopy/snoopy.conf && sudo chmod 0644 /etc/snoopy/snoopy.conf
```

`SNOOPY_MANAGED` accepts `1`, `true` or `yes` (any case) to turn it on and
`0`, `false`, `no` or empty to leave it off; any other value exits with
status 2.

Managed mode cannot stop a user from stopping or overriding the user unit;
pair it with the root `snoopy-attest` service (see
[Supervised desktops: snoopy-attest](#supervised-desktops-snoopy-attest)),
which checks that recording is happening to the configured location.

## Watch in VLC

Snoopy can broadcast the captured screen over RTSP for VLC. It is **off by
default**; read [Security](#security), add these lines to that desktop's
`/etc/snoopy/snoopy.conf` and restart the service (or log in again):

```bash
SNOOPY_RTSP=1
SNOOPY_RTSP_USER=viewer
SNOOPY_RTSP_PASSWORD=change-me
```

In VLC open **View → Playlist → Local Network → Zeroconf network services**
(Linux) or **mDNS Network Discovery** (Windows, macOS, Android) and pick the
desktop, or open `rtsp://<host>:8554/` (any path works) directly.

- **Picture:** the web page's frames, a new one every `-image-interval`,
  repeated at 1 frame/s with a keyframe every 2 s, a few seconds behind; the
  "waiting" image until the first capture. Players must reconnect after a
  resolution change (also once shortly after start, when the first capture
  replaces the "waiting" image). Height is capped at `-rtsp-max-height`.
- **Codec:** `auto` uses MPEG-4 Part 2 (ffmpeg's `mpeg4` encoder), which
  Fedora's `ffmpeg-free` has, so Fedora desktops get MPEG-4 Part 2; failing
  that, H.264 via `libx264`, `libopenh264` or `h264_vaapi` (`ffmpeg-free`
  lacks `libx264`; its `libopenh264` needs Cisco's `openh264`, not the
  `noopenh264` stub). An encoder that fails to start is skipped.
- **Network:** RTSP over TCP only (no UDP ports; VLC falls back to TCP by
  itself): open `8554/tcp` (`sudo firewall-cmd --permanent
  --add-port=8554/tcp && sudo firewall-cmd --reload`). mDNS (`_rtsp._tcp`)
  needs Avahi and a non-loopback `SNOOPY_RTSP_ADDR`. Publishing is refused.
- **Failures:** if ffmpeg, an encoder or the port is unavailable, Snoopy
  logs why and keeps recording. A crashed encoder restarts with backoff.

## Supervised desktops: snoopy-attest

`snoopy-attest` is an optional root system service for supervised desktops.
From outside the user session (logind, `/proc`, the filesystem) it verifies
that the packaged recorder `/usr/bin/snoopy` is running in
[managed mode](#managed-mode) as the user's `snoopy.service`, and that GNOME's
screencast service is writing the newest segment in the expected output
directory (`SNOOPY_OUT` from `/etc/snoopy/snoopy.conf`, else
`~/.cache/snoopy/video`). It then reports a state (`recording`, `locked`,
`no_session`, `degraded`, `absent`) over mutually authenticated TLS to a
KProxy presence endpoint, which can allow or block the desktop's traffic.

> [!NOTE]
> The KProxy side is not implemented yet (tracked in the KProxy repository).
> Today the attestor is useful for monitoring with `snoopy-attest check` and
> `/run/snoopy-attest/state.json`, and for enrolling devices ahead of
> enforcement.

| Path | What it is |
| --- | --- |
| `/usr/bin/snoopy-attest` | The attestor binary |
| `/usr/lib/systemd/system/snoopy-attest.service` | System unit, installed but **not enabled** by the package |
| `/etc/snoopy/attest.conf` | Configuration you create from `/usr/share/doc/snoopy/attest.conf.example` |
| `/var/lib/snoopy-attest/` | Device key and certificate (root only) |
| `/run/snoopy-attest/state.json` | Latest evaluation and heartbeat result (world-readable) |

Commands (as root): `snoopy-attest keygen` creates the device key and
self-signed certificate, `snoopy-attest pin` prints its SPKI pin to register
in KProxy, and `snoopy-attest check` runs every check once and prints the
state (exit status 0 means `recording`). After a package upgrade the user's
running recorder is still the old, deleted binary, so the state is `absent`
(`exe_mismatch`) until snoopy restarts (`systemctl --user restart
snoopy.service` or a new login). See [docs/attestation.md](docs/attestation.md)
for the threat model and its limits, the checks, the heartbeat protocol and
the setup runbook.

## Web interface and API

Open `http://<host>:8900/` in a browser for a live view that updates over SSE
(`http://127.0.0.1:8900/` with the default bind address).

| Endpoint | Description |
| --- | --- |
| `GET /` | HTML live viewer |
| `GET /sse/image` | SSE stream; each event's `data` is the path of the newest frame, e.g. `/images/<uuid>.jpg` |
| `GET /images/<uuid>.jpg` | A cached JPEG frame |
| `GET /images/waiting.jpg` | Placeholder shown until the first frame is available |

The mDNS advertisement (`_snoopy._tcp`, only when not bound to loopback) carries TXT records `ver=<version>`
(the server's real version, as printed by `snoopy -version`; `dev` or a
Go pseudo-version for local builds), `proto=http`, `path=/`, `sse=/sse/image`,
`caps=stream,screencast` and, while the [RTSP stream](#watch-in-vlc) runs on
a non-loopback address, `rtsp=<port>`; the stream itself is advertised as
`_rtsp._tcp` (TXT `path=/`) under the same name.


The HTTP API is not yet stable: endpoints, flags and TXT records may change
before 1.0.

## Mobile app

> [!NOTE]
> The app finds servers over mDNS, which needs the server
> [exposed on the LAN](#expose-on-the-lan) (`SNOOPY_ADDR=0.0.0.0`). With the
> default loopback bind (`127.0.0.1`) the server is neither reachable from the
> phone nor advertised.

**Install (Android):** download `snoopy_<version>_android.apk` from the
[releases page](https://github.com/goodtune/snoopy/releases) and open it on
the device (allow installing from unknown sources). If a release was built
without the signing secrets, its APK is debug-signed: it installs fine, but
you must uninstall a differently signed build first.

**Use:**

1. The selector screen lists Snoopy servers found via mDNS/Bonjour. The server
   must be [exposed on the LAN](#expose-on-the-lan) (with the default
   `127.0.0.1` bind it is neither reachable nor advertised), and the phone must
   be on the same network segment.
2. Tap a server to open the full-screen viewer. New frames arrive over SSE
   (the app reconnects with backoff if the stream drops, and closes the
   connection when you leave the viewer) and are cached on the device; swipe to move through history and save a frame to
   the photo library.
3. The cleanup screen shows cached frames per server and deletes them.

**Permissions:** local network access (Bonjour/mDNS discovery; iOS prompts for
this), Wi-Fi multicast (Android), and photo library or storage access (only
when saving to the gallery).

The app identifier is `network.touchtechnology.snoopy`.

## Development

Repository layout:

```
server/     Go server (module root: go.mod, main.go, snoopy.service)
            and the attestor (cmd/snoopy-attest, internal/attest)
docs/       attestation.md (snoopy-attest design and runbook, packaged)
mobile/     Flutter app (android, ios, macos)
scripts/    Package install/remove scripts used by GoReleaser
assets/     Logo and shared images (Git LFS)
.github/    CI and release workflows
```

Image assets are stored in Git LFS; run `git lfs install` before cloning.

**Server** (Go, see `server/go.mod` for the required version):

```bash
cd server
go test ./...
go run .                       # needs a GNOME session; binds 127.0.0.1
golangci-lint run              # config in server/.golangci.yml
```

**Mobile** (Flutter 3.47.9 or newer; see [`mobile/README.md`](mobile/README.md) for
release builds, signing and permissions):

```bash
cd mobile
flutter pub get
flutter test
flutter run
```

**Packages** locally: `goreleaser release --snapshot --clean --skip=publish`
from the repository root writes the tarballs, rpm and deb to `dist/`.

CI (`.github/workflows/`) runs gofmt, vet, tests, golangci-lint, govulncheck
and a GoReleaser snapshot for server changes, and format, analyze, tests and
an Android debug build for mobile changes. Dependencies are kept current by
Renovate (`renovate.json`).

## Releasing

Snoopy is pre-1.0: flags, endpoints and app behaviour may change between minor
versions.

1. Use [Conventional Commits](https://www.conventionalcommits.org/)
   (`feat:`, `fix:`, `sec:`, `perf:`); release notes are generated from them.
2. Create the release from `main` (this creates the tag and triggers the
   `release` workflow): `gh release create v0.1.0 --target main --generate-notes`
3. GoReleaser appends the server tarballs, rpm, deb, checksums and install
   instructions to that release, then the Android APK is attached to it.

To sign the APK with a release key, set these repository secrets (otherwise
the APK is debug-signed and suitable only for sideloading):

| Secret | Contents |
| --- | --- |
| `ANDROID_KEYSTORE_BASE64` | The upload keystore (`.jks`), base64-encoded (`base64 -w0 upload-keystore.jks`) |
| `ANDROID_KEYSTORE_PASSWORD` | Keystore password |
| `ANDROID_KEY_ALIAS` | Key alias |
| `ANDROID_KEY_PASSWORD` | Key password |

## License

[MIT](LICENSE)
