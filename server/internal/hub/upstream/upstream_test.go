package upstream

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"snoopy/internal/hub/hubtest"
	"snoopy/internal/hub/registry"
)

func TestValidImageName(t *testing.T) {
	for _, ok := range []string{"waiting.jpg", "0b7d2c1e-1234-4abc-9def-001122334455.jpg", "a_b-c.d.jpg"} {
		if !ValidImageName(ok) {
			t.Errorf("ValidImageName(%q) = false", ok)
		}
	}
	for _, bad := range []string{
		"", ".jpg", "..jpg", "../x.jpg", "a/b.jpg", `a\b.jpg`, "x.png", "x.jpg/", "x.jpg?y", "%2e%2e.jpg",
		"..%2f..%2fetc.jpg", "x .jpg", strings.Repeat("a", 200) + ".jpg", "x.JPG", "x.jpg\x00",
	} {
		if ValidImageName(bad) {
			t.Errorf("ValidImageName(%q) = true", bad)
		}
	}
}

type env struct {
	fake *hubtest.Fake
	reg  *registry.Registry
	m    *Manager
	id   string
	stop func()
}

func setup(t *testing.T, opts ...func(*Manager)) *env {
	t.Helper()
	f := hubtest.NewFake()
	host, port := f.HostPort()
	reg := registry.New(nil, nil)
	reg.AddStatic([]registry.StaticEntry{{Name: "alice", Host: host, Port: port}})
	id := registry.ID("alice", host)
	reg.ReportProbe(id, true)
	m := New(reg.Get, registry.NewHTTPClient(time.Second, time.Second), t.Logf)
	m.MinBackoff, m.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
	for _, o := range opts {
		o(m)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Follow(ctx, reg) }()
	e := &env{fake: f, reg: reg, m: m, id: id}
	e.stop = func() { cancel(); <-done; f.Close() }
	t.Cleanup(e.stop)
	return e
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recv(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
		return Frame{}
	}
}

func TestSharedUpstreamFanOutAndCache(t *testing.T) {
	e := setup(t)
	waitFor(t, "upstream connection", func() bool { return e.fake.OpenStreams() == 1 })

	subs := make([]<-chan Frame, 5)
	for i := range subs {
		ch, cancel := e.m.Subscribe(e.id)
		defer cancel()
		subs[i] = ch
	}
	all, cancelAll := e.m.SubscribeAll()
	defer cancelAll()

	jpeg := []byte("\xff\xd8frame-1\xff\xd9")
	e.fake.Publish("f1.jpg", jpeg)
	for _, ch := range subs {
		f := recv(t, ch)
		if f.Path != "/s/"+e.id+"/images/f1.jpg" || f.ID != e.id || f.At.IsZero() {
			t.Fatalf("frame = %+v", f)
		}
	}
	if f := recv(t, all); f.File != "f1.jpg" {
		t.Fatalf("all-services frame = %+v", f)
	}
	if e.fake.Connects() != 1 {
		t.Fatalf("%d upstream connections for 5 subscribers, want 1", e.fake.Connects())
	}
	file, at, b := e.m.Latest(e.id)
	if file != "f1.jpg" || at.IsZero() || !bytes.Equal(b, jpeg) {
		t.Fatalf("Latest = %q %v %q", file, at, b)
	}
	fetches := e.fake.CountRequests("/images/f1.jpg")
	for range 10 {
		got, err := e.m.Image(context.Background(), e.id, "f1.jpg")
		if err != nil || !bytes.Equal(got, jpeg) {
			t.Fatalf("Image = %q, %v", got, err)
		}
	}
	if e.fake.CountRequests("/images/f1.jpg") != fetches {
		t.Fatal("the newest frame must be served from the cache")
	}
	if fetches != 1 {
		t.Fatalf("frame fetched %d times, want once", fetches)
	}

	// Older images are proxied; missing ones are ErrNotFound.
	if _, err := e.m.Image(context.Background(), e.id, "nope.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}
	if _, err := e.m.Image(context.Background(), e.id, "../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid name: %v", err)
	}
	if _, err := e.m.Image(context.Background(), "unknown-id", "f1.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestCacheCap(t *testing.T) {
	e := setup(t, func(m *Manager) { m.MaxFrameBytes = 16 })
	waitFor(t, "upstream connection", func() bool { return e.fake.OpenStreams() == 1 })
	ch, cancel := e.m.Subscribe(e.id)
	defer cancel()
	e.fake.Publish("big.jpg", bytes.Repeat([]byte("x"), 17))
	if f := recv(t, ch); f.File != "big.jpg" {
		t.Fatalf("frame = %+v", f)
	}
	if file, _, b := e.m.Latest(e.id); file != "big.jpg" || b != nil {
		t.Fatalf("oversized frame cached: %q %d bytes", file, len(b))
	}
	if _, err := e.m.Image(context.Background(), e.id, "big.jpg"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized image: %v", err)
	}
	e.fake.Publish("small.jpg", []byte("tiny"))
	recv(t, ch)
	if _, _, b := e.m.Latest(e.id); string(b) != "tiny" {
		t.Fatalf("small frame not cached: %q", b)
	}
}

func TestWaitingAndJunkEvents(t *testing.T) {
	e := setup(t)
	ch, cancel := e.m.Subscribe(e.id)
	defer cancel()
	// The fake has no frame yet: snoopy sends a bare /images/.
	if f := recv(t, ch); f.File != WaitingImage || !f.At.IsZero() {
		t.Fatalf("initial frame = %+v", f)
	}
	e.fake.Send("/images/../../secret.jpg")
	e.fake.Send("http://evil.example/x.jpg")
	e.fake.Send("/images/ok.jpg?t=1")
	if f := recv(t, ch); f.File != "ok.jpg" {
		t.Fatalf("junk events must be ignored; got %+v", f)
	}
	for _, p := range e.fake.Requests() {
		if strings.Contains(p, "secret") {
			t.Fatalf("hub requested %q", p)
		}
	}
}

func TestReconnectAndOffline(t *testing.T) {
	e := setup(t)
	ch, cancel := e.m.Subscribe(e.id)
	defer cancel()
	waitFor(t, "first connection", func() bool { return e.fake.Connects() == 1 })
	recv(t, ch)
	e.fake.DropStreams()
	waitFor(t, "reconnect", func() bool { return e.fake.Connects() >= 2 && e.fake.OpenStreams() == 1 })
	e.fake.Publish("after.jpg", []byte("x"))
	if f := recv(t, ch); f.File != "after.jpg" {
		t.Fatalf("frame after reconnect = %+v", f)
	}

	// Offline: disconnected, last frame kept.
	e.reg.ReportProbe(e.id, false)
	e.reg.ReportProbe(e.id, false)
	waitFor(t, "disconnect", func() bool { return !e.m.Connected(e.id) && e.fake.OpenStreams() == 0 })
	if file, _, _ := e.m.Latest(e.id); file != "after.jpg" {
		t.Fatalf("last frame lost while offline: %q", file)
	}
	// Online again: reconnected.
	e.reg.ReportProbe(e.id, true)
	waitFor(t, "connect again", func() bool { return e.fake.OpenStreams() == 1 })
}

func TestForgetClosesSubscribers(t *testing.T) {
	e := setup(t)
	ch, _ := e.m.Subscribe(e.id)
	e.m.Forget(e.id)
	for range ch {
	}
	if file, _, _ := e.m.Latest(e.id); file != "" {
		t.Fatal("state kept after Forget")
	}
}
