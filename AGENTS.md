<p align="center">
   <img src="assets/snoopy-500.jpg" width="350" />
</p>

## Summary

**Snoopy** is a Go program that provides **continuous screen recording and web-based streaming** for GNOME-based Linux systems. Here's what it does:

### Core Functionality
- **Automated Screencast Recording**: Continuously records your screen in segments by interfacing with GNOME Shell's screencast D-Bus API
- **Segmented Recording**: Records in configurable time segments (default: 30 minutes) to avoid creating overly large video files
- **Overlapping Recording**: Starts the next recording **before** stopping the current one to eliminate gaps and ensure complete coverage
- **Seamless Transitions**: The brief pause (default: 1 second) between segments is only for cleanup, not recording - ensuring no data loss
- **Web Streaming**: Provides real-time screen capture streaming via a built-in HTTP server with Server-Sent Events (SSE)

### Key Features
1. **Video Recording Options** (via command-line flags):
   - `-out`: Output directory (defaults to `~/.cache/snoopy/video`)
   - `-segment`: Duration of each recording segment (default: 30 minutes)
   - `-pause`: Pause duration between segments (default: 1 second)
   - `-template`: Filename template for video files (default: `screen-%d-%t.webm`)
   - `-health-check`: Interval for DBus connection health checks (default: `15s`)

2. **Web Streaming Options**:
   - `-addr`: HTTP server bind address (default: `0.0.0.0`)
   - `-port`: HTTP server port (default: `8900`)
   - `-image-interval`: Interval between screen captures for web streaming (default: `5s`)
   - `-image-cache-size`: Maximum number of images to keep in cache (default: `100`)

3. **Web Interface**:
   - Access the live screen stream at `http://localhost:8900/`
   - Real-time updates using Server-Sent Events (SSE)
   - Clean, responsive HTML interface
   - No authentication required (intended for local/trusted network use)

4. **Image Cache Management**:
   - Captures stored in `~/.cache/snoopy/images/*.jpg`
   - Automatic pruning to maintain cache size limit
   - Displays a "Waiting for next screen capture" placeholder on initial load

5. **Error Resilience**:
   - If a recording fails to start or stop, it logs the error and continues trying after a delay
   - Periodic DBus connection health checks (every 15 seconds by default) detect session pauses/locks
   - When DBus connection is lost, the service exits cleanly with error code 1
   - Prevents processing stale frames from closed video files during session interruptions

6. **System Service**:
   - Includes a systemd service file (`snoopy.service`) to run as a background service
   - Configured with `Restart=on-failure` to automatically restart when DBus connection is lost
   - Gracefully recovers from session pause/lock events (screen lock, system suspend, etc.)
   - Service automatically resumes recording when the user session becomes active again

### DBus Connection Health Monitoring

Snoopy includes robust connection monitoring to handle session interruptions gracefully:

**Problem Solved**: Previously, when a user session was paused (screen lock, inactivity timeout, suspend), the DBus connection to GNOME Shell would be lost, but the service would continue running. This caused the ffmpeg frame extraction to process stale frames from closed video files.

**Solution**:
- **Periodic Health Checks**: Every 15 seconds (configurable), the service pings the DBus connection using `org.freedesktop.DBus.Peer.Ping`
- **Connection Loss Detection**: If the ping fails, the service immediately detects the lost connection
- **Clean Shutdown**: The service stops the screencast gracefully and exits with code 1
- **Automatic Recovery**: systemd's `Restart=on-failure` configuration automatically restarts the service
- **Fresh Start**: When the session resumes, a new service instance connects to DBus and starts recording fresh

**Benefits**:
- ✅ No stale frame processing during session pauses
- ✅ Automatic recovery when session resumes
- ✅ Clear logging of connection loss events
- ✅ Configurable health check interval
- ✅ Fast detection (within 15 seconds by default)

### Technical Details
- Written in Go
- Uses D-Bus to communicate with GNOME Shell's Screencast interface for video recording
- Implements periodic DBus health checks via `org.freedesktop.DBus.Peer.Ping`
- Extracts frames from recorded video files (MP4/WebM) using ffmpeg for web streaming
- Outputs video files in MP4 or WebM format (GNOME Shell configurable)
- Web streaming uses JPEG format for real-time compatibility
- Built-in HTTP server with SSE support for real-time streaming
- Advertises via Avahi with TXT records including `path=/` and `sse=/sse/image`
- Designed to run as a user session service on Linux systems with GNOME
- Exits with non-zero code on DBus connection loss for systemd restart

### Dependencies
- **Required**: GNOME Shell with Screencast support (for video recording)
- **Optional**: ffmpeg (for web streaming feature)
  - If ffmpeg is not available, video recording still works normally
  - Web interface will display a static "waiting" image
  - Install ffmpeg to enable live frame streaming: `apt install ffmpeg` or `dnf install ffmpeg`

### API Endpoints
- `GET /`: Main web interface (HTML page with live stream viewer)
- `GET /sse/image`: Server-Sent Events endpoint for image updates
- `GET /images/<uuid>.jpg`: Serves cached screenshot images
- `GET /images/waiting.jpg`: Static placeholder image

This type of tool is useful for security monitoring, creating activity logs, remote monitoring, or capturing long-duration screen activity without manual intervention.
