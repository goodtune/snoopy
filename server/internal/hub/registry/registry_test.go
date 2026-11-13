package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"snoopy/internal/hub/discovery"
)

var safeID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestIDStableAndSafe(t *testing.T) {
	if a, b := ID("alice", "alice-pc.local"), ID("alice", "alice-pc.local"); a != b {
		t.Fatalf("ID not stable: %q vs %q", a, b)
	}
	if got := ID("alice", "alice-pc.local"); !strings.HasPrefix(got, "alice-alice-pc-") {
		t.Fatalf("ID = %q, want a readable alice-alice-pc-<hash>", got)
	}
	if ID("alice", "pc.local") == ID("alice", "pc2.local") || ID("a-b", "c") == ID("a", "b-c") {
		t.Fatal("different services share an id")
	}
	if ID("alice", "ALICE-PC.local.") != ID("alice", "alice-pc.local") {
		t.Fatal("host case and trailing dot must not change the id")
	}
	weird := []string{
		"", " ", "alice smith", "Zoë", "日本語", "<script>alert(1)</script>", "../../etc/passwd",
		"a/b\\c", "name\x00with\nnull", strings.Repeat("long", 100), "-dash-", "alice [12]",
	}
	seen := map[string]string{}
	for _, n := range weird {
		id := ID(n, "host.local")
		if !safeID.MatchString(id) || len(id) > maxSlug+7 {
			t.Errorf("ID(%q) = %q: not a safe slug", n, id)
		}
		if url.PathEscape(id) != id {
			t.Errorf("ID(%q) = %q needs escaping", n, id)
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("ID(%q) == ID(%q) = %q", n, prev, id)
		}
		seen[id] = n
	}
}

func TestValidHost(t *testing.T) {
	for _, h := range []string{"alice-pc.local", "alice-pc.local.", "localhost", "10.0.0.1", "fd00::5", "a.b-c.d"} {
		if !ValidHost(h) {
			t.Errorf("ValidHost(%q) = false", h)
		}
	}
	for _, h := range []string{"", "-bad", "bad-", "a..b", "with space", "evil\"quote", "a/b", "<x>", strings.Repeat("a", 64) + ".local"} {
		if ValidHost(h) {
			t.Errorf("ValidHost(%q) = true", h)
		}
	}
}

func TestParseStatic(t *testing.T) {
	got, err := ParseStatic(" alice=alice-pc.example.net:8900 ;rtsp=8554, bob=10.0.0.5, carol=[fd00::7]:9000,dave=fd00::8 ,, ")
	if err != nil {
		t.Fatal(err)
	}
	want := []StaticEntry{
		{"alice", "alice-pc.example.net", 8900, 8554},
		{"bob", "10.0.0.5", 8900, 0},
		{"carol", "fd00::7", 9000, 0},
		{"dave", "fd00::8", 8900, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if l, err := ParseStatic(""); err != nil || len(l) != 0 {
		t.Fatalf("empty: %v %v", l, err)
	}
	for _, bad := range []string{
		"alice", "=host", "alice=", "alice=host:0", "alice=host:65536", "alice=host:x",
		"alice=bad host", "alice=host;rtsp=0", "alice=host;foo=1", "a=h,a=h2", "A=h,a=h2",
		"alice=fe80::1%eth0", "ali\x01ce=host", strings.Repeat("n", 64) + "=host",
	} {
		if _, err := ParseStatic(bad); err == nil {
			t.Errorf("ParseStatic(%q) accepted", bad)
		}
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func sighting(ifc, proto int32, name, host, addr string, txt map[string]string) discovery.Event {
	k := discovery.Key{Interface: ifc, Protocol: proto, Name: name, Domain: "local"}
	return discovery.Event{Kind: discovery.Found, Key: k, Sighting: discovery.Sighting{
		Key: k, Name: name, Host: host, Address: addr, Port: 8900, TXT: txt,
	}}
}

func drain(ch <-chan Change) []ChangeKind {
	var out []ChangeKind
	for {
		select {
		case c := <-ch:
			out = append(out, c.Kind)
		default:
			return out
		}
	}
}

func TestRegistryDedupeAndIPv4Preference(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now, nil)
	ch, cancel := r.Subscribe(64)
	defer cancel()

	txt := map[string]string{"ver": "1.2.3", "rtsp": "8554"}
	r.Apply(sighting(3, 1, "alice", "alice-pc.local", "fd00::20", txt))
	r.Apply(sighting(2, 0, "alice", "alice-pc.local", "192.168.1.20", txt))
	r.Apply(sighting(4, 0, "alice", "alice-pc.local", "10.8.0.20", txt))
	list := r.List()
	if len(list) != 1 {
		t.Fatalf("want one deduplicated service, got %d", len(list))
	}
	s := list[0]
	if s.Addresses[0] != "192.168.1.20" || s.DialAddr() != "192.168.1.20:8900" {
		t.Fatalf("IPv4 on the lowest interface must be preferred: %v", s.Addresses)
	}
	if len(s.Addresses) != 3 || s.Addresses[2] != "fd00::20" {
		t.Fatalf("addresses = %v", s.Addresses)
	}
	if s.Version != "1.2.3" || s.RTSPPort != 8554 || !s.Online || s.Source != SourceMDNS {
		t.Fatalf("service = %+v", s)
	}
	if s.RTSPURL() != "rtsp://alice-pc.local:8554/" || s.RTSPURLIP() != "rtsp://192.168.1.20:8554/" {
		t.Fatalf("rtsp urls %q %q", s.RTSPURL(), s.RTSPURLIP())
	}
	if got := drain(ch); strings.Join(kinds(got), ",") != "added,updated,updated" {
		t.Fatalf("changes = %v, want added then two updates (new addresses)", got)
	}

	// Same name on another host is a different service.
	// (On a real network instance names are unique, so this can only
	// happen through another interface.)
	r.Apply(sighting(5, 0, "alice", "other.local", "192.168.1.30", nil))
	if len(r.List()) != 2 {
		t.Fatal("same name on another host must be a separate entry")
	}

	// Losing one sighting keeps it online; losing all marks it offline.
	r.Apply(discovery.Event{Kind: discovery.Lost, Key: discovery.Key{Interface: 2, Protocol: 0, Name: "alice", Domain: "local"}})
	s, _ = r.Get(s.ID)
	if !s.Online || s.Addresses[0] != "10.8.0.20" {
		t.Fatalf("after losing one sighting: %+v", s)
	}
	drain(ch)
	r.Apply(discovery.Event{Kind: discovery.Reset})
	for _, s := range r.List() {
		if s.Online {
			t.Fatalf("%s still online after reset", s.Name)
		}
	}
	if got := drain(ch); len(got) < 2 || got[len(got)-1] != Offline || strings.Count(strings.Join(kinds(got), ","), "offline") != 2 {
		t.Fatalf("changes = %v, want both offline", got)
	}

	// Coming back: online again, same id.
	r.Apply(sighting(2, 0, "alice", "alice-pc.local", "192.168.1.20", txt))
	if s2, _ := r.Get(s.ID); !s2.Online {
		t.Fatal("not back online")
	}
	// Probes do not revive a withdrawn entry; Sweep forgets it later.
	other := ID("alice", "other.local")
	r.ReportProbe(other, true)
	if s, _ := r.Get(other); s.Online {
		t.Fatal("probe revived a withdrawn mDNS entry")
	}
	c.add(DefaultForgetAfter - time.Minute)
	r.Sweep()
	if _, ok := r.Get(other); !ok {
		t.Fatal("swept too early")
	}
	c.add(2 * time.Minute)
	r.Sweep()
	if _, ok := r.Get(other); ok {
		t.Fatal("withdrawn entry not forgotten")
	}
	if _, ok := r.Get(s.ID); !ok {
		t.Fatal("live entry swept")
	}
}

func TestRegistryHostChangeAndUpdate(t *testing.T) {
	r := New(nil, nil)
	r.Apply(sighting(2, 0, "bob", "bob-old.local", "10.0.0.2", nil))
	r.Apply(sighting(2, 0, "bob", "bob-new.local", "10.0.0.3", nil))
	list := r.List()
	var online []string
	for _, s := range list {
		if s.Online {
			online = append(online, s.Host)
		}
	}
	if len(online) != 1 || online[0] != "bob-new.local" {
		t.Fatalf("a re-resolved sighting must move to the new host: %+v", list)
	}
	ch, cancel := r.Subscribe(8)
	defer cancel()
	r.Apply(sighting(2, 0, "bob", "bob-new.local", "10.0.0.3", map[string]string{"rtsp": "8554"}))
	if got := drain(ch); len(got) != 1 || got[0] != Updated {
		t.Fatalf("changes = %v, want updated", got)
	}
	r.Apply(sighting(2, 0, "bob", "bob-new.local", "10.0.0.3", map[string]string{"rtsp": "8554"}))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("an identical re-announcement must not notify: %v", got)
	}
	// An invalid rtsp TXT value is ignored.
	r.Apply(sighting(2, 0, "bob", "bob-new.local", "10.0.0.3", map[string]string{"rtsp": "99999"}))
	if s, _ := r.Get(ID("bob", "bob-new.local")); s.RTSPPort != 0 || s.RTSPURL() != "" {
		t.Fatalf("invalid rtsp port accepted: %+v", s)
	}
}

func TestRegistryUntrustedHostInRTSPURL(t *testing.T) {
	r := New(nil, nil)
	r.Apply(sighting(2, 0, "x", `evil"host<.local`, "10.0.0.9", map[string]string{"rtsp": "8554"}))
	s := r.List()[0]
	if s.RTSPURL() != "rtsp://10.0.0.9:8554/" {
		t.Fatalf("invalid host name must fall back to the address: %q", s.RTSPURL())
	}
	r.Apply(sighting(3, 1, "y", "y.local", "fe80::1%eth0", map[string]string{"rtsp": "8554"}))
	for _, s := range r.List() {
		if s.Name == "y" && s.RTSPURLIP() != "" {
			t.Fatalf("zoned address in an RTSP URL: %q", s.RTSPURLIP())
		}
	}
}

func TestRegistryStaticAndProbes(t *testing.T) {
	r := New(nil, nil)
	r.FailThreshold = 2
	ch, cancel := r.Subscribe(16)
	defer cancel()
	r.AddStatic([]StaticEntry{{Name: "alice", Host: "10.0.0.1", Port: 8900, RTSPPort: 8554}, {Name: "bob", Host: "bob.example", Port: 9000}})
	id := ID("alice", "10.0.0.1")
	s, ok := r.Get(id)
	if !ok || s.Online || s.Source != SourceStatic || s.DialAddr() != "10.0.0.1:8900" || s.RTSPURL() != "rtsp://10.0.0.1:8554/" {
		t.Fatalf("static entry = %+v", s)
	}
	if b, _ := r.Get(ID("bob", "bob.example")); b.DialAddr() != "bob.example:9000" {
		t.Fatalf("static host name dial = %q", b.DialAddr())
	}
	r.ReportProbe(id, true)
	r.ReportProbe(id, false)
	if s, _ := r.Get(id); !s.Online {
		t.Fatal("one failure must not mark it offline")
	}
	r.ReportProbe(id, false)
	if s, _ := r.Get(id); s.Online {
		t.Fatal("two failures must mark it offline")
	}
	got := drain(ch)
	want := []ChangeKind{Added, Added, Online, Offline}
	if strings.Join(kinds(got), ",") != strings.Join(kinds(want), ",") {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if len(r.ProbeTargets()) != 2 {
		t.Fatal("static entries are always probe targets")
	}
	// An mDNS service with the same id as a static entry is ignored.
	r.Apply(sighting(2, 0, "alice", "10.0.0.1", "10.0.0.1", nil))
	if s, _ := r.Get(id); s.Source != SourceStatic {
		t.Fatal("static entry replaced by mDNS")
	}
}

func kinds(k []ChangeKind) []string {
	out := make([]string, len(k))
	for i, x := range k {
		out[i] = string(x)
	}
	return out
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	r := New(nil, nil)
	ch, cancel := r.Subscribe(1)
	defer cancel()
	r.AddStatic([]StaticEntry{{Name: "a", Host: "h1", Port: 1}, {Name: "b", Host: "h2", Port: 1}})
	<-ch
	if _, ok := <-ch; ok {
		t.Fatal("slow subscriber should have been closed")
	}
	cancel() // safe after the drop
}

func TestProber(t *testing.T) {
	var waitingHits, rootHits atomic.Int32
	mode := atomic.Value{}
	mode.Store("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/images/waiting.jpg":
			waitingHits.Add(1)
			switch mode.Load() {
			case "ok":
				w.WriteHeader(http.StatusOK)
			case "redirect":
				http.Redirect(w, r, "http://198.51.100.1/", http.StatusFound)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case "/":
			rootHits.Add(1)
			if mode.Load() == "rootonly" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, port, _ := strings.Cut(u.Host, ":")
	r := New(nil, nil)
	p := &Prober{Reg: r, Client: NewHTTPClient(time.Second, time.Second), Interval: time.Hour, Timeout: time.Second}
	pn := 0
	for _, c := range port {
		pn = pn*10 + int(c-'0')
	}
	s := Service{Host: host, Port: pn}
	ctx := context.Background()
	if !p.Probe(ctx, s) {
		t.Fatal("waiting.jpg answering must count as up")
	}
	mode.Store("rootonly")
	if !p.Probe(ctx, s) {
		t.Fatal("/ answering must count as up")
	}
	mode.Store("down")
	if p.Probe(ctx, s) {
		t.Fatal("errors must count as down")
	}
	mode.Store("redirect")
	if p.Probe(ctx, s) {
		t.Fatal("a redirect must not be followed or count as up")
	}
	if p.Probe(ctx, Service{Host: "127.0.0.1", Port: 1}) {
		t.Fatal("closed port must count as down")
	}

	// Run probes the registry and Poke probes at once.
	mode.Store("ok")
	r.AddStatic([]StaticEntry{{Name: "x", Host: host, Port: pn}})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go p.Run(runCtx)
	waitFor(t, func() bool { s, _ := r.Get(ID("x", host)); return s.Online })
	before := waitingHits.Load()
	p.Poke(ID("x", host))
	waitFor(t, func() bool { return waitingHits.Load() > before })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
