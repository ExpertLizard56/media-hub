package main

// An optional HTTPS listener beside the hub's own port (main.go): some setups want a certificate
// from a real certificate authority, on its own port, instead of the hub's own certificate.
//
// The add-on's options https_port (0 = off) and https_domain turn it on; both must be set. It serves
// the same handler as port 8765, so the same access rules apply. The certificate pair comes from
// /ssl/<https_domain>/fullchain.pem and privkey.pem, which another add-on keeps renewed. Those files
// are read again once they change, looked at at most once a minute; a missing or bad pair is logged
// and tried again, and port 8765 keeps serving either way.

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sslDir is where Home Assistant keeps the certificates of its add-ons (map: ssl:ro in config.yaml).
const sslDir = "/ssl"

// certCheckEvery is how often the certificate files are looked at again: renewals are rare, so a
// minute with the previous pair costs nothing.
const certCheckEvery = time.Minute

// reloadingCert hands TLS the certificate of /ssl/<domain>, and reads the pair again once the files
// have changed since it last looked.
type reloadingCert struct {
	certPath, keyPath string
	// now and every are fields so a test can move the clock instead of waiting a minute.
	now   func() time.Time
	every time.Duration

	mu      sync.Mutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
	checked time.Time
}

func newReloadingCert(certPath, keyPath string) *reloadingCert {
	return &reloadingCert{certPath: certPath, keyPath: keyPath, now: time.Now, every: certCheckEvery}
}

// GetCertificate is tls.Config's hook: the pair as it is now, read again when the files may have
// changed. It never returns an error: with no certificate at all the handshake fails on its own,
// and the next connection tries the files again.
func (c *reloadingCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !c.checked.IsZero() && now.Sub(c.checked) < c.every {
		return c.cert, nil
	}
	c.checked = now
	certMod, err := modTime(c.certPath)
	if err != nil {
		c.logFailure(err)
		return c.cert, nil
	}
	keyMod, err := modTime(c.keyPath)
	if err != nil {
		c.logFailure(err)
		return c.cert, nil
	}
	if c.cert != nil && certMod.Equal(c.certMod) && keyMod.Equal(c.keyMod) {
		return c.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
	if err != nil {
		c.logFailure(err)
		return c.cert, nil
	}
	if c.cert == nil {
		log.Printf("https: using the certificate in %s", filepath.Dir(c.certPath))
	} else {
		log.Printf("https: the certificate in %s was renewed; using it", filepath.Dir(c.certPath))
	}
	c.cert, c.certMod, c.keyMod = &cert, certMod, keyMod
	return c.cert, nil
}

// logFailure says plainly that the pair can't be read, what is still being served, and that it will
// be tried again.
func (c *reloadingCert) logFailure(err error) {
	if c.cert == nil {
		log.Printf("https: %v; port 8765 keeps serving, and this is tried again in a minute", err)
		return
	}
	log.Printf("https: %v; the certificate in use stays, and this is tried again in a minute", err)
}

func modTime(path string) (time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("can't read %s: %w", path, err)
	}
	return st.ModTime(), nil
}

// serveHTTPS serves the same handler over TLS on https_port, in its own goroutine (main.go). It
// reports and returns rather than stopping the hub: port 8765 serves whether or not this listener can.
func serveHTTPS(handler http.Handler, port int, domain string) {
	if domain != filepath.Base(domain) || domain == "." || domain == ".." {
		log.Printf("https: https_domain %q isn't one folder name in %s; no HTTPS listener", domain, sslDir)
		return
	}
	dir := filepath.Join(sslDir, domain)
	addr := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("https: listen %s: %v; port 8765 keeps serving", addr, err)
		return
	}
	cert := newReloadingCert(filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem"))
	server := &http.Server{
		Handler:   handler,
		TLSConfig: &tls.Config{GetCertificate: cert.GetCertificate, MinVersion: tls.VersionTLS12},
	}
	log.Printf("Media Hub also listening on %s (HTTPS with the certificate in %s)", addr, dir)
	if err := server.ServeTLS(listener, "", ""); err != nil {
		log.Printf("https: serve %s: %v; port 8765 keeps serving", addr, err)
	}
}
