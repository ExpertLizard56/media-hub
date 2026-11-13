package main

// The home's address on the internet. Devices away from home reach the hub by name, with the
// router's dynamic DNS keeping that name pointed at the address, so a change only matters if
// the name fails to follow it. The hub checks every quarter of an hour, logs it, and notifies
// through Home Assistant (its mobile app's notifications) when it changes.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const publicAddressEvery = 15 * time.Minute

var publicAddressFile = filepath.Join(root, "public-address.txt")

// Services that answer with the address a request came from; no account needed.
var addressServices = []string{"https://api.ipify.org", "https://icanhazip.com"}

func watchPublicAddress() {
	last := ""
	if data, err := os.ReadFile(publicAddressFile); err == nil {
		last = strings.TrimSpace(string(data))
	}
	for {
		if address := lookUpPublicAddress(); address != "" && address != last {
			hubLog.event("public_address", "address", address, "was", last)
			if last != "" {
				notifyPhone("Home internet address changed",
					fmt.Sprintf("It's now %s (was %s). The router's dynamic DNS should point the hub's name at it within 5 minutes; if devices away from home can't connect after that, check the router's dynamic DNS.", address, last))
			}
			if err := os.WriteFile(publicAddressFile, []byte(address+"\n"), 0o644); err != nil {
				hubLog.event("public_address_save_failed", "error", err)
			}
			last = address
		}
		time.Sleep(publicAddressEvery)
	}
}

func lookUpPublicAddress() string {
	for _, service := range addressServices {
		resp, err := httpClient.Get(service)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		closeQuietly(resp.Body) // its content is read above
		if ip := net.ParseIP(strings.TrimSpace(string(body))); resp.StatusCode == http.StatusOK && ip != nil {
			return ip.String()
		}
	}
	return ""
}

// notifyPhone sends a notification to every phone with the Home Assistant app, and leaves one in
// Home Assistant itself. Needs homeassistant_api in config.yaml.
func notifyPhone(title, message string) {
	token := os.Getenv("SUPERVISOR_TOKEN")
	if token == "" {
		hubLog.event("notify_failed", "error", "no Home Assistant access")
		return
	}
	call := func(method, path string, body any) ([]byte, error) {
		var payload io.Reader
		if body != nil {
			data, _ := json.Marshal(body)
			payload = bytes.NewReader(data)
		}
		req, _ := http.NewRequest(method, "http://supervisor/core/api"+path, payload)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer closeQuietly(resp.Body) // its content is read below
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
		}
		return data, nil
	}
	note := map[string]string{"title": title, "message": message}
	if _, err := call(http.MethodPost, "/services/persistent_notification/create", note); err != nil {
		hubLog.event("notify_failed", "error", err)
	}
	data, err := call(http.MethodGet, "/services", nil)
	if err != nil {
		hubLog.event("notify_failed", "error", err)
		return
	}
	var domains []struct {
		Domain   string                     `json:"domain"`
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(data, &domains); err != nil {
		hubLog.event("notify_failed", "error", err) // no services to notify this time
	}
	sent := 0
	for _, d := range domains {
		if d.Domain != "notify" {
			continue
		}
		for service := range d.Services {
			if !strings.HasPrefix(service, "mobile_app_") {
				continue
			}
			if _, err := call(http.MethodPost, "/services/notify/"+service, note); err != nil {
				hubLog.event("notify_failed", "service", service, "error", err)
			} else {
				sent++
			}
		}
	}
	hubLog.event("notified", "title", title, "phones", sent)
}
