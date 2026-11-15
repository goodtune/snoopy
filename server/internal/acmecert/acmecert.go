// Package acmecert obtains and renews one TLS certificate for a fixed set
// of DNS names from an ACME server (RFC 8555), such as the ACME endpoint of
// a HashiCorp Vault PKI role, answering http-01 challenges only.
//
// http-01 is the one challenge type that does not need the TLS port to be
// 443: the CA always validates on port 80 of each name, which the caller
// routes to HTTPHandler. The certificate, its key and the ACME account key
// are kept in Dir, so a restart reuses them instead of issuing again.
package acmecert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// Files in Config.Dir.
const (
	AccountKeyFile = "account.key"
	CertFile       = "cert.pem"
	KeyFile        = "key.pem"
)

// challengePrefix is the URL path prefix of http-01 challenge responses.
const challengePrefix = "/.well-known/acme-challenge/"

// Retry bounds after a failed issuance.
const (
	minRetry = time.Minute
	maxRetry = time.Hour
)

// Config describes the certificate to keep.
type Config struct {
	// DirectoryURL is the ACME directory, e.g.
	// https://vault.example.net:8200/v1/pki/roles/web/acme/directory.
	DirectoryURL string
	// Domains are the DNS names of the certificate; the first is its
	// common name.
	Domains []string
	// Email, if set, is registered as the account contact.
	Email string
	// Dir holds the account key, certificate and key (created 0700).
	Dir string
	// Logf logs one line (required).
	Logf func(string, ...any)
	// HTTPClient talks to the ACME server; nil means http.DefaultClient
	// (the system trust store must then trust the server's certificate).
	HTTPClient *http.Client
}

// Issuer keeps the certificate. Its methods are safe for concurrent use.
type Issuer struct {
	cfg    Config
	client *acme.Client
	tokens sync.Map // http-01 token -> key authorization

	mu   sync.RWMutex
	cert *tls.Certificate
}

// New loads (or creates) the account key and loads a cached certificate
// that is unexpired and covers every domain. It does not contact the ACME
// server: call Run for that.
func New(cfg Config) (*Issuer, error) {
	if cfg.DirectoryURL == "" || len(cfg.Domains) == 0 || cfg.Dir == "" || cfg.Logf == nil {
		return nil, errors.New("acmecert: DirectoryURL, Domains, Dir and Logf are required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(cfg.Dir, AccountKeyFile))
	if err != nil {
		return nil, fmt.Errorf("ACME account key: %w", err)
	}
	i := &Issuer{
		cfg:    cfg,
		client: &acme.Client{Key: key, DirectoryURL: cfg.DirectoryURL, HTTPClient: cfg.HTTPClient, UserAgent: "snoopy-acmecert"},
	}
	if c, err := tls.LoadX509KeyPair(filepath.Join(cfg.Dir, CertFile), filepath.Join(cfg.Dir, KeyFile)); err == nil && Covers(c.Leaf, cfg.Domains, time.Now()) {
		i.cert = &c
	}
	return i, nil
}

// Certificate is the current certificate, or nil before the first one.
func (i *Issuer) Certificate() *tls.Certificate {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.cert
}

// Covers reports whether leaf is valid at now for every name in domains.
func Covers(leaf *x509.Certificate, domains []string, now time.Time) bool {
	if leaf == nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return false
	}
	for _, d := range domains {
		if leaf.VerifyHostname(d) != nil {
			return false
		}
	}
	return true
}

// RenewAt is when a certificate is renewed: two thirds into its validity,
// which suits both 90-day public certificates and short-lived internal ones.
func RenewAt(leaf *x509.Certificate) time.Time {
	return leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
}

// HTTPHandler answers http-01 challenges and passes every other request to
// fallback.
func (i *Issuer) HTTPHandler(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.URL.Path, challengePrefix)
		if !ok {
			fallback.ServeHTTP(w, r)
			return
		}
		v, found := i.tokens.Load(token)
		if !found || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(v.(string)))
	})
}

// Run issues a certificate when there is none (or it is due) and renews it
// at RenewAt until ctx is done, calling onUpdate with each new certificate.
// Failures are logged and retried with backoff (1m doubling to 1h); a
// working certificate keeps being served meanwhile.
func (i *Issuer) Run(ctx context.Context, onUpdate func(*tls.Certificate)) {
	retry := minRetry
	for {
		wait := time.Duration(0)
		if c := i.Certificate(); c != nil {
			wait = time.Until(RenewAt(c.Leaf))
		}
		if wait > 0 {
			i.cfg.Logf("ACME certificate for %s valid until %s, renewing at %s",
				strings.Join(i.cfg.Domains, ", "), i.Certificate().Leaf.NotAfter.Format(time.RFC3339), time.Now().Add(wait).Format(time.RFC3339))
			if !sleep(ctx, wait) {
				return
			}
		}
		octx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		c, err := i.Obtain(octx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			i.cfg.Logf("ACME certificate request to %s failed, retrying in %s: %v", i.cfg.DirectoryURL, retry, err)
			if !sleep(ctx, retry) {
				return
			}
			retry = min(retry*2, maxRetry)
			continue
		}
		retry = minRetry
		i.cfg.Logf("ACME certificate issued for %s by %q, valid until %s",
			strings.Join(i.cfg.Domains, ", "), c.Leaf.Issuer.CommonName, c.Leaf.NotAfter.Format(time.RFC3339))
		if onUpdate != nil {
			onUpdate(c)
		}
	}
}

// Obtain issues a new certificate now, stores it in Dir and makes it
// current.
func (i *Issuer) Obtain(ctx context.Context) (*tls.Certificate, error) {
	acct := &acme.Account{}
	if i.cfg.Email != "" {
		acct.Contact = []string{"mailto:" + i.cfg.Email}
	}
	if _, err := i.client.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("register account: %w", err)
	}
	order, err := i.client.AuthorizeOrder(ctx, acme.DomainIDs(i.cfg.Domains...))
	if err != nil {
		return nil, fmt.Errorf("new order: %w", err)
	}
	for _, u := range order.AuthzURLs {
		if err := i.authorize(ctx, u); err != nil {
			return nil, err
		}
	}
	if order, err = i.client.WaitOrder(ctx, order.URI); err != nil {
		return nil, fmt.Errorf("order: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: i.cfg.Domains[0]},
		DNSNames: i.cfg.Domains,
	}, key)
	if err != nil {
		return nil, err
	}
	chain, _, err := i.client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize order: %w", err)
	}
	var certPEM []byte
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("issued certificate: %w", err)
	}
	if !Covers(c.Leaf, i.cfg.Domains, time.Now()) {
		return nil, fmt.Errorf("issued certificate does not cover %s", strings.Join(i.cfg.Domains, ", "))
	}
	// Key first: a crash in between leaves a pair that fails to load (and
	// is re-issued) rather than a new certificate with the old key.
	if err := writeFileAtomic(filepath.Join(i.cfg.Dir, KeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(i.cfg.Dir, CertFile), certPEM, 0o644); err != nil {
		return nil, err
	}
	i.mu.Lock()
	i.cert = &c
	i.mu.Unlock()
	return &c, nil
}

// authorize completes one authorization with its http-01 challenge.
func (i *Issuer) authorize(ctx context.Context, url string) error {
	z, err := i.client.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	if z.Status == acme.StatusValid {
		return nil
	}
	var chal *acme.Challenge
	for _, c := range z.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return fmt.Errorf("%s: the CA offers no http-01 challenge", z.Identifier.Value)
	}
	resp, err := i.client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return err
	}
	i.tokens.Store(chal.Token, resp)
	defer i.tokens.Delete(chal.Token)
	if _, err := i.client.Accept(ctx, chal); err != nil {
		return fmt.Errorf("%s: accept challenge: %w", z.Identifier.Value, err)
	}
	if _, err := i.client.WaitAuthorization(ctx, z.URI); err != nil {
		return fmt.Errorf("%s: http-01 validation (the CA must reach http://%s/ on port 80): %w", z.Identifier.Value, z.Identifier.Value, err)
	}
	return nil
}

func loadOrCreateKey(path string) (crypto.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: no PEM block", path)
		}
		return x509.ParseECPrivateKey(blk.Bytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
