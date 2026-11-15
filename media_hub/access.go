package main

// Reaching the hub from outside the house (boxes at other people's homes), through one port
// forwarded on the home router to the hub's port:
//
//   - The port speaks both plain HTTP and TLS (splitListener). The TLS certificate is the hub's own,
//     made once and kept in /share/media-hub/tls; the app trusts that certificate alone (it carries
//     its fingerprint, pin.txt), so no certificate authority, domain or sign-up is involved.
//   - From the home network (and VPNs into it) everything works as before, with no key.
//   - From anywhere else: the app's requests come over TLS with the hub key (X-Hub-Key); the
//     only requests without the key are a new box's first install, /i/<install code> (plain HTTP or
//     HTTPS: Downloader trusts the real certificate on https_port, not the hub's own), and an app pairing, /pair/<install code>, which
//     hands it the key, the certificate's fingerprint and the internet address (servePair: the apps carry
//     none of these built in any more, so anyone's hub can be used). Browsing diagnostics stays home-only.
//
// Pairing at home without the code: while /share/media-hub/pair-home-until.txt holds a time still to come
// (RFC 3339), an app on the home network that asks /pair/home is paired as if it had the install code,
// so devices can be readied for travel without typing it. Off once that time passes or the file is gone;
// devices paired meanwhile stay paired. Asked from anywhere else, or while closed, it's a plain 404.
//
// The key and install code are made once, at random, and kept in /share/media-hub/access.json.

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	accessFile = filepath.Join(root, "access.json")
	tlsDir     = filepath.Join(root, "tls")
)

type accessSecrets struct {
	Key         string `json:"key"`
	InstallCode string `json:"installCode"`
}

// loadAccess reads the key and install code, making them the first time.
func loadAccess() (accessSecrets, error) {
	var a accessSecrets
	if data, err := os.ReadFile(accessFile); err == nil {
		if err := json.Unmarshal(data, &a); err != nil {
			return a, fmt.Errorf("%s: %w", accessFile, err)
		}
		if a.Key != "" && a.InstallCode != "" {
			return a, nil
		}
	}
	a = accessSecrets{Key: randomText(32, keyAlphabet), InstallCode: randomText(6, codeAlphabet)}
	data, _ := json.MarshalIndent(a, "", "  ")
	if err := os.WriteFile(accessFile, data, 0o600); err != nil {
		return a, err
	}
	hubLog.event("access_created")
	return a, nil
}

const (
	keyAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	// Easy to read off a screen and type with a remote: no 0/o, 1/l.
	codeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
)

func randomText(n int, alphabet string) string {
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

// loadCertificate reads the hub's own certificate, making it (good for 30 years) the first time,
// and writes its fingerprint, the pin an app trusts it by, next to it.
func loadCertificate() (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(tlsDir, "cert.pem"), filepath.Join(tlsDir, "key.pem")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return cert, writePin(cert)
	}
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "Media Hub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	hubLog.event("certificate_created")
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return cert, err
	}
	return cert, writePin(cert)
}

// writePin saves the certificate's public-key fingerprint, as OkHttp writes pins: sha256/<base64>.
func writePin(cert tls.Certificate) error {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return os.WriteFile(filepath.Join(tlsDir, "pin.txt"), []byte("sha256/"+base64.StdEncoding.EncodeToString(sum[:])+"\n"), 0o644)
}

// guard lets home-network requests through as before, and from elsewhere only what's described above.
func guard(access *hubAccess, remoteAddress string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := access.installCode()
		installPath := "/i/" + code
		if r.Method == http.MethodGet && r.URL.Path == "/pair/"+code {
			servePair(w, r, access, remoteAddress)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/pair/home" {
			if fromHome(r) && homePairingOpen(time.Now()) {
				servePair(w, r, access, remoteAddress)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if fromHome(r) || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == installPath {
			hubLog.event("remote_install", "from", remoteHost(r))
			serveInstall(w, r)
			return
		}
		if r.TLS == nil {
			http.NotFound(w, r)
			return
		}
		if device, ok := access.allowed(r.Header.Get("X-Hub-Key")); !ok {
			hubLog.event("remote_refused", "from", remoteHost(r), "path", r.URL.Path, "device", device)
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/diagnostics/") {
			http.NotFound(w, r) // the boxes' logs are read at home
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pairing is what an app needs to use this hub from anywhere: {"key", "pin", "remoteUrl"}.
type pairing struct {
	Key       string `json:"key"`
	Pin       string `json:"pin"`
	RemoteURL string `json:"remoteUrl"`
}

// servePair answers an app that was given this hub's address and install code: a key of the device's own
// (?id=<its diagnostics id>&name=<its name>; one is made up for an app that sends none).
func servePair(w http.ResponseWriter, r *http.Request, access *hubAccess, remoteAddress string) {
	pin, err := os.ReadFile(filepath.Join(tlsDir, "pin.txt"))
	if err != nil {
		http.Error(w, "no certificate yet", http.StatusServiceUnavailable)
		return
	}
	id := r.URL.Query().Get("id")
	if !boxID.MatchString(id) {
		id = "device-" + randomText(12, codeAlphabet)
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || len(name) > 80 {
		name = "Device"
	}
	hubLog.event("app_paired", "from", remoteHost(r), "device", id, "name", name)
	p := pairing{Key: access.pair(id, name), Pin: strings.TrimSpace(string(pin))}
	if remoteAddress != "" {
		p.RemoteURL = "https://" + strings.TrimPrefix(strings.TrimPrefix(remoteAddress, "https://"), "http://")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(p); err != nil {
		log.Printf("pair: %v", err)
	}
}

// pairHomeFile: while it holds a time to come, home devices pair without the install code (/pair/home).
var pairHomeFile = filepath.Join(root, "pair-home-until.txt")

func homePairingOpen(now time.Time) bool {
	data, err := os.ReadFile(pairHomeFile)
	if err != nil {
		return false
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return err == nil && now.Before(until)
}

// fromHome: the home network, this machine, or a VPN into the home (Tailscale, WireGuard).
func fromHome(r *http.Request) bool {
	host := remoteHost(r)
	// A link-local IPv6 address carries its interface ("fe80::…%end0"), which ParseIP refuses:
	// a phone reaching homeassistant.local this way was turned away (404 on /music/install).
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || carrierGrade.Contains(ip)
}

// 100.64.0.0/10: Tailscale's (and other VPNs') addresses.
var carrierGrade = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// splitListener serves plain HTTP and TLS on one port, by the first byte of each connection
// (a TLS handshake starts with 0x16), so the router needs only one port forwarded. Each connection
// is sorted on its own goroutine, so a slow one never holds up the others.
type splitListener struct {
	net.Listener
	tls   *tls.Config
	ready chan net.Conn
	done  chan error
}

func newSplitListener(inner net.Listener, config *tls.Config) *splitListener {
	l := &splitListener{Listener: inner, tls: config, ready: make(chan net.Conn), done: make(chan error, 1)}
	go l.acceptLoop()
	return l
}

func (l *splitListener) acceptLoop() {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			l.done <- err
			return
		}
		go func() {
			peeked := &peekedConn{Conn: conn, reader: bufio.NewReader(conn)}
			// A deadline that can't be set means the connection is already gone, which Peek reports.
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			first, err := peeked.reader.Peek(1)
			_ = conn.SetReadDeadline(time.Time{})
			if err != nil {
				_ = conn.Close() // nothing will read it now
				return
			}
			if first[0] == 0x16 {
				l.ready <- tls.Server(peeked, l.tls)
			} else {
				l.ready <- peeked
			}
		}()
	}
}

func (l *splitListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.ready:
		return conn, nil
	case err := <-l.done:
		return nil, err
	}
}

type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
