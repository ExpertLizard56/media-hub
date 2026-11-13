package main

// Who may use the hub from outside the home. Each app that pairs (/pair/, access.go) gets a key of its own,
// kept here in devices.json, so one device can be blocked or removed on the hub's page (admin.go) without the others.
// Devices from before keep using the one shared key in access.json. At home no key is asked for, as always, so
// blocking a device stops it from outside the home only.

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var devicesFile = filepath.Join(root, "devices.json")

type pairedDevice struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Key     string    `json:"key"`
	Paired  time.Time `json:"paired"`
	Blocked bool      `json:"blocked,omitempty"`
	// When it last used the hub from outside the home.
	LastAway time.Time `json:"lastAway"`
}

// hubAccess holds the shared key, the install code and the paired devices; the page changes them while the hub runs.
type hubAccess struct {
	mu      sync.Mutex
	secrets accessSecrets
	devices map[string]*pairedDevice
	savedAt time.Time
}

func newHubAccess(secrets accessSecrets) *hubAccess {
	a := &hubAccess{secrets: secrets, devices: map[string]*pairedDevice{}}
	if data, err := os.ReadFile(devicesFile); err == nil {
		var list []*pairedDevice
		if err := json.Unmarshal(data, &list); err != nil {
			log.Printf("devices: %v", err)
		}
		for _, d := range list {
			a.devices[d.ID] = d
		}
	}
	return a
}

func (a *hubAccess) installCode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.secrets.InstallCode
}

// newInstallCode replaces the install code: nobody can pair or install with the old one. Paired devices keep working.
func (a *hubAccess) newInstallCode() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.secrets.InstallCode = randomText(6, codeAlphabet)
	data, _ := json.MarshalIndent(a.secrets, "", "  ")
	hubLog.event("install_code_changed")
	return os.WriteFile(accessFile, data, 0o600)
}

// pair gives the device a key of its own (a new one each time it pairs) and returns it.
func (a *hubAccess) pair(id, name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.devices[id]
	if d == nil {
		d = &pairedDevice{ID: id}
		a.devices[id] = d
	}
	d.Name, d.Key, d.Paired, d.Blocked = name, randomText(32, keyAlphabet), time.Now(), false
	a.saveLocked()
	return d.Key
}

// allowed says whether a key may use the hub from outside: the shared key, or a device's own while it isn't blocked.
// The device is "" for the shared key.
func (a *hubAccess) allowed(key string) (device string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if key == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(a.secrets.Key)) == 1 {
		return "", true
	}
	for _, d := range a.devices {
		if subtle.ConstantTimeCompare([]byte(key), []byte(d.Key)) == 1 {
			if d.Blocked {
				return d.ID, false
			}
			d.LastAway = time.Now()
			// Last-seen times are saved at most every 10 minutes, not on every request.
			if time.Since(a.savedAt) > 10*time.Minute {
				a.saveLocked()
			}
			return d.ID, true
		}
	}
	return "", false
}

func (a *hubAccess) list() []pairedDevice {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]pairedDevice, 0, len(a.devices))
	for _, d := range a.devices {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rename gives a device the name shown on the page; a device that never paired gets an entry with no key.
func (a *hubAccess) rename(id, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.devices[id]
	if d == nil {
		d = &pairedDevice{ID: id}
		a.devices[id] = d
	}
	d.Name = name
	a.saveLocked()
}

func (a *hubAccess) setBlocked(id string, blocked bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d := a.devices[id]; d != nil {
		d.Blocked = blocked
		hubLog.event("device_blocked", "device", id, "blocked", blocked)
		a.saveLocked()
	}
}

// remove forgets a device: its key stops working and its logs go.
func (a *hubAccess) remove(id string) {
	a.mu.Lock()
	delete(a.devices, id)
	a.saveLocked()
	a.mu.Unlock()
	hubLog.event("device_removed", "device", id)
	if err := os.RemoveAll(filepath.Join(diagnosticsDir, id)); err != nil {
		log.Printf("devices: removing %s's logs: %v", id, err)
	}
}

func (a *hubAccess) saveLocked() {
	list := make([]*pairedDevice, 0, len(a.devices))
	for _, d := range a.devices {
		list = append(list, d)
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(devicesFile, data, 0o600); err != nil {
		log.Printf("devices: %v", err)
	}
	a.savedAt = time.Now()
}
