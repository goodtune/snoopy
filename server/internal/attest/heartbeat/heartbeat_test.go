package heartbeat

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"snoopy/internal/attest/pki"
)

// kproxy is a minimal KProxy presence endpoint as specified in
// docs/attestation.md: TLS 1.3, any client certificate requested and then
// checked against a registered SPKI pin.
type kproxy struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []Payload
	peers   []string
	status  func(n int) int // status for the n-th request (0-based)
	conns   atomic.Int32
	caFile  string
	srvPin  string
	devPin  string
	reqs    chan Payload
	allowed atomic.Bool
}

func newKProxy(t *testing.T, devPin string) *kproxy {
	t.Helper()
	k := &kproxy{devPin: devPin, reqs: make(chan Payload, 100), status: func(int) int { return http.StatusNoContent }}
	k.allowed.Store(true)
	k.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no tls", http.StatusForbidden)
			return
		}
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		k.mu.Lock()
		n := len(k.got)
		k.got = append(k.got, p)
		k.peers = append(k.peers, r.RemoteAddr)
		st := k.status(n)
		k.mu.Unlock()
		if !k.allowed.Load() {
			st = http.StatusForbidden
		}
		k.reqs <- p
		w.WriteHeader(st)
	}))
	k.srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			c, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if pki.Pin(c) != k.devPin {
				return errors.New("unknown device pin")
			}
			return nil
		},
	}
	k.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			k.conns.Add(1)
		}
	}
	k.srv.StartTLS()
	t.Cleanup(k.srv.Close)
	cert := k.srv.Certificate()
	k.caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(k.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	k.srvPin = pki.Pin(cert)
	return k
}

func (k *kproxy) wait(t *testing.T, d time.Duration) (Payload, bool) {
	t.Helper()
	select {
	case p := <-k.reqs:
		return p, true
	case <-time.After(d):
		return Payload{}, false
	}
}

func device(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := pki.Generate(dir, "desk1", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	kp, err := pki.LoadKeyPair(dir)
	if err != nil {
		t.Fatal(err)
	}
	return kp, pki.Pin(c)
}

type results struct {
	mu  sync.Mutex
	all []Result
}

func (r *results) add(x Result) { r.mu.Lock(); r.all = append(r.all, x); r.mu.Unlock() }
func (r *results) list() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Result(nil), r.all...)
}

func startSender(t *testing.T, k *kproxy, cert tls.Certificate, interval time.Duration, res *results) *Sender {
	t.Helper()
	cfg, err := TLSConfig(cert, k.caFile, k.srvPin)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		URL:             k.srv.URL + "/v1/presence",
		Client:          NewHTTPClient(cfg, 2*time.Second),
		DeviceID:        "desk1",
		BootID:          "boot-1",
		AttestorVersion: "0.2.0",
		FirstSeq:        1000,
		Interval:        interval,
		RetryBase:       10 * time.Millisecond,
		RetryMax:        40 * time.Millisecond,
		Logf:            t.Logf,
		OnResult:        res.add,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

func TestSenderDeliversAndReusesConnection(t *testing.T) {
	cert, pin := device(t)
	k := newKProxy(t, pin)
	var res results
	s := startSender(t, k, cert, 30*time.Millisecond, &res)
	s.Update(Report{State: "recording", User: "kid1", UID: 1000, Evidence: Evidence{RecorderPID: 7, Segment: "screen-1.webm", FrameWH: []int{2560, 1440}}})

	var last uint64
	for i := range 4 {
		p, ok := k.wait(t, 2*time.Second)
		if !ok {
			t.Fatalf("heartbeat %d not received", i)
		}
		if p.V != 1 || p.DeviceID != "desk1" || p.State != "recording" || p.User != "kid1" || p.UID != 1000 ||
			p.BootID != "boot-1" || p.AttestorVersion != "0.2.0" || p.Reasons == nil || len(p.Reasons) != 0 ||
			p.Evidence.RecorderPID != 7 || p.Evidence.Segment != "screen-1.webm" || len(p.Evidence.FrameWH) != 2 {
			t.Fatalf("unexpected payload %+v", p)
		}
		if _, err := time.Parse(time.RFC3339, p.TS); err != nil || !strings.HasSuffix(p.TS, "Z") {
			t.Fatalf("ts %q: %v", p.TS, err)
		}
		if p.Seq <= last || p.Seq < 1000 {
			t.Fatalf("seq %d after %d", p.Seq, last)
		}
		last = p.Seq
	}
	if n := k.conns.Load(); n != 1 {
		t.Errorf("connections = %d, want 1 (keep-alive reuse)", n)
	}
	for _, r := range res.list() {
		if !r.OK || r.Status != http.StatusNoContent {
			t.Errorf("result %+v", r)
		}
	}
}

func TestSenderImmediateOnStateChange(t *testing.T) {
	cert, pin := device(t)
	k := newKProxy(t, pin)
	var res results
	s := startSender(t, k, cert, time.Hour, &res)
	s.Update(Report{State: "recording"})
	if p, ok := k.wait(t, 2*time.Second); !ok || p.State != "recording" {
		t.Fatalf("first heartbeat: %+v %v", p, ok)
	}
	s.Update(Report{State: "recording"}) // unchanged: no immediate send
	if p, ok := k.wait(t, 100*time.Millisecond); ok {
		t.Fatalf("unexpected heartbeat for unchanged state: %+v", p)
	}
	s.Update(Report{State: "absent", Reasons: []string{"recorder_not_running"}})
	p, ok := k.wait(t, 2*time.Second)
	if !ok || p.State != "absent" || len(p.Reasons) != 1 || p.Reasons[0] != "recorder_not_running" {
		t.Fatalf("state change heartbeat: %+v %v", p, ok)
	}
}

func TestSenderRejectedDoesNotHammer(t *testing.T) {
	cert, pin := device(t)
	k := newKProxy(t, pin)
	k.allowed.Store(false)
	var res results
	s := startSender(t, k, cert, 150*time.Millisecond, &res)
	s.Update(Report{State: "recording"})
	if _, ok := k.wait(t, 2*time.Second); !ok {
		t.Fatal("no request")
	}
	// No fast retry: the next attempt only comes with the regular interval.
	if _, ok := k.wait(t, 80*time.Millisecond); ok {
		t.Fatal("403 was retried before the heartbeat interval")
	}
	if _, ok := k.wait(t, 2*time.Second); !ok {
		t.Fatal("no retry at the heartbeat interval after 403")
	}
	r := res.list()
	if len(r) == 0 || r[0].OK || r[0].Status != http.StatusForbidden {
		t.Fatalf("results %+v", r)
	}
	k.allowed.Store(true)
	if _, ok := k.wait(t, 2*time.Second); !ok {
		t.Fatal("no request after re-registration")
	}
	time.Sleep(20 * time.Millisecond)
	r = res.list()
	if !r[len(r)-1].OK {
		t.Fatalf("last result %+v", r[len(r)-1])
	}
}

func TestSenderTransientBackoff(t *testing.T) {
	cert, pin := device(t)
	k := newKProxy(t, pin)
	k.status = func(n int) int {
		if n < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}
	var res results
	s := startSender(t, k, cert, time.Hour, &res)
	start := time.Now()
	s.Update(Report{State: "recording"})
	var seqs []uint64
	for i := range 4 {
		p, ok := k.wait(t, 2*time.Second)
		if !ok {
			t.Fatalf("attempt %d not received", i)
		}
		seqs = append(seqs, p.Seq)
	}
	// Three retries with backoff 10, 20, 40ms (jittered to [d/2, d]) take at
	// least 35ms; with an hour-long interval they can only come from backoff.
	if el := time.Since(start); el < 35*time.Millisecond {
		t.Errorf("retries too fast: %v", el)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Errorf("seqs %v not increasing by one per request", seqs)
		}
	}
	if _, ok := k.wait(t, 100*time.Millisecond); ok {
		t.Error("kept retrying after success")
	}
}

func TestBackoffCap(t *testing.T) {
	s := New(Config{RetryBase: 500 * time.Millisecond, RetryMax: 5 * time.Second, Interval: time.Second})
	for a := range 20 {
		d := s.backoff(a)
		if d > 5*time.Second || d < 250*time.Millisecond {
			t.Fatalf("backoff(%d) = %v", a, d)
		}
	}
	if d := s.backoff(10); d < 2500*time.Millisecond {
		t.Errorf("backoff(10) = %v, want >= cap/2", d)
	}
}

func TestTLSConfigServerPinMismatch(t *testing.T) {
	cert, pin := device(t)
	k := newKProxy(t, pin)
	cfg, err := TLSConfig(cert, k.caFile, pin) // the device pin is not the server's
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewHTTPClient(cfg, 2*time.Second).Post(k.srv.URL, "application/json", strings.NewReader("{}"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("connected despite server pin mismatch")
	}
	if !strings.Contains(err.Error(), "SERVER_PIN") {
		t.Errorf("err = %v", err)
	}
	if _, err := TLSConfig(cert, k.caFile, "sha256:bogus"); err == nil {
		t.Error("invalid pin accepted")
	}
	if _, err := TLSConfig(cert, filepath.Join(t.TempDir(), "missing.pem"), ""); err == nil {
		t.Error("missing CA file accepted")
	}
}

func TestServerRejectsUnknownDevice(t *testing.T) {
	cert, _ := device(t)
	_, otherPin := device(t)
	k := newKProxy(t, otherPin)
	cfg, err := TLSConfig(cert, k.caFile, k.srvPin)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewHTTPClient(cfg, 2*time.Second).Post(k.srv.URL, "application/json", strings.NewReader("{}"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("server accepted an unregistered device certificate")
	}
}

func TestSeqBase(t *testing.T) {
	a := SeqBase(10 * time.Second)
	b := SeqBase(10*time.Second + 20*time.Microsecond)
	if b <= a || a == 0 || SeqBase(-1) != 1 {
		t.Fatalf("SeqBase: %d %d", a, b)
	}
}
