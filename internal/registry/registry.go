// Package registry is the supervisor's durable record of cells. A cell is
// one tenant's OpenClaw (or NanoClaw/Isthmus) instance: a container the
// supervisor may stop when idle and restart on demand. The store is an
// atomically-rewritten JSON file — sufficient for hundreds of cells and
// dependency-free; swap for SQLite when multi-host lands.
package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Class is the lifecycle policy a cell is under.
type Class string

const (
	// ClassHibernate: stop when idle past IdleAfter; wake on demand.
	ClassHibernate Class = "hibernate"
	// ClassAlwaysOn: never stop; restart if it exits (e.g. WhatsApp cells).
	ClassAlwaysOn Class = "always-on"
)

// Tier is how a hibernate-class cell is put to sleep.
type Tier string

const (
	// TierPause freezes processes with the cgroup freezer. Memory stays
	// resident (reclaimable only via swap/zram); wake is sub-second.
	TierPause Tier = "pause"
	// TierStop stops the container. Memory is freed; wake costs a full
	// gateway start (measured 70–90 s for OpenClaw on a slow host).
	TierStop Tier = "stop"
)

// Phase is the supervisor's view of a cell (distinct from runtime state).
type Phase string

const (
	PhaseActive     Phase = "active"
	PhaseHibernated Phase = "hibernated"
	PhaseWaking     Phase = "waking"
	PhaseFailed     Phase = "failed"
)

// Cell is one managed instance.
type Cell struct {
	Name      string        `json:"name"`
	Container string        `json:"container"`
	Port      int           `json:"port"` // loopback port the gateway listens on
	Class     Class         `json:"class"`
	Tier      Tier          `json:"tier"`
	IdleAfter time.Duration `json:"idle_after"`
	// NextDueAt is the earliest time a scheduled job (cron) inside the cell
	// is due. Interim cron-aware wake (upstream #119035): a cell is never
	// hibernated while a job is due inside its idle window, and a
	// hibernated cell is woken shortly before NextDueAt. Zero = no schedule.
	NextDueAt    time.Time `json:"next_due_at"`
	Phase        Phase     `json:"phase"`
	LastActivity time.Time `json:"last_activity"`
	RxBytes      int64     `json:"rx_bytes"`
	TxBytes      int64     `json:"tx_bytes"`
	Restarts     int       `json:"restarts"`
	LastError    string    `json:"last_error,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store is a file-backed cell registry safe for concurrent use in-process.
type Store struct {
	path  string
	mu    sync.Mutex
	cells map[string]Cell
}

// ErrNotFound is returned for unknown cell names.
var ErrNotFound = errors.New("registry: cell not found")

// Open loads (or creates) the registry file.
func Open(path string) (*Store, error) {
	s := &Store{path: path, cells: map[string]Cell{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err != nil {
		return nil, err
	}
	var list []Cell
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, c := range list {
		s.cells[c.Name] = c
	}
	return s, nil
}

// Put inserts or replaces a cell.
func (s *Store) Put(c Cell) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Name == "" || c.Container == "" {
		return errors.New("registry: name and container are required")
	}
	if c.Class == "" {
		c.Class = ClassHibernate
	}
	if c.Tier == "" {
		c.Tier = TierPause
	}
	if c.Phase == "" {
		c.Phase = PhaseActive
	}
	c.UpdatedAt = time.Now().UTC()
	s.cells[c.Name] = c
	return s.flush()
}

// Get returns one cell.
func (s *Store) Get(name string) (Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cells[name]
	if !ok {
		return Cell{}, ErrNotFound
	}
	return c, nil
}

// Delete removes a cell record (never touches the container).
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cells[name]; !ok {
		return ErrNotFound
	}
	delete(s.cells, name)
	return s.flush()
}

// List returns cells sorted by name.
func (s *Store) List() []Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Cell, 0, len(s.cells))
	for _, c := range s.cells {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Update applies fn to a cell under the lock and persists it.
func (s *Store) Update(name string, fn func(*Cell)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cells[name]
	if !ok {
		return ErrNotFound
	}
	fn(&c)
	c.UpdatedAt = time.Now().UTC()
	s.cells[name] = c
	return s.flush()
}

// flush writes the file atomically (temp + rename). Caller holds mu.
func (s *Store) flush() error {
	list := make([]Cell, 0, len(s.cells))
	for _, c := range s.cells {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
