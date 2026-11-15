package discovery

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestParseTXT(t *testing.T) {
	got := ParseTXT([][]byte{
		[]byte("ver=0.3.0"), []byte("PROTO=http"), []byte("proto=ignored"),
		[]byte("caps=stream,screencast"), []byte("flag"), []byte("=nokey"), []byte("rtsp=8554"),
	})
	want := map[string]string{"ver": "0.3.0", "proto": "http", "caps": "stream,screencast", "flag": "", "rtsp": "8554"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTXT = %v, want %v", got, want)
	}
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'a'
	}
	if v := ParseTXT([][]byte{append([]byte("k="), long...)})["k"]; len(v) != maxTXTValue {
		t.Fatalf("long value not capped: %d", len(v))
	}
}

type failingBrowser struct {
	mu    sync.Mutex
	calls int
}

func (f *failingBrowser) Browse(ctx context.Context, emit func(Event)) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	emit(Event{Kind: Found, Key: Key{Name: "x"}})
	return errors.New("no bus")
}

func TestSuperviseRetriesAndResets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := &failingBrowser{}
	var mu sync.Mutex
	var kinds []Kind
	var logs int
	done := make(chan struct{})
	go func() {
		defer close(done)
		Supervise(ctx, b, func(e Event) {
			mu.Lock()
			kinds = append(kinds, e.Kind)
			mu.Unlock()
		}, func(string, ...any) { mu.Lock(); logs++; mu.Unlock() }, time.Millisecond, 4*time.Millisecond)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		n := b.calls
		b.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Supervise did not retry")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if kinds[0] != Found || kinds[1] != Reset {
		t.Fatalf("events = %v, want Found then Reset", kinds)
	}
	if logs != 1 {
		t.Fatalf("identical errors logged %d times, want once", logs)
	}
}

// fakeBus is a scripted avahiBus.
type fakeBus struct {
	sigs     chan *dbus.Signal
	resolve  func(name string, iface, proto int32) (resolved, error)
	browsers int
	closed   bool
	mu       sync.Mutex
}

func (f *fakeBus) Signals() <-chan *dbus.Signal { return f.sigs }
func (f *fakeBus) ServiceBrowserNew(_ context.Context, iface, proto int32, typ, domain string, flags uint32) (dbus.ObjectPath, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.browsers++
	if iface != -1 || proto != -1 || typ != ServiceType || domain != "" || flags != 0 {
		return "", errors.New("unexpected browser arguments")
	}
	return "/Client1/ServiceBrowser1", nil
}
func (f *fakeBus) ResolveService(_ context.Context, iface, proto int32, name, typ, domain string, aproto int32, flags uint32) (resolved, error) {
	if aproto != proto {
		return resolved{}, errors.New("aprotocol should match the item's protocol")
	}
	return f.resolve(name, iface, proto)
}
func (f *fakeBus) Close() error { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return nil }

func item(member string, iface, proto int32, name string) *dbus.Signal {
	return &dbus.Signal{
		Path: "/Client1/ServiceBrowser1",
		Name: avahiBrowserIface + "." + member,
		Body: []any{iface, proto, name, ServiceType, "local", uint32(0)},
	}
}

func startAvahi(t *testing.T, bus *fakeBus) (events chan Event, errc chan error, cancel func()) {
	t.Helper()
	a := &Avahi{dial: func() (avahiBus, error) { return bus, nil }, ifName: func(i int) string { return "eth" + string(rune('0'+i)) }, resolveTimeout: time.Second}
	ctx, c := context.WithCancel(context.Background())
	events = make(chan Event, 16)
	errc = make(chan error, 1)
	go func() { errc <- a.Browse(ctx, func(e Event) { events <- e }) }()
	return events, errc, c
}

func next(t *testing.T, ch chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func TestAvahiFoundAndLost(t *testing.T) {
	block := make(chan struct{})
	bus := &fakeBus{sigs: make(chan *dbus.Signal, 16), resolve: func(name string, iface, proto int32) (resolved, error) {
		switch name {
		case "slow":
			<-block
			return resolved{Host: "slow.local", Address: "10.0.0.9", Port: 8900}, nil
		case "bad":
			return resolved{Host: "bad.local", Address: "not-an-ip", Port: 8900}, nil
		case "ll":
			return resolved{Interface: iface, Host: "ll.local", Address: "fe80::1", Port: 8900}, nil
		}
		return resolved{Interface: iface, Protocol: proto, Host: "alice-pc.local.", Address: "192.168.1.20", Port: 8900,
			TXT: [][]byte{[]byte("ver=1.0"), []byte("rtsp=8554")}}, nil
	}}
	events, errc, cancel := startAvahi(t, bus)

	// A signal for another browser object is ignored.
	other := item("ItemNew", 2, 0, "ghost")
	other.Path = "/Client9/ServiceBrowser9"
	bus.sigs <- other
	bus.sigs <- item("ItemNew", 2, 0, "alice")
	e := next(t, events)
	if e.Kind != Found || e.Sighting.Name != "alice" || e.Sighting.Host != "alice-pc.local" ||
		e.Sighting.Address != "192.168.1.20" || e.Sighting.Port != 8900 || e.Sighting.TXT["rtsp"] != "8554" {
		t.Fatalf("unexpected event %+v", e)
	}
	if e.Key != (Key{Interface: 2, Protocol: 0, Name: "alice", Domain: "local"}) {
		t.Fatalf("key = %+v", e.Key)
	}

	// Link-local IPv6 gets its zone.
	bus.sigs <- item("ItemNew", 3, 1, "ll")
	if e := next(t, events); e.Sighting.Address != "fe80::1%eth3" {
		t.Fatalf("link-local address = %q", e.Sighting.Address)
	}

	// An unusable address is dropped.
	bus.sigs <- item("ItemNew", 2, 0, "bad")
	// A removal before the resolve finishes suppresses the stale result.
	bus.sigs <- item("ItemNew", 2, 0, "slow")
	bus.sigs <- item("ItemRemove", 2, 0, "slow")
	time.Sleep(20 * time.Millisecond)
	close(block)

	bus.sigs <- item("ItemRemove", 2, 0, "alice")
	e = next(t, events)
	if e.Kind != Lost || e.Key.Name != "alice" {
		t.Fatalf("want Lost alice, got %+v", e)
	}
	select {
	case e := <-events:
		t.Fatalf("unexpected extra event %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Browse after cancel = %v", err)
	}
	if !bus.closed {
		t.Fatal("bus not closed")
	}
}

func TestAvahiFailures(t *testing.T) {
	for name, sig := range map[string]*dbus.Signal{
		"failure":   {Path: "/Client1/ServiceBrowser1", Name: avahiBrowserIface + ".Failure", Body: []any{"boom"}},
		"restarted": {Path: "/org/freedesktop/DBus", Name: nameOwnerChanged, Body: []any{avahiDest, ":1.4", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			bus := &fakeBus{sigs: make(chan *dbus.Signal, 4)}
			_, errc, cancel := startAvahi(t, bus)
			defer cancel()
			bus.sigs <- sig
			select {
			case err := <-errc:
				if err == nil {
					t.Fatal("want an error")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Browse did not stop")
			}
		})
	}
	t.Run("bus closed", func(t *testing.T) {
		bus := &fakeBus{sigs: make(chan *dbus.Signal)}
		_, errc, cancel := startAvahi(t, bus)
		defer cancel()
		close(bus.sigs)
		if err := <-errc; err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("dial", func(t *testing.T) {
		a := &Avahi{dial: func() (avahiBus, error) { return nil, errors.New("no system bus") }}
		if err := a.Browse(context.Background(), func(Event) {}); err == nil {
			t.Fatal("want an error")
		}
	})
}
