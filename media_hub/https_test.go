package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReloadingCertFollowsRenewal: a certificate replaced by another add-on's renewal is the one the
// next handshake gets, but the files are looked at at most once a minute.
func TestReloadingCertFollowsRenewal(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	now := start
	writeCert(t, certPath, keyPath, "first", start.Add(-time.Hour))

	r := newReloadingCert(certPath, keyPath)
	r.now = func() time.Time { return now }
	if got := certName(t, reload(t, r)); got != "first" {
		t.Fatalf("the certificate in use is %q, want first", got)
	}

	// Renewed, but less than a minute since the last look: the old one is still served.
	writeCert(t, certPath, keyPath, "second", start.Add(10*time.Minute))
	now = start.Add(30 * time.Second)
	if got := certName(t, reload(t, r)); got != "first" {
		t.Fatalf("a minute hasn't passed, so the certificate should still be first, got %q", got)
	}

	// A minute later the renewal is picked up.
	now = start.Add(90 * time.Second)
	if got := certName(t, reload(t, r)); got != "second" {
		t.Fatalf("the renewed certificate should be second, got %q", got)
	}

	// And the next renewal too.
	writeCert(t, certPath, keyPath, "third", start.Add(20*time.Minute))
	now = start.Add(3 * time.Minute)
	if got := certName(t, reload(t, r)); got != "third" {
		t.Fatalf("the second renewal should be third, got %q", got)
	}
}

// TestReloadingCertWaitsForFiles: no certificate yet is not fatal, and the files appearing later are
// used without restarting anything.
func TestReloadingCertWaitsForFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	r := newReloadingCert(certPath, keyPath)
	r.now = func() time.Time { return now }

	if cert, err := r.GetCertificate(nil); err != nil || cert != nil {
		t.Fatalf("no files yet: certificate %v, error %v; want nil, nil", cert, err)
	}
	writeCert(t, certPath, keyPath, "later", now.Add(-time.Minute))
	now = now.Add(time.Minute)
	if got := certName(t, reload(t, r)); got != "later" {
		t.Fatalf("the certificate written later should be used, got %q", got)
	}
}

// reload asks for the certificate as a handshake would.
func reload(t *testing.T, r *reloadingCert) *tls.Certificate {
	t.Helper()
	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil {
		t.Fatal("GetCertificate gave no certificate")
	}
	return cert
}

func certName(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return leaf.Subject.CommonName
}

// writeCert makes a throwaway self-signed pair, as a renewal would leave it, with the files' time
// set exactly: a rewrite in the same second could otherwise keep the old modification time.
func writeCert(t *testing.T, certPath, keyPath, name string, modified time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(modified.Unix()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    modified.Add(-time.Hour),
		NotAfter:     modified.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write %s: %v", certPath, err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write %s: %v", keyPath, err)
	}
	for _, path := range []string{certPath, keyPath} {
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatalf("set %s's time: %v", path, err)
		}
	}
}
