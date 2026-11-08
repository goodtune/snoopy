# Snoopy mobile

A Flutter app for watching [Snoopy](../README.md) screen streams from your phone.
It finds Snoopy servers on the local network over mDNS/Bonjour, shows a live
preview of each one, and opens a full-screen viewer that follows the server's
`/sse/image` Server-Sent Events feed.

- **Discovery**: browses for `_snoopy._tcp` services. The Snoopy server
  advertises this type with TXT records `path=/` and `sse=/sse/image`. Servers
  can also be added by host and port.
- **Viewer**: swipe between recent captures, pinch to zoom, tap to toggle
  controls, and save the current capture to the photo gallery.
- **Cache**: received images are kept in the app's documents directory and
  can be browsed and deleted from the *Cached images* screen.

The app talks plain HTTP to servers on your LAN. Snoopy has no
authentication, so only use it on networks you trust.

## Requirements

- Flutter 3.47.9 or newer (Dart 3.13)
- Android 7.0+ (Flutter's default `minSdk`) or iOS 15+
- macOS 12+ also builds, but it is not a release target

## Running

```sh
cd mobile
flutter pub get
flutter run            # pick a connected device or emulator
```

Before committing, run:

```sh
dart format --set-exit-if-changed .
flutter analyze --fatal-infos
flutter test
```

## Release builds

```sh
flutter build apk --release        # build/app/outputs/flutter-apk/app-release.apk
flutter build appbundle --release  # for Google Play
flutter build ipa --release        # needs Xcode and a signing team
```

### Android signing

Release builds are signed with the key described in `android/key.properties`.
Without that file they fall back to the debug key, which is fine for local
testing but cannot be uploaded to a store. Create it like this (it and any
`*.jks`/`*.keystore` files are git-ignored):

```properties
storeFile=/absolute/path/to/upload-keystore.jks
storePassword=...
keyAlias=upload
keyPassword=...
```

`storeFile` may also be relative to `android/app/`. To create a keystore:
`keytool -genkey -v -keystore upload-keystore.jks -keyalg RSA -keysize 2048 -validity 10000 -alias upload`.

## Permissions

| Permission | Platform | Why |
| --- | --- | --- |
| Local network (`NSLocalNetworkUsageDescription`) | iOS | Discover and connect to servers on the LAN |
| Bonjour services (`NSBonjourServices`: `_snoopy._tcp`) | iOS | Browse for Snoopy servers |
| Photo library add (`NSPhotoLibraryAddUsageDescription`) | iOS | Save a capture to Photos (asked on first save) |
| `INTERNET` | Android | Fetch the stream from servers |
| `CHANGE_WIFI_MULTICAST_STATE` | Android | Receive mDNS multicast for discovery |
| `WRITE_EXTERNAL_STORAGE` (API 29 and lower only) | Android | Save a capture to the gallery on old Android versions |

Android allows cleartext HTTP through
`android/app/src/main/res/xml/network_security_config.xml`, because servers
are addressed by arbitrary LAN IPs. iOS allows it for local networking
through `NSAllowsLocalNetworking` in `Info.plist`.
