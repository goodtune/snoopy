package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/google/uuid"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	dest        = "org.gnome.Shell.Screencast"
	objPath     = "/org/gnome/Shell/Screencast"
	iface       = "org.gnome.Shell.Screencast"
	startMethod = iface + ".Screencast"
	stopMethod  = iface + ".StopScreencast"

	// Avahi constants
	avahiDest                  = "org.freedesktop.Avahi"
	avahiServerPath            = "/"
	avahiServerIface           = "org.freedesktop.Avahi.Server"
	avahiEntryGroupIface       = "org.freedesktop.Avahi.EntryGroup"
	avahiIfaceUnspec     int32 = -1
	avahiProtoUnspec     int32 = -1
)

// ImageCache manages the image cache directory and provides access to images
type ImageCache struct {
	mu         sync.RWMutex
	dir        string
	maxImages  int
	images     []string // sorted by modification time, oldest first
	latest     string   // latest image filename
	waitingImg string   // path to the waiting placeholder image
}

// SSEBroadcaster manages SSE clients
type SSEBroadcaster struct {
	mu      sync.RWMutex
	clients map[chan string]bool
}

func newSSEBroadcaster() *SSEBroadcaster {
	return &SSEBroadcaster{
		clients: make(map[chan string]bool),
	}
}

func (b *SSEBroadcaster) addClient(ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[ch] = true
}

func (b *SSEBroadcaster) removeClient(ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, ch)
	close(ch)
}

func (b *SSEBroadcaster) broadcast(msg string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default:
			// Skip slow clients
		}
	}
}

// AvahiService manages the Avahi mDNS service advertisement
type AvahiService struct {
	conn        *dbus.Conn
	entryGroup  dbus.ObjectPath
	baseName    string
	serviceName string
	port        int
}

// newAvahiService creates and advertises a new Avahi service
func newAvahiService(port int) (*AvahiService, error) {
	// Connect to system bus
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect to system bus: %w", err)
	}

	// Get username for service name
	username := os.Getenv("USER")
	if username == "" {
		username = "unknown"
	}

	baseName := username

	as := &AvahiService{
		conn:     conn,
		baseName: baseName,
		port:     port,
	}

	// Create and advertise the service
	if err := as.advertise(); err != nil {
		conn.Close()
		return nil, err
	}

	return as, nil
}

// advertise creates an EntryGroup and advertises the service
func (as *AvahiService) advertise() error {
	server := as.conn.Object(avahiDest, dbus.ObjectPath(avahiServerPath))

	// Create EntryGroup
	var entryGroupPath dbus.ObjectPath
	err := server.Call(avahiServerIface+".EntryGroupNew", 0).Store(&entryGroupPath)
	if err != nil {
		return fmt.Errorf("create entry group: %w", err)
	}
	as.entryGroup = entryGroupPath

	// Get local IP for collision suffix
	ipSuffix := as.getIPSuffix()
	serviceName := as.baseName
	if ipSuffix != "" {
		serviceName = fmt.Sprintf("%s [%s]", as.baseName, ipSuffix)
	}
	as.serviceName = serviceName

	// Prepare TXT records
	txtRecords := as.prepareTXTRecords()

	// Add service to entry group
	entryGroup := as.conn.Object(avahiDest, entryGroupPath)
	err = entryGroup.Call(
		avahiEntryGroupIface+".AddService",
		0,
		avahiIfaceUnspec, // interface (-1 = all)
		avahiProtoUnspec, // protocol (-1 = all)
		uint32(0),        // flags
		serviceName,
		"_snoopy._tcp",
		"",              // domain (empty = default "local")
		"",              // host (empty = default hostname)
		uint16(as.port), // port
		txtRecords,
	).Err
	if err != nil {
		return fmt.Errorf("add service: %w", err)
	}

	// Commit the entry group
	err = entryGroup.Call(avahiEntryGroupIface+".Commit", 0).Err
	if err != nil {
		return fmt.Errorf("commit entry group: %w", err)
	}

	log.Printf("Avahi: advertising service '%s' on port %d", serviceName, as.port)
	return nil
}

// prepareTXTRecords creates TXT records for the service
func (as *AvahiService) prepareTXTRecords() [][]byte {
	records := []string{
		"ver=1.0.0",
		"proto=http",
		"path=/",
		"sse=/sse/image",
		"caps=stream,screencast",
	}

	txtRecords := make([][]byte, len(records))
	for i, record := range records {
		txtRecords[i] = []byte(record)
	}
	return txtRecords
}

// getIPSuffix returns the last octet of the local IP address
func (as *AvahiService) getIPSuffix() string {
	// Get all network interfaces
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Printf("Avahi: failed to get network interfaces: %v", err)
		return ""
	}

	// Find the first non-loopback IPv4 address
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				// Get the IP address string
				ip := ipNet.IP.String()
				// Extract the last octet
				parts := strings.Split(ip, ".")
				if len(parts) == 4 {
					return parts[3]
				}
			}
		}
	}

	return ""
}

// Close cleans up the Avahi service
func (as *AvahiService) Close() error {
	if as.entryGroup != "" {
		entryGroup := as.conn.Object(avahiDest, as.entryGroup)
		// Reset and free the entry group
		entryGroup.Call(avahiEntryGroupIface+".Reset", 0)
		entryGroup.Call(avahiEntryGroupIface+".Free", 0)
	}
	return as.conn.Close()
}

// GetServiceName returns the advertised service name
func (as *AvahiService) GetServiceName() string {
	return as.serviceName
}

func newImageCache(dir string, maxImages int) (*ImageCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	ic := &ImageCache{
		dir:       dir,
		maxImages: maxImages,
		images:    []string{},
	}

	// Create waiting placeholder image
	waitingPath := filepath.Join(dir, "waiting.jpg")
	if err := ic.createWaitingImage(waitingPath); err != nil {
		return nil, err
	}
	ic.waitingImg = waitingPath
	ic.latest = "waiting.jpg"

	return ic, nil
}

func (ic *ImageCache) createWaitingImage(path string) error {
	// Create a 800x600 image with text
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))

	// Fill with dark gray background
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{40, 40, 40, 255}}, image.Point{}, draw.Src)

	// Draw text
	text := "Waiting for next screen capture"
	point := fixed.Point26_6{
		X: fixed.I(800/2 - len(text)*7/2),
		Y: fixed.I(600 / 2),
	}

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.RGBA{200, 200, 200, 255}),
		Face: basicfont.Face7x13,
		Dot:  point,
	}
	d.DrawString(text)

	// Save as JPEG
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return jpeg.Encode(f, img, &jpeg.Options{Quality: 90})
}

func (ic *ImageCache) addImage(data []byte) (string, error) {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	// Generate UUID for the image
	id := uuid.New().String()
	filename := fmt.Sprintf("%s.jpg", id)
	path := filepath.Join(ic.dir, filename)

	// Write the image
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}

	// Add to list
	ic.images = append(ic.images, filename)
	ic.latest = filename

	// Prune if necessary
	if len(ic.images) > ic.maxImages {
		toRemove := len(ic.images) - ic.maxImages
		for i := 0; i < toRemove; i++ {
			oldPath := filepath.Join(ic.dir, ic.images[i])
			os.Remove(oldPath) // Ignore errors
		}
		ic.images = ic.images[toRemove:]
	}

	return filename, nil
}

func (ic *ImageCache) getLatest() string {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	return ic.latest
}

func (ic *ImageCache) getImagePath(filename string) string {
	return filepath.Join(ic.dir, filename)
}

func main() {
	var (
		outDir         = flag.String("out", "", "Output directory (default: ~/.cache/snoopy/video)")
		segment        = flag.Duration("segment", 30*time.Minute, "Segment duration")
		pause          = flag.Duration("pause", 1*time.Second, "Pause between segments")
		template       = flag.String("template", "screen-%d-%t.webm", "Filename template used by GNOME Shell")
		addr           = flag.String("addr", "0.0.0.0", "HTTP server bind address")
		port           = flag.Int("port", 8900, "HTTP server port")
		imageInterval  = flag.Duration("image-interval", 5*time.Second, "Interval between screen captures for web streaming")
		imageCacheSize = flag.Int("image-cache-size", 100, "Maximum number of images to keep in cache")
	)
	flag.Parse()

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("UserHomeDir: %v", err)
	}
	if *outDir == "" {
		*outDir = filepath.Join(home, ".cache", "snoopy", "video")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *outDir, err)
	}

	// Setup image cache
	cacheDir := filepath.Join(home, ".cache", "snoopy", "images")
	imageCache, err := newImageCache(cacheDir, *imageCacheSize)
	if err != nil {
		log.Fatalf("Failed to create image cache: %v", err)
	}

	// Setup SSE broadcaster
	broadcaster := newSSEBroadcaster()

	// Get username for default service name
	username := os.Getenv("USER")
	if username == "" {
		username = "unknown"
	}
	serviceName := username

	// Start HTTP server
	go startHTTPServer(*addr, *port, imageCache, broadcaster, serviceName)

	// Start screen capture loop for web streaming
	// Check if ffmpeg is available
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Printf("Warning: ffmpeg not found in PATH - web streaming will show static waiting image")
		log.Printf("Install ffmpeg to enable live screen streaming feature")
	} else {
		go startScreenCaptureLoop(imageCache, broadcaster, *imageInterval, *outDir)
	}

	// Start Avahi service advertisement
	avahiService, err := newAvahiService(*port)
	if err != nil {
		log.Printf("Warning: Failed to start Avahi service: %v", err)
		log.Printf("Service will not be advertised via mDNS/Bonjour")
	} else {
		defer avahiService.Close()
		serviceName = avahiService.GetServiceName()
		log.Printf("Avahi service started successfully")
	}

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Connect to the *session* bus (this must run in the logged-in user session).
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Fatalf("ConnectSessionBus: %v", err)
	}
	defer conn.Close()

	obj := conn.Object(dest, dbus.ObjectPath(objPath))
	ctx := context.Background()

	fullTemplate := filepath.Join(*outDir, *template)

	log.Printf("Starting screencast loop: out=%s segment=%s", *outDir, segment.String())
	log.Printf("HTTP server running on http://%s:%d", *addr, *port)

	// Start the first recording
	opts := map[string]dbus.Variant{}
	call := obj.CallWithContext(ctx, startMethod, 0, fullTemplate, opts)
	if call.Err != nil {
		log.Fatalf("Failed to start initial screencast: %v", call.Err)
	}
	log.Printf("Started initial recording")

	for {
		select {
		case <-sigChan:
			// Received shutdown signal
			log.Printf("\nReceived shutdown signal, cleaning up...")
			// Stop the screencast
			obj.CallWithContext(ctx, stopMethod, 0)
			log.Printf("Shutdown complete")
			return

		case <-time.After(*segment):
			// Wait for the segment duration

			// Start the next recording BEFORE stopping the current one
			// This ensures continuous coverage with no gaps
			call = obj.CallWithContext(ctx, startMethod, 0, fullTemplate, opts)
			if call.Err != nil {
				log.Printf("Start next screencast failed: %v", call.Err)
				// If we can't start the next one, try to stop and restart cleanly
				obj.CallWithContext(ctx, stopMethod, 0)
				time.Sleep(5 * time.Second)
				continue
			}

			// Now stop the previous recording
			// GNOME Shell may have already auto-stopped it when we started the new one
			call = obj.CallWithContext(ctx, stopMethod, 0)
			if call.Err != nil {
				log.Printf("Stop previous screencast failed (may already be stopped): %v", call.Err)
				// This is often okay - GNOME may auto-stop when starting a new one
			}

			// Brief pause to ensure clean transition
			time.Sleep(*pause)

			// Optional: print progress heartbeat
			fmt.Print(".")
		}
	}
}

func startScreenCaptureLoop(cache *ImageCache, broadcaster *SSEBroadcaster, interval time.Duration, videoDir string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		// Find the most recent video file
		videoFile, err := findMostRecentVideo(videoDir)
		if err != nil {
			log.Printf("Failed to find recent video: %v", err)
			continue
		}

		// Create temp file for frame extraction
		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("snoopy-frame-%d.jpg", time.Now().UnixNano()))

		// Extract a frame from the video using ffmpeg
		// Use -sseof to seek from the end, getting a recent frame
		cmd := exec.Command("ffmpeg",
			"-loglevel", "error", // Only show errors
			"-sseof", "-3", // Seek to 3 seconds before end of file
			"-i", videoFile,
			"-frames:v", "1", // Extract 1 frame
			"-q:v", "2", // JPEG quality (2 is high quality)
			"-y", // Overwrite output file
			tmpFile,
		)

		// Capture stderr for error messages
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			log.Printf("Failed to extract frame from %s: %v", filepath.Base(videoFile), err)
			if stderr.Len() > 0 {
				log.Printf("ffmpeg error: %s", stderr.String())
			}
			continue
		}

		// Read the frame file
		jpegData, err := os.ReadFile(tmpFile)
		if err != nil {
			log.Printf("Failed to read extracted frame: %v", err)
			os.Remove(tmpFile)
			continue
		}

		// Remove temp file
		os.Remove(tmpFile)

		// Add to cache
		filename, err := cache.addImage(jpegData)
		if err != nil {
			log.Printf("Failed to save screenshot: %v", err)
			continue
		}

		// Broadcast to SSE clients
		imageURL := fmt.Sprintf("/images/%s", filename)
		broadcaster.broadcast(imageURL)
		log.Printf("Captured and broadcast image: %s", filename)
	}
}

func findMostRecentVideo(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var mostRecent string
	var mostRecentTime time.Time

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Look for video files (mp4 or webm)
		ext := filepath.Ext(name)
		if ext != ".mp4" && ext != ".webm" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(mostRecentTime) {
			mostRecentTime = info.ModTime()
			mostRecent = filepath.Join(dir, name)
		}
	}

	if mostRecent == "" {
		return "", fmt.Errorf("no video files found in %s", dir)
	}

	return mostRecent, nil
}

// corsMiddleware adds CORS headers to allow web clients to access the API
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow requests from any origin
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Cache-Control")
		w.Header().Set("Access-Control-Max-Age", "3600")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func startHTTPServer(addr string, port int, cache *ImageCache, broadcaster *SSEBroadcaster, serviceName string) {
	mux := http.NewServeMux()

	// Serve static HTML at /
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveIndexHTML(w, r, serviceName)
	})

	// SSE endpoint
	mux.HandleFunc("/sse/image", func(w http.ResponseWriter, r *http.Request) {
		serveSSE(w, r, cache, broadcaster)
	})

	// Image serving endpoint
	mux.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		serveImage(w, r, cache)
	})

	// Wrap with CORS middleware
	handler := corsMiddleware(mux)

	listenAddr := fmt.Sprintf("%s:%d", addr, port)
	log.Printf("Starting HTTP server on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, handler); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}

func serveIndexHTML(w http.ResponseWriter, r *http.Request, serviceName string) {
	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Snoopy - Screen Stream</title>
    <style>
        body {
            margin: 0;
            padding: 20px;
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            background-color: #1a1a1a;
            color: #e0e0e0;
            display: flex;
            flex-direction: column;
            align-items: center;
            min-height: 100vh;
        }
        h1 {
            color: #ffffff;
            margin-bottom: 10px;
        }
        .status {
            color: #888;
            margin-bottom: 20px;
            font-size: 14px;
        }
        .status.connected {
            color: #4caf50;
        }
        .status.disconnected {
            color: #f44336;
        }
        .container {
            max-width: 1200px;
            width: 100%;
        }
        #screen {
            width: 100%;
            height: auto;
            border: 2px solid #333;
            border-radius: 8px;
            background-color: #000;
        }
        .info {
            margin-top: 10px;
            font-size: 12px;
            color: #666;
        }
    </style>
</head>
<body>
    <h1>%s</h1>
    <div class="status" id="status">Connecting...</div>
    <div class="container">
        <img id="screen" alt="Screen capture">
        <div class="info">
            <span id="timestamp">-</span> |
            <span id="update-count">Updates: 0</span>
        </div>
    </div>

    <script>
        const statusEl = document.getElementById('status');
        const screenEl = document.getElementById('screen');
        const timestampEl = document.getElementById('timestamp');
        const updateCountEl = document.getElementById('update-count');
        let updateCount = 0;

        // Set initial waiting image
        screenEl.src = '/images/waiting.jpg';

        const eventSource = new EventSource('/sse/image');

        eventSource.onopen = function() {
            statusEl.textContent = 'Connected';
            statusEl.className = 'status connected';
        };

        eventSource.onmessage = function(event) {
            const imageUrl = event.data;
            screenEl.src = imageUrl + '?t=' + Date.now(); // Cache bust
            updateCount++;
            updateCountEl.textContent = 'Updates: ' + updateCount;
            timestampEl.textContent = new Date().toLocaleTimeString();
        };

        eventSource.onerror = function() {
            statusEl.textContent = 'Disconnected - Reconnecting...';
            statusEl.className = 'status disconnected';
        };
    </script>
</body>
</html>`, serviceName)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(html))
}

func serveSSE(w http.ResponseWriter, r *http.Request, cache *ImageCache, broadcaster *SSEBroadcaster) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Create client channel
	clientChan := make(chan string, 10)
	broadcaster.addClient(clientChan)
	defer broadcaster.removeClient(clientChan)

	// Send initial image
	initialImage := cache.getLatest()
	fmt.Fprintf(w, "data: /images/%s\n\n", initialImage)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Stream updates
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-clientChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

func serveImage(w http.ResponseWriter, r *http.Request, cache *ImageCache) {
	// Extract filename from path
	filename := filepath.Base(r.URL.Path)

	// Get full path
	imagePath := cache.getImagePath(filename)

	// Check if file exists
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}

	// Serve the image
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, imagePath)
}
