// Package pki creates and loads the snoopy-attest device identity: an Ed25519
// key and a long-lived self-signed client certificate, identified to KProxy
// by its SPKI pin.
package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File names inside the state directory.
const (
	KeyFile  = "key.pem"
	CertFile = "cert.pem"
)

// Validity of the self-signed device certificate. Revocation is done by
// removing the pin on the KProxy side, so a long lifetime is fine.
const Validity = 20 * 365 * 24 * time.Hour

// PinPrefix prefixes every SPKI pin.
const PinPrefix = "sha256:"

// ErrExists is returned by Generate when a key or certificate is already
// present and force is false.
var ErrExists = errors.New("device key or certificate already exists (use -force to replace it)")

// Generate creates key.pem (0600) and cert.pem (0644) in dir, creating dir
// with mode 0700 if needed. The certificate's CN is deviceID.
func Generate(dir, deviceID string, force bool, now time.Time) (*x509.Certificate, error) {
	if deviceID == "" {
		return nil, errors.New("empty device id")
	}
	keyPath := filepath.Join(dir, KeyFile)
	certPath := filepath.Join(dir, CertFile)
	if !force {
		for _, p := range []string{keyPath, certPath} {
			if _, err := os.Lstat(p); err == nil {
				return nil, fmt.Errorf("%s: %w", p, ErrExists)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: deviceID, OrganizationalUnit: []string{"snoopy-attest"}},
		NotBefore:             now.Add(-time.Hour).UTC(),
		NotAfter:              now.Add(Validity).UTC(),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		return nil, err
	}
	if err := writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadCertificate reads cert.pem from dir.
func LoadCertificate(dir string) (*x509.Certificate, error) {
	data, err := os.ReadFile(filepath.Join(dir, CertFile)) //nolint:gosec // state directory chosen by the administrator
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no CERTIFICATE PEM block", filepath.Join(dir, CertFile))
	}
	return x509.ParseCertificate(block.Bytes)
}

// LoadKeyPair loads key.pem and cert.pem from dir as a TLS client
// certificate. It refuses a key file readable by group or others.
func LoadKeyPair(dir string) (tls.Certificate, error) {
	keyPath := filepath.Join(dir, KeyFile)
	fi, err := os.Stat(keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return tls.Certificate{}, fmt.Errorf("%s: mode %04o is accessible by group or others; chmod 0600", keyPath, fi.Mode().Perm())
	}
	return tls.LoadX509KeyPair(filepath.Join(dir, CertFile), keyPath)
}

// Pin returns the SPKI pin of cert: "sha256:" followed by the standard
// base64 encoding of SHA-256 over the DER SubjectPublicKeyInfo (the same
// construction as HPKP and curl --pinnedpubkey).
func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return PinPrefix + base64.StdEncoding.EncodeToString(sum[:])
}

// ValidatePin checks the syntax of a pin string.
func ValidatePin(pin string) error {
	b64, ok := strings.CutPrefix(pin, PinPrefix)
	if !ok {
		return fmt.Errorf("pin %q must start with %q", pin, PinPrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("pin %q: want base64 of a 32-byte SHA-256 digest", pin)
	}
	return nil
}
