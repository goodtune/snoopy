// Package upstream keeps one SSE connection to each online snoopy desktop
// in the registry, remembers the newest frame (and caches its JPEG bytes,
// up to a size cap) and fans frame events out to any number of local
// subscribers, so browsers never multiply the load on a desktop and never
// talk to one directly. It only connects to addresses taken from the
// registry.
package upstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"snoopy/internal/hub/registry"
)

// Defaults for Manager.
const (
	DefaultMaxFrameBytes = 8 << 20
	DefaultMinBackoff    = time.Second
	DefaultMaxBackoff    = 30 * time.Second
	maxSSELine           = 64 << 10
	imageFetchTimeout    = 15 * time.Second
	// WaitingImage is snoopy's placeholder, served before the first frame.
	WaitingImage = "waiting.jpg"
)

var imageName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}\.jpg$`)

// ValidImageName reports whether s is an acceptable image file name:
// letters, digits, ".", "_" and "-" ending in ".jpg", no slashes, not
// starting with a dot.
func ValidImageName(s string) bool {
	return imageName.MatchString(s) && !strings.HasPrefix(s, ".")
}

// ImagePath is the hub URL path of an image of service id.
func ImagePath(id, file string) string {
	return "/s/" + id + "/images/" + file
}

// Frame is a frame event.
type Frame struct {
	ID   string    `json:"id"`
	File string    `json:"-"`
	Path string    `json:"path"` // hub path, /s/<id>/images/<file>
	At   time.Time `json:"at"`
}

// ErrNotFound is returned by Image when the desktop has no such image.
var ErrNotFound = errors.New("image not found")

// ErrTooLarge is returned by Image when the image exceeds MaxFrameBytes.
var ErrTooLarge = errors.New("image too large")

type state struct {
	mu       sync.Mutex
	file     string    // newest image file name (may be waiting.jpg)
	frameAt  time.Time // when the newest real frame arrived
	jpegFile string
	jpeg     []byte
	subs     map[chan Frame]struct{}

	cancel context.CancelFunc
	addr   string
	done   chan struct{}
}

// Manager is safe for concurrent use.
type Manager struct {
	// MaxFrameBytes caps the size of a cached (and proxied) JPEG.
	MaxFrameBytes int64
	// MinBackoff and MaxBackoff bound the reconnect delay.
	MinBackoff, MaxBackoff time.Duration
	// OnDrop is called (if set) when a service's stream fails or ends.
	OnDrop func(id string)

	lookup func(id string) (registry.Service, bool)
	client *http.Client
	logf   func(string, ...any)
	now    func() time.Time

	mu      sync.Mutex
	states  map[string]*state
	allSubs map[chan Frame]struct{}
}

// New returns a manager that looks services up with lookup and talks to
// them with client (which must not follow redirects; see
// registry.NewHTTPClient). logf may be nil.
func New(lookup func(string) (registry.Service, bool), client *http.Client, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		MaxFrameBytes: DefaultMaxFrameBytes,
		MinBackoff:    DefaultMinBackoff,
		MaxBackoff:    DefaultMaxBackoff,
		lookup:        lookup,
		client:        client,
		logf:          logf,
		now:           time.Now,
		states:        map[string]*state{},
		allSubs:       map[chan Frame]struct{}{},
	}
}

func (m *Manager) state(id string) *state {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[id]
	if !ok {
		st = &state{subs: map[chan Frame]struct{}{}}
		m.states[id] = st
	}
	return st
}

// Start connects to service id (no-op if already connected to its current
// address; reconnects if the address changed).
func (m *Manager) Start(ctx context.Context, id string) {
	svc, ok := m.lookup(id)
	if !ok {
		return
	}
	st := m.state(id)
	st.mu.Lock()
	if st.cancel != nil && st.addr == svc.DialAddr() {
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	m.Stop(id)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	st.mu.Lock()
	st.cancel, st.addr, st.done = cancel, svc.DialAddr(), done
	st.mu.Unlock()
	go func() {
		defer close(done)
		m.loop(cctx, id, st)
	}()
}

// Stop disconnects from id but keeps its newest frame.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	st := m.states[id]
	m.mu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	cancel, done := st.cancel, st.done
	st.cancel, st.done, st.addr = nil, nil, ""
	st.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Forget disconnects from id, drops its frame and closes its subscribers.
func (m *Manager) Forget(id string) {
	m.Stop(id)
	m.mu.Lock()
	st := m.states[id]
	delete(m.states, id)
	m.mu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	for ch := range st.subs {
		close(ch)
	}
	st.subs = map[chan Frame]struct{}{}
	st.mu.Unlock()
}

// Connected reports whether a stream loop is running for id.
func (m *Manager) Connected(id string) bool {
	m.mu.Lock()
	st := m.states[id]
	m.mu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cancel != nil
}

// Latest returns the newest image of id: its file name, when the newest
// real frame arrived (zero if none yet) and, if cached, its JPEG bytes.
func (m *Manager) Latest(id string) (file string, at time.Time, jpeg []byte) {
	m.mu.Lock()
	st := m.states[id]
	m.mu.Unlock()
	if st == nil {
		return "", time.Time{}, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.jpegFile == st.file {
		jpeg = st.jpeg
	}
	return st.file, st.frameAt, jpeg
}

// Subscribe returns frame events of id. The channel is closed by Forget or
// cancel. A slow subscriber misses frames rather than blocking others.
func (m *Manager) Subscribe(id string) (<-chan Frame, func()) {
	st := m.state(id)
	ch := make(chan Frame, 16)
	st.mu.Lock()
	st.subs[ch] = struct{}{}
	st.mu.Unlock()
	return ch, func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		if _, ok := st.subs[ch]; ok {
			delete(st.subs, ch)
			close(ch)
		}
	}
}

// SubscribeAll returns frame events of every service.
func (m *Manager) SubscribeAll() (<-chan Frame, func()) {
	ch := make(chan Frame, 64)
	m.mu.Lock()
	m.allSubs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.allSubs[ch]; ok {
			delete(m.allSubs, ch)
			close(ch)
		}
	}
}

func (m *Manager) publish(st *state, f Frame) {
	st.mu.Lock()
	for ch := range st.subs {
		select {
		case ch <- f:
		default:
		}
	}
	st.mu.Unlock()
	m.mu.Lock()
	for ch := range m.allSubs {
		select {
		case ch <- f:
		default:
		}
	}
	m.mu.Unlock()
}

// Follow keeps connections in line with the registry until ctx is done:
// online services are connected, offline ones disconnected, removed ones
// forgotten.
func (m *Manager) Follow(ctx context.Context, reg *registry.Registry) {
	for ctx.Err() == nil {
		changes, cancel := reg.Subscribe(1024)
		m.sync(ctx, reg)
		m.follow(ctx, changes)
		cancel()
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.states))
	for id := range m.states {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
}

func (m *Manager) sync(ctx context.Context, reg *registry.Registry) {
	known := map[string]bool{}
	for _, s := range reg.List() {
		known[s.ID] = true
		if s.Online {
			m.Start(ctx, s.ID)
		} else {
			m.Stop(s.ID)
		}
	}
	m.mu.Lock()
	var gone []string
	for id := range m.states {
		if !known[id] {
			gone = append(gone, id)
		}
	}
	m.mu.Unlock()
	for _, id := range gone {
		m.Forget(id)
	}
}

func (m *Manager) follow(ctx context.Context, changes <-chan registry.Change) {
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-changes:
			if !ok {
				return // fell behind: resync
			}
			switch {
			case c.Kind == registry.Removed:
				m.Forget(c.Service.ID)
			case c.Service.Online:
				m.Start(ctx, c.Service.ID)
			default:
				m.Stop(c.Service.ID)
			}
		}
	}
}

func (m *Manager) loop(ctx context.Context, id string, st *state) {
	backoff := m.MinBackoff
	quiet := false // log a failing desktop once, not on every retry
	for ctx.Err() == nil {
		svc, ok := m.lookup(id)
		if !ok {
			return
		}
		got, err := m.stream(ctx, svc, st)
		if ctx.Err() != nil {
			return
		}
		if got {
			backoff, quiet = m.MinBackoff, false
		}
		if err != nil && !quiet {
			m.logf("Stream from %q (%s) failed: %v; retrying with backoff", svc.Name, svc.DialAddr(), err)
			quiet = true
		}
		if m.OnDrop != nil {
			m.OnDrop(id)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff *= 2
		if backoff > m.MaxBackoff {
			backoff = m.MaxBackoff
		}
	}
}

// stream reads one SSE connection until it ends; got reports whether any
// event arrived.
func (m *Manager) stream(ctx context.Context, svc registry.Service, st *state) (got bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+svc.DialAddr()+"/sse/image", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := m.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 4096), maxSSELine)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				got = true
				m.handle(ctx, svc, st, strings.Join(data, "\n"))
				data = data[:0]
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if err := sc.Err(); err != nil {
		return got, err
	}
	return got, errors.New("stream ended")
}

// handle processes one event: data is a path like /images/<file>.jpg, or
// a bare /images/ while the desktop has no frame yet.
func (m *Manager) handle(ctx context.Context, svc registry.Service, st *state, data string) {
	file, ok := strings.CutPrefix(strings.TrimSpace(data), "/images/")
	if !ok {
		return
	}
	file, _, _ = strings.Cut(file, "?")
	if file == "" {
		file = WaitingImage
	}
	if !ValidImageName(file) {
		return
	}
	st.mu.Lock()
	dup := st.file == file
	st.mu.Unlock()
	if dup {
		return
	}
	now := m.now()
	var jpeg []byte
	if file != WaitingImage {
		if b, err := m.fetch(ctx, svc, file); err == nil {
			jpeg = b
		}
	}
	st.mu.Lock()
	st.file = file
	if file != WaitingImage {
		st.frameAt = now
	}
	if jpeg != nil {
		st.jpegFile, st.jpeg = file, jpeg
	}
	at := st.frameAt
	st.mu.Unlock()
	m.publish(st, Frame{ID: svc.ID, File: file, Path: ImagePath(svc.ID, file), At: at})
}

// Image returns image file of service id: from the cache when it is the
// newest frame, else fetched from the desktop. file must already have
// passed ValidImageName.
func (m *Manager) Image(ctx context.Context, id, file string) ([]byte, error) {
	if !ValidImageName(file) {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	st := m.states[id]
	m.mu.Unlock()
	if st != nil {
		st.mu.Lock()
		if st.jpegFile == file && st.jpeg != nil {
			b := st.jpeg
			st.mu.Unlock()
			return b, nil
		}
		st.mu.Unlock()
	}
	svc, ok := m.lookup(id)
	if !ok {
		return nil, ErrNotFound
	}
	return m.fetch(ctx, svc, file)
}

func (m *Manager) fetch(ctx context.Context, svc registry.Service, file string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+svc.DialAddr()+"/images/"+file, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	limit := m.MaxFrameBytes
	if limit <= 0 {
		limit = DefaultMaxFrameBytes
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}
