package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
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
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
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

// Build metadata, set at link time by GoReleaser via -X main.version=... etc.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// buildVersion returns the version stamped at link time, falling back to the
// module version recorded by `go install` when built without ldflags.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return version
}

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

// mdnsService is one DNS-SD service in snoopy's Avahi entry group.
type mdnsService struct {
	typ  string // e.g. "_snoopy._tcp"
	port int
	txt  [][]byte
}

// advertisedServices decides what to announce over mDNS. _snoopy._tcp is
// announced when the HTTP server is reachable from other machines
// (shouldAdvertise), _rtsp._tcp when the RTSP server is running on a
// non-loopback address; the snoopy record then also names the RTSP port.
// An empty result means Avahi is not used at all.
func advertisedServices(ver, httpAddr string, httpPort int, rtspRunning bool, rtspAddr string, rtspPort int) []mdnsService {
	rtspAdvertised := rtspRunning && shouldAdvertise(rtspAddr)
	var services []mdnsService
	if shouldAdvertise(httpAddr) {
		port := 0
		if rtspAdvertised {
			port = rtspPort
		}
		services = append(services, mdnsService{"_snoopy._tcp", httpPort, txtRecords(ver, port)})
	}
	if rtspAdvertised {
		services = append(services, mdnsService{"_rtsp._tcp", rtspPort, rtspTXTRecords()})
	}
	return services
}

// AvahiService manages the Avahi mDNS service advertisement. All services
// share one entry group and one name, so a name collision renames them
// together.
type AvahiService struct {
	conn        *dbus.Conn
	entryGroup  dbus.ObjectPath
	baseName    string
	serviceName string
	services    []mdnsService
}

// newAvahiService creates and advertises the given services.
func newAvahiService(services []mdnsService) (*AvahiService, error) {
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

	as := &AvahiService{
		conn:     conn,
		baseName: username,
		services: services,
	}

	// Create and advertise the service
	if err := as.advertise(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return as, nil
}

// advertise creates an EntryGroup and advertises the services
func (as *AvahiService) advertise() error {
	// Try to register with just the username first
	err := as.tryAdvertise(as.baseName)
	if err != nil {
		// If registration failed, try with IP suffix
		// Note: We retry on any error since Avahi doesn't provide specific collision error codes
		// through D-Bus. This is a reasonable fallback for most error scenarios.
		log.Printf("Avahi: failed to register with name '%s': %v", as.baseName, err)
		ipSuffix := as.getIPSuffix()
		if ipSuffix != "" {
			nameWithSuffix := fmt.Sprintf("%s [%s]", as.baseName, ipSuffix)
			log.Printf("Avahi: retrying with name '%s'", nameWithSuffix)
			err = as.tryAdvertise(nameWithSuffix)
			if err != nil {
				return fmt.Errorf("failed to register with suffix: %w", err)
			}
		} else {
			return fmt.Errorf("failed to register and no IP suffix available: %w", err)
		}
	}

	var list []string
	for _, svc := range as.services {
		list = append(list, fmt.Sprintf("%s on port %d", svc.typ, svc.port))
	}
	log.Printf("Avahi: advertising '%s' as %s", as.serviceName, strings.Join(list, ", "))
	return nil
}

// tryAdvertise attempts to register every service under the given name in
// a single entry group.
func (as *AvahiService) tryAdvertise(name string) error {
	server := as.conn.Object(avahiDest, dbus.ObjectPath(avahiServerPath))

	// Create EntryGroup
	var entryGroupPath dbus.ObjectPath
	err := server.Call(avahiServerIface+".EntryGroupNew", 0).Store(&entryGroupPath)
	if err != nil {
		return fmt.Errorf("create entry group: %w", err)
	}
	entryGroup := as.conn.Object(avahiDest, entryGroupPath)
	discard := func() {
		if resetErr := entryGroup.Call(avahiEntryGroupIface+".Reset", 0).Err; resetErr != nil {
			log.Printf("Avahi: warning - failed to reset entry group: %v", resetErr)
		}
		entryGroup.Call(avahiEntryGroupIface+".Free", 0)
	}

	for _, svc := range as.services {
		err = entryGroup.Call(
			avahiEntryGroupIface+".AddService",
			0,
			avahiIfaceUnspec, // interface (-1 = all)
			avahiProtoUnspec, // protocol (-1 = all)
			uint32(0),        // flags
			name,
			svc.typ,
			"",               // domain (empty = default "local")
			"",               // host (empty = default hostname)
			uint16(svc.port), //nolint:gosec // ports are validated to 1-65535
			svc.txt,
		).Err
		if err != nil {
			discard()
			return fmt.Errorf("add service %s: %w", svc.typ, err)
		}
	}

	// Commit the entry group
	err = entryGroup.Call(avahiEntryGroupIface+".Commit", 0).Err
	if err != nil {
		discard()
		return fmt.Errorf("commit entry group: %w", err)
	}

	as.entryGroup = entryGroupPath
	as.serviceName = name
	return nil
}

// txtRecords builds the _snoopy._tcp TXT records advertised for the given
// version; rtspPort, when not 0, adds rtsp=<port> for the RTSP stream.
func txtRecords(ver string, rtspPort int) [][]byte {
	records := []string{
		"ver=" + ver,
		"proto=http",
		"path=/",
		"sse=/sse/image",
		"caps=stream,screencast",
	}
	if rtspPort != 0 {
		records = append(records, fmt.Sprintf("rtsp=%d", rtspPort))
	}
	return toTXT(records)
}

// rtspTXTRecords are the _rtsp._tcp TXT records. VLC's Avahi module (Linux)
// appends path; its microdns module (Windows, macOS, Android) ignores it and
// opens rtsp://host:port, which snoopy serves too.
func rtspTXTRecords() [][]byte {
	return toTXT([]string{"path=/"})
}

func toTXT(records []string) [][]byte {
	txt := make([][]byte, len(records))
	for i, record := range records {
		txt[i] = []byte(record)
	}
	return txt
}

// getIPSuffix returns the last octet of the local IP address
func (as *AvahiService) getIPSuffix() string {
	// Get all network interfaces
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Printf("Avahi: failed to get network interfaces: %v", err)
		return ""
	}
	return ipSuffixFromAddrs(addrs)
}

// ipSuffixFromAddrs returns the last octet of the first non-loopback IPv4
// address in addrs, or "" if there is none.
func ipSuffixFromAddrs(addrs []net.Addr) string {
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
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
			_ = os.Remove(oldPath) // Ignore errors
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

// checkDBusConnection verifies that the DBus connection is still alive
// by calling the Ping method on the Peer interface
func checkDBusConnection(conn *dbus.Conn) error {
	// Use the Peer.Ping method which is available on all DBus connections
	// This is a standard DBus interface for checking connection health
	obj := conn.BusObject()
	call := obj.Call("org.freedesktop.DBus.Peer.Ping", 0)
	if call.Err != nil {
		return fmt.Errorf("DBus connection lost: %w", call.Err)
	}
	return nil
}

func main() {
	os.Exit(realMain())
}

// realMain resolves the configuration, runs the service and returns the
// process exit code. Keeping os.Exit out of run lets deferred cleanup
// (stopping the screencast, closing DBus connections, withdrawing the Avahi
// record) always execute. It returns 0 without running anything in a
// display-manager greeter session (see inGreeterSession).
func realMain() int {
	res, err := resolveConfig(loadManagedFile(managedConfigPath), os.LookupEnv, os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		log.Printf("%v", err)
		return 2
	}
	if res.showVersion {
		fmt.Printf("snoopy %s (commit %s, built %s)\n", buildVersion(), commit, date)
		return 0
	}
	for _, w := range res.warnings {
		log.Printf("WARNING: %s", w)
	}
	if res.managed {
		log.Printf("Managed mode: configuration from %s only (environment and flags ignored)", managedConfigPath)
		if len(res.ignored) > 0 {
			log.Printf("Managed mode: ignored user-supplied settings: %s", strings.Join(res.ignored, ", "))
		}
	}

	// A display manager's login screen runs its own user manager, which also
	// starts a globally enabled unit. Exit 0 there (before HTTP, RTSP, Avahi
	// or recording start) so Restart=on-failure leaves the unit inactive.
	if inGreeterSession() {
		log.Print(greeterMessage)
		return 0
	}

	err = run(res.opts)
	if err != nil {
		log.Printf("ERROR: %v", err)
		if errors.Is(err, errConnectionLost) {
			log.Printf("Exiting with error code 1 for systemd restart")
		}
		return 1
	}
	return 0
}

// shouldAdvertise reports whether the server should be announced over mDNS
// when bound to addr. A loopback bind is unreachable from other machines, so
// advertising it would only list a server that clients cannot connect to.
func shouldAdvertise(addr string) bool {
	host := strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

type options struct {
	outDir         string
	segment        time.Duration
	pause          time.Duration
	fileTemplate   string
	addr           string
	port           int
	imageInterval  time.Duration
	imageCacheSize int
	healthCheck    time.Duration
	stallTimeout   time.Duration
	rtsp           rtspConfig
}

func run(o options) error {
	log.Printf("snoopy %s starting", buildVersion())

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("UserHomeDir: %w", err)
	}
	if o.outDir == "" {
		o.outDir = filepath.Join(home, ".cache", "snoopy", "video")
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", o.outDir, err)
	}

	// Setup image cache
	cacheDir := filepath.Join(home, ".cache", "snoopy", "images")
	imageCache, err := newImageCache(cacheDir, o.imageCacheSize)
	if err != nil {
		return fmt.Errorf("create image cache: %w", err)
	}

	// Setup SSE broadcaster, and the fan-out of captured frames to the
	// optional RTSP stream.
	broadcaster := newSSEBroadcaster()
	hub := newFrameHub()

	// Get username for default service name. The page reads the name through
	// an atomic so it reflects any rename Avahi makes after registration.
	username := os.Getenv("USER")
	if username == "" {
		username = "unknown"
	}
	var serviceName atomic.Value
	serviceName.Store(username)
	getServiceName := func() string { return serviceName.Load().(string) }

	// Shut down on SIGINT/SIGTERM, or if the HTTP server fails.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancelCause(sigCtx)
	defer cancel(nil)

	// Start HTTP server
	go func() {
		if err := startHTTPServer(o.addr, o.port, imageCache, broadcaster, getServiceName); err != nil {
			cancel(fmt.Errorf("HTTP server failed: %w", err))
		}
	}()

	// Start screen capture loop for web streaming
	// Check if ffmpeg is available
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Printf("Warning: ffmpeg not found in PATH - web streaming will show static waiting image")
		log.Printf("Install ffmpeg to enable live screen streaming feature")
	} else {
		go startScreenCaptureLoop(ctx, imageCache, broadcaster, hub, o.imageInterval, o.outDir)
	}

	// Optional RTSP broadcast of the same frames. It never stops recording:
	// any failure is logged and snoopy carries on without it.
	rtspRunning := false
	if o.rtsp.enabled {
		if streamer, err := startRTSPFromCache(ctx, o.rtsp, hub, imageCache); err != nil {
			log.Printf("Warning: RTSP stream disabled: %v", err)
		} else {
			defer streamer.Close()
			rtspRunning = true
		}
	}

	// Start Avahi service advertisement, unless only reachable locally.
	if !shouldAdvertise(o.addr) {
		log.Printf("mDNS: _snoopy._tcp not advertised: bound to loopback %s (set %s=0.0.0.0 to expose on the LAN)", o.addr, envName("addr"))
	}
	if services := advertisedServices(buildVersion(), o.addr, o.port, rtspRunning, o.rtsp.addr, o.rtsp.port); len(services) == 0 {
		log.Printf("mDNS advertisement skipped: nothing reachable from the LAN")
	} else if avahiService, err := newAvahiService(services); err != nil {
		log.Printf("Warning: Failed to start Avahi service: %v", err)
		log.Printf("Service will not be advertised via mDNS/Bonjour")
	} else {
		defer func() { _ = avahiService.Close() }()
		serviceName.Store(avahiService.GetServiceName())
		log.Printf("Avahi service started successfully")
	}

	// Connect to the *session* bus (this must run in the logged-in user
	// session). GNOME Shell allows one recording per DBus client, so two
	// independent connections are used and alternated between: the next
	// segment is started on the idle connection before the current one is
	// stopped, giving overlapping coverage with no gap.
	connA, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("ConnectSessionBus: %w", err)
	}
	defer func() { _ = connA.Close() }()
	connB, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("ConnectSessionBus (second connection): %w", err)
	}
	defer func() { _ = connB.Close() }()

	log.Printf("Starting screencast loop: out=%s segment=%s", o.outDir, o.segment)
	log.Printf("HTTP server running on http://%s:%d", o.addr, o.port)
	log.Printf("DBus health check enabled: interval=%s", o.healthCheck)
	if o.stallTimeout > 0 {
		log.Printf("Stall detection enabled: timeout=%s", o.stallTimeout)
	}

	// Lock/unlock, resume and screencast-error notifications let recording
	// resume as soon as the shell allows it. Without them the loop still
	// recovers through stall detection and the segment timer.
	var events sessionEvents
	if ev, err := newDBusSessionEvents(ctx); err != nil {
		log.Printf("Warning: session events unavailable, relying on stall detection: %v", err)
	} else {
		events = ev
	}

	err = recordLoop(ctx, loopConfig{
		template:     filepath.Join(o.outDir, o.fileTemplate),
		segment:      o.segment,
		pause:        o.pause,
		healthCheck:  o.healthCheck,
		retryDelay:   5 * time.Second,
		stallTimeout: o.stallTimeout,
		events:       events,
	}, newDBusScreencaster(connA), newDBusScreencaster(connB))
	if err != nil {
		return err
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	log.Printf("Shutdown complete")
	return nil
}

func startScreenCaptureLoop(ctx context.Context, cache *ImageCache, broadcaster *SSEBroadcaster, hub *frameHub, interval time.Duration, videoDir string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
			_ = os.Remove(tmpFile)
			continue
		}

		// Remove temp file
		_ = os.Remove(tmpFile)

		// Add to cache
		filename, err := cache.addImage(jpegData)
		if err != nil {
			log.Printf("Failed to save screenshot: %v", err)
			continue
		}

		// Broadcast to SSE clients
		imageURL := fmt.Sprintf("/images/%s", filename)
		broadcaster.broadcast(imageURL)
		hub.Publish(jpegData)
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

func startHTTPServer(addr string, port int, cache *ImageCache, broadcaster *SSEBroadcaster, serviceName func() string) error {
	listenAddr := net.JoinHostPort(addr, fmt.Sprint(port))
	log.Printf("Starting HTTP server on %s", listenAddr)
	return http.ListenAndServe(listenAddr, newHTTPHandler(cache, broadcaster, serviceName))
}

// newHTTPHandler builds the HTTP routes, wrapped with CORS headers.
func newHTTPHandler(cache *ImageCache, broadcaster *SSEBroadcaster, serviceName func() string) http.Handler {
	mux := http.NewServeMux()

	// Serve static HTML at /
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveIndexHTML(w, r, serviceName())
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
	return corsMiddleware(mux)
}

var indexTemplate = template.Must(template.New("index").Parse(`<!DOCTYPE html>
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
    <h1>{{.ServiceName}}</h1>
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
</html>`))

func serveIndexHTML(w http.ResponseWriter, _ *http.Request, serviceName string) {
	var buf bytes.Buffer
	if err := indexTemplate.Execute(&buf, struct{ ServiceName string }{serviceName}); err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
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
	if _, err := fmt.Fprintf(w, "data: /images/%s\n\n", initialImage); err != nil {
		return
	}
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
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
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
