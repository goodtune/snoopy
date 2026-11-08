<p align="center">
   <img src="assets/snoopy-500.jpg" width="200" />
</p>

# Notes for contributors and agents

`README.md` is the single user-facing document (what Snoopy is, install,
flags, endpoints, security, mobile app, release secrets). This file covers how
the repository is put together and the conventions to follow. Do not
duplicate README content here.

## Layout

| Path | What lives there |
| --- | --- |
| `server/` | Go module root (`go.mod`, `main.go`, tests, `.golangci.yml`, `snoopy.service`) |
| `server/main.go` | Flags (top of `realMain`), screencast/D-Bus loop, Avahi advertisement (`txtRecords`), ffmpeg frame extraction, image cache, HTTP handlers (`newHTTPHandler`) |
| `server/snoopy.service` | systemd **user** unit, installed to `/usr/lib/systemd/user/` |
| `scripts/` | nfpm post-install / pre-remove scripts |
| `.goreleaser.yaml` | Release build (`dir: server`): tar.gz, rpm, deb, changelog groups |
| `mobile/` | Flutter app; platforms: android, ios, macos. Code in `mobile/lib/{models,screens,services,widgets}` |
| `assets/`, `mobile/assets/` | Images, stored in Git LFS (CI checkouts use `lfs: true`) |
| `.github/workflows/` | `server.yml`, `mobile.yml` (CI), `release.yml` (tag-driven release) |
| `renovate.json` | Renovate dependency updates (gomod, pub, gradle, GitHub Actions, Flutter version) |

## Build and test

Server (run from `server/`):

```bash
gofmt -l .                 # must print nothing
go vet ./...
go test -race -count=1 ./...
golangci-lint run          # v2 config in server/.golangci.yml
go run . -version
```

Running the server for real needs a GNOME Shell session on the D-Bus session
bus; tests must not depend on one. Use `-addr 127.0.0.1` when trying it out.

Mobile (run from `mobile/`):

```bash
flutter pub get
dart format --set-exit-if-changed .
flutter analyze --fatal-infos
flutter test
flutter build apk --debug
```

Packages: `goreleaser check` and
`goreleaser release --snapshot --clean --skip=publish` from the repo root.

CI mirrors these commands; keep them passing locally before pushing.

## Conventions

- **Conventional Commits.** GoReleaser groups the changelog by prefix:
  `feat:` (Features), `fix:` (Bug Fixes), `sec:` (Security Updates), `perf:`
  (Performance Improvements); everything else lands in Others. `docs:`,
  `test:`, `ci:` and `chore:` commits are excluded from release notes.
- **Docs follow code.** When you add or change a flag, endpoint, mDNS TXT
  record, file location or package contents, update `README.md` in the same
  change (flag table, API table, install steps).
- **Segment rotation** (`recordLoop`/`rotate` in `server/main.go`): GNOME
  Shell refuses a second screencast from the same D-Bus client, so the server
  holds two session-bus connections and starts the next segment on the idle
  one before stopping the current one (overlap, no gap). If that start fails
  it falls back to stop, `-pause`, start on the same connection. The loop is
  tested through the `screencaster` interface with fakes; keep it that way
  rather than requiring a live GNOME session.
- **Exit codes**: `realMain` returns 0 on SIGINT/SIGTERM, 1 on errors
  (including D-Bus loss, after stopping the screencast and withdrawing the
  Avahi record via deferred cleanup) and 2 on invalid flags. Do not call
  `os.Exit` or `log.Fatal` inside `run`; it would skip the cleanup.
  `Restart=on-failure` in the user unit relies on the status-1 exit.
- **systemd unit**: `WantedBy=graphical-session.target` with `PartOf=` and
  `After=` on the same target, so it only runs in graphical sessions. Do not
  switch back to `default.target` (SSH/TTY logins would restart-loop).
- **Version**: injected at link time (`-X main.version=...`, plus `commit`
  and `date`); `snoopy -version` prints it and the same value is advertised
  in the mDNS TXT record `ver=`. Clients must treat `ver` as informational
  and not compare it to a fixed value.
- Dependencies are updated by Renovate; do not add Dependabot config.

## Releasing

A single `vX.Y.Z` tag releases the whole monorepo:

1. `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`
2. `release.yml` runs GoReleaser, which creates the GitHub release with the
   server artifacts (`release.mode: append`).
3. The `android` job then builds the APK with `--build-name` from the tag and
   uploads `snoopy_<version>_android.apk` to the same release. It signs with
   the `ANDROID_*` secrets when present (see README), otherwise the APK is
   debug-signed.

Pre-1.0, minor versions may change flags and endpoints; call this out in the
commit message (`feat!:` / `BREAKING CHANGE:`).
