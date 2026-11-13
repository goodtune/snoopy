package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"snoopy/internal/hub/discovery"
	"snoopy/internal/hub/hubtest"
	"snoopy/internal/hub/registry"
	"snoopy/internal/hub/upstream"
)

type env struct {
	fake *hubtest.Fake
	evil *hubtest.Fake
	reg  *registry.Registry
	srv  *httptest.Server
	id   string
}

func setup(t *testing.T, o Options) *env {
	t.Helper()
	f := hubtest.NewFake()
	evil := hubtest.NewFake()
	host, port := f.HostPort()
	reg := registry.New(nil, nil)
	reg.AddStatic([]registry.StaticEntry{{Name: "alice", Host: host, Port: port, RTSPPort: 8554}})
	id := registry.ID("alice", host)
	reg.ReportProbe(id, true)
	m := upstream.New(reg.Get, registry.NewHTTPClient(time.Second, time.Second), t.Logf)
	m.MinBackoff = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Follow(ctx, reg) }()
	o.Registry, o.Frames = reg, m
	o.Heartbeat = 50 * time.Millisecond
	srv := httptest.NewServer(New(o))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		cancel()
		<-done
		f.Close()
		evil.Close()
	})
	return &env{fake: f, evil: evil, reg: reg, srv: srv, id: id}
}

func get(t *testing.T, url string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
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

// sseReader reads events from a streaming response.
type sseReader struct {
	resp *http.Response
	sc   *bufio.Scanner
	ch   chan [2]string
}

func openSSE(t *testing.T, url string) *sseReader {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	r := &sseReader{resp: resp, sc: bufio.NewScanner(resp.Body), ch: make(chan [2]string, 64)}
	go func() {
		defer close(r.ch)
		var event, data string
		for r.sc.Scan() {
			line := r.sc.Text()
			switch {
			case line == "":
				if data != "" {
					r.ch <- [2]string{event, data}
				}
				event, data = "", ""
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return r
}

// next returns the next event, skipping kinds in skip.
func (r *sseReader) next(t *testing.T, skip ...string) (string, string) {
	t.Helper()
	for {
		select {
		case ev, ok := <-r.ch:
			if !ok {
				t.Fatal("stream ended")
			}
			skipped := false
			for _, s := range skip {
				if ev[0] == s {
					skipped = true
				}
			}
			if !skipped {
				return ev[0], ev[1]
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
	}
}

func TestServiceSSERewritesAndShares(t *testing.T) {
	e := setup(t, Options{})
	waitFor(t, "upstream", func() bool { return e.fake.OpenStreams() == 1 })
	var readers []*sseReader
	for range 3 {
		readers = append(readers, openSSE(t, e.srv.URL+"/s/"+e.id+"/sse"))
	}
	time.Sleep(50 * time.Millisecond)
	e.fake.Publish("abc.jpg", []byte("jpeg"))
	want := "/s/" + e.id + "/images/abc.jpg"
	for _, r := range readers {
		for {
			_, data := r.next(t)
			if data == want {
				break
			}
			if !strings.HasPrefix(data, "/s/"+e.id+"/images/") {
				t.Fatalf("unrewritten event data %q", data)
			}
		}
	}
	if n := e.fake.Connects(); n != 1 {
		t.Fatalf("%d upstream connections for 3 browsers, want 1", n)
	}
	// A new client gets the newest frame at once (flushed before any new event).
	r := openSSE(t, e.srv.URL+"/s/"+e.id+"/sse")
	if _, data := r.next(t); data != want {
		t.Fatalf("initial event %q, want %q", data, want)
	}
	resp, body := get(t, e.srv.URL+want)
	if resp.StatusCode != 200 || body != "jpeg" || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("image: %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, e.srv.URL+"/s/"+e.id+"/latest.jpg")
	if resp.StatusCode != 200 || body != "jpeg" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("latest: %d %q %q", resp.StatusCode, body, resp.Header.Get("Cache-Control"))
	}
}

func TestImageValidationAndSSRF(t *testing.T) {
	e := setup(t, Options{})
	evilHost, evilPort := e.evil.HostPort()
	evilAddr := net.JoinHostPort(evilHost, strconv.Itoa(evilPort))
	cases := map[string]int{
		"/s/" + e.id + "/images/..%2f..%2fetc%2fpasswd":         http.StatusBadRequest,
		"/s/" + e.id + "/images/%2e%2e":                         http.StatusBadRequest,
		"/s/" + e.id + "/images/x.png":                          http.StatusBadRequest,
		"/s/" + e.id + "/images/missing.jpg":                    http.StatusNotFound,
		"/s/" + e.id + "/images/..%5c..%5cwin.jpg":              http.StatusBadRequest,
		"/s/unknown-123456/images/x.jpg":                        http.StatusNotFound,
		"/s/unknown-123456/latest.jpg":                          http.StatusNotFound,
		"/s/unknown-123456/sse":                                 http.StatusNotFound,
		"/s/" + evilAddr + "/images/x.jpg":                      http.StatusNotFound,
		"/s/http:%2f%2f" + evilAddr + "/latest.jpg":             http.StatusNotFound,
		"/view/unknown-123456":                                  http.StatusNotFound,
		"/s/" + e.id + "/images/x.jpg?host=" + evilAddr:         http.StatusNotFound,
		"/s/" + e.id + "/images/" + "a%2f..%2f..%2fsse%2fimage": http.StatusBadRequest,
	}
	for path, want := range cases {
		resp, _ := get(t, e.srv.URL+path, "Host", evilAddr, "X-Forwarded-Host", evilAddr)
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	// Path cleaning redirects instead of reaching a handler.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/s/"+e.id+"/images/../../../etc/passwd", nil)
	req.URL.Opaque = "/s/" + e.id + "/images/../../../etc/passwd"
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("traversal path served")
	}
	if n := len(e.evil.Requests()); n != 0 {
		t.Fatalf("the hub made %d requests to a host not in the registry", n)
	}
	for _, p := range e.fake.Requests() {
		if strings.Contains(p, "etc") || strings.Contains(p, "..") {
			t.Fatalf("the hub forwarded %q", p)
		}
	}
	// Writes are not allowed.
	r2, err := http.Post(e.srv.URL+"/api/services", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", r2.StatusCode)
	}
}

func TestLatestWhileWaitingAndOffline(t *testing.T) {
	e := setup(t, Options{})
	waitFor(t, "upstream", func() bool { return e.fake.OpenStreams() == 1 })
	resp, body := get(t, e.srv.URL+"/s/"+e.id+"/latest.jpg")
	if resp.StatusCode != 200 || body != string(hubtest.WaitingJPEG) {
		t.Fatalf("latest before any frame: %d %q", resp.StatusCode, body)
	}
	e.reg.ReportProbe(e.id, false)
	e.reg.ReportProbe(e.id, false)
	resp, _ = get(t, e.srv.URL+"/s/"+e.id+"/latest.jpg")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("latest of an offline service without a frame = %d", resp.StatusCode)
	}
}

func TestAPIServicesEscapesNetworkStrings(t *testing.T) {
	e := setup(t, Options{})
	e.reg.Apply(discovery.Event{Kind: discovery.Found, Key: discovery.Key{Interface: 2, Name: "x"}, Sighting: discovery.Sighting{
		Key: discovery.Key{Interface: 2, Name: "x"}, Name: `<script>alert("x")</script>`, Host: "evil<b>.local",
		Address: "10.9.9.9", Port: 8900, TXT: map[string]string{"ver": "</script><img src=x>", "rtsp": "8554"},
	}})
	resp, body := get(t, e.srv.URL+"/api/services")
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type %q", resp.Header.Get("Content-Type"))
	}
	if strings.ContainsAny(body, "<>") {
		t.Fatalf("unescaped markup in JSON: %s", body)
	}
	var list []serviceJSON
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d services", len(list))
	}
	var bad serviceJSON
	for _, s := range list {
		if s.Source == "mdns" {
			bad = s
		}
	}
	if bad.Name != `<script>alert("x")</script>` || bad.Version != "</script><img src=x>" {
		t.Fatalf("round trip lost data: %+v", bad)
	}
	if bad.RTSPURL != "rtsp://10.9.9.9:8554/" || bad.RTSPURLIP != "" {
		t.Fatalf("an invalid host name must not reach the RTSP URL: %q / %q", bad.RTSPURL, bad.RTSPURLIP)
	}
	if !registryIDSafe(bad.ID) {
		t.Fatalf("unsafe id %q", bad.ID)
	}
	for _, s := range list {
		if s.Source == "static" && (!s.HasRTSP || !s.Online || s.LastSeen == nil) {
			t.Fatalf("static entry %+v", s)
		}
	}

	// The view page escapes the name in HTML.
	resp, page := get(t, e.srv.URL+"/view/"+bad.ID)
	if resp.StatusCode != 200 || strings.Contains(page, "<script>alert") || !strings.Contains(page, "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;") {
		t.Fatalf("view page does not escape the name:\n%s", page)
	}
	if strings.Contains(page, "<img src=x>") {
		t.Fatal("version not escaped")
	}
}

func registryIDSafe(id string) bool { return idPattern.MatchString(id) }

func TestAPIEventsStream(t *testing.T) {
	e := setup(t, Options{})
	r := openSSE(t, e.srv.URL+"/api/events")
	kind, data := r.next(t)
	if kind != "snapshot" {
		t.Fatalf("first event %q", kind)
	}
	var snap snapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil || len(snap.Services) != 1 || snap.Now.IsZero() {
		t.Fatalf("snapshot %q: %v", data, err)
	}
	e.reg.Apply(discovery.Event{Kind: discovery.Found, Key: discovery.Key{Interface: 2, Name: "bob"}, Sighting: discovery.Sighting{
		Key: discovery.Key{Interface: 2, Name: "bob"}, Name: "bob", Host: "bob.local", Address: "127.0.0.1", Port: 1,
	}})
	if kind, data := r.next(t, "frame"); kind != "added" || !strings.Contains(data, `"name":"bob"`) {
		t.Fatalf("got %s %s", kind, data)
	}
	e.fake.Publish("p.jpg", []byte("x"))
	for {
		kind, data := r.next(t)
		if kind == "frame" && strings.Contains(data, `"path":"/s/`+e.id+`/images/p.jpg"`) {
			break
		}
	}
	e.reg.ReportProbe(e.id, false)
	e.reg.ReportProbe(e.id, false)
	if kind, data := r.next(t, "frame", "updated"); kind != "offline" || !strings.Contains(data, `"online":false`) {
		t.Fatalf("got %s %s, want offline", kind, data)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := setup(t, Options{HSTS: true})
	for _, p := range []string{"/", "/wall?s=a,b", "/api/services", "/static/hub.js", "/healthz", "/nope"} {
		resp, _ := get(t, e.srv.URL+p)
		h := resp.Header
		if h.Get("Content-Security-Policy") != ContentSecurityPolicy || !strings.Contains(ContentSecurityPolicy, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", p, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: headers %v", p, h)
		}
		if h.Get("Strict-Transport-Security") == "" {
			t.Errorf("%s: no HSTS", p)
		}
	}
	e2 := setup(t, Options{})
	resp, _ := get(t, e2.srv.URL+"/")
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS sent without a trusted certificate")
	}
	resp, _ = get(t, e2.srv.URL+"/static/index.html")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("templates must not be served raw: %d", resp.StatusCode)
	}
}

func TestBasicAuth(t *testing.T) {
	e := setup(t, Options{User: "viewer", Password: "s3cret"})
	for _, p := range []string{"/", "/api/services", "/api/events", "/s/" + e.id + "/latest.jpg", "/static/hub.css", "/healthz"} {
		resp, _ := get(t, e.srv.URL+p)
		if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("%s without credentials = %d", p, resp.StatusCode)
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: 401 without security headers", p)
		}
	}
	for _, creds := range [][2]string{{"viewer", "wrong"}, {"other", "s3cret"}, {"", ""}, {"viewer", "s3cret "}} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/", nil)
		req.SetBasicAuth(creds[0], creds[1])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%v accepted", creds)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/services", nil)
	req.SetBasicAuth("viewer", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid credentials = %d", resp.StatusCode)
	}
}

func TestPagesRender(t *testing.T) {
	e := setup(t, Options{})
	resp, body := get(t, e.srv.URL+"/")
	if resp.StatusCode != 200 || !strings.Contains(body, `data-page="index"`) || !strings.Contains(body, `<script src="/static/hub.js" defer></script>`) {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, " style=") || strings.Contains(body, "onclick") {
		t.Fatal("inline script or style would violate the CSP")
	}
	_, body = get(t, e.srv.URL+"/wall?s=b-1,"+e.id+",BAD!,%3Cx%3E,b-1")
	if !strings.Contains(body, `data-ids="b-1,`+e.id+`"`) {
		t.Fatalf("wall ids not sanitised:\n%s", body)
	}
	resp, body = get(t, e.srv.URL+"/view/"+e.id)
	if resp.StatusCode != 200 || !strings.Contains(body, "<h1 id=\"v-name\">alice</h1>") {
		t.Fatalf("view: %d", resp.StatusCode)
	}
	for _, f := range []string{"hub.js", "hub.css", "favicon.svg"} {
		if resp, _ := get(t, e.srv.URL+"/static/"+f); resp.StatusCode != 200 {
			t.Errorf("/static/%s = %d", f, resp.StatusCode)
		}
	}
}

func TestWallIDs(t *testing.T) {
	ids := wallIDs(strings.Repeat("a-1,", 3) + "b-2, c-3 ,../x,<y>," + strings.Repeat("z", 65))
	if strings.Join(ids, ",") != "a-1,b-2,c-3" {
		t.Fatalf("wallIDs = %v", ids)
	}
	var many []string
	for i := range 50 {
		many = append(many, "s-"+strconv.Itoa(i))
	}
	if n := len(wallIDs(strings.Join(many, ","))); n != maxWall {
		t.Fatalf("wall capped at %d, want %d", n, maxWall)
	}
}
