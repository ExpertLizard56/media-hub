package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthSaysWhere(t *testing.T) {
	for _, tc := range []struct{ from, where, remote string }{
		{"192.168.1.50:4000", "home", "https://pi.example.com:8766"},
		{"172.27.66.2:4000", "home", "https://pi.example.com:8766"},
		{"203.0.113.9:4000", "away", ""},
	} {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = tc.from
		rec := httptest.NewRecorder()
		serveHealth(rec, req, "pi.example.com:8766")
		if got := rec.Header().Get("X-Hub-Where"); got != tc.where {
			t.Errorf("%s: where %q", tc.from, got)
		}
		if got := rec.Header().Get("X-Hub-Remote"); got != tc.remote {
			t.Errorf("%s: remote %q", tc.from, got)
		}
		if rec.Body.String() != "ok\n" {
			t.Errorf("%s: body %q", tc.from, rec.Body.String())
		}
	}
}
