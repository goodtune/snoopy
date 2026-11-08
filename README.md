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
- **Mobile app** (`mobile/`, Flutter): discovers Snoopy servers on the LAN and
  shows the live stream, with on-device history, cleanup and save-to-gallery.

Typical uses: activity logs, monitoring a kiosk or unattended machine, or
capturing long-running screen activity without manual effort.

## Security

> [!WARNING]
> **Security: Snoopy has no authentication.** By default the server binds
> `0.0.0.0:8900`, serves live screenshots to anyone who can reach that port,
> sends permissive CORS headers (`Access-Control-Allow-Origin: *`) and
> announces itself over mDNS. Run it only on a trusted LAN. Otherwise bind it
> to loopback with `-addr 127.0.0.1` (and use an SSH tunnel or a reverse proxy
> with authentication), or firewall port 8900. Recordings and snapshots on
> disk are as sensitive as your screen.

## Requirements

**Server**

- Linux with a GNOME Shell session (Wayland or X11) that exposes
  `org.gnome.Shell.Screencast` on the session bus.
- systemd (the server runs as a systemd **user** service).
- Avahi daemon, for mDNS advertisement (optional: without it the server still
  works, but the mobile app cannot discover it).
- `ffmpeg`, optional: needed to extract live frames. Without it recording
  still works and the web page shows a static "waiting" image.
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

The packages install `/usr/bin/snoopy` and the user unit
`/usr/lib/systemd/user/snoopy.service`, recommend `ffmpeg`, and enable the unit
for all users. The unit is `WantedBy=graphical-session.target` and
`PartOf=graphical-session.target`, so it starts with your GNOME session and
stops with it (it does not start on SSH or TTY logins). To start it in your
current desktop session without logging out (as your user, not root):

```bash
systemctl --user enable --now snoopy.service
journalctl --user -u snoopy.service -f   # logs
```

**Tarball** (any distribution):

```bash
wget https://github.com/goodtune/snoopy/releases/download/v0.1.0/snoopy_0.1.0_Linux_x86_64.tar.gz
tar -xzf snoopy_0.1.0_Linux_x86_64.tar.gz
sudo install -m 0755 snoopy /usr/bin/snoopy
sudo install -m 0644 snoopy.service /usr/lib/systemd/user/snoopy.service
systemctl --user daemon-reload
systemctl --user enable --now snoopy.service
```

Check the installed version with `snoopy -version`.

### How recording works

Each segment is written by GNOME Shell to `-out` using `-template`. GNOME
Shell allows only one screencast per D-Bus client, so the server opens two
session-bus connections and alternates between them: at each rotation it
starts the next segment on the idle connection, then stops the current one,
so consecutive files briefly overlap and there is no gap. `-pause` is only a
settle time after the stop. If the second connection cannot start a
recording, the server falls back to stop, pause, start on the same
connection, which leaves a gap of roughly `-pause`.

### Exit status and restarts

| Exit status | Meaning |
| --- | --- |
| `0` | Stopped by SIGINT/SIGTERM (e.g. `systemctl --user stop`) |
| `1` | Error, including loss of the D-Bus session connection |
| `2` | Invalid command-line flags |

The server always stops the active screencast and withdraws its mDNS record
before exiting. When the session is locked or suspended the D-Bus connection
to GNOME Shell can drop; a periodic health check
(`org.freedesktop.DBus.Peer.Ping`) notices, the server exits with status 1,
and `Restart=on-failure` starts a fresh instance (up to 10 restarts in 5
minutes) once the session is back.

## Configuration

Flags are passed on the command line; to change them for the service, run
`systemctl --user edit snoopy.service` and override `ExecStart`.

| Flag | Default | Description |
| --- | --- | --- |
| `-out` | `~/.cache/snoopy/video` | Directory for recorded video segments |
| `-segment` | `30m` | Duration of each recording segment |
| `-pause` | `1s` | Settle time after stopping the previous segment (the next segment normally starts before the previous one stops; see [How recording works](#how-recording-works)) |
| `-template` | `screen-%d-%t.webm` | Filename template passed to GNOME Shell |
| `-health-check` | `15s` | Interval between D-Bus connection health checks |
| `-addr` | `0.0.0.0` | HTTP bind address (see [Security](#security)) |
| `-port` | `8900` | HTTP port |
| `-image-interval` | `5s` | Interval between frames extracted for streaming |
| `-image-cache-size` | `100` | Maximum number of JPEG frames kept in `~/.cache/snoopy/images` |
| `-version` | | Print version information and exit |

Recorded segments are never deleted by Snoopy; prune `~/.cache/snoopy/video`
yourself (for example with a systemd timer or `find -mtime`).

## Web interface and API

Open `http://<host>:8900/` in a browser for a live view that updates over SSE.

| Endpoint | Description |
| --- | --- |
| `GET /` | HTML live viewer |
| `GET /sse/image` | SSE stream; each event's `data` is the path of the newest frame, e.g. `/images/<uuid>.jpg` |
| `GET /images/<uuid>.jpg` | A cached JPEG frame |
| `GET /images/waiting.jpg` | Placeholder shown until the first frame is available |

The mDNS advertisement (`_snoopy._tcp`) carries TXT records `ver=<version>`
(the server's real version, as printed by `snoopy -version`; `dev` or a
Go pseudo-version for local builds), `proto=http`, `path=/`, `sse=/sse/image` and
`caps=stream,screencast`.

The HTTP API is not yet stable: endpoints, flags and TXT records may change
before 1.0.

## Mobile app

**Install (Android):** download `snoopy_<version>_android.apk` from the
[releases page](https://github.com/goodtune/snoopy/releases) and open it on
the device (allow installing from unknown sources). If a release was built
without the signing secrets, its APK is debug-signed: it installs fine, but
you must uninstall a differently signed build first.

**Use:**

1. The selector screen lists Snoopy servers found via mDNS/Bonjour. The phone
   must be on the same network segment as the server.
2. Tap a server to open the full-screen viewer. New frames arrive over SSE and
   are cached on the device; swipe to move through history and save a frame to
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
go run . -addr 127.0.0.1      # needs a GNOME session
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
   (`feat:`, `fix:`, `sec:`, `perf:`); they become the release notes.
2. Tag and push from `main`:

   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

3. The `release` workflow runs GoReleaser to create the GitHub release (server
   tarballs, rpm, deb, checksums), then builds the Android APK and attaches it
   to the same release.

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
