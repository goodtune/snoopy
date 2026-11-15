package acmecert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCA is a minimal RFC 8555 server: it ignores JWS signatures, validates
// http-01 by fetching the response from challengeSrv, and signs CSRs with a
// throwaway CA for lifetime.
type fakeCA struct {
	t            *testing.T
	srv          *httptest.Server
	challengeSrv string // base URL standing in for http://<name>:80
	lifetime     time.Duration
	caKey        *ecdsa.PrivateKey
	caCert       *x509.Certificate

	mu        sync.Mutex
	accounts  int
	validated map[string]bool // domain -> authz valid
	failed    map[string]bool // domain -> authz invalid
	domains   []string
	certDER   []byte
	orders    int
	offerHTTP bool
}

func newFakeCA(t *testing.T, lifetime time.Duration) *fakeCA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake ACME Root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	f := &fakeCA{t: t, lifetime: lifetime, caKey: key, caCert: cert, validated: map[string]bool{}, failed: map[string]bool{}, offerHTTP: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCA) url(p string) string { return f.srv.URL + p }

func (f *fakeCA) payload(r *http.Request) []byte {
	var jws struct{ Payload string }
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &jws); err != nil {
		f.t.Errorf("bad JWS: %v", err)
	}
	p, _ := base64.RawURLEncoding.DecodeString(jws.Payload)
	return p
}

func (f *fakeCA) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", fmt.Sprint(time.Now().UnixNano()))
	writeJSON := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/directory":
		writeJSON(200, map[string]string{"newNonce": f.url("/nonce"), "newAccount": f.url("/account"), "newOrder": f.url("/order")})
	case r.URL.Path == "/nonce":
		w.WriteHeader(200)
	case r.URL.Path == "/account":
		f.payload(r)
		w.Header().Set("Location", f.url("/acct/1"))
		code := http.StatusCreated
		if f.accounts > 0 {
			code = http.StatusOK
		}
		f.accounts++
		writeJSON(code, map[string]string{"status": "valid"})
	case r.URL.Path == "/order":
		var req struct{ Identifiers []struct{ Value string } }
		_ = json.Unmarshal(f.payload(r), &req)
		f.domains = nil
		for _, id := range req.Identifiers {
			f.domains = append(f.domains, id.Value)
		}
		f.certDER = nil
		f.validated, f.failed = map[string]bool{}, map[string]bool{}
		f.orders++
		w.Header().Set("Location", f.url("/order/1"))
		writeJSON(http.StatusCreated, f.order())
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		f.payload(r)
		d := strings.TrimPrefix(r.URL.Path, "/authz/")
		writeJSON(200, f.authz(d))
	case strings.HasPrefix(r.URL.Path, "/chal/"):
		f.payload(r)
		d := strings.TrimPrefix(r.URL.Path, "/chal/")
		// Validate like a CA would: GET the token from the "port 80" server.
		f.mu.Unlock()
		resp, err := http.Get(f.challengeSrv + "/.well-known/acme-challenge/tok-" + d)
		f.mu.Lock()
		if err != nil {
			f.t.Logf("fake CA: http-01 for %s: %v", d, err)
		}
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			f.validated[d] = resp.StatusCode == 200 && strings.HasPrefix(string(b), "tok-"+d+".")
			f.failed[d] = !f.validated[d]
			f.t.Logf("fake CA: http-01 for %s: %d %q", d, resp.StatusCode, b)
		}
		writeJSON(200, map[string]string{"type": "http-01", "url": f.url(r.URL.Path), "token": "tok-" + d, "status": "valid"})
	case r.URL.Path == "/order/1":
		f.payload(r)
		writeJSON(200, f.order())
	case r.URL.Path == "/finalize":
		var req struct{ CSR string }
		_ = json.Unmarshal(f.payload(r), &req)
		der, _ := base64.RawURLEncoding.DecodeString(req.CSR)
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil {
			writeJSON(400, map[string]string{"type": "urn:ietf:params:acme:error:badCSR", "detail": err.Error()})
			return
		}
		now := time.Now()
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(now.UnixNano()), Subject: csr.Subject, DNSNames: csr.DNSNames,
			NotBefore: now.Truncate(time.Second), NotAfter: now.Truncate(time.Second).Add(f.lifetime),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		f.certDER, _ = x509.CreateCertificate(rand.Reader, tmpl, f.caCert, csr.PublicKey, f.caKey)
		writeJSON(200, f.order())
	case r.URL.Path == "/cert/1":
		f.payload(r)
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.certDER})
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.caCert.Raw})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCA) authz(d string) map[string]any {
	status := "pending"
	if f.validated[d] {
		status = "valid"
	} else if f.failed[d] {
		status = "invalid"
	}
	chals := []map[string]string{{"type": "tls-alpn-01", "url": f.url("/chal-alpn/" + d), "token": "x", "status": "pending"}}
	if f.offerHTTP {
		chals = append(chals, map[string]string{"type": "http-01", "url": f.url("/chal/" + d), "token": "tok-" + d, "status": "pending"})
	}
	return map[string]any{"identifier": map[string]string{"type": "dns", "value": d}, "status": status, "challenges": chals}
}

func (f *fakeCA) order() map[string]any {
	status := "ready"
	for _, d := range f.domains {
		if !f.validated[d] {
			status = "pending"
		}
	}
	var ids []map[string]string
	for _, d := range f.domains {
		ids = append(ids, map[string]string{"type": "dns", "value": d})
	}
	o := map[string]any{"identifiers": ids, "finalize": f.url("/finalize")}
	var authzs []string
	for _, d := range f.domains {
		authzs = append(authzs, f.url("/authz/"+d))
	}
	o["authorizations"] = authzs
	if f.certDER != nil {
		status = "valid"
		o["certificate"] = f.url("/cert/1")
	}
	o["status"] = status
	return o
}

func logf(t *testing.T) func(string, ...any) {
	return func(f string, a ...any) { t.Logf(f, a...) }
}

// setup returns an issuer whose challenge handler is what the fake CA
// fetches tokens from.
func setup(t *testing.T, lifetime time.Duration, domains ...string) (*fakeCA, *Issuer, string) {
	ca := newFakeCA(t, lifetime)
	dir := t.TempDir()
	iss, err := New(Config{DirectoryURL: ca.url("/directory"), Domains: domains, Dir: dir, Logf: logf(t), Email: "ops@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fallback", http.StatusTeapot) })
	chal := httptest.NewServer(iss.HTTPHandler(fallback))
	t.Cleanup(chal.Close)
	ca.challengeSrv = chal.URL
	return ca, iss, dir
}

func TestObtainStoresAndReusesCertificate(t *testing.T) {
	ca, iss, dir := setup(t, 7*24*time.Hour, "hub.example.net", "alt.example.net")
	if iss.Certificate() != nil {
		t.Fatal("certificate before Obtain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := iss.Obtain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !Covers(c.Leaf, []string{"hub.example.net", "alt.example.net"}, time.Now()) || len(c.Certificate) != 2 {
		t.Fatalf("certificate %v (chain %d)", c.Leaf.DNSNames, len(c.Certificate))
	}
	if iss.Certificate() != c {
		t.Fatal("Obtain did not make the certificate current")
	}
	if fi, err := os.Stat(filepath.Join(dir, KeyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}

	// A new Issuer on the same directory reuses the account and certificate.
	again, err := New(Config{DirectoryURL: ca.url("/directory"), Domains: []string{"hub.example.net", "alt.example.net"}, Dir: dir, Logf: logf(t)})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Certificate(); got == nil || !got.Leaf.Equal(c.Leaf) {
		t.Fatal("cached certificate not reused")
	}
	// ...but not for a different set of names.
	other, _ := New(Config{DirectoryURL: ca.url("/directory"), Domains: []string{"new.example.net"}, Dir: dir, Logf: logf(t)})
	if other.Certificate() != nil {
		t.Fatal("cached certificate reused for other names")
	}
	// A second issuance registers again (account exists: 200) and works.
	if _, err := iss.Obtain(ctx); err != nil {
		t.Fatal(err)
	}
	if ca.accounts != 2 || ca.orders != 2 {
		t.Fatalf("accounts %d orders %d", ca.accounts, ca.orders)
	}
}

func TestObtainFailsWhenValidationFails(t *testing.T) {
	ca, iss, _ := setup(t, time.Hour, "hub.example.net")
	ca.challengeSrv = ca.url("/nowhere") // the CA cannot reach our port 80
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := iss.Obtain(ctx); err == nil || !strings.Contains(err.Error(), "port 80") || ctx.Err() != nil {
		t.Fatalf("err = %v (ctx %v)", err, ctx.Err())
	}
	if iss.Certificate() != nil {
		t.Fatal("certificate after failure")
	}
}

func TestObtainFailsWithoutHTTP01(t *testing.T) {
	ca, iss, _ := setup(t, time.Hour, "hub.example.net")
	ca.offerHTTP = false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := iss.Obtain(ctx); err == nil || !strings.Contains(err.Error(), "http-01") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunIssuesThenRenews(t *testing.T) {
	// 3s certificates renew after 2s.
	_, iss, _ := setup(t, 3*time.Second, "hub.example.net")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := make(chan *tls.Certificate, 4)
	go iss.Run(ctx, func(c *tls.Certificate) { got <- c })
	first := <-got
	second := <-got
	if first.Leaf.Equal(second.Leaf) || !second.Leaf.NotAfter.After(first.Leaf.NotAfter) {
		t.Fatal("not renewed")
	}
}

func TestHTTPHandler(t *testing.T) {
	iss := &Issuer{}
	iss.tokens.Store("abc", "abc.thumb")
	h := iss.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for _, tc := range []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/.well-known/acme-challenge/abc", 200, "abc.thumb"},
		{"GET", "/.well-known/acme-challenge/nope", 404, ""},
		{"POST", "/.well-known/acme-challenge/abc", 404, ""},
		{"GET", "/", http.StatusTeapot, ""},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.code || (tc.body != "" && rec.Body.String() != tc.body) {
			t.Errorf("%s %s = %d %q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestRenewAtAndCovers(t *testing.T) {
	nb := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	leaf := &x509.Certificate{NotBefore: nb, NotAfter: nb.Add(90 * 24 * time.Hour), DNSNames: []string{"a.example.net"}}
	if got := RenewAt(leaf); !got.Equal(nb.Add(60 * 24 * time.Hour)) {
		t.Fatalf("RenewAt = %v", got)
	}
	if !Covers(leaf, []string{"a.example.net"}, nb.Add(time.Hour)) {
		t.Fatal("should cover")
	}
	if Covers(leaf, []string{"b.example.net"}, nb.Add(time.Hour)) || Covers(leaf, []string{"a.example.net"}, nb.Add(91*24*time.Hour)) || Covers(nil, nil, nb) {
		t.Fatal("should not cover")
	}
}
