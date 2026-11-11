package pki

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateLoadPin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cert, err := Generate(dir, "desk1", false, now)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "desk1" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("EKU = %v", cert.ExtKeyUsage)
	}
	if cert.PublicKeyAlgorithm != x509.Ed25519 {
		t.Errorf("alg = %v", cert.PublicKeyAlgorithm)
	}
	if !cert.NotAfter.After(now.Add(19 * 365 * 24 * time.Hour)) {
		t.Errorf("NotAfter = %v", cert.NotAfter)
	}
	for name, want := range map[string]os.FileMode{KeyFile: 0o600, CertFile: 0o644} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, %v; want %v", name, fi.Mode(), err, want)
		}
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, %v", fi.Mode(), err)
	}

	loaded, err := LoadCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	pin := Pin(loaded)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if want := "sha256:" + base64.StdEncoding.EncodeToString(sum[:]); pin != want {
		t.Errorf("pin = %q, want %q", pin, want)
	}
	if err := ValidatePin(pin); err != nil {
		t.Error(err)
	}
	kp, err := LoadKeyPair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if kp.Leaf == nil || Pin(kp.Leaf) != pin {
		t.Error("key pair leaf does not match pin")
	}

	if _, err := Generate(dir, "desk1", false, now); !errors.Is(err, ErrExists) {
		t.Fatalf("second Generate err = %v, want ErrExists", err)
	}
	cert2, err := Generate(dir, "desk1", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if Pin(cert2) == pin {
		t.Error("forced Generate kept the old key")
	}

	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyPair(dir); err == nil {
		t.Error("LoadKeyPair accepted a group-readable key")
	}
}

func TestValidatePin(t *testing.T) {
	for _, bad := range []string{"", "sha256:", "sha1:AAAA", "sha256:not-base64!", "sha256:" + base64.StdEncoding.EncodeToString([]byte("short"))} {
		if ValidatePin(bad) == nil {
			t.Errorf("ValidatePin(%q) = nil", bad)
		}
	}
	if _, err := Generate(t.TempDir(), "", false, time.Now()); err == nil {
		t.Error("empty device id accepted")
	}
}
