## Summary

**Snoopy** is a Go program that provides **continuous screen recording** for GNOME-based Linux systems. Here's what it does:

### Core Functionality
- **Automated Screencast Recording**: Continuously records your screen in segments by interfacing with GNOME Shell's screencast D-Bus API
- **Segmented Recording**: Records in configurable time segments (default: 30 minutes) to avoid creating overly large video files
- **Overlapping Recording**: Starts the next recording **before** stopping the current one to eliminate gaps and ensure complete coverage
- **Seamless Transitions**: The brief pause (default: 1 second) between segments is only for cleanup, not recording - ensuring no data loss

### Key Features
1. **Configurable Options** (via command-line flags):
   - `-out`: Output directory (defaults to `~/Videos/screencapture`)
   - `-segment`: Duration of each recording segment (default: 30 minutes)
   - `-pause`: Pause duration between segments (default: 1 second)
   - `-template`: Filename template for video files (default: `screen-%d-%t.webm`)

2. **Error Resilience**: If a recording fails to start or stop, it logs the error and continues trying after a delay

3. **System Service**: Includes a systemd service file (`screencast-loop.service`) to run as a background service that automatically restarts on failure

### Technical Details
- Written in Go
- Uses D-Bus to communicate with GNOME Shell's Screencast interface
- Outputs video files in WebM format (GNOME Shell default)
- Designed to run as a user session service on Linux systems with GNOME

This type of tool is useful for security monitoring, creating activity logs, or capturing long-duration screen activity without manual intervention.
