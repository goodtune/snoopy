# Snoopy attestation (`snoopy-attest`)

`snoopy-attest` is a small **root system service** that checks, from outside
the user session, that Snoopy is really recording a supervised user's GNOME
desktop. It reports the result to KProxy (a home network proxy) over mutually
authenticated TLS, and KProxy lets the desktop's traffic through only while
recording is attested. It ships in the `snoopy` package and is **not
enabled** by it.

## Threat model and honest limits

The adversary is a child with an **unprivileged account** on the desktop. They
can run any program as themselves, stop or override their own systemd user
units, and read anything their uid can read. They do not have root.

A heartbeat sent by the user-level recorder could be forged by such a user, so
the vouching is done by root: the device key (`/var/lib/snoopy-attest/key.pem`)
is readable only by root, and the checks read `/proc`, logind and the
filesystem as root.

What it does **not** stop:

- anyone with root (including `wheel`/`sudo` membership, or `tss` access to
  the TPM once keys are sealed there);
- booting a live USB or another OS, unless the firmware is password-protected
  and the disk is LUKS-encrypted;
- a second device cloning the desktop's IP and MAC address (KProxy binds
  presence to the peer address, which is spoofable on the LAN);
- phones or tablets on cellular data, or any device KProxy does not gate;
- activity that never appears on screen (background downloads, a terminal
  hidden behind another window is still recorded, but traffic is not content).

Be candid about one more: the recording **evidence** is user-influenced. A
process's `cmdline` and the files in the output directory belong to the user,
and a determined user running custom code under their own uid could try to
fabricate a convincing "gjs writing a segment" picture (for example a genuine
`gjs` binary started with forged arguments, or GStreamer plugins loaded from
their home). The checks raise the bar (root-owned binaries compared by inode,
cgroup placement, environment and plugin-directory checks, the newest segment
must be the file being written), the optional frame check looks at the
pixels, KProxy keeps the evidence fields for audit, and desktop hardening
closes the easy paths. Stronger evidence (for example from the compositor)
is future work.

## Components

| Piece | Runs as | Role |
| --- | --- | --- |
| `snoopy` (`snoopy.service`, user unit) | the user | Records the screen through GNOME Shell's screencast D-Bus API |
| `/etc/snoopy/snoopy.conf` with `SNOOPY_MANAGED=1` | root-owned file | Managed mode: the genuine recorder ignores user environment and flags, so its settings are this file plus built-in defaults |
| GNOME screencast service | the user | D-Bus activated `gjs -m /usr/share/gnome-shell/org.gnome.Shell.Screencast`; GStreamer writes the segment |
| `snoopy-attest` (`snoopy-attest.service`) | root, reduced capabilities | Evaluates the checks, writes `/run/snoopy-attest/state.json`, sends heartbeats |
| KProxy | network appliance | Verifies the device pin, applies the state to the desktop's address |

## Checks

Every `SNOOPY_ATTEST_CHECK_INTERVAL` (10s), on logind changes (lock/unlock,
session start/stop, seat switch) and on resume from sleep:

1. **Session** (logind on the system bus). The active session on `seat0`
   must be `Class=user`, `Type=wayland` or `x11`, owned by `SNOOPY_ATTEST_USER`.
   `LockedHint=true` means locked. Other sessions of the user with a user
   class that are TTY or `Remote=true` (SSH, text console) are
   `extra_session` unless `ALLOW_EXTRA_SESSIONS=true`.
2. **Recorder integrity** (`/proc`, as root). Exactly one process of the user
   whose `/proc/<pid>/exe` is `/usr/bin/snoopy` with the same device and inode
   as the file on disk; that file is root-owned and not group/world-writable;
   its cgroup is `.../user@<uid>.service/.../snoopy.service`; its environment
   has no `LD_*` variable; `/etc/snoopy/snoopy.conf` (and `/etc/snoopy`) is
   a root-owned regular file, not group/world-writable, with
   `SNOOPY_MANAGED` set to `1`, `true` or `yes` (as the recorder parses it). The
   expected output directory is that file's `SNOOPY_OUT`, else
   `~/.cache/snoopy/video` (symlinks resolved as root).
3. **Genuine GNOME recording**. A process of the user whose exe is a
   root-owned `gjs-console`/`gjs` (same inode as on disk) running exactly
   `gjs [-m] /usr/share/gnome-shell/org.gnome.Shell.Screencast`, without
   `LD_*`, `GST_PLUGIN_*`, `GST_REGISTRY*`, `GJS_PATH`, `GI_TYPELIB_PATH`,
   `GIO_EXTRA_MODULES`, `GTK_MODULES` or `GTK_PATH` in its environment, with
   no files in `~/.local/share/gstreamer-1.0/plugins`, holding a descriptor
   **open for writing** (`/proc/<pid>/fdinfo` flags) on a regular file in the
   output directory that is the newest `.webm`/`.mp4` there, modified within
   `SEGMENT_MAX_AGE` (5m; GNOME writes only when the screen changes).
4. **Frame check** (experimental, `FRAME_CHECK=true`). Every 5 minutes ffmpeg
   (6.0+, run as `nobody`, reading the segment from an inherited read-only
   descriptor) decodes a frame near the end of the newest segment; it must be
   at least 640x480 and not a uniform image. Two consecutive failures count.

## States

| State | Meaning | KProxy |
| --- | --- | --- |
| `recording` | All checks pass | allowed |
| `locked` | Session locked; recorder integrity (check 2) still verified | restricted |
| `no_session` | Nobody, the greeter or another user is active on the seat | restricted |
| `degraded` | Session and recorder fine, but recording evidence missing or stale, an extra session, frame check failing, or a recorder not yet (re)started within 30s (`STARTUP_GRACE`) of service start, unlock, login or resume | allowed for 30s, then blocked |
| `absent` | Recorder not running or any integrity failure; logind or `/proc` unreadable | blocked |

Reasons (`reasons` array): session `logind_unavailable`, `no_active_session`,
`greeter`, `other_user`, `unsupported_session`, `locked`, `extra_session`;
recorder `proc_unavailable`, `recorder_not_running`, `multiple_recorders`,
`exe_mismatch`, `exe_insecure`, `wrong_cgroup`, `ld_preload`, `not_managed`,
`conf_insecure`; evidence `no_screencast_process`, `screencast_insecure`,
`screencast_env`, `user_gst_plugins`, `no_screencast_writer`, `no_segments`,
`writer_not_newest`, `segment_stale`, `frame_check_failed`; and
`startup_grace`. New reasons may be added; servers must accept unknown ones.

## Heartbeat protocol (normative, version 1)

**Transport.** `POST <SNOOPY_ATTEST_URL>` over HTTPS, **TLS 1.3 only**, HTTP/1.1
with keep-alive; at most one request in flight per device. The client
presents its device certificate: self-signed, Ed25519, `CN=<device_id>`,
`ExtKeyUsage=clientAuth`, valid 20 years. The server requests any client
certificate (Go: `tls.RequireAnyClientCert`; nginx: `ssl_verify_client
optional_no_ca`) and identifies the device **only** by its SPKI pin, never by
CA chain or CN. The client verifies the server against `CA_FILE` (or system
roots) and, if `SERVER_PIN` is set, also requires a certificate in the
verified chain with that pin. No HTTP proxies, no redirects.

**Pin format.** `sha256:` followed by standard base64 (with padding) of
SHA-256 over the DER `SubjectPublicKeyInfo` of the certificate: the HPKP /
`curl --pinnedpubkey` construction. `snoopy-attest pin` prints it; with
OpenSSL: `openssl x509 -in cert.pem -pubkey -noout | openssl pkey -pubin
-outform der | openssl dgst -sha256 -binary | base64`.

**Request.** `Content-Type: application/json`, body:

```json
{"v":1,"device_id":"kids-desk","state":"recording","reasons":[],"seq":123,
 "ts":"2026-11-11T10:00:00Z","boot_id":"0f1e2d3c-...","user":"kid1","uid":1000,
 "attestor_version":"0.2.0","recorder_version":"0.2.0",
 "evidence":{"recorder_pid":1234,"recorder_start":"2026-11-11T09:00:00Z",
  "segment":"screen-2026-11-11-09-30-00.webm","segment_size":104857600,
  "segment_mtime":"2026-11-11T09:59:58Z","screencast_pid":2345,"frame_wh":[2560,1440]}}
```

| Field | Rules |
| --- | --- |
| `v` | Protocol version, `1`. Reject other values with 400. |
| `device_id` | Informational; the pin is the identity. |
| `state` | One of the five states. Treat unknown values as `absent`. |
| `reasons` | Array of strings, empty when `recording`. |
| `seq` | Strictly increasing within one `boot_id`, also across attestor restarts; not contiguous. Reject `seq` ≤ last seen for the same pin and `boot_id` (replay). |
| `ts` | Send time, RFC 3339 UTC. Servers may reject large clock skew. |
| `boot_id` | `/proc/sys/kernel/random/boot_id`; a new value means a reboot (reset the `seq` window). |
| `user`, `uid` | The supervised account. |
| `attestor_version`, `recorder_version` | Informational; `recorder_version` may be absent. |
| `evidence` | Optional, informational fields (all may be absent): store for audit/diagnostics, never base the decision on them. |

**Responses.** `200` or `204`: accepted. `401`/`403`: unknown or revoked pin;
the client logs a warning and retries at the heartbeat interval. `400`/`404`
and other 4xx: logged, retried at the interval. `408`, `429`, 5xx and
network errors: retried with jittered exponential backoff (0.5s doubling,
capped at 5s) until delivered. Response bodies are ignored.

**Timing.** A heartbeat is sent every `HEARTBEAT_INTERVAL` (15s) and
immediately whenever the state or reasons change (including on resume and
lock/unlock, which trigger an immediate evaluation).

**Server behaviour.** Bind presence to the **TCP peer address** of the
heartbeat connection (the desktop's LAN address) and expire it after a
**TTL of 60s** without an accepted heartbeat. Policy per state:
`recording` allowed; `locked` and `no_session` restricted; `absent`, unknown
or expired blocked; `degraded` allowed for at most **30s** from the first
`degraded` heartbeat of a streak, then blocked until another state arrives.
Log every state change with its reasons and evidence.

## Admin setup

1. Install the `snoopy` package (rpm/deb) on the desktop.
2. Managed recorder config, as root:
   `mkdir -p /etc/snoopy && echo SNOOPY_MANAGED=1 > /etc/snoopy/snoopy.conf`
   (add other `SNOOPY_*` settings there; `chmod 0644`, owned by root; see
   [Managed mode](../README.md#managed-mode) in the README).
3. Attestor config: `cp /usr/share/doc/snoopy/attest.conf.example
   /etc/snoopy/attest.conf` and set at least `SNOOPY_ATTEST_USER`,
   `SNOOPY_ATTEST_DEVICE_ID`, `SNOOPY_ATTEST_URL` and `SNOOPY_ATTEST_CA_FILE`.
4. `sudo snoopy-attest keygen`, then `sudo snoopy-attest pin` and register the
   pin (with the device id) in KProxy.
5. `sudo systemctl enable --now snoopy-attest.service`
6. `sudo systemctl --global enable snoopy.service` (the recorder starts at
   the user's next graphical login).
7. While the user is logged in: `sudo snoopy-attest check` should print
   `state: recording`.

Revoking a device: remove its pin in KProxy. Rotating the key:
`snoopy-attest keygen -force`, register the new pin, restart the service.

## Recommended desktop hardening

- **Firewall**: Fedora Workstation's `FedoraWorkstation` zone opens inbound
  1025-65535; remove it (`firewall-cmd --permanent --zone=FedoraWorkstation
  --remove-port=1025-65535/tcp --remove-port=1025-65535/udp && firewall-cmd
  --reload`) so nothing on the desktop can serve as a relay.
- **NetworkManager**: a polkit rule denying the supervised user
  `org.freedesktop.NetworkManager.settings.modify.system`,
  `...network-control`, `...wifi.share.protected`/`open` and
  `...enable-disable-wifi`, so they cannot create hotspots, shared
  connections or switch networks (e.g. to a phone hotspot).
- **Firmware password and LUKS** full-disk encryption, against live USBs.
- Keep the supervised user out of `wheel`, `tss` and any sudoers entry.
- After upgrading the package, restart the user's recorder (or have them log
  in again): until then the running recorder is the old, deleted binary and
  the state is `absent` with `exe_mismatch`.

## Troubleshooting

- `sudo snoopy-attest check` runs every check once and prints the state, the
  reasons and a PASS/FAIL/SKIP line per check with details (`-json` for a
  machine-readable form). Exit status 0 means `recording`. It needs root:
  without `CAP_SYS_PTRACE` the user's `/proc` entries are unreadable.
- `/run/snoopy-attest/state.json` holds the service's latest evaluation and
  the last heartbeat result (`heartbeat.ok`, `status`, `error`, `seq`).
- `journalctl -u snoopy-attest` logs state changes and heartbeat problems;
  `journalctl --user -u snoopy` (as the user) the recorder's.
- `heartbeat.status` 403: the pin is not registered in KProxy (compare with
  `snoopy-attest pin`).

## Roadmap

- Seal the device key to the TPM with `systemd-creds` (`LoadCredentialEncrypted=`)
  so a copied disk image cannot impersonate the device.
- Short-lived client certificates from an OpenBao PKI instead of a long-lived
  self-signed one.
- Stronger recording evidence from the compositor.
