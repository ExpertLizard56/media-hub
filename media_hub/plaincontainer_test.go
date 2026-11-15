package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminAllowed(t *testing.T) {
	cases := []struct {
		name, addr, user string
		want             bool
	}{
		{"ingress proxy", "172.30.32.2:5000", "", true},
		{"local proxy with user", "127.0.0.1:5000", "user1", true},
		{"local proxy ipv6 with user", "[::1]:5000", "user1", true},
		{"local without user", "127.0.0.1:5000", "", false},
		{"local blank user", "127.0.0.1:5000", "  ", false},
		{"home network with user", "192.168.1.50:5000", "user1", false},
		{"home network", "192.168.1.50:5000", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.RemoteAddr = c.addr
		if c.user != "" {
			r.Header.Set("X-Remote-User-Id", c.user)
		}
		if got := adminAllowed(r); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNotifyNtfy(t *testing.T) {
	var gotTitle, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle, gotPath = r.Header.Get("Title"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()
	if err := notifyNtfy(srv.Client(), srv.URL+"/home", "Hub", "New address"); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "Hub" || gotBody != "New address" || gotPath != "/home" {
		t.Errorf("got title %q body %q path %q", gotTitle, gotBody, gotPath)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer fail.Close()
	if err := notifyNtfy(fail.Client(), fail.URL+"/home", "t", "m"); err == nil {
		t.Error("want an error for 403")
	}
}
