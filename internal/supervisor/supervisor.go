// Package supervisor is the reconcile loop: it samples every registered
// cell, hibernates the ones that have been idle past their policy, restores
// always-on cells that died, and serves wake requests with a bounded
// concurrency so a thundering herd cannot OOM the host.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prathish-ks/clawnap/internal/idle"
	"github.com/prathish-ks/clawnap/internal/reclaim"
	"github.com/prathish-ks/clawnap/internal/registry"
	"github.com/prathish-ks/clawnap/internal/runtime"
	"github.com/prathish-ks/clawnap/internal/walcheck"
)

// Options tune the loop.
type Options struct {
	Interval        time.Duration // sampling period
	StopGrace       time.Duration // graceful stop timeout
	WakeTimeout     time.Duration // max wait for readiness after an unpause (pause tier)
	StopWakeTimeout time.Duration // max wait for readiness after a start (stop tier: full gateway boot)
	MaxConcurrent   int           // simultaneous page-ins/starts (the disk-bound step)
	// MaxRecovering bounds cells between unpause and ready: a thawed gateway's
	// recovery is CPU-bound (channel restart, tool catalogue), and ten of them at
	// once on 8 vCPUs took every wake to ~18 s where four at a time take 3–5 s.
	MaxRecovering int
	NoiseBytes    int64 // per-sample traffic ignored as background
	// IdleCPUPct: a running cell whose CPU use over the sample is above this
	// (percent of one core) counts as active even with no traffic. Measured:
	// after a thaw OpenClaw runs cron catch-up and maintenance at ~20 % of a
	// core for 30–60 s, then ~5 % steady; pausing inside that minute made the
	// next thaw resume the interrupted work first (wakes 3–4x slower).
	// 0 = default (10), negative = off.
	IdleCPUPct float64
	// MinAwake: never hibernate a cell within this long of its last wake.
	// OpenClaw runs cron catch-up, database verification and memory
	// maintenance after a thaw, some of it I/O-bound and invisible to the CPU
	// gate; a cell paused inside that window resumes it on the next thaw and
	// wakes 3–4x slower. Measured clean after ~3 minutes awake.
	// 0 = default (3 m), negative = off.
	MinAwake time.Duration
	// MaxRestarts is the always-on self-heal budget per cell. The count is
	// forgiven once a cell has stayed up for restartForgiveAfter, so the
	// budget bounds crash loops, not a cell's lifetime.
	MaxRestarts int
	PreWake     time.Duration // wake a hibernated cell this long before NextDueAt
	// MaintainEvery, when > 0, turns on the maintenance rotation: the
	// longest-unwoken hibernated cell is woken on a pace derived from the
	// fleet (MaintainEvery / candidates), so every cell is reached at least
	// this often even when nothing is ever sent to it. Without it a cell
	// nobody messages never runs its own internal schedule at all. OpenClaw
	// coalesces the ticks a cell missed into one catch-up run on its next
	// wake, so one wake per interval is enough for memory consolidation,
	// skill review and a heartbeat turn; it does not make timed user-facing
	// work land on time, which is what NextDueAt and PreWake are for.
	// Lowest-priority work on the host: it stands aside for wakes in flight
	// and for the headroom policy. 0 = off.
	MaintainEvery time.Duration
	// MaxPause caps how long a cell stays frozen. OpenClaw tolerated a 48 s
	// freeze but restarted itself after ~3 h (lease constants 125 s / 30 min);
	// beyond the cap the cell is pulsed (unpaused for PulseWindow so its
	// heartbeat runs, then re-paused) or, with PauseFallthrough="stop",
	// stopped instead.
	MaxPause         time.Duration
	PulseWindow      time.Duration
	PauseFallthrough string // "pulse" (default) or "stop"
	// ReclaimAfter: once a cell has been paused this long, push its memory to
	// swap via cgroup memory.reclaim (Linux, cgroup v2). 0 disables. Measured
	// locally: ~790 → ~20 MiB resident; wake then pages in from the swap
	// device (zram/NVMe on a fleet host, ~17 s on a laptop swap file).
	ReclaimAfter time.Duration
	// ThawSettle bounds how long a wake waits, after /health answers, for the
	// gateway to finish its own post-thaw recovery. OpenClaw exposes no
	// readiness signal for this (its /ready is a liveness alias), but it logs
	// the recovery with millisecond timestamps, so the wait keys on the
	// cell's own log rather than on a fixed delay: it ends as soon as the
	// log shows recovery has run (or shows no recovery was triggered), and
	// gives up after ThawSettle. Measured: 22 ms to ~800 ms; a 1 s bound
	// produced no aborted first turn in 15 real wakes.
	ThawSettle time.Duration
	// ReclaimWakeTimeout bounds readiness after unpausing a cell whose memory
	// was reclaimed to swap (measured 9–17 s on a laptop swap file; faster
	// on zram/NVMe). Larger than WakeTimeout, smaller than a full boot.
	ReclaimWakeTimeout time.Duration
	// ReclaimKeep is the resident floor left in RAM when reclaiming (bytes).
	// 0 reclaims everything reclaimable (max saving, slowest wake); a value
	// near the gateway's working set keeps wakes near pause speed.
	ReclaimKeep int64
	// WarmKeep, when > 0, adds a first reclaim stage: a paused cell is trimmed
	// to this floor at once (the idle gateway's working set, ~300 MiB
	// measured), the pages still resident are recorded as its hot set, and
	// only later (ReclaimAfter, or memory pressure) is it taken down to
	// ReclaimKeep. A cold wake then prefetches the hot set alone instead of
	// everything that was swapped: fewer bytes from the disk per wake.
	WarmKeep int64
	// HotSetDir is where hot sets are kept (one JSON file per cell). Empty
	// disables recording and every cold wake prefetches everything.
	HotSetDir string
	// PrefetchOnWake pages a reclaimed cell's memory back in bulk before the
	// unpause, using the swap device's bandwidth instead of its latency.
	PrefetchOnWake bool
	// Headroom is the MemAvailable the host should keep (bytes). When the
	// host is below it, the longest-paused resident cells are reclaimed
	// first, before their ReclaimAfter clock; cells stay resident (wake in
	// ~0.2 s) while memory is plentiful. Measured on a 16 GB host: a burst
	// of wakes into a host with no headroom paid page-in plus eviction
	// (25 s for the first wave); with headroom, page-in only. 0 disables.
	Headroom int64
	// MemAvailable reports the host's available memory in bytes; false when
	// the host cannot say (non-Linux). Default reads /proc/meminfo.
	MemAvailable func() (int64, bool)
	Reclaimer    *reclaim.Reclaimer
	// Probe reports whether the cell's gateway is ready to serve. The
	// default issues GET http://127.0.0.1:<port>/health and requires 200.
	// A bare TCP connect is deliberately not used: Docker Desktop's port
	// proxy accepts connections before the process listens.
	Probe  func(ctx context.Context, port int) error
	Now    func() time.Time
	Logger *slog.Logger
}

func (o *Options) defaults() {
	if o.Interval == 0 {
		o.Interval = 30 * time.Second
	}
	if o.StopGrace == 0 {
		o.StopGrace = 10 * time.Second
	}
	if o.WakeTimeout == 0 {
		o.WakeTimeout = 15 * time.Second
	}
	if o.StopWakeTimeout == 0 {
		o.StopWakeTimeout = 3 * time.Minute // OpenClaw gateway measured 50–90 s to ready after start
	}
	if o.MaxRecovering <= 0 {
		o.MaxRecovering = 8
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 4 // hot-set wakes read ~270 MiB: four in flight stay under a cloud volume's bandwidth (measured 2026-10-02); sequencing still gives earlier cells earlier wakes
	}
	if o.MemAvailable == nil {
		o.MemAvailable = memAvailable
	}
	if o.NoiseBytes == 0 {
		o.NoiseBytes = 2048
	}
	if o.IdleCPUPct == 0 {
		o.IdleCPUPct = 10
	}
	if o.MinAwake == 0 {
		o.MinAwake = 3 * time.Minute
	}
	if o.MinAwake < 0 {
		o.MinAwake = 0
	}
	if o.IdleCPUPct < 0 {
		o.IdleCPUPct = 0
	}
	if o.MaxRestarts == 0 {
		o.MaxRestarts = 5
	}
	if o.PreWake == 0 {
		o.PreWake = 2 * time.Minute // covers a stop-tier boot (45–90 s measured)
	}
	if o.MaxPause == 0 {
		o.MaxPause = 20 * time.Minute // under the 30 min lease constant; measure the real threshold on Linux
	}
	if o.MaxPause < 0 {
		o.MaxPause = 0 // explicit "never cap" (experiments, or hosts that measured a longer tolerance)
	}
	if o.PulseWindow == 0 {
		o.PulseWindow = 5 * time.Second
	}
	if o.PauseFallthrough == "" {
		o.PauseFallthrough = "pulse"
	}
	if o.Reclaimer == nil {
		o.Reclaimer = &reclaim.Reclaimer{}
	}
	if o.ReclaimWakeTimeout == 0 {
		o.ReclaimWakeTimeout = 2 * time.Minute
	}
	if o.ThawSettle == 0 {
		o.ThawSettle = time.Second // 2026-09-29: 15/15 real turns clean at 1.5 s, 1 s and 0.5 s bounds; 1 s keeps margin over the measured 0.8 s window
	}
	if o.Probe == nil {
		o.Probe = HTTPHealthProbe
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// HTTPHealthProbe is the default readiness check: GET /health must return 200.
func HTTPHealthProbe(ctx context.Context, port int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/health", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned %d", res.StatusCode)
	}
	return nil
}

// Supervisor owns the loop.
type Supervisor struct {
	opt      Options
	reg      *registry.Store
	rt       runtime.Client
	sem      chan struct{}
	recov    chan struct{} // cells between unpause and ready+settle
	mu       sync.Mutex
	victims  map[string]bool        // cells chosen for reclaim under memory pressure, set per reconcile pass
	inflt    map[string]*wakeShare  // per-cell in-flight wake, so concurrent wakes coalesce and share the outcome
	cell     map[string]*sync.Mutex // per-cell lock: wake, hibernate, reclaim, pulse never interleave on one cell
	wakeWant map[string]bool        // a wake is waiting for this cell's lock; reclaim yields between chunks
	recon    chan struct{}          // reconcile pool, separate from the wake semaphore (a reconcile goroutine may itself wake)
	pulsing  int                    // max-pause pulses in flight: thaws too, so pressure reclaim yields to them as to wakes
	// pressureDeferred counts consecutive passes on which pressure reclaim
	// stood aside for wakes in flight; bounded, so steady inbound traffic
	// cannot starve the headroom policy.
	pressureDeferred atomic.Int32
	// lastMaintain is when the maintenance rotation last started a wake, and
	// maintainSkip holds cells whose maintenance wake failed, so one broken
	// cell cannot sit at the head of the rotation and block it. Both are
	// in-memory: a restart costs at most one extra rotation wake.
	lastMaintain time.Time
	maintainSkip map[string]time.Time
	active       sync.WaitGroup // wakes and reconcile passes in flight, for Drain
	m            *Metrics
}

// maxPressureDefer is how many consecutive passes pressure reclaim may
// yield to wakes in flight before it runs regardless (30 s at the shipped
// 5 s interval). Reclaiming under a burst slows the burst (measured 10–15 s
// wakes instead of 3–4 s); never reclaiming under steady traffic lets the
// host run out of headroom, which is worse (20 s+ cold wakes).
const maxPressureDefer = 6

// minMaintainPace floors the maintenance rotation's pace when MinAwake is
// disabled, so rotation wakes still cannot stack up on a large fleet.
const minMaintainPace = time.Minute

// restartForgiveAfter is how long an always-on cell must stay up before its
// self-heal restarts stop counting against MaxRestarts.
const restartForgiveAfter = 10 * time.Minute

// wakeShare lets coalesced callers observe the leader's real outcome.
type wakeShare struct {
	done chan struct{}
	res  WakeResult
	err  error
}

func (s *Supervisor) wakeInFlight(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inflt[name]
	return ok
}

// lockCell returns the mutex for a cell, creating it on first use.
func (s *Supervisor) lockCell(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.cell[name]
	if !ok {
		l = &sync.Mutex{}
		s.cell[name] = l
	}
	return l
}

// Metrics exposes the supervisor's counters (for the ingress /metrics handler).
func (s *Supervisor) Metrics() *Metrics { return s.m }

// Cells lists the registry (for metrics gauges).
func (s *Supervisor) Cells() []registry.Cell { return s.reg.List() }

// New builds a Supervisor.
func New(reg *registry.Store, rt runtime.Client, opt Options) *Supervisor {
	opt.defaults()
	return &Supervisor{opt: opt, reg: reg, rt: rt, sem: make(chan struct{}, opt.MaxConcurrent), recov: make(chan struct{}, opt.MaxRecovering),
		recon: make(chan struct{}, opt.MaxConcurrent*4),
		inflt: map[string]*wakeShare{}, cell: map[string]*sync.Mutex{}, wakeWant: map[string]bool{},
		// start the rotation one pace in, not at boot, when the host is busiest
		lastMaintain: opt.Now(), maintainSkip: map[string]time.Time{}, m: newMetrics()}
}

// Run loops until ctx is done. A pass is bounded by its slowest action (a
// stop-tier boot can take minutes), so passes may overlap: a tick that
// arrives while one pass is still waiting starts another, which samples the
// cells the first one is not holding. At most two run at once.
func (s *Supervisor) Run(ctx context.Context) error {
	t := time.NewTicker(s.opt.Interval)
	defer t.Stop()
	passes := make(chan struct{}, 2)
	for {
		select {
		case passes <- struct{}{}:
			go func() {
				defer func() { <-passes }()
				s.ReconcileOnce(ctx)
			}()
		default: // two passes already in flight: this tick waits
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Drain waits for wakes and reconcile actions in flight to finish, or for
// ctx. Called at shutdown so a tenant's wake is not killed half-way.
func (s *Supervisor) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.active.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReconcileOnce inspects every cell and acts, cells in parallel under a
// bounded pool, so one slow action (a 30 s reclaim, a 90 s stop-tier boot)
// never stalls the others. A cell already being acted on (e.g. a wake in
// flight from the ingress) is skipped this pass rather than waited for.
func (s *Supervisor) ReconcileOnce(ctx context.Context) {
	s.active.Add(1)
	defer s.active.Done()
	cells := s.reg.List()
	// Pressure reclaim yields to wakes in flight: evicting warm cells while a
	// burst is paging in only adds CPU and I/O to the burst (measured: wakes
	// that overlapped a pressure reclaim took 10–15 s instead of 3–4 s), and
	// the kernel's own reclaim already keeps the wakes fed. The target is
	// restored on the first quiet pass after the burst, or after
	// maxPressureDefer busy passes when the traffic never goes quiet.
	var victims map[string]bool
	if s.anyWakeInFlight() && s.pressureDeferred.Load() < maxPressureDefer {
		s.pressureDeferred.Add(1)
	} else {
		s.pressureDeferred.Store(0)
		victims = s.pressureVictims(ctx, cells)
	}
	s.mu.Lock()
	s.victims = victims
	s.mu.Unlock()
	// One runtime snapshot for the whole pass instead of an inspect per cell.
	names := make([]string, 0, len(cells))
	for _, c := range cells {
		names = append(names, c.Container)
	}
	snap, err := s.rt.Snapshot(ctx, names)
	if err != nil {
		s.opt.Logger.Warn("reconcile: runtime snapshot", "err", err)
		return // nothing can be decided without the runtime; next pass retries
	}
	var wg sync.WaitGroup
	for _, c := range cells {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Pool slot first, cell lock second: a goroutine queued for a slot
			// must not hold its cell's lock, or an inbound wake for that cell
			// waits behind unrelated cells' slow actions.
			select {
			case s.recon <- struct{}{}: // reconcile pool: never the wake semaphore, which a reconcile-driven wake needs
				defer func() { <-s.recon }()
			case <-ctx.Done():
				return
			}
			l := s.lockCell(c.Name)
			if !l.TryLock() {
				return // busy: wake/hibernate in progress
			}
			defer l.Unlock()
			// The snapshot may predate a wake that finished while this
			// goroutine queued; decide from the cell as it is now.
			fresh, err := s.reg.Get(c.Name)
			if err != nil {
				return // removed meanwhile
			}
			c = fresh
			// A persisted Waking phase with no wake in flight is a crash
			// leftover; adopt the runtime's real state instead of honouring it.
			if c.Phase == registry.PhaseWaking && !s.wakeInFlight(c.Name) {
				c.Phase = registry.PhaseActive
				_ = s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseActive })
			}
			info, ok := snap[c.Container]
			if !ok { // container renamed since the snapshot: ask for it alone
				if info, err = s.rt.Info(ctx, c.Container); err != nil {
					s.opt.Logger.Warn("reconcile", "cell", c.Name, "err", err)
					return
				}
			}
			if err := s.reconcileCell(ctx, c, info); err != nil {
				s.opt.Logger.Warn("reconcile", "cell", c.Name, "err", err)
				_ = s.reg.Update(c.Name, func(x *registry.Cell) { x.LastError = err.Error() })
			}
		}()
	}
	wg.Wait()
	if s.opt.MaintainEvery > 0 { // guard before List: a registry copy per pass is not free
		s.maintenanceWake(ctx, s.reg.List())
	}
}

func (s *Supervisor) reconcileCell(ctx context.Context, c registry.Cell, info runtime.Info) error {
	// A recurring due time that slipped past its window (its wakes kept
	// failing) is moved on, so the schedule survives one bad morning; a
	// one-shot that slipped stays as a record and is ignored by dueSoon.
	if c.NextDueEvery > 0 && !c.NextDueAt.IsZero() && c.NextDueAt.Before(s.opt.Now().Add(-s.opt.PreWake)) {
		_ = s.advanceDue(c.Name)
	}
	switch info.State {
	case runtime.StateMissing:
		return s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseFailed; x.LastError = "container missing" })
	case runtime.StateRunning:
		return s.observeRunning(ctx, c, info.Pid)
	case runtime.StatePaused:
		if s.dueSoon(c) {
			return s.wakeDue(ctx, c)
		}
		if c.Phase == registry.PhaseHibernated && !c.PausedAt.IsZero() && s.opt.MaxPause > 0 && s.opt.Now().Sub(c.PausedAt) >= s.pauseCap(c.Name) {
			return s.capPause(ctx, c)
		}
		if c.Phase == registry.PhaseHibernated && !c.PausedAt.IsZero() {
			switch f := floorOf(c); {
			case f == floorCold:
				// done for this freeze
			case f == floorResident && s.opt.WarmKeep > 0:
				return s.warmCell(ctx, c) // stage one: trim to the warm floor now, record the hot set
			default:
				timed := s.opt.ReclaimAfter > 0 && s.opt.Now().Sub(c.PausedAt) >= s.opt.ReclaimAfter
				if timed || s.isVictim(c.Name) {
					return s.reclaimCell(ctx, c) // stage two: down to the cold floor
				}
			}
		}
		if c.Phase == registry.PhaseHibernated {
			return nil // expected
		}
		if c.Phase == registry.PhaseFailed && !c.PausedAt.IsZero() {
			// a wake failed mid-pause: keep the original pause clock so the
			// cap and the once-per-freeze reclaim still hold; retry on demand
			return s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseHibernated })
		}
		// paused by someone else, age unknown: adopt it as hibernated with
		// its clock already expired, so the next pass pulses it (cap on) or
		// reclaims it at once (cap off) rather than trusting a clock we
		// never saw start.
		back := s.opt.MaxPause
		if back <= 0 || (s.opt.ReclaimAfter > 0 && s.opt.ReclaimAfter > back) {
			back = s.opt.ReclaimAfter
		}
		return s.reg.Update(c.Name, func(x *registry.Cell) {
			x.Phase = registry.PhaseHibernated
			x.PausedAt = s.opt.Now().Add(-back)
			x.ReclaimedAt = time.Time{}
			x.WarmAt = time.Time{}
			x.Swapped = false
		})
	case runtime.StateExited, runtime.StateCreated:
		if c.Phase == registry.PhaseHibernated && s.dueSoon(c) {
			return s.wakeDue(ctx, c)
		}
		if c.Phase == registry.PhaseHibernated {
			return nil // expected
		}
		if c.Class == registry.ClassAlwaysOn {
			return s.selfHeal(ctx, c)
		}
		// hibernate-class cell that exited on its own: treat as hibernated; wake on demand
		return s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseHibernated })
	}
	return nil
}

// wakeDue wakes a hibernated cell for its scheduled job and only then moves
// the due time on, so a wake that fails is retried on the next pass instead
// of the schedule being consumed by the attempt.
func (s *Supervisor) wakeDue(ctx context.Context, c registry.Cell) error {
	_, err := s.wakeLocked(ctx, c.Name)
	if err == nil {
		_ = s.advanceDue(c.Name)
	}
	return err
}

func (s *Supervisor) observeRunning(ctx context.Context, c registry.Cell, pid int) error {
	st, err := s.rt.Stats(ctx, c.Container, pid)
	if err != nil {
		return err
	}
	cur := idle.Sample{At: s.opt.Now(), RxBytes: st.Net.RxBytes, TxBytes: st.Net.TxBytes}
	prev := idle.Sample{RxBytes: c.RxBytes, TxBytes: c.TxBytes} // last persisted counters
	d := idle.Evaluate(prev, cur, c.LastActivity, c.IdleAfter, s.opt.NoiseBytes)
	if s.opt.IdleCPUPct > 0 && st.CPUPct > s.opt.IdleCPUPct {
		d.LastActivity = cur.At // busy on CPU (post-thaw maintenance, cron): not idle yet
		d.ShouldSleep = false
	}
	forgive := c.Restarts > 0 && !c.WokeAt.IsZero() && cur.At.Sub(c.WokeAt) >= restartForgiveAfter
	// Persist only when something moved: every Update rewrites and fsyncs
	// the registry file, and a quiet host has nothing new to say.
	if c.Phase != registry.PhaseActive || !d.LastActivity.Equal(c.LastActivity) || cur.RxBytes != c.RxBytes || cur.TxBytes != c.TxBytes || c.LastError != "" || forgive {
		if err := s.reg.Update(c.Name, func(x *registry.Cell) {
			x.Phase = registry.PhaseActive
			x.LastActivity = d.LastActivity
			x.RxBytes, x.TxBytes = cur.RxBytes, cur.TxBytes
			x.LastError = ""
			if forgive {
				x.Restarts = 0 // up long enough: not a crash loop
			}
		}); err != nil {
			return err
		}
	}
	if d.ShouldSleep && c.Class == registry.ClassHibernate && !s.jobInsideIdleWindow(c) {
		if s.opt.MinAwake > 0 && !c.WokeAt.IsZero() && s.opt.Now().Sub(c.WokeAt) < s.opt.MinAwake {
			return nil // let the gateway finish its post-thaw work first
		}
		return s.hibernateLocked(ctx, c.Name)
	}
	return nil
}

// checkpointWAL truncates the stopped cell's SQLite WAL from the host, if
// its state directory is a bind mount we can see. Volumes are skipped.
func (s *Supervisor) checkpointWAL(ctx context.Context, c registry.Cell) {
	mounts, err := s.rt.Mounts(ctx, c.Container)
	if err != nil {
		s.opt.Logger.Warn("wal checkpoint: mounts", "cell", c.Name, "err", err)
		return
	}
	for _, m := range mounts {
		if m.Destination != walcheck.StatePathInContainer || m.Type != "bind" {
			continue
		}
		res, err := walcheck.Checkpoint(ctx, m.Source, nil)
		for _, r := range res {
			s.opt.Logger.Info("wal checkpoint", "cell", c.Name, "db", r.Path, "wal_before", r.WALBefore, "wal_after", r.WALAfter, "skipped", r.Skipped)
		}
		if err != nil {
			s.opt.Logger.Warn("wal checkpoint", "cell", c.Name, "err", err)
		}
		return
	}
	s.opt.Logger.Info("wal checkpoint skipped: state dir is not a host bind mount", "cell", c.Name)
}

// pressureVictims picks the paused, still-resident cells to reclaim when the
// host is below its memory headroom: longest paused first, enough of them
// (by their cgroup's resident bytes) to cover the deficit. Nil when the
// headroom is off, unknown, or met.
func (s *Supervisor) pressureVictims(ctx context.Context, cells []registry.Cell) map[string]bool {
	if s.opt.Headroom <= 0 || s.opt.Reclaimer == nil {
		return nil
	}
	avail, ok := s.opt.MemAvailable()
	if !ok || avail >= s.opt.Headroom {
		return nil
	}
	deficit := s.opt.Headroom - avail
	var cand []registry.Cell
	for _, c := range cells {
		if c.Phase == registry.PhaseHibernated && !c.PausedAt.IsZero() && floorOf(c) != floorCold {
			cand = append(cand, c)
		}
	}
	if len(cand) == 0 {
		return nil
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].PausedAt.Before(cand[j].PausedAt) })
	victims := map[string]bool{}
	var covered int64
	for _, c := range cand {
		size := int64(512 << 20) // when the cgroup cannot be read, assume a typical idle gateway
		if id, err := s.rt.ID(ctx, c.Container); err == nil {
			if st, err := s.opt.Reclaimer.Stats(id); err == nil && st.CurrentBytes > 0 {
				size = st.CurrentBytes
			}
		}
		victims[c.Name] = true
		covered += size
		if covered >= deficit {
			break
		}
	}
	s.opt.Logger.Info("memory headroom below target: reclaiming longest-paused cells", "available_mib", avail>>20, "target_mib", s.opt.Headroom>>20, "cells", len(victims))
	return victims
}

// maintenanceWake is the rotation: it wakes the longest-unwoken hibernated
// cell so that a cell nobody ever messages still runs its own internal
// schedule. OpenClaw coalesces the ticks a cell missed into a single catch-up
// run on its next wake, so one wake per MaintainEvery is enough to keep memory
// consolidation, the weekly skill review and one heartbeat turn happening.
// What it deliberately does not do is make timed user-facing work land on
// time: a reminder reached only by the rotation could be most of a day late,
// which is what NextDueAt and PreWake exist for.
//
// It is the lowest-priority work on the host. Maintenance that is a few hours
// late costs nothing, so it stands aside for any wake in flight and for the
// headroom policy instead of competing with them for memory and page-in
// bandwidth.
func (s *Supervisor) maintenanceWake(ctx context.Context, cells []registry.Cell) {
	if s.opt.MaintainEvery <= 0 || ctx.Err() != nil {
		return // shutting down: a maintenance wake has no tenant waiting on it
	}
	now := s.opt.Now()
	// Both sampled before the lock: anyWakeInFlight takes s.mu itself (it is
	// not reentrant), and MemAvailable reads /proc, which has no business
	// inside the critical section. A slightly stale read is harmless for
	// best-effort work.
	busy := s.anyWakeInFlight()
	tight := false
	if s.opt.Headroom > 0 {
		if avail, ok := s.opt.MemAvailable(); ok && avail < s.opt.Headroom {
			tight = true
		}
	}
	s.mu.Lock()
	var cand []registry.Cell
	seen := make(map[string]bool, len(cells))
	for _, c := range cells {
		seen[c.Name] = true
		if c.Class == registry.ClassAlwaysOn || c.Phase != registry.PhaseHibernated {
			continue
		}
		if until, ok := s.maintainSkip[c.Name]; ok && now.Before(until) {
			continue // its last maintenance wake failed; let it rest
		}
		if s.dueSoon(c) {
			continue // about to wake for its own schedule anyway
		}
		cand = append(cand, c)
	}
	// Drop holds that have expired or that name a cell the operator removed,
	// so the map tracks the fleet rather than growing with it.
	for n, until := range s.maintainSkip {
		if !seen[n] || !now.Before(until) {
			delete(s.maintainSkip, n)
		}
	}
	if len(cand) == 0 {
		s.mu.Unlock()
		return
	}
	sort.Slice(cand, func(i, j int) bool { return maintainAge(cand[i]).Before(maintainAge(cand[j])) })
	c := cand[0]
	// Yield to real work only while the rotation is ahead of schedule. Once
	// the oldest candidate is actually overdue, stop yielding: a host that is
	// always busy, or that sits at its headroom target (which is what the
	// headroom policy is for), would otherwise never maintain a single cell
	// and the feature would silently do nothing. This is the starvation the
	// pressure path bounds with maxPressureDefer; here the bound is the
	// guarantee the flag makes, so it is expressed in the same units.
	if (busy || tight) && now.Sub(maintainAge(c)) < s.opt.MaintainEvery {
		s.mu.Unlock()
		return
	}
	// Pace from the fleet, so the target interval holds at any cell count.
	// Floored so rotation wakes cannot stack up: a wake keeps its cell awake
	// for at least MinAwake, and when the operator has turned MinAwake off a
	// fixed floor still applies. On a fleet large enough to reach the floor
	// the effective interval is longer than MaintainEvery, which is the safe
	// direction to be wrong in.
	pace := s.opt.MaintainEvery / time.Duration(len(cand))
	floor := s.opt.MinAwake
	if floor <= 0 {
		floor = minMaintainPace
	}
	if pace < floor {
		pace = floor
	}
	if now.Sub(s.lastMaintain) < pace {
		s.mu.Unlock()
		return
	}
	s.lastMaintain = now
	s.mu.Unlock()
	s.active.Add(1)
	go func() {
		defer s.active.Done()
		s.opt.Logger.Info("maintenance wake", "cell", c.Name,
			"asleep_for", now.Sub(maintainAge(c)).Round(time.Second), "pace", pace.Round(time.Second), "candidates", len(cand))
		if _, err := s.Wake(ctx, c.Name); err != nil {
			// Hold this cell out of the rotation for one full interval, so a
			// cell that cannot wake does not sit at the head of the queue and
			// starve every other cell of its maintenance.
			s.mu.Lock()
			s.maintainSkip[c.Name] = s.opt.Now().Add(s.opt.MaintainEvery)
			s.mu.Unlock()
			s.opt.Logger.Warn("maintenance wake", "cell", c.Name, "err", err)
			return
		}
		s.mu.Lock()
		delete(s.maintainSkip, c.Name)
		s.mu.Unlock()
		s.m.maintain()
	}()
}

// maintainAge is when a cell was last awake, for the rotation's ordering: its
// last successful wake, else when its current pause began, else when its row
// was last written. A cell woken by real traffic sorts last and so never
// consumes a rotation slot.
func maintainAge(c registry.Cell) time.Time {
	if !c.WokeAt.IsZero() {
		return c.WokeAt
	}
	if !c.PausedAt.IsZero() {
		return c.PausedAt
	}
	return c.UpdatedAt
}

func (s *Supervisor) anyWakeInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflt) > 0 || s.pulsing > 0
}

func (s *Supervisor) pulseInFlight(delta int) {
	s.mu.Lock()
	s.pulsing += delta
	s.mu.Unlock()
}

// pauseCap is MaxPause less a per-cell offset of up to a quarter of it, so
// cells frozen together (a fleet fill, a restart) do not all reach the cap
// on the same pass and thaw as one cohort every cycle after. Stable per
// cell, so the spacing persists.
func (s *Supervisor) pauseCap(name string) time.Duration {
	if s.opt.MaxPause <= 0 {
		return s.opt.MaxPause
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	spread := s.opt.MaxPause / 4
	return s.opt.MaxPause - time.Duration(uint64(h.Sum32())%uint64(spread))
}

func (s *Supervisor) isVictim(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.victims[name]
}

// warmCell trims a paused cell to the warm floor and records which pages the
// kernel kept: the cell's hot set. Cheap when the cell has been reclaimed
// before (its cold pages are clean copies of what is already in swap).
func (s *Supervisor) warmCell(ctx context.Context, c registry.Cell) error {
	id, err := s.rt.ID(ctx, c.Container)
	if err != nil {
		return err
	}
	before, _ := s.opt.Reclaimer.Stats(id)
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stop := func() bool { return s.wakeWanted(c.Name) }
	after, err := s.opt.Reclaimer.ReclaimKeeping(rctx, id, 8<<30, s.opt.WarmKeep, stop)
	if err != nil {
		if errors.Is(err, reclaim.ErrUnsupported) || errors.Is(err, reclaim.ErrNoCgroup) {
			return s.reg.Update(c.Name, func(x *registry.Cell) { x.WarmAt = s.opt.Now() })
		}
		return err
	}
	if s.wakeWanted(c.Name) {
		return nil // a wake is waiting: leave the clock alone, it will run again after the next pause
	}
	hot := "none"
	if s.opt.HotSetDir != "" {
		if hs, err := s.opt.Reclaimer.HotSet(id); err == nil {
			if err := s.saveHotSet(c.Name, hs); err == nil {
				hot = fmt.Sprintf("%d MiB in %d procs", hs.Bytes>>20, len(hs.PIDs))
			} else {
				s.opt.Logger.Warn("hot set not saved", "cell", c.Name, "err", err)
			}
		} else if !errors.Is(err, reclaim.ErrUnsupported) {
			s.opt.Logger.Warn("hot set", "cell", c.Name, "err", err)
		}
	}
	moved := before.CurrentBytes - after.CurrentBytes
	s.m.reclaimed(moved)
	s.opt.Logger.Info("warmed", "cell", c.Name, "before_mib", before.CurrentBytes>>20, "after_mib", after.CurrentBytes>>20, "swap_mib", after.SwapBytes>>20, "hot_set", hot)
	return s.reg.Update(c.Name, func(x *registry.Cell) {
		x.WarmAt = s.opt.Now()
		x.Swapped = x.Swapped || moved > 0 || after.SwapBytes > 0
	})
}

func (s *Supervisor) hotSetPath(name string) string {
	return filepath.Join(s.opt.HotSetDir, name+".json")
}

func (s *Supervisor) saveHotSet(name string, hs reclaim.HotSet) error {
	if err := os.MkdirAll(s.opt.HotSetDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(hs)
	if err != nil {
		return err
	}
	tmp := s.hotSetPath(name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.hotSetPath(name))
}

func (s *Supervisor) loadHotSet(name string) (reclaim.HotSet, bool) {
	if s.opt.HotSetDir == "" {
		return reclaim.HotSet{}, false
	}
	b, err := os.ReadFile(s.hotSetPath(name))
	if err != nil {
		return reclaim.HotSet{}, false
	}
	var hs reclaim.HotSet
	if json.Unmarshal(b, &hs) != nil || len(hs.PIDs) == 0 {
		return reclaim.HotSet{}, false
	}
	return hs, true
}

// reclaimCell pushes a paused cell's memory to swap. Unsupported hosts are
// logged once per cell and never retried until the next pause.
func (s *Supervisor) reclaimCell(ctx context.Context, c registry.Cell) error {
	id, err := s.rt.ID(ctx, c.Container)
	if err != nil {
		return err
	}
	before, _ := s.opt.Reclaimer.Stats(id)
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	stop := func() bool { return s.wakeWanted(c.Name) }
	after, err := s.opt.Reclaimer.ReclaimKeeping(rctx, id, 8<<30, s.opt.ReclaimKeep, stop)
	if err != nil {
		if errors.Is(err, reclaim.ErrUnsupported) || errors.Is(err, reclaim.ErrNoCgroup) {
			s.opt.Logger.Info("reclaim not possible on this host; leaving cell resident", "cell", c.Name, "reason", err)
			return s.reg.Update(c.Name, func(x *registry.Cell) { x.ReclaimedAt = s.opt.Now(); x.Swapped = false })
		}
		return err
	}
	if s.wakeWanted(c.Name) && after.CurrentBytes >= before.CurrentBytes {
		return nil // interrupted before anything moved; leave it re-armed
	}
	moved := before.CurrentBytes - after.CurrentBytes
	s.m.reclaimed(moved)
	s.opt.Logger.Info("reclaimed", "cell", c.Name, "before_mib", before.CurrentBytes>>20, "after_mib", after.CurrentBytes>>20, "swap_mib", after.SwapBytes>>20)
	return s.reg.Update(c.Name, func(x *registry.Cell) { x.ReclaimedAt = s.opt.Now(); x.Swapped = moved > 0 || after.SwapBytes > 0 })
}

// prefetch pages a reclaimed cell's memory back in bulk before it is
// unpaused. Best effort: a failure just means the gateway faults its pages
// in itself, as it would without prefetch.
func (s *Supervisor) prefetch(ctx context.Context, c registry.Cell) {
	id, err := s.rt.ID(ctx, c.Container)
	if err != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	before, _ := s.opt.Reclaimer.Stats(id)
	scope := "all"
	var st reclaim.PrefetchStats
	if hs, ok := s.loadHotSet(c.Name); ok && hotSetCurrent(c) {
		st, err = s.opt.Reclaimer.PrefetchHot(pctx, id, hs)
		scope = "hot"
		if err != nil {
			// Stale (the cell restarted) or unusable (an advise failed):
			// either way the bulk page-in must still happen, so fall back to
			// everything that is in swap rather than faulting it in one page
			// at a time.
			reason := "stale"
			if !errors.Is(err, reclaim.ErrStaleHotSet) {
				reason = "unusable"
				s.opt.Logger.Warn("hot set prefetch failed; prefetching everything", "cell", c.Name, "err", err)
			}
			_ = os.Remove(s.hotSetPath(c.Name))
			st, err = s.opt.Reclaimer.Prefetch(pctx, id, 0)
			scope = "all (hot set " + reason + ")"
		}
	} else {
		st, err = s.opt.Reclaimer.Prefetch(pctx, id, 0)
	}
	if err != nil {
		s.opt.Logger.Warn("prefetch", "cell", c.Name, "scope", scope, "err", err, "mechanism", st.Mechanism)
		return
	}
	expect := st.SwapBefore
	if scope == "hot" {
		// Only the hot set is coming back, and the pages still resident at
		// the cold floor are its hottest part, already here: wait for the
		// difference, or the wait can only end on the stall detector.
		hot := st.Bytes - before.CurrentBytes
		if hot < 0 {
			hot = 0
		}
		if hot < expect {
			expect = hot
		}
	}
	landed, wait := s.waitPagedIn(pctx, id, before.CurrentBytes, expect)
	s.opt.Logger.Info("prefetched", "cell", c.Name, "scope", scope, "mechanism", st.Mechanism, "procs", st.Processes, "mappings", st.Mappings, "advised_mib", st.Bytes>>20, "swap_before_mib", st.SwapBefore>>20, "landed_mib", landed>>20, "advise_took", st.Took, "pagein_took", wait)
}

// waitPagedIn blocks until the advised pages have actually arrived. The
// advise returns once the reads are issued, not completed; holding the
// concurrency slot until the cgroup's resident bytes stop growing is what
// sequences page-ins on a bandwidth-bound swap device, so earlier cells
// wake earlier instead of every cell in a burst finishing together.
// Swap usage cannot be watched instead: a page read back keeps its swap
// slot until it is rewritten. Returns bytes landed and the time waited.
func (s *Supervisor) waitPagedIn(ctx context.Context, id string, residentBefore, swapped int64) (int64, time.Duration) {
	t0 := time.Now()
	if swapped <= 0 {
		return 0, 0
	}
	target := residentBefore + swapped*9/10 // most of what was out is back
	last, stall := int64(-1), 0
	deadline := time.After(30 * time.Second)
	for {
		st, err := s.opt.Reclaimer.Stats(id)
		if err != nil {
			return 0, time.Since(t0)
		}
		if st.CurrentBytes >= target {
			return st.CurrentBytes - residentBefore, time.Since(t0)
		}
		if last >= 0 && st.CurrentBytes-last < 4<<20 {
			if stall++; stall >= 5 { // no progress for ~0.5 s: the device is done with us
				return st.CurrentBytes - residentBefore, time.Since(t0)
			}
		} else {
			stall = 0
		}
		last = st.CurrentBytes
		select {
		case <-ctx.Done():
			return st.CurrentBytes - residentBefore, time.Since(t0)
		case <-deadline:
			return st.CurrentBytes - residentBefore, time.Since(t0)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// capPause handles a cell frozen longer than MaxPause. "pulse": unpause,
// give the gateway PulseWindow to run its lease/liveness heartbeat (and
// require /health to answer), then pause again with a fresh PausedAt.
// "stop": unpause then stop, so the cell sleeps on the stop tier instead.
func (s *Supervisor) capPause(ctx context.Context, c registry.Cell) error {
	frozen := s.opt.Now().Sub(c.PausedAt)
	if s.opt.PauseFallthrough == "stop" {
		if _, err := s.rt.Unpause(ctx, c.Container); err != nil {
			return err
		}
		took, err := s.rt.Stop(ctx, c.Container, s.opt.StopGrace)
		if err != nil {
			return err
		}
		s.checkpointWAL(ctx, c)
		s.m.fellThrough()
		s.opt.Logger.Info("pause cap: fell through to stop", "cell", c.Name, "frozen", frozen, "took", took)
		return s.reg.Update(c.Name, func(x *registry.Cell) { x.PausedAt = time.Time{} })
	}
	// A pulse is a thaw and is bounded like one: the page-in slot covers the
	// prefetch and the unpause, the recovery slot the time the gateway runs.
	// Without this, every cell that reached the cap in the same interval
	// thawed at once, outside both bounds.
	s.pulseInFlight(1)
	defer s.pulseInFlight(-1)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	release := sync.OnceFunc(func() { <-s.sem })
	defer release()
	if s.opt.PrefetchOnWake && reclaimed(c) {
		s.prefetch(ctx, c) // a pulse is a thaw: pay page-in in bulk, not fault by fault
	}
	select {
	case s.recov <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.recov }()
	if _, err := s.rt.Unpause(ctx, c.Container); err != nil {
		return err
	}
	release()
	if err := s.waitReady(ctx, c.Port, s.pauseWakeTimeout(c)); err != nil {
		// gateway did not come back healthy after the thaw: leave it running
		// and let the next reconcile observe it (self-exit shows as exited).
		// It just thawed, so MinAwake applies from now.
		s.opt.Logger.Warn("pause cap: pulse, gateway not ready", "cell", c.Name, "err", err)
		now := s.opt.Now()
		return s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseActive; x.PausedAt = time.Time{}; x.WokeAt = now })
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.opt.PulseWindow):
	}
	took, err := s.rt.Pause(ctx, c.Container)
	if err != nil {
		return err
	}
	s.m.pulse()
	s.opt.Logger.Info("pause cap: pulsed", "cell", c.Name, "frozen", frozen, "window", s.opt.PulseWindow, "repause", took)
	now := s.opt.Now()
	cold := floorOf(c) == floorCold
	return s.reg.Update(c.Name, func(x *registry.Cell) {
		x.PausedAt = now
		if cold {
			// A pulse brings back only what it touched; the rest is still in
			// swap. Keep the cold mark (the next wake prefetches and gets the
			// longer timeout; no second full reclaim for the same freeze) and
			// keep the hot set current with it.
			x.ReclaimedAt = now
			x.WarmAt = now
		} else {
			// Resident or warm only: the thaw pulled pages back, so the
			// stages start over from the new pause (the warm trim is cheap,
			// its cold pages are clean copies of what is already in swap).
			x.ReclaimedAt = time.Time{}
			x.WarmAt = time.Time{}
			x.Swapped = false
		}
	})
}

// floor is how far the current pause has taken a cell's memory. It is read
// from the stamps here and nowhere else, so every decision (reclaim stages,
// pressure victims, wake kind, prefetch scope, the pulse) sees one tier.
type floor int

const (
	floorResident floor = iota // nothing reclaimed in this pause
	floorWarm                  // trimmed to WarmKeep; hot set recorded
	floorCold                  // reclaimed to ReclaimKeep (or reclaim found impossible; see Swapped)
)

func floorOf(c registry.Cell) floor {
	if c.PausedAt.IsZero() {
		return floorResident
	}
	if !c.ReclaimedAt.IsZero() && !c.ReclaimedAt.Before(c.PausedAt) {
		return floorCold
	}
	if !c.WarmAt.IsZero() && !c.WarmAt.Before(c.PausedAt) {
		return floorWarm
	}
	return floorResident
}

// hotSetCurrent: the recorded hot set describes this pause (the warm stage
// ran in it, before the cold stage), so a cold wake may prefetch it alone.
func hotSetCurrent(c registry.Cell) bool {
	return floorOf(c) == floorCold && !c.WarmAt.IsZero() && !c.WarmAt.Before(c.PausedAt) && !c.ReclaimedAt.Before(c.WarmAt)
}

// reclaimed reports whether the cell's memory was actually pushed to swap
// during the current pause. The cold floor alone is not enough: it is also
// stamped when reclaim is impossible so the loop stops retrying. Only a
// reclaim that moved bytes marks the cell as swapped.
func reclaimed(c registry.Cell) bool {
	return c.Swapped && floorOf(c) == floorCold
}

// thawTriggered is the line OpenClaw's freeze detector logs the instant it
// fires after a thaw. What follows it is a recovery window during which the
// session placement's "turn settlement" is closed and a first agent turn
// aborts (the user then gets a "heartbeat failed" notice). Verified in the
// shipped code (2026.9.6): the settlement is closed by an internal closure
// and its reopening is not logged and not exposed on any endpoint, so there
// is no observable completion event. What is certain is the trigger, and a
// measured window: 22 ms to ~800 ms on this host, never past 3 s.
const thawTriggered = "host timing gap detected"

// settleThaw holds a pause-wake for the gateway's post-thaw recovery
// window. It reads the cell's own log for the trigger: if the detector did
// not fire (a short freeze), there is no window and it returns at once; if
// it did, it holds for ThawSettle, the measured bound, since the end of the
// window is not observable in this build. A readiness signal from the
// gateway would replace the bound; that is the upstream ask.
func (s *Supervisor) settleThaw(ctx context.Context, c registry.Cell, thawAt time.Time) {
	since := thawAt.Add(-2 * time.Second).Format(time.RFC3339)
	t0 := time.Now()
	for time.Since(t0) < 500*time.Millisecond { // give the detector time to fire
		out, err := s.rt.LogsSince(ctx, c.Container, since)
		if err == nil && strings.Contains(out, thawTriggered) {
			remaining := s.opt.ThawSettle - time.Since(t0)
			s.opt.Logger.Info("thaw recovery window: holding first forward", "cell", c.Name, "hold", remaining)
			select {
			case <-ctx.Done():
			case <-time.After(remaining):
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Millisecond):
		}
	}
	// no trigger logged: a freeze below the detector's threshold, nothing to wait for
}

// pauseWakeTimeout: a reclaimed cell pages in from swap and needs longer
// than a merely frozen one.
func (s *Supervisor) pauseWakeTimeout(c registry.Cell) time.Duration {
	if reclaimed(c) {
		return s.opt.ReclaimWakeTimeout
	}
	return s.opt.WakeTimeout
}

// dueSoon: a hibernated cell whose next job is within PreWake should wake now.
// A due time already in the past by more than PreWake is stale (the job has
// fired, or the operator set a one-shot) and is ignored until advanced.
func (s *Supervisor) dueSoon(c registry.Cell) bool {
	if c.NextDueAt.IsZero() {
		return false
	}
	now := s.opt.Now()
	return !c.NextDueAt.After(now.Add(s.opt.PreWake)) && c.NextDueAt.After(now.Add(-s.opt.PreWake))
}

// advanceDue moves a fired due time forward by NextDueEvery, or clears a
// one-shot, so a cell can never become permanently un-hibernatable.
func (s *Supervisor) advanceDue(name string) error {
	return s.reg.Update(name, func(x *registry.Cell) {
		if x.NextDueEvery > 0 {
			// advance past the pre-wake window so this pass's wake is not refired
			horizon := s.opt.Now().Add(s.opt.PreWake)
			for !x.NextDueAt.After(horizon) {
				x.NextDueAt = x.NextDueAt.Add(x.NextDueEvery)
			}
			return
		}
		x.NextDueAt = time.Time{}
	})
}

// jobInsideIdleWindow: never hibernate a cell that would only have to be
// woken again before its idle timeout elapsed (interim rule until cron
// schedules are read from the cell itself). Past due times do not count.
func (s *Supervisor) jobInsideIdleWindow(c registry.Cell) bool {
	if c.NextDueAt.IsZero() || c.NextDueAt.Before(s.opt.Now()) {
		return false
	}
	window := c.IdleAfter
	if window < s.opt.PreWake {
		window = s.opt.PreWake
	}
	return !c.NextDueAt.After(s.opt.Now().Add(window))
}

// Hibernate puts a cell to sleep according to its tier and records it.
func (s *Supervisor) Hibernate(ctx context.Context, name string) error {
	l := s.lockCell(name)
	l.Lock()
	defer l.Unlock()
	return s.hibernateLocked(ctx, name)
}

func (s *Supervisor) hibernateLocked(ctx context.Context, name string) error {
	c, err := s.reg.Get(name)
	if err != nil {
		return err
	}
	var took time.Duration
	switch c.Tier {
	case registry.TierStop:
		took, err = s.rt.Stop(ctx, c.Container, s.opt.StopGrace)
		if err == nil {
			s.checkpointWAL(ctx, c) // best effort; logged, never fatal
		}
	default:
		took, err = s.rt.Pause(ctx, c.Container)
	}
	if err != nil {
		return err
	}
	s.opt.Logger.Info("hibernated", "cell", name, "tier", c.Tier, "took", took)
	s.m.hibernate(string(c.Tier))
	pausedAt := time.Time{}
	if c.Tier != registry.TierStop {
		pausedAt = s.opt.Now()
	}
	return s.reg.Update(name, func(x *registry.Cell) { x.Phase = registry.PhaseHibernated; x.PausedAt = pausedAt })
}

// WakeResult reports timings a provider cares about.
type WakeResult struct {
	AlreadyRunning bool
	StartTook      time.Duration // runtime start
	ReadyTook      time.Duration // until the gateway port accepts connections
}

// Wake restores a cell and waits until it is ready. Concurrent wakes of the
// same cell coalesce; total concurrency is bounded; a wake takes the cell
// lock, so it waits for any in-progress hibernate/pulse and interrupts an
// in-progress reclaim at its next chunk (reclaim polls wakeWanted).
func (s *Supervisor) Wake(ctx context.Context, name string) (WakeResult, error) {
	// coalesce: followers wait for the leader and receive its real outcome
	s.mu.Lock()
	if sh, ok := s.inflt[name]; ok {
		s.mu.Unlock()
		select {
		case <-sh.done:
			if sh.err != nil {
				return WakeResult{}, sh.err
			}
			return WakeResult{AlreadyRunning: true, ReadyTook: sh.res.ReadyTook}, nil
		case <-ctx.Done():
			return WakeResult{}, ctx.Err()
		}
	}
	sh := &wakeShare{done: make(chan struct{})}
	s.inflt[name] = sh
	s.mu.Unlock()
	s.active.Add(1)
	defer s.active.Done()
	defer func() {
		s.mu.Lock()
		delete(s.inflt, name)
		s.mu.Unlock()
		close(sh.done)
	}()
	// The wake itself must not die with the caller (a webhook client that
	// gives up mid-boot): run it under a detached, bounded context.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opt.StopWakeTimeout+30*time.Second)
	defer cancel()
	s.setWakeWanted(name, true)
	l := s.lockCell(name)
	l.Lock()
	defer l.Unlock()
	s.setWakeWanted(name, false)
	sh.res, sh.err = s.wakeLocked(wctx, name)
	return sh.res, sh.err
}

func (s *Supervisor) setWakeWanted(name string, v bool) {
	s.mu.Lock()
	if v {
		s.wakeWant[name] = true
	} else {
		delete(s.wakeWant, name)
	}
	s.mu.Unlock()
}

func (s *Supervisor) wakeWanted(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wakeWant[name]
}

// wakeLocked does the wake; caller holds the cell lock.
func (s *Supervisor) wakeLocked(ctx context.Context, name string) (WakeResult, error) {
	c, err := s.reg.Get(name)
	if err != nil {
		return WakeResult{}, err
	}

	state, err := s.rt.Inspect(ctx, c.Container)
	if err != nil {
		return WakeResult{}, err
	}
	if state == runtime.StateRunning {
		return WakeResult{AlreadyRunning: true}, nil
	}
	if state == runtime.StateMissing {
		return WakeResult{}, fmt.Errorf("cell %s: container %s missing", name, c.Container)
	}

	// The concurrency slot covers only the step that commits host resources
	// (page-in and unpause/start). Readiness polling and the thaw settle run
	// outside it, so the next cell's page-in overlaps this cell's settle
	// instead of leaving the swap device idle (measured: ~3 s idle per wave).
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return WakeResult{}, ctx.Err()
	}
	release := sync.OnceFunc(func() { <-s.sem })
	defer release()
	_ = s.reg.Update(name, func(x *registry.Cell) { x.Phase = registry.PhaseWaking })
	t0 := s.opt.Now()
	thawAt := time.Now().UTC() // wall clock, for the container log's --since
	var startTook time.Duration
	timeout := s.opt.StopWakeTimeout
	kind := "stop"
	if state == runtime.StatePaused {
		kind = "pause"
		switch {
		case reclaimed(c):
			kind = "reclaimed" // pages come back from the swap device: a different tier in any dashboard
			if s.opt.PrefetchOnWake {
				s.prefetch(ctx, c)
			}
		case floorOf(c) == floorWarm:
			kind = "warm" // trimmed to the warm floor only: its hot set is resident, nothing to prefetch
		}
		timeout = s.pauseWakeTimeout(c)
	}
	// Recovery slot: taken before the process runs, held until it is ready and
	// settled, so a burst does not start more post-thaw recoveries than the
	// CPU can serve; the page-in slot above is released right after.
	select {
	case s.recov <- struct{}{}:
	case <-ctx.Done():
		// Nothing committed yet: put the phase back so a restart does not
		// find a Waking cell that nothing is waking.
		_ = s.reg.Update(name, func(x *registry.Cell) {
			if x.Phase == registry.PhaseWaking {
				x.Phase = c.Phase
			}
		})
		return WakeResult{}, ctx.Err()
	}
	defer func() { <-s.recov }()
	if state == runtime.StatePaused {
		startTook, err = s.rt.Unpause(ctx, c.Container)
	} else {
		startTook, err = s.rt.Start(ctx, c.Container)
	}
	release()
	if err != nil {
		s.m.wakeFail()
		_ = s.reg.Update(name, func(x *registry.Cell) { x.Phase = registry.PhaseFailed; x.LastError = err.Error() })
		return WakeResult{}, err
	}
	if err := s.waitReadyFor(ctx, c.Container, c.Port, timeout); err != nil {
		s.m.wakeFail()
		_ = s.reg.Update(name, func(x *registry.Cell) { x.Phase = registry.PhaseFailed; x.LastError = err.Error() })
		return WakeResult{StartTook: startTook}, err
	}
	if state == runtime.StatePaused {
		s.settleThaw(ctx, c, thawAt)
	}
	res := WakeResult{StartTook: startTook, ReadyTook: s.opt.Now().Sub(t0)}
	s.m.wake(kind, res.ReadyTook)
	s.opt.Logger.Info("woke", "cell", name, "start", res.StartTook, "ready", res.ReadyTook)
	return res, s.reg.Update(name, func(x *registry.Cell) {
		x.Phase = registry.PhaseActive
		x.LastActivity = s.opt.Now()
		x.WokeAt = s.opt.Now()
		x.PausedAt = time.Time{}
		x.ReclaimedAt = time.Time{}
		x.WarmAt = time.Time{}
		x.Swapped = false
		x.LastError = ""
	})
}

func (s *Supervisor) waitReady(ctx context.Context, port int, timeout time.Duration) error {
	return s.waitReadyFor(ctx, "", port, timeout)
}

// waitReadyFor polls the health probe until ready or timeout. When a
// container name is given it also watches the runtime state every second
// and fails fast if the container has exited: a gateway that died at boot
// must not cost a caller the full readiness timeout.
func (s *Supervisor) waitReadyFor(ctx context.Context, container string, port int, timeout time.Duration) error {
	if port == 0 {
		return nil // no readiness probe configured
	}
	deadline := time.After(timeout)
	var last error
	lastState := time.Time{}
	for {
		if last = s.opt.Probe(ctx, port); last == nil {
			return nil
		}
		if container != "" && time.Since(lastState) >= time.Second {
			lastState = time.Now()
			if st, err := s.rt.Inspect(ctx, container); err == nil && (st == runtime.StateExited || st == runtime.StateMissing) {
				return fmt.Errorf("wake failed: container %s is %s (exited during startup); last probe: %w", container, st, last)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("wake timeout: gateway never became ready: %w", last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Supervisor) selfHeal(ctx context.Context, c registry.Cell) error {
	if c.Restarts >= s.opt.MaxRestarts {
		return s.reg.Update(c.Name, func(x *registry.Cell) {
			x.Phase = registry.PhaseFailed
			x.LastError = "restart budget exhausted"
		})
	}
	_ = s.reg.Update(c.Name, func(x *registry.Cell) { x.Restarts++ })
	s.m.selfHeal()
	_, err := s.wakeLocked(ctx, c.Name)
	return err
}
