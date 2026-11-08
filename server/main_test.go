package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFindMostRecentVideo(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	tests := []struct {
		name    string
		files   map[string]time.Duration // name -> mtime offset from base
		dirs    []string
		want    string
		wantErr bool
	}{
		{
			name:  "newest webm wins",
			files: map[string]time.Duration{"a.webm": 0, "b.webm": 2 * time.Minute, "c.webm": time.Minute},
			want:  "b.webm",
		},
		{
			name:  "mp4 and webm both considered",
			files: map[string]time.Duration{"a.webm": time.Minute, "b.mp4": 3 * time.Minute},
			want:  "b.mp4",
		},
		{
			name:  "other extensions ignored even if newer",
			files: map[string]time.Duration{"a.webm": 0, "notes.txt": 10 * time.Minute, "x.jpg": 9 * time.Minute, "y.webm.part": 8 * time.Minute},
			want:  "a.webm",
		},
		{
			name:  "directories ignored",
			files: map[string]time.Duration{"a.mp4": 0},
			dirs:  []string{"dir.webm"},
			want:  "a.mp4",
		},
		{
			name:    "no videos",
			files:   map[string]time.Duration{"readme.md": 0},
			wantErr: true,
		},
		{
			name:    "empty dir",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, off := range tt.files {
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				mt := base.Add(off)
				if err := os.Chtimes(p, mt, mt); err != nil {
					t.Fatal(err)
				}
			}
			for _, d := range tt.dirs {
				if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			got, err := findMostRecentVideo(dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(dir, tt.want); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	t.Run("missing dir", func(t *testing.T) {
		if _, err := findMostRecentVideo(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("expected error for missing directory")
		}
	})
}

func TestImageCachePrunesOldest(t *testing.T) {
	dir := t.TempDir()
	ic, err := newImageCache(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := ic.getLatest(); got != "waiting.jpg" {
		t.Fatalf("initial latest = %q, want waiting.jpg", got)
	}

	var names []string
	for i := 0; i < 3; i++ {
		name, err := ic.addImage([]byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}

	if _, err := os.Stat(filepath.Join(dir, names[0])); !os.IsNotExist(err) {
		t.Errorf("oldest image %s should have been removed from disk (stat err=%v)", names[0], err)
	}
	for _, n := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("image %s should still exist: %v", n, err)
		}
	}
	if len(ic.images) != 2 || ic.images[0] != names[1] || ic.images[1] != names[2] {
		t.Errorf("cache images = %v, want %v", ic.images, names[1:])
	}
	if got := ic.getLatest(); got != names[2] {
		t.Errorf("latest = %q, want %q", got, names[2])
	}
	if _, err := os.Stat(filepath.Join(dir, "waiting.jpg")); err != nil {
		t.Errorf("waiting placeholder must never be pruned: %v", err)
	}
}

func TestTXTRecords(t *testing.T) {
	for _, ver := range []string{"0.1.0", "dev", "1.2.3-next"} {
		t.Run(ver, func(t *testing.T) {
			var got []string
			for _, r := range txtRecords(ver) {
				got = append(got, string(r))
			}
			want := []string{"ver=" + ver, "proto=http", "path=/", "sse=/sse/image", "caps=stream,screencast"}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}

	t.Run("method uses build version", func(t *testing.T) {
		old := version
		t.Cleanup(func() { version = old })
		version = "9.8.7"
		recs := (&AvahiService{}).prepareTXTRecords()
		if string(recs[0]) != "ver=9.8.7" {
			t.Errorf("first record = %q, want ver=9.8.7", recs[0])
		}
	})
}

func TestIPSuffixFromAddrs(t *testing.T) {
	cidr := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	tests := []struct {
		name  string
		addrs []net.Addr
		want  string
	}{
		{"none", nil, ""},
		{"loopback only", []net.Addr{cidr("127.0.0.1/8"), cidr("::1/128")}, ""},
		{"skips loopback and ipv6", []net.Addr{cidr("127.0.0.1/8"), cidr("fe80::1/64"), cidr("192.168.1.42/24")}, "42"},
		{"first ipv4 wins", []net.Addr{cidr("10.0.0.7/8"), cidr("192.168.1.42/24")}, "7"},
		{"non-IPNet ignored", []net.Addr{&net.IPAddr{IP: net.ParseIP("10.1.1.1")}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ipSuffixFromAddrs(tt.addrs); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServeIndexHTML(t *testing.T) {
	ic, err := newImageCache(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	name := `alice <script>&"x"`
	h := newHTTPHandler(ic, newSSEBroadcaster(), func() string { return name })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<h1>alice &lt;script&gt;&amp;&#34;x&#34;</h1>") {
		t.Errorf("escaped service name not found in body")
	}
	if strings.Contains(body, "<script>&") {
		t.Errorf("service name was injected unescaped")
	}
	if !strings.Contains(body, "width: 100%") {
		t.Errorf("CSS width: 100%% missing from body")
	}
	if strings.Contains(body, "%!") {
		t.Errorf("body contains fmt verb error sequence %%!")
	}
	if !strings.Contains(body, "new EventSource('/sse/image')") {
		t.Errorf("SSE wiring missing from body")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q, want *", got)
	}

	t.Run("unknown path is 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("name changes are reflected", func(t *testing.T) {
		name = "bob [42]"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if !strings.Contains(rec.Body.String(), "<h1>bob [42]</h1>") {
			t.Errorf("updated service name not rendered")
		}
	})
}

// fakeScreencaster records calls and simulates GNOME Shell's rule that a
// client may only have one recording at a time.
type fakeScreencaster struct {
	mu        sync.Mutex
	name      string
	recording bool
	starts    int
	stops     int
	startErr  error
	pingErr   error
	log       *[]string
	logMu     *sync.Mutex
}

func (f *fakeScreencaster) event(s string) {
	f.logMu.Lock()
	*f.log = append(*f.log, f.name+":"+s)
	f.logMu.Unlock()
}

func (f *fakeScreencaster) Start(string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	if f.recording {
		return "", errors.New("already recording")
	}
	f.recording = true
	f.starts++
	f.event("start")
	return f.name + ".webm", nil
}

func (f *fakeScreencaster) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.recording = false
	f.event("stop")
	return nil
}

func (f *fakeScreencaster) Ping() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

func (f *fakeScreencaster) setPingErr(err error) {
	f.mu.Lock()
	f.pingErr = err
	f.mu.Unlock()
}

func (f *fakeScreencaster) setStartErr(err error) {
	f.mu.Lock()
	f.startErr = err
	f.mu.Unlock()
}

func (f *fakeScreencaster) counts() (starts, stops int, recording bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops, f.recording
}

func newFakePair() (a, b *fakeScreencaster, events func() []string) {
	var (
		log []string
		mu  sync.Mutex
	)
	a = &fakeScreencaster{name: "a", log: &log, logMu: &mu}
	b = &fakeScreencaster{name: "b", log: &log, logMu: &mu}
	return a, b, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), log...)
	}
}

// TestRecordLoopRotatesDespiteHealthChecks guards against the segment timer
// being re-armed by every health-check tick: with health checks firing much
// more often than the segment length, segments must still rotate.
func TestRecordLoopRotatesDespiteHealthChecks(t *testing.T) {
	a, b, events := newFakePair()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	err := recordLoop(ctx, loopConfig{
		segment:     60 * time.Millisecond,
		healthCheck: 5 * time.Millisecond,
		retryDelay:  10 * time.Millisecond,
	}, a, b)
	if err != nil {
		t.Fatalf("recordLoop returned %v, want nil on cancel", err)
	}

	ev := events()
	starts := 0
	for _, e := range ev {
		if strings.HasSuffix(e, ":start") {
			starts++
		}
	}
	if starts < 4 { // initial + at least 3 rotations in ~400ms with 60ms segments
		t.Fatalf("expected at least 4 starts (segments rotating), got %d: %v", starts, ev)
	}

	// Overlap: each rotation starts the other client before stopping the current one.
	if ev[0] != "a:start" || ev[1] != "b:start" || ev[2] != "a:stop" || ev[3] != "a:start" || ev[4] != "b:stop" {
		t.Errorf("unexpected rotation order: %v", ev)
	}

	// Cleanup: nothing left recording after return.
	for _, f := range []*fakeScreencaster{a, b} {
		if _, _, rec := f.counts(); rec {
			t.Errorf("%s still recording after recordLoop returned", f.name)
		}
	}
}

func TestRecordLoopConnectionLost(t *testing.T) {
	a, b, _ := newFakePair()
	go func() {
		time.Sleep(30 * time.Millisecond)
		b.setPingErr(errors.New("bus gone"))
	}()

	done := make(chan error, 1)
	go func() {
		done <- recordLoop(context.Background(), loopConfig{
			segment:     time.Hour,
			healthCheck: 5 * time.Millisecond,
			retryDelay:  time.Millisecond,
		}, a, b)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errConnectionLost) {
			t.Fatalf("err = %v, want errConnectionLost", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recordLoop did not exit on lost connection")
	}
	if _, stops, rec := a.counts(); rec || stops != 1 {
		t.Errorf("active recording must be stopped before exit: stops=%d recording=%v", stops, rec)
	}
}

func TestRecordLoopInitialStartFails(t *testing.T) {
	a, b, _ := newFakePair()
	a.setStartErr(errors.New("disabled"))
	err := recordLoop(context.Background(), loopConfig{segment: time.Hour, healthCheck: time.Hour}, a, b)
	if err == nil || errors.Is(err, errConnectionLost) {
		t.Fatalf("err = %v, want non-connection start error", err)
	}
}

func TestRotate(t *testing.T) {
	t.Run("overlap via spare", func(t *testing.T) {
		a, b, events := newFakePair()
		_, _ = a.Start("")
		got, err := rotate(context.Background(), a, b, "", 0)
		if err != nil || got != b {
			t.Fatalf("rotate = %v, %v; want b, nil", got, err)
		}
		if ev := events(); strings.Join(ev, ",") != "a:start,b:start,a:stop" {
			t.Errorf("events = %v", ev)
		}
	})

	t.Run("fallback to stop then start when spare fails", func(t *testing.T) {
		a, b, events := newFakePair()
		_, _ = a.Start("")
		b.setStartErr(errors.New("nope"))
		got, err := rotate(context.Background(), a, b, "", 0)
		if err != nil || got != a {
			t.Fatalf("rotate = %v, %v; want a, nil", got, err)
		}
		if ev := events(); strings.Join(ev, ",") != "a:start,a:stop,a:start" {
			t.Errorf("events = %v", ev)
		}
	})

	t.Run("both fail", func(t *testing.T) {
		a, b, _ := newFakePair()
		_, _ = a.Start("")
		b.setStartErr(errors.New("nope"))
		a.setStartErr(errors.New("nope"))
		if got, err := rotate(context.Background(), a, b, "", 0); err == nil || got != nil {
			t.Fatalf("rotate = %v, %v; want nil, error", got, err)
		}
	})
}

func TestRecordLoopRecoversAfterFailedRotation(t *testing.T) {
	a, b, _ := newFakePair()
	b.setStartErr(errors.New("b broken"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- recordLoop(ctx, loopConfig{
			segment:     20 * time.Millisecond,
			healthCheck: time.Hour,
			retryDelay:  5 * time.Millisecond,
		}, a, b)
	}()

	// Make a's restart fail once mid-rotation, then recover.
	time.Sleep(10 * time.Millisecond)
	a.setStartErr(errors.New("transient"))
	time.Sleep(40 * time.Millisecond)
	a.setStartErr(nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, rec := a.counts(); rec {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recording was not restarted after transient failure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
}

func TestBuildVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = "0.1.0"
	if got := buildVersion(); got != "0.1.0" {
		t.Errorf("buildVersion() = %q, want ldflags value 0.1.0", got)
	}
	version = "dev"
	if got := buildVersion(); got == "" {
		t.Error("buildVersion() returned empty string")
	}
}
