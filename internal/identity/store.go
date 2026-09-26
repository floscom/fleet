package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// touchInterval bounds how often DeviceStore.Touch persists LastSeen.
const touchInterval = time.Minute

// PairedDevice is a device record kept by the daemon.
type PairedDevice struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Platform  string    `json:"platform"`
	PublicKey []byte    `json:"public_key"`
	PairedAt  time.Time `json:"paired_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// DeviceStore persists paired devices to a JSON file (0600, atomic writes).
// Safe for concurrent use.
type DeviceStore struct {
	mu      sync.Mutex
	path    string
	devices map[string]PairedDevice
	// saved is the LastSeen value last written to disk, per device.
	saved map[string]time.Time
}

// OpenDeviceStore loads (or starts empty) the store at path.
func OpenDeviceStore(path string) (*DeviceStore, error) {
	var list []PairedDevice
	if err := loadJSON(path, &list); err != nil {
		return nil, err
	}
	s := &DeviceStore{path: path, devices: map[string]PairedDevice{}, saved: map[string]time.Time{}}
	for _, d := range list {
		s.devices[d.ID] = d
		s.saved[d.ID] = d.LastSeen
	}
	return s, nil
}

// Add stores a device (replacing one with the same id) and saves.
func (s *DeviceStore) Add(d PairedDevice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.PublicKey = slices.Clone(d.PublicKey)
	s.devices[d.ID] = d
	return s.saveLocked()
}

// Get returns a device by id.
func (s *DeviceStore) Get(id string) (PairedDevice, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	return cloneDevice(d), ok
}

// Find returns a device by id, id prefix (>=6 chars, unique) or name.
func (s *DeviceStore) Find(idOrName string) (PairedDevice, bool) {
	if d, ok := s.Get(idOrName); ok {
		return d, true
	}
	list := s.List()
	if len(idOrName) >= minPrefix {
		var match []PairedDevice
		lq := strings.ToLower(idOrName)
		for _, d := range list {
			if strings.HasPrefix(d.ID, lq) {
				match = append(match, d)
			}
		}
		if len(match) == 1 {
			return match[0], true
		}
	}
	for _, d := range list {
		if d.Name == idOrName {
			return d, true
		}
	}
	return PairedDevice{}, false
}

// List returns devices sorted by pairing time.
func (s *DeviceStore) List() []PairedDevice {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *DeviceStore) listLocked() []PairedDevice {
	out := make([]PairedDevice, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, cloneDevice(d))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].PairedAt.Equal(out[j].PairedAt) {
			return out[i].PairedAt.Before(out[j].PairedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Remove deletes a device by id and saves.
func (s *DeviceStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[id]; !ok {
		return fmt.Errorf("no device %q", id)
	}
	delete(s.devices, id)
	delete(s.saved, id)
	return s.saveLocked()
}

// Touch updates LastSeen (saved lazily, at most once a minute per device).
func (s *DeviceStore) Touch(id string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok || !t.After(d.LastSeen) {
		return
	}
	d.LastSeen = t
	s.devices[id] = d
	if t.Sub(s.saved[id]) >= touchInterval {
		_ = s.saveLocked() // best effort; the next save persists it
	}
}

func (s *DeviceStore) saveLocked() error {
	list := s.listLocked()
	if err := saveJSON(s.path, list); err != nil {
		return err
	}
	for _, d := range list {
		s.saved[d.ID] = d.LastSeen
	}
	return nil
}

func cloneDevice(d PairedDevice) PairedDevice {
	d.PublicKey = slices.Clone(d.PublicKey)
	return d
}

// KnownServer is a daemon this machine paired with as a client.
type KnownServer struct {
	ID       string    `json:"id"` // server id (cert sha256 hex) - pinned
	Name     string    `json:"name"`
	Address  string    `json:"address"`   // last known host:port
	DeviceID string    `json:"device_id"` // id the server assigned us
	PairedAt time.Time `json:"paired_at"`
}

// minPrefix is the shortest id prefix ServerStore.Find accepts.
const minPrefix = 6

// ServerStore persists KnownServers (client side), JSON, 0600, atomic.
type ServerStore struct {
	mu      sync.Mutex
	path    string
	servers map[string]KnownServer
}

// OpenServerStore loads (or starts empty) the store at path.
func OpenServerStore(path string) (*ServerStore, error) {
	var list []KnownServer
	if err := loadJSON(path, &list); err != nil {
		return nil, err
	}
	s := &ServerStore{path: path, servers: map[string]KnownServer{}}
	for _, k := range list {
		s.servers[k.ID] = k
	}
	return s, nil
}

// Put stores/replaces by ID and saves.
func (s *ServerStore) Put(k KnownServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers[k.ID] = k
	return saveJSON(s.path, s.listLocked())
}

// Find returns a server by id, id prefix (>=6 chars, unique) or name.
func (s *ServerStore) Find(q string) (KnownServer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.servers[q]; ok {
		return k, true
	}
	if len(q) >= minPrefix {
		var match []KnownServer
		lq := strings.ToLower(q)
		for id, k := range s.servers {
			if strings.HasPrefix(id, lq) {
				match = append(match, k)
			}
		}
		if len(match) == 1 {
			return match[0], true
		}
	}
	for _, k := range s.listLocked() {
		if k.Name == q {
			return k, true
		}
	}
	return KnownServer{}, false
}

// List returns all known servers sorted by name.
func (s *ServerStore) List() []KnownServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *ServerStore) listLocked() []KnownServer {
	out := make([]KnownServer, 0, len(s.servers))
	for _, k := range s.servers {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Remove deletes by id and saves.
func (s *ServerStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.servers[id]; !ok {
		return fmt.Errorf("no server %q", id)
	}
	delete(s.servers, id)
	return saveJSON(s.path, s.listLocked())
}

// loadJSON decodes path into v; a missing file leaves v untouched.
func loadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func saveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}
