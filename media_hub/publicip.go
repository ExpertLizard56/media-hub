package main

// The home's address on the internet. Devices away from home reach the hub by name, with the
// router's dynamic DNS keeping that name pointed at the address, so a change only matters if
// the name fails to follow it. The hub checks every quarter of an hour, logs it, and notifies
// when it changes: a notification in Home Assistant itself always, and the phones unless they
// are quiet (/share/phone-quiet-until.txt, below).

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

var (
	publicAddressFile = filepath.Join(root, "public-address.txt")
	// The household's quiet time for phone notices: one ISO time with its zone, e.g.
	// 2026-11-16T12:00:00-05:00. It lives in /share, outside the hub's own folder, so it can be set
	// by hand; share is mapped in config.yaml. Missing or unreadable means phones aren't quiet.
	phoneQuietFile = filepath.Join("/share", "phone-quiet-until.txt")
)

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

// notifyPhone sends a notice to the phones. With NTFY_URL set (an ntfy topic, outside Home
// Assistant) it goes there. Otherwise it's left in Home Assistant itself, then sent to every notify
// service whose name starts with mobile_app_ (needs homeassistant_api in config.yaml). Phones are
// skipped while phoneQuietFile says it's quiet. A failure is logged, never fatal.
func notifyPhone(title, message string) {
	if topic := strings.TrimSpace(os.Getenv("NTFY_URL")); topic != "" {
		if until, quiet := phonesQuiet(time.Now()); quiet {
			hubLog.event("notify_quiet", "title", title, "until", until.Format(time.RFC3339))
			return
		}
		if err := notifyNtfy(http.DefaultClient, topic, title, message); err != nil {
			hubLog.event("notify_failed", "error", err)
			return
		}
		hubLog.event("notified", "title", title, "via", "ntfy")
		return
	}
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
	if _, err := call(http.MethodPost, "/services/persistent_notification/create",
		map[string]string{"title": title, "message": message}); err != nil {
		hubLog.event("notify_failed", "error", err)
	}
	if until, quiet := phonesQuiet(time.Now()); quiet {
		hubLog.event("notify_quiet", "title", title, "until", until.Format(time.RFC3339))
		return
	}
	services, err := phoneNotifyServices(call)
	if err != nil {
		hubLog.event("notify_failed", "error", err) // no services to notify this time
		return
	}
	note := map[string]any{
		"title":   title,
		"message": message,
		// The app's notification options go in data; none are needed for these notices.
		"data": map[string]any{},
	}
	sent := 0
	for _, service := range services {
		if _, err := call(http.MethodPost, "/services/notify/"+service, note); err != nil {
			hubLog.event("notify_failed", "service", service, "error", err)
		} else {
			sent++
		}
	}
	hubLog.event("notified", "title", title, "phones", sent)
}

// notifyNtfy posts one notice to an ntfy topic URL: the message as the body, the title as a header.
func notifyNtfy(client *http.Client, topic, title, message string) error {
	req, err := http.NewRequest(http.MethodPost, topic, strings.NewReader(message))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body) // nothing in it is needed
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: %s", resp.Status)
	}
	return nil
}

// phonesQuiet reports the time phones stay quiet until, read from phoneQuietFile: one ISO time with
// its zone. A missing file, bad text or a time without a zone means phones aren't quiet.
func phonesQuiet(now time.Time) (time.Time, bool) {
	data, err := os.ReadFile(phoneQuietFile)
	if err != nil {
		return time.Time{}, false
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

// phoneNotifyServices is where phone notices go: every notify service whose name starts with
// mobile_app_ that Home Assistant lists.
func phoneNotifyServices(call func(method, path string, body any) ([]byte, error)) ([]string, error) {
	var services []string
	data, err := call(http.MethodGet, "/services", nil)
	if err != nil {
		return nil, err
	}
	var domains []struct {
		Domain   string                     `json:"domain"`
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(data, &domains); err != nil {
		return nil, err
	}
	for _, d := range domains {
		if d.Domain != "notify" {
			continue
		}
		for service := range d.Services {
			if strings.HasPrefix(service, "mobile_app_") {
				services = append(services, service)
			}
		}
	}
	return services, nil
}
