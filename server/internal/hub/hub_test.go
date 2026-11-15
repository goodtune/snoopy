package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"snoopy/internal/hub/discovery"
	"snoopy/internal/hub/hubtest"
	"snoopy/internal/hub/registry"
)

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestParseConfigDefaultsAndPrecedence(t *testing.T) {
	c, err := ParseConfig(lookup(nil), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "0.0.0.0" || c.HTTPSPort != 443 || c.HTTPPort != 80 || c.PlainHTTP || c.ProbeInterval != 15*time.Second ||
		c.StateDir != DefaultStateDir || c.User != "" || len(c.Static) != 0 {
		t.Fatalf("defaults = %+v", c)
	}
	env := map[string]string{
		"SNOOPY_HUB_ADDR": "127.0.0.1", "SNOOPY_HUB_HTTPS_PORT": "8443", "SNOOPY_HUB_HTTP_PORT": " ",
		"SNOOPY_HUB_PROBE_INTERVAL": "5s", "SNOOPY_HUB_STATIC": "a=10.0.0.1;rtsp=8554",
		"SNOOPY_HUB_USER": "viewer", "SNOOPY_HUB_PASSWORD": "pw",
	}
	c, err = ParseConfig(lookup(env), []string{"-https-port", "9443"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1" || c.HTTPSPort != 9443 || c.HTTPPort != 80 || c.ProbeInterval != 5*time.Second ||
		c.User != "viewer" || c.Password != "pw" || len(c.Static) != 1 || c.Static[0].RTSPPort != 8554 {
		t.Fatalf("env/flags = %+v", c)
	}
	c, err = ParseConfig(lookup(env), []string{"-password", "flagpw"}, io.Discard)
	if err != nil || c.Password != "flagpw" {
		t.Fatalf("flag password must win: %q %v", c.Password, err)
	}
	c, err = ParseConfig(lookup(map[string]string{"SNOOPY_HUB_PLAIN_HTTP": "yes", "SNOOPY_HUB_HTTP_PORT": "8080"}), nil, io.Discard)
	if err != nil || !c.PlainHTTP || c.HTTPPort != 8080 {
		t.Fatalf("plain http: %+v %v", c, err)
	}
	c, err = ParseConfig(lookup(nil), []string{"-version"}, io.Discard)
	if err != nil || !c.ShowVersion {
		t.Fatalf("-version: %v", err)
	}
}

func TestParseConfigErrors(t *testing.T) {
	cases := []struct {
		env  map[string]string
		args []string
	}{
		{map[string]string{"SNOOPY_HUB_HTTPS_PORT": "x"}, nil},
		{map[string]string{"SNOOPY_HUB_PROBE_INTERVAL": "soon"}, nil},
		{map[string]string{"SNOOPY_HUB_PLAIN_HTTP": "maybe"}, nil},
		{nil, []string{"-https-port", "0"}},
		{nil, []string{"-https-port", "70000"}},
		{nil, []string{"-http-port", "-1"}},
		{nil, []string{"-http-port", "443"}},
		{nil, []string{"-tls-cert", "c.pem"}},
		{nil, []string{"-tls-key", "k.pem"}},
		{nil, []string{"-user", "u"}},
		{map[string]string{"SNOOPY_HUB_PASSWORD": "p"}, nil},
		{nil, []string{"-user", "a:b", "-password", "p"}},
		{nil, []string{"-probe-interval", "100ms"}},
		{nil, []string{"-plain-http", "-http-port", "0"}},
		{nil, []string{"-plain-http", "-tls-cert", "c", "-tls-key", "k"}},
		{nil, []string{"-static", "nonsense"}},
		{nil, []string{"-state-dir", ""}},
		{nil, []string{"extra"}},
		{nil, []string{"-nope"}},
	}
	for _, tc := range cases {
		if _, err := ParseConfig(lookup(tc.env), tc.args, io.Discard); err == nil {
			t.Errorf("env %v args %v accepted", tc.env, tc.args)
		}
	}
	_, err := ParseConfig(lookup(nil), []string{"-help"}, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-help: %v", err)
	}
}

func TestHelpNeverShowsPassword(t *testing.T) {
	var out bytes.Buffer
	_, err := ParseConfig(lookup(map[string]string{"SNOOPY_HUB_PASSWORD": "hunter2-secret", "SNOOPY_HUB_USER": "viewer"}), []string{"-help"}, &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hunter2-secret") {
		t.Fatal("password shown in -help")
	}
	if !strings.Contains(out.String(), "SNOOPY_HUB_PASSWORD") || !strings.Contains(out.String(), `(default "viewer")`) {
		t.Fatalf("help lacks env names/defaults:\n%s", out.String())
	}
}

func TestEveryFlagIsInTheExampleConf(t *testing.T) {
	b, err := os.ReadFile("../../hub.conf.example")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, _ = ParseConfig(lookup(nil), []string{"-help"}, &out)
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-") || strings.HasPrefix(line, "-version") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "-"), " ")
		if !strings.Contains(string(b), EnvName(name)) {
			t.Errorf("%s missing from server/hub.conf.example", EnvName(name))
		}
	}
}

func TestRedirectHandler(t *testing.T) {
	cases := []struct {
		port      int
		host, uri string
		want      string
	}{
		{443, "hub.local", "/view/x?y=1", "https://hub.local/view/x?y=1"},
		{443, "hub.local:80", "/", "https://hub.local/"},
		{8443, "10.0.0.2:8080", "/a", "https://10.0.0.2:8443/a"},
		{443, "[fd00::1]:80", "/", "https://[fd00::1]/"},
		{8443, "[fd00::1]", "/", "https://[fd00::1]:8443/"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.uri, nil)
		req.Host = tc.host
		w := httptest.NewRecorder()
		RedirectHandler(tc.port).ServeHTTP(w, req)
		if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != tc.want {
			t.Errorf("%s%s -> %d %q, want %q", tc.host, tc.uri, w.Code, w.Header().Get("Location"), tc.want)
		}
	}
	for _, bad := range []string{"evil.com/x", "a b", "", "x\"y"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = bad
		w := httptest.NewRecorder()
		RedirectHandler(443).ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("Host %q -> %d", bad, w.Code)
		}
	}
}

func TestSelfSignedGenerateAndReuse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Now()
	names := []string{"hub", "hub.local", "localhost"}
	c1, created, err := loadOrCreateSelfSigned(dir, names, nil, now)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	leaf := c1.Leaf
	if leaf.Subject.CommonName != "hub" || strings.Join(leaf.DNSNames, ",") != "hub,hub.local,localhost" ||
		leaf.NotAfter.Sub(now) > 398*24*time.Hour || leaf.IsCA {
		t.Fatalf("certificate %+v", leaf)
	}
	for f, want := range map[string]os.FileMode{selfKeyFile: 0o600, selfCertFile: 0o644} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", f, fi.Mode().Perm(), want)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode %o", fi.Mode().Perm())
	}
	c2, created, err := loadOrCreateSelfSigned(dir, names, nil, now.Add(time.Hour))
	if err != nil || created || !bytes.Equal(c2.Certificate[0], c1.Certificate[0]) {
		t.Fatalf("restart must reuse the certificate (created=%v, %v)", created, err)
	}
	c3, created, err := loadOrCreateSelfSigned(dir, names, nil, now.Add(370*24*time.Hour))
	if err != nil || !created || bytes.Equal(c3.Certificate[0], c1.Certificate[0]) {
		t.Fatalf("a certificate close to expiry must be renewed (created=%v, %v)", created, err)
	}
	if fp := Fingerprint(c1.Certificate[0]); len(fp) != 95 || strings.Count(fp, ":") != 31 {
		t.Fatalf("fingerprint %q", fp)
	}
	// Garbage files are replaced.
	if err := os.WriteFile(filepath.Join(dir, selfCertFile), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, created, err := loadOrCreateSelfSigned(dir, names, nil, now); err != nil || !created {
		t.Fatalf("corrupt files: %v %v", created, err)
	}
}

func writePair(t *testing.T, dir, cn string) (string, string) {
	t.Helper()
	sub := filepath.Join(t.TempDir(), cn)
	c, _, err := loadOrCreateSelfSigned(sub, []string{cn}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = c
	cert, _ := os.ReadFile(filepath.Join(sub, selfCertFile))
	key, _ := os.ReadFile(filepath.Join(sub, selfKeyFile))
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := writeFileAtomic(cp, cert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(kp, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return cp, kp
}

func TestCertReload(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, "first.example")
	var logs syncBuf
	src, err := newFileCertSource(cp, kp, logs.logf)
	if err != nil {
		t.Fatal(err)
	}
	cn := func() string {
		c, _ := src.get(nil)
		l, _ := x509.ParseCertificate(c.Certificate[0])
		return l.Subject.CommonName
	}
	if cn() != "first.example" {
		t.Fatal(cn())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hup := make(chan os.Signal, 1)
	go src.watch(ctx, 10*time.Millisecond, hup)

	// A change of the files is picked up.
	writePair(t, dir, "second.example")
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(cp, future, future)
	waitFor(t, func() bool { return cn() == "second.example" })

	// A broken file keeps the previous certificate.
	if err := os.WriteFile(cp, []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	hup <- os.Interrupt
	waitFor(t, func() bool { return strings.Contains(logs.String(), "reload failed") })
	if cn() != "second.example" {
		t.Fatal("broken file replaced the certificate")
	}
	// SIGHUP reloads a fixed file.
	writePair(t, dir, "third.example")
	hup <- os.Interrupt
	waitFor(t, func() bool { return cn() == "third.example" })

	if _, err := newFileCertSource(filepath.Join(dir, "none.pem"), kp, logs.logf); err == nil {
		t.Fatal("missing certificate accepted")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(&s.b, format+"\n", args...)
}

func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type idleBrowser struct{}

func (idleBrowser) Browse(ctx context.Context, _ func(discovery.Event)) error {
	<-ctx.Done()
	return nil
}

type runResult struct {
	app, redir net.Addr
	logs       *syncBuf
	stop       func() error
}

func startRun(t *testing.T, c Config) runResult {
	t.Helper()
	logs := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan [2]net.Addr, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, c, Options{Logf: logs.logf, Browser: idleBrowser{}, Ready: func(a, r net.Addr) { ready <- [2]net.Addr{a, r} }})
	}()
	select {
	case a := <-ready:
		return runResult{app: a[0], redir: a[1], logs: logs, stop: func() error { cancel(); return <-errc }}
	case err := <-errc:
		cancel()
		t.Fatalf("Run failed: %v\n%s", err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("Run not ready")
	}
	return runResult{}
}

func TestRunPlainHTTPWithStaticEntryAndAuth(t *testing.T) {
	fake := hubtest.NewFake()
	defer fake.Close()
	host, port := fake.HostPort()
	c := Config{
		Addr: "127.0.0.1", PlainHTTP: true, HTTPPort: 0, ProbeInterval: time.Second,
		Static: mustStatic(t, fmt.Sprintf("alice=%s:%d", host, port)), User: "viewer", Password: "pw-never-logged",
	}
	r := startRun(t, c)
	base := "http://" + r.app.String()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("healthz without credentials = %d", resp.StatusCode)
	}
	waitFor(t, func() bool {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/services", nil)
		req.SetBasicAuth("viewer", "pw-never-logged")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return strings.Contains(string(b), `"online":true`)
	})
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS in plain-http mode")
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	logs := r.logs.String()
	if strings.Contains(logs, "pw-never-logged") {
		t.Fatalf("password in logs:\n%s", logs)
	}
	if strings.Contains(logs, "anyone who can reach") {
		t.Fatal("auth warning logged although credentials are set")
	}
	if !strings.Contains(logs, "plain HTTP") || !strings.Contains(logs, `"alice" (`+host+`) is online`) {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestRunTLSSelfSigned(t *testing.T) {
	dir := t.TempDir()
	c := Config{Addr: "127.0.0.1", HTTPSPort: 0, HTTPPort: 0, ProbeInterval: time.Second, StateDir: dir}
	r := startRun(t, c)
	logs := r.logs.String()
	if !strings.Contains(logs, "anyone who can reach the hub can watch every screen") {
		t.Fatal("no warning without credentials")
	}
	if !strings.Contains(logs, "Generated self-signed TLS certificate") || !strings.Contains(logs, "SHA-256 fingerprint") {
		t.Fatalf("logs:\n%s", logs)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11}}} //nolint:gosec // test against a self-signed certificate
	if _, err := client.Get("https://" + r.app.String() + "/healthz"); err == nil {
		t.Fatal("TLS 1.1 accepted")
	}
	pemBytes, _ := os.ReadFile(filepath.Join(dir, selfCertFile))
	block, _ := pem.Decode(pemBytes)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}, ForceAttemptHTTP2: true}}
	resp, err := client.Get("https://" + r.app.String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") != "" || resp.ProtoMajor != 2 {
		t.Fatalf("status %d, HSTS %q, proto %s", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"), resp.Proto)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	// Restart reuses the certificate.
	r = startRun(t, c)
	if !strings.Contains(r.logs.String(), "Using self-signed TLS certificate") {
		t.Fatalf("restart did not reuse the certificate:\n%s", r.logs.String())
	}
	_ = r.stop()
}

func TestRunTLSUserCertificateWithRedirect(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, "hub.example")
	// Find a free port for the redirect listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	c := Config{Addr: "127.0.0.1", HTTPSPort: 0, HTTPPort: httpPort, TLSCert: cp, TLSKey: kp, ProbeInterval: time.Second}
	r := startRun(t, c)
	defer func() { _ = r.stop() }()
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test certificate
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get("http://" + r.redir.String() + "/wall?s=a")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://127.0.0.1:0/wall?s=a" {
		t.Fatalf("redirect %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = client.Get("https://" + r.app.String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatal("no HSTS with a user-supplied certificate")
	}
	if resp.TLS == nil || resp.TLS.PeerCertificates[0].Subject.CommonName != "hub.example" {
		t.Fatal("wrong certificate served")
	}
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	err = Run(context.Background(), Config{Addr: "127.0.0.1", PlainHTTP: true, HTTPPort: port, ProbeInterval: time.Second},
		Options{Logf: func(string, ...any) {}, Browser: idleBrowser{}})
	if err == nil {
		t.Fatal("binding a used port must fail")
	}
}

func mustStatic(t *testing.T, s string) []registry.StaticEntry {
	t.Helper()
	c, err := ParseConfig(lookup(map[string]string{"SNOOPY_HUB_STATIC": s}), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return c.Static
}
