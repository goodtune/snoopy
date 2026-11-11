package attest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"snoopy/internal/attest/heartbeat"
	"snoopy/internal/attest/logind"
	"snoopy/internal/attest/pki"
	"snoopy/internal/attest/procfs"
)

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logBuf) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

// readState returns the current state.json, or a zero StateFile if it has
// not been written yet.
func readState(t *testing.T, e *env) StateFile {
	t.Helper()
	var sf StateFile
	data, err := os.ReadFile(filepath.Join(e.cfg.RuntimeDir, StateFileName))
	if errors.Is(err, os.ErrNotExist) {
		return sf
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	return sf
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

func (e *env) deps(l *logBuf) Deps {
	return Deps{
		Logind:     e.lg,
		Proc:       procfs.FS{Root: e.tr.Root},
		TrustedUID: uint32(os.Getuid()), //nolint:gosec // test uid fits
		Now:        time.Now,
		Logf:       l.logf,
	}
}

func TestServiceWritesStateWithoutURL(t *testing.T) {
	e := newEnv(t)
	e.now = time.Now()
	e.touch(e.segment, 0)
	e.install()
	e.cfg.CheckInterval = 20 * time.Millisecond
	var l logBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, e.cfg, "0.2.0", e.deps(&l)) }()

	waitFor(t, "state.json", func() bool {
		_, err := os.Stat(filepath.Join(e.cfg.RuntimeDir, StateFileName))
		return err == nil
	})
	sf := readState(t, e)
	if sf.State != StateRecording || sf.DeviceID != "desk1" || sf.AttestorVersion != "0.2.0" || sf.Heartbeat != nil ||
		len(sf.Checks) != 4 || sf.Evidence.RecorderPID != recorderPID || sf.Session == nil || sf.Session.ID != "2" {
		t.Fatalf("state file %+v", sf)
	}
	fi, _ := os.Stat(filepath.Join(e.cfg.RuntimeDir, StateFileName))
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("state.json mode %v", fi.Mode())
	}

	// Lock the session; a logind event triggers an immediate re-evaluation
	// (the check interval alone would also catch it).
	e.lg.set(func(f *fakeLogind) { f.active.LockedHint = true })
	e.lg.events <- logind.EventChanged
	waitFor(t, "locked state", func() bool { return readState(t, e).State == StateLocked })
	if !l.contains("recording -> locked") {
		t.Errorf("state change not logged: %q", l.lines)
	}

	// Losing logind ends the loop with an error (systemd restarts us).
	close(e.lg.events)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "logind") {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after logind loss")
	}
	cancel()
}

func TestServiceResumeOpensGrace(t *testing.T) {
	e := newEnv(t)
	e.now = time.Now()
	e.touch(e.segment, 0)
	e.install()
	e.cfg.CheckInterval = time.Hour
	e.cfg.StartupGrace = 0 // no start-up grace; only resume opens one below
	var l logBuf
	s, err := NewService(e.cfg, "0.2.0", e.deps(&l))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Loop(ctx, e.lg.events) }()
	waitFor(t, "recording", func() bool { return readState(t, e).State == StateRecording })

	s.Eval.Grace = time.Hour // the loop goroutine reads it only on the next event
	e.tr.Remove(recorderPID)
	e.lg.events <- logind.EventResume
	waitFor(t, "degraded after resume", func() bool {
		sf := readState(t, e)
		return sf.State == StateDegraded && sf.GraceEnd != nil
	})
	if !l.contains("resumed") {
		t.Error("resume not logged")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestServiceHeartbeats runs the whole service against a KProxy stand-in:
// mutual TLS 1.3 with the device pin verified server-side, an immediate
// heartbeat on start and on state change, and the result in state.json.
func TestServiceHeartbeats(t *testing.T) {
	e := newEnv(t)
	e.now = time.Now()
	e.touch(e.segment, 0)
	e.install()
	cert, err := pki.Generate(e.cfg.StateDir, e.cfg.DeviceID, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	devPin := pki.Pin(cert)

	got := make(chan heartbeat.Payload, 100)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p heartbeat.Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			c, err := x509.ParseCertificate(raw[0])
			if err != nil || pki.Pin(c) != devPin {
				return errors.New("unknown device")
			}
			return nil
		},
	}
	srv.StartTLS()
	defer srv.Close()
	ca := filepath.Join(e.root, "ca.pem")
	writeFile(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})), 0o644)

	e.cfg.URL = srv.URL + "/v1/presence"
	e.cfg.CAFile = ca
	e.cfg.ServerPin = pki.Pin(srv.Certificate())
	e.cfg.CheckInterval = 20 * time.Millisecond
	e.cfg.HeartbeatInterval = time.Hour

	var l logBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, e.cfg, "0.2.0", e.deps(&l)) }()

	next := func() heartbeat.Payload {
		t.Helper()
		select {
		case p := <-got:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no heartbeat")
		}
		return heartbeat.Payload{}
	}
	p1 := next()
	if p1.State != "recording" || p1.DeviceID != "desk1" || p1.User != "kid1" || p1.UID != kidUID ||
		p1.BootID != "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" || p1.Evidence.ScreencastPID != gjsPID {
		t.Fatalf("first heartbeat %+v", p1)
	}
	// SeqBase(12345.67s uptime) from the fake /proc.
	if p1.Seq != heartbeat.SeqBase(12345670*time.Millisecond) {
		t.Errorf("seq %d", p1.Seq)
	}

	// Kill the recorder: the new state (degraded, as we are still within the
	// start-up grace) is sent immediately although the heartbeat interval is
	// an hour.
	e.tr.Remove(recorderPID)
	var p2 heartbeat.Payload
	for p2 = next(); p2.State == "recording"; p2 = next() {
	}
	if p2.Seq <= p1.Seq || p2.State != "degraded" || !strings.Contains(strings.Join(p2.Reasons, ","), ReasonRecorderNotRunning) {
		t.Fatalf("second heartbeat %+v", p2)
	}
	waitFor(t, "heartbeat result in state.json", func() bool {
		sf := readState(t, e)
		return sf.Heartbeat != nil && sf.Heartbeat.OK && sf.Heartbeat.Seq >= p2.Seq
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNewServiceRequiresKey(t *testing.T) {
	e := newEnv(t)
	e.cfg.URL = "https://kproxy.invalid/v1/presence"
	var l logBuf
	if _, err := NewService(e.cfg, "x", e.deps(&l)); err == nil || !strings.Contains(err.Error(), "keygen") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteText(t *testing.T) {
	e := newEnv(t)
	e.recorder.Environ = append(e.recorder.Environ, "LD_PRELOAD=/tmp/x.so")
	e.install()
	var l logBuf
	e.cfg.StartupGrace = 0
	s, err := NewService(e.cfg, "x", e.deps(&l))
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return e.now }
	s.Checker.Now = s.Now
	ev := s.Evaluate(context.Background())
	var b bytes.Buffer
	if err := WriteText(&b, ev, "desk1"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"state:   absent", "reasons: ld_preload", "user:    kid1 (uid 1000)", "device:  desk1",
		"[PASS] session", "[FAIL] recorder", "ld_preload: pid 100 has LD_PRELOAD", "[PASS] screencast", "[SKIP] frame",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	if err := WriteJSON(&b, ev); err != nil {
		t.Fatal(err)
	}
	var back Evaluation
	if err := json.Unmarshal(b.Bytes(), &back); err != nil || back.State != StateAbsent {
		t.Fatalf("json: %v %+v", err, back)
	}
}
