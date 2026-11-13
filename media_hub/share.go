package main

// Handing a box's provider login to another of the person's devices: the box that has it locks
// it with a short code it shows on screen and leaves it here; the other device, given the code,
// takes it once. The hub holds it locked, in memory only, for shareLifetime, keyed by a hash of the
// code: it never sees the code, so it can't read what it holds.
//
//   PUT /share/{id}  the locked login (id = SHA-256 of the code, in hex)
//   GET /share/{id}  takes it, once: it's gone after
//
// Behind the same guard as everything else (access.go): home network, or the hub key over TLS.

import (
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"
)

const (
	shareLifetime = 10 * time.Minute
	shareMaxBytes = 4 << 10
	sharesHeld    = 20
)

var shareID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type shares struct {
	mu   sync.Mutex
	held map[string]share
}

type share struct {
	locked  []byte
	expires time.Time
}

func newShares() *shares { return &shares{held: map[string]share{}} }

func (s *shares) routes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /share/{id}", s.put)
	mux.HandleFunc("GET /share/{id}", s.take)
}

func (s *shares) put(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !shareID.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, shareMaxBytes+1))
	if err != nil || len(body) == 0 || len(body) > shareMaxBytes {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired()
	if len(s.held) >= sharesHeld {
		http.Error(w, "too many waiting", http.StatusTooManyRequests)
		return
	}
	s.held[id] = share{locked: body, expires: time.Now().Add(shareLifetime)}
	hubLog.event("login_share_offered", "box", boxLabel(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *shares) take(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	s.dropExpired()
	held, ok := s.held[id]
	delete(s.held, id)
	s.mu.Unlock()
	if !ok {
		hubLog.event("login_share_missed", "from", boxLabel(r))
		http.NotFound(w, r)
		return
	}
	hubLog.event("login_share_taken", "by", boxLabel(r))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(held.locked); err != nil {
		log.Printf("share: writing the login: %v", err)
	}
}

// dropExpired forgets what's past its time. Under lock.
func (s *shares) dropExpired() {
	now := time.Now()
	for id, held := range s.held {
		if now.After(held.expires) {
			delete(s.held, id)
		}
	}
}
