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
	"syscall"
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
	Name      string `json:"name"`
	Container string `json:"container"`
	Port      int    `json:"port"`      // loopback port the gateway listens on
	HookPort  int    `json:"hook_port"` // loopback port for inbound webhooks (0 = same as Port); OpenClaw serves Telegram webhooks on a separate listener
	// HookVerifier names how inbound webhooks for this cell prove themselves
	// before the cell is woken: "" or "none" (accept), "telegram" (header
	// secret), "slack" (HMAC v0 with timestamp), "github" (X-Hub-Signature-256),
	// "whatsapp" (X-Hub-Signature-256 + hub.challenge handshake), "bearer".
	// HookSecretFile holds the verify-only secret; never a bot token.
	HookVerifier   string        `json:"hook_verifier,omitempty"`
	HookSecretFile string        `json:"hook_secret_file,omitempty"`
	Class          Class         `json:"class"`
	Tier           Tier          `json:"tier"`
	IdleAfter      time.Duration `json:"idle_after"`
	// NextDueAt is the earliest time a scheduled job (cron) inside the cell
	// is due. Interim cron-aware wake (upstream #119035): a cell is never
	// hibernated while a job is due inside its idle window, and a
	// hibernated cell is woken shortly before NextDueAt. Zero = no schedule.
	NextDueAt    time.Time     `json:"next_due_at"`
	NextDueEvery time.Duration `json:"next_due_every,omitempty"` // recurrence for NextDueAt; 0 = one-shot (cleared after it fires)
	Phase        Phase         `json:"phase"`
	PausedAt     time.Time     `json:"paused_at,omitempty"`    // when the current pause began (pause tier)
	ReclaimedAt  time.Time     `json:"reclaimed_at,omitempty"` // when the paused cell's memory was last reclaimed to swap
	WarmAt       time.Time     `json:"warm_at,omitempty"`      // when the paused cell was last trimmed to the warm floor (hot set recorded)
	WokeAt       time.Time     `json:"woke_at,omitempty"`      // last successful wake; hibernation waits MinAwake after it
	Swapped      bool          `json:"swapped,omitempty"`      // the last reclaim actually moved pages to swap (false when reclaim was unsupported)
	LastActivity time.Time     `json:"last_activity"`
	RxBytes      int64         `json:"rx_bytes"`
	TxBytes      int64         `json:"tx_bytes"`
	Restarts     int           `json:"restarts"`
	LastError    string        `json:"last_error,omitempty"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// Store is a file-backed cell registry safe for concurrent use in-process.
// staleAfter bounds how long an unchanged-looking file is trusted without
// re-reading it. See Store.loadedAt.
const staleAfter = 2 * time.Second

type Store struct {
	path  string
	mu    sync.Mutex
	cells map[string]Cell
	// loaded identifies the file contents the map reflects, so readers can
	// skip re-parsing an unchanged file.
	loadedMod  time.Time
	loadedSize int64
	// loadedAt bounds how long that skip may be trusted. Matching mtime and
	// size almost always means unchanged, but two writes can share both where
	// the filesystem clock is coarse enough, and a reader must not then serve
	// a stale cell for ever. Re-reading at most this often costs a parse of a
	// small file and turns an unbounded staleness into a bounded one.
	loadedAt time.Time
}

// refresh re-reads the file only when its size or mtime changed since the
// last load: cheap enough for every read, so a daemon sees cells the CLI
// adds or removes without waiting for the next full pass. Caller holds mu.
func (s *Store) refresh() {
	fi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.cells = map[string]Cell{}
			s.loadedMod, s.loadedSize = time.Time{}, 0
		}
		return
	}
	if fi.ModTime().Equal(s.loadedMod) && fi.Size() == s.loadedSize && time.Since(s.loadedAt) < staleAfter {
		return
	}
	_ = s.reload() // best effort; on error keep the last good map
}

// ErrNotFound is returned for unknown cell names.
var ErrNotFound = errors.New("registry: cell not found")

// Open loads (or creates) the registry file.
func Open(path string) (*Store, error) {
	s := &Store{path: path, cells: map[string]Cell{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload replaces the in-memory map with the file's contents. Caller holds
// mu (or is Open). A missing file is an empty registry.
func (s *Store) reload() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.cells = map[string]Cell{}
		return nil
	}
	if err != nil {
		return err
	}
	var list []Cell
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	m := make(map[string]Cell, len(list))
	for _, c := range list {
		m[c.Name] = c
	}
	s.cells = m
	if fi, err := os.Stat(s.path); err == nil {
		s.loadedMod, s.loadedSize = fi.ModTime(), fi.Size()
	}
	s.loadedAt = time.Now()
	return nil
}

// withFileLock serialises mutations across processes: it takes an advisory
// lock on <path>.lock, re-reads the file so another process's writes are
// never clobbered, applies fn, and flushes. `fleetd cells add` while
// `fleetd serve` runs is therefore safe in both directions.
func (s *Store) withFileLock(fn func() error) error {
	lf, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	if err := s.reload(); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return s.flush()
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
	return s.withFileLock(func() error { s.cells[c.Name] = c; return nil })
}

// Get returns one cell, picking up another process's edits first.
func (s *Store) Get(name string) (Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
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
	return s.withFileLock(func() error {
		if _, ok := s.cells[name]; !ok {
			return ErrNotFound
		}
		delete(s.cells, name)
		return nil
	})
}

// List returns cells sorted by name, re-reading the file first so a
// long-running daemon sees cells added or removed by the CLI.
func (s *Store) List() []Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
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
	return s.withFileLock(func() error {
		c, ok := s.cells[name]
		if !ok {
			return ErrNotFound
		}
		fn(&c)
		c.UpdatedAt = time.Now().UTC()
		s.cells[name] = c
		return nil
	})
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
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // the rename is only atomic if the bytes are durable first
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.loadedMod, s.loadedSize = fi.ModTime(), fi.Size() // our own write: the map already matches
	}
	return nil
}
