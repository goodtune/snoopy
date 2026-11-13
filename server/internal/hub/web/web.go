// Package web serves the hub's user interface (embedded, vanilla HTML, CSS
// and JavaScript; no build step), its JSON/SSE API and the reverse proxy to
// the desktops' frames. Browsers only ever talk to the hub; the hub only
// fetches from addresses in the registry, looked up by id.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"

	"snoopy/internal/hub/registry"
	"snoopy/internal/hub/upstream"
)

//go:embed ui
var uiFS embed.FS

var pages = template.Must(template.ParseFS(uiFS, "ui/*.html"))

// Options configures the handler.
type Options struct {
	Registry *registry.Registry
	Frames   *upstream.Manager
	// User and Password enable HTTP Basic authentication when both are set.
	User, Password string
	// HSTS adds Strict-Transport-Security (only with a trusted certificate).
	HSTS bool
	// Heartbeat is the interval of SSE keep-alive comments (default 25s).
	Heartbeat time.Duration
	// WriteTimeout bounds every non-streaming response (default 60s).
	WriteTimeout time.Duration
}

// ContentSecurityPolicy is sent with every response.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

type handler struct {
	Options
	mux *http.ServeMux
}

// New returns the hub's HTTP handler.
func New(o Options) http.Handler {
	if o.Heartbeat <= 0 {
		o.Heartbeat = 25 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 60 * time.Second
	}
	h := &handler{Options: o, mux: http.NewServeMux()}
	static, _ := fs.Sub(uiFS, "ui")
	files := http.StripPrefix("/static/", http.FileServerFS(static))

	h.mux.HandleFunc("GET /{$}", h.index)
	h.mux.HandleFunc("GET /wall", h.wall)
	h.mux.HandleFunc("GET /view/{id}", h.view)
	h.mux.HandleFunc("GET /static/{file}", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.PathValue("file"), ".html") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	h.mux.HandleFunc("GET /api/services", h.apiServices)
	h.mux.HandleFunc("GET /api/events", h.apiEvents)
	h.mux.HandleFunc("GET /s/{id}/sse", h.serviceSSE)
	h.mux.HandleFunc("GET /s/{id}/images/{file}", h.serviceImage)
	h.mux.HandleFunc("GET /s/{id}/latest.jpg", h.serviceLatest)

	var next http.Handler = http.HandlerFunc(h.serve)
	if o.User != "" && o.Password != "" {
		next = basicAuth(o.User, o.Password, next)
	}
	return securityHeaders(o.HSTS, next)
}

func isStream(path string) bool {
	return path == "/api/events" || (strings.HasPrefix(path, "/s/") && strings.HasSuffix(path, "/sse"))
}

// serve bounds the write time of every response except event streams.
func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if !isStream(r.URL.Path) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.WriteTimeout))
	}
	h.mux.ServeHTTP(w, r)
}

func securityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", ContentSecurityPolicy)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if hsts {
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// basicAuth compares SHA-256 digests in constant time, so neither the
// length nor the content of the credentials leaks through timing.
func basicAuth(user, password string, next http.Handler) http.Handler {
	wantUser := sha256.Sum256([]byte(user))
	wantPass := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(u))
		gotPass := sha256.Sum256([]byte(p))
		userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
		passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
		if !ok || userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="snoopy-hub", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pageData is what the HTML templates render. html/template escapes every
// field for its context.
type pageData struct {
	Page    string
	Title   string
	ID      string
	IDs     string
	Name    string
	Host    string
	Version string
}

func (h *handler) render(w http.ResponseWriter, status int, name string, d pageData) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, d); err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (h *handler) index(w http.ResponseWriter, _ *http.Request) {
	h.render(w, http.StatusOK, "index.html", pageData{Page: "index", Title: "Screens"})
}

var idPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// maxWall is the largest number of screens a wall shows.
const maxWall = 36

func wallIDs(s string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Split(s, ",") {
		id = strings.TrimSpace(id)
		if !idPattern.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) == maxWall {
			break
		}
	}
	return ids
}

func (h *handler) wall(w http.ResponseWriter, r *http.Request) {
	ids := wallIDs(r.URL.Query().Get("s"))
	title := "All online screens"
	if len(ids) > 0 {
		title = fmt.Sprintf("%d selected screens", len(ids))
		if len(ids) == 1 {
			title = "1 selected screen"
		}
	}
	h.render(w, http.StatusOK, "wall.html", pageData{Page: "wall", Title: title, IDs: strings.Join(ids, ",")})
}

func (h *handler) view(w http.ResponseWriter, r *http.Request) {
	s, ok := h.Registry.Get(r.PathValue("id"))
	if !ok {
		h.render(w, http.StatusNotFound, "notfound.html", pageData{Page: "notfound", Title: "Screen not found"})
		return
	}
	h.render(w, http.StatusOK, "view.html", pageData{
		Page: "view", Title: s.Name, ID: s.ID, Name: s.Name, Host: s.Host, Version: s.Version,
	})
}

// serviceJSON is the API view of a service. encoding/json escapes <, >
// and & in strings, so network-supplied names are safe even in HTML.
type serviceJSON struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Host        string     `json:"host"`
	Addresses   []string   `json:"addresses"`
	Port        int        `json:"port"`
	Source      string     `json:"source"`
	Online      bool       `json:"online"`
	Version     string     `json:"version"`
	HasRTSP     bool       `json:"has_rtsp"`
	RTSPURL     string     `json:"rtsp_url,omitempty"`
	RTSPURLIP   string     `json:"rtsp_url_ip,omitempty"`
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    *time.Time `json:"last_seen"`
	LastFrameAt *time.Time `json:"last_frame_at"`
	Image       string     `json:"image,omitempty"`
}

func (h *handler) toJSON(s registry.Service) serviceJSON {
	j := serviceJSON{
		ID: s.ID, Name: s.Name, Host: s.Host, Addresses: s.Addresses, Port: s.Port,
		Source: string(s.Source), Online: s.Online, Version: s.Version,
		RTSPURL: s.RTSPURL(), RTSPURLIP: s.RTSPURLIP(), FirstSeen: s.FirstSeen,
	}
	if j.Addresses == nil {
		j.Addresses = []string{}
	}
	if j.RTSPURLIP == j.RTSPURL {
		j.RTSPURLIP = ""
	}
	j.HasRTSP = j.RTSPURL != ""
	if !s.LastSeen.IsZero() {
		t := s.LastSeen
		j.LastSeen = &t
	}
	file, at, _ := h.Frames.Latest(s.ID)
	if !at.IsZero() {
		j.LastFrameAt = &at
	}
	if file != "" {
		j.Image = upstream.ImagePath(s.ID, file)
	}
	return j
}

func (h *handler) list() []serviceJSON {
	list := h.Registry.List()
	out := make([]serviceJSON, len(list))
	for i, s := range list {
		out[i] = h.toJSON(s)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *handler) apiServices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, h.list())
}

// sse prepares an event stream response.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.WriteHeader(http.StatusOK)
	return &sse{w: w, rc: rc}
}

// send writes one event; data must not contain newlines (JSON and paths
// never do).
func (s *sse) send(event, data string) error {
	var err error
	if event != "" {
		_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	} else {
		_, err = fmt.Fprintf(s.w, "data: %s\n\n", data)
	}
	if err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sse) comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	return s.rc.Flush()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

type snapshot struct {
	Now      time.Time     `json:"now"`
	Services []serviceJSON `json:"services"`
}

// apiEvents streams registry changes and frame events. It starts with a
// "snapshot" event holding the full list; then "added", "removed",
// "online", "offline", "updated" (data: the service) and "frame" (data:
// {id, path, at}).
func (h *handler) apiEvents(w http.ResponseWriter, r *http.Request) {
	changes, cancelChanges := h.Registry.Subscribe(256)
	defer cancelChanges()
	frames, cancelFrames := h.Frames.SubscribeAll()
	defer cancelFrames()

	s := startSSE(w)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if err := s.send("snapshot", mustJSON(snapshot{Now: time.Now().UTC(), Services: h.list()})); err != nil {
		return
	}
	tick := time.NewTicker(h.Heartbeat)
	defer tick.Stop()
	ctx := r.Context()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case c, ok := <-changes:
			if !ok {
				return // too slow: the browser reconnects and gets a snapshot
			}
			if c.Kind == registry.Removed {
				err = s.send(string(c.Kind), mustJSON(map[string]string{"id": c.Service.ID}))
			} else {
				err = s.send(string(c.Kind), mustJSON(h.toJSON(c.Service)))
			}
		case f, ok := <-frames:
			if !ok {
				return
			}
			err = s.send("frame", mustJSON(f))
		case <-tick.C:
			err = s.comment("ping")
		}
		if err != nil {
			return
		}
	}
}

// serviceSSE mirrors snoopy's /sse/image for one service, through the
// hub's single upstream connection: each event's data is a hub image path.
func (h *handler) serviceSSE(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.Registry.Get(id); !ok {
		http.NotFound(w, r)
		return
	}
	frames, cancel := h.Frames.Subscribe(id)
	defer cancel()
	s := startSSE(w)
	if file, _, _ := h.Frames.Latest(id); file != "" {
		if err := s.send("", upstream.ImagePath(id, file)); err != nil {
			return
		}
	} else if err := s.comment("waiting for the first frame"); err != nil {
		return
	}
	tick := time.NewTicker(h.Heartbeat)
	defer tick.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-frames:
			if !ok {
				return // service removed
			}
			err = s.send("", f.Path)
		case <-tick.C:
			err = s.comment("ping")
		}
		if err != nil {
			return
		}
	}
}

func writeJPEG(w http.ResponseWriter, b []byte, cache string) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	_, _ = w.Write(b)
}

func (h *handler) serviceImage(w http.ResponseWriter, r *http.Request) {
	id, file := r.PathValue("id"), r.PathValue("file")
	if _, ok := h.Registry.Get(id); !ok {
		http.NotFound(w, r)
		return
	}
	if !upstream.ValidImageName(file) {
		http.Error(w, "invalid image name", http.StatusBadRequest)
		return
	}
	h.image(w, r, id, file, false)
}

func (h *handler) image(w http.ResponseWriter, r *http.Request, id, file string, latest bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	b, err := h.Frames.Image(ctx, id, file)
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "screen unavailable", http.StatusBadGateway)
		return
	}
	cache := "private, max-age=86400, immutable"
	if latest {
		cache = "no-store"
	} else if file == upstream.WaitingImage {
		cache = "no-cache"
	}
	writeJPEG(w, b, cache)
}

// serviceLatest serves the newest frame the hub has seen for a service
// (from memory), or the desktop's waiting image while there is none.
func (h *handler) serviceLatest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, ok := h.Registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	file, _, jpeg := h.Frames.Latest(id)
	if jpeg != nil {
		writeJPEG(w, jpeg, "no-store")
		return
	}
	if !s.Online {
		http.NotFound(w, r)
		return
	}
	if file == "" {
		file = upstream.WaitingImage
	}
	h.image(w, r, id, file, true)
}
