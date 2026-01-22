# Snoopy

![Snoopy Logo](assets/snoopy.png)

Snoopy is a screen streaming solution that allows you to capture and view screen contents across your local network using mDNS/Bonjour service discovery.

## Project Structure

This repository is organized into two main components:

```
snoopy/
├── server/          # Go-based server for screen capture and streaming
├── mobile/          # Flutter mobile app for iOS, macOS, and web
└── assets/          # Shared assets (logos, images)
```

### Server

The server component is written in Go and provides:
- Screen capture functionality
- HTTP/SSE (Server-Sent Events) streaming endpoint
- mDNS/Avahi service advertisement
- Image serving over HTTP

**Location:** `server/`

**Key files:**
- `main.go` - Main server application
- `snoopy.service` - Systemd service file
- `go.mod` - Go module dependencies

**Running the server:**
```bash
cd server
go run main.go
```

**Installing as a systemd service:**
```bash
sudo cp server/snoopy.service /etc/systemd/system/
sudo systemctl enable snoopy
sudo systemctl start snoopy
```

### Mobile App

The mobile app is built with Flutter and supports iOS, macOS, and web platforms.

**Location:** `mobile/`

**Features:**
- **Service Discovery:** Automatically discovers Snoopy servers on the local network via Bonjour/mDNS
- **Live Viewing:** Real-time screen streaming with SSE
- **Image History:** Swipe through previously received images
- **Local Caching:** All images are cached locally for offline viewing
- **Photo Saving:** Save screenshots to your device's photo library
- **Cleanup Management:** View and delete cached images grouped by service

**Screens:**
1. **Selector Screen** - Discovers and displays available Snoopy servers
2. **Viewer Screen** - Fullscreen image viewing with history navigation
3. **Cleanup Screen** - Manage cached images

**Running the mobile app:**
```bash
cd mobile
flutter pub get
flutter run
```

**Building for production:**
```bash
# iOS
flutter build ios

# macOS
flutter build macos

# Web
flutter build web
```

## App Identifier

The mobile app uses the identifier: `network.touchtechnology.snoopy`

## Requirements

### Server
- Go 1.21 or higher
- Linux with X11 (for screen capture)
- Avahi daemon (for mDNS advertisement)

### Mobile
- Flutter SDK (stable channel)
- For iOS: Xcode and CocoaPods
- For macOS: Xcode
- For Web: Chrome/Safari

## Network Protocol

The server advertises itself via mDNS using the service type `_snoopy._tcp`.

**Endpoints:**
- `GET /sse` - Server-Sent Events stream (sends image IDs)
- `GET /images/{id}` - Retrieve image by ID

## Permissions

### iOS
- Local Network Access (for mDNS/Bonjour discovery)
- Photo Library Access (for saving images)

### Android
- Internet
- Network State
- WiFi State
- Multicast (for mDNS)
- Storage/Media (for saving images)

## Development

The project uses:
- **Server:** Go with standard library and Avahi for mDNS
- **Mobile:** Flutter with packages:
  - `nsd` - Network Service Discovery
  - `flutter_client_sse` - Server-Sent Events client
  - `http` - HTTP client
  - `path_provider` - Local storage paths
  - `image_gallery_saver` - Save to photo gallery
  - `permission_handler` - Runtime permissions

## License

[Add your license here]

## Contributing

[Add contributing guidelines here]
