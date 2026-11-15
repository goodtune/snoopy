package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"snoopy/internal/hub/registry"
)

// Self-signed certificate files in the state directory.
const (
	selfCertFile = "cert.pem"
	selfKeyFile  = "key.pem"
	// selfValidity stays under the 398 days browsers accept for TLS
	// server certificates.
	selfValidity = 397 * 24 * time.Hour
	// selfRenewBefore regenerates a certificate this close to expiry.
	selfRenewBefore = 30 * 24 * time.Hour
)

// certSource serves the current certificate to the TLS stack and reloads
// user-supplied files when they change.
type certSource struct {
	certFile, keyFile string
	logf              func(string, ...any)

	mu      sync.RWMutex
	cert    *tls.Certificate
	modTime time.Time
}

func (c *certSource) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cert, nil
}

// fileModTime is the newest modification time of the certificate and key.
func (c *certSource) fileModTime() time.Time {
	var t time.Time
	for _, f := range []string{c.certFile, c.keyFile} {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	return t
}

// newFileCertSource loads a user-supplied certificate and key.
func newFileCertSource(certFile, keyFile string, logf func(string, ...any)) (*certSource, error) {
	c := &certSource{certFile: certFile, keyFile: keyFile, logf: logf}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// reload re-reads the files; on failure the previous certificate stays.
func (c *certSource) reload() error {
	mt := c.fileModTime()
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("load %s / %s: %w", c.certFile, c.keyFile, err)
	}
	c.mu.Lock()
	c.cert, c.modTime = &cert, mt
	c.mu.Unlock()
	return nil
}

// watch reloads the files on SIGHUP and when their modification time
// changes (checked every interval).
func (c *certSource) watch(ctx context.Context, interval time.Duration, hup <-chan os.Signal) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		case <-t.C:
			c.mu.RLock()
			same := c.fileModTime().Equal(c.modTime)
			c.mu.RUnlock()
			if same {
				continue
			}
		}
		if err := c.reload(); err != nil {
			c.logf("TLS certificate reload failed, keeping the previous one: %v", err)
		} else {
			c.logf("TLS certificate reloaded from %s", c.certFile)
		}
	}
}

// Fingerprint is the SHA-256 fingerprint of a DER certificate, as
// browsers show it (upper-case hex pairs separated by colons).
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// localNames returns the names and addresses a self-signed certificate
// covers: the host name, <hostname>.local, localhost and every local
// unicast address.
func localNames() ([]string, []net.IP) {
	names := []string{"localhost"}
	if h, err := os.Hostname(); err == nil {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		short, _, _ := strings.Cut(h, ".")
		for _, n := range []string{h, short + ".local"} {
			if registry.ValidHost(n) && !slices.Contains(names, n) {
				names = append([]string{n}, names...)
			}
		}
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() && n.IP.IsGlobalUnicast() {
				ips = append(ips, n.IP)
			}
		}
	}
	return names, ips
}

// loadOrCreateSelfSigned reuses dir/cert.pem and dir/key.pem when they
// hold a valid pair that is not close to expiry, and otherwise generates a
// new ECDSA P-256 certificate (key file mode 0600). created reports whether
// a new one was made.
func loadOrCreateSelfSigned(dir string, names []string, ips []net.IP, now time.Time) (cert *tls.Certificate, created bool, err error) {
	certPath, keyPath := filepath.Join(dir, selfCertFile), filepath.Join(dir, selfKeyFile)
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && c.Leaf != nil && now.Add(selfRenewBefore).Before(c.Leaf.NotAfter) {
		return &c, false, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, false, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, false, err
	}
	cn := "snoopy-hub"
	if len(names) > 0 {
		cn = names[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"snoopy-hub (self-signed)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, false, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, false, err
	}
	if err := writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, false, err
	}
	if err := writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, false, err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, false, err
	}
	if c.Leaf == nil {
		return nil, false, errors.New("generated certificate did not parse")
	}
	return &c, true, nil
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

// tlsConfig is the server TLS configuration: TLS 1.2 or later with Go's
// default cipher suites (ECDHE only).
func tlsConfig(get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: get,
	}
}
