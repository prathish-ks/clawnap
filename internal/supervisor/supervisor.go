// Package supervisor is the reconcile loop: it samples every registered
// cell, hibernates the ones that have been idle past their policy, restores
// always-on cells that died, and serves wake requests with a bounded
// concurrency so a thundering herd cannot OOM the host.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/idle"
	"github.com/prathish-ks/fleet-supervisor/internal/reclaim"
	"github.com/prathish-ks/fleet-supervisor/internal/registry"
	"github.com/prathish-ks/fleet-supervisor/internal/runtime"
	"github.com/prathish-ks/fleet-supervisor/internal/walcheck"
)

// Options tune the loop.
type Options struct {
	Interval        time.Duration // sampling period
	StopGrace       time.Duration // graceful stop timeout
	WakeTimeout     time.Duration // max wait for readiness after an unpause (pause tier)
	StopWakeTimeout time.Duration // max wait for readiness after a start (stop tier: full gateway boot)
	MaxConcurrent   int           // simultaneous page-ins/starts; readiness waits and the thaw settle run outside this bound
	NoiseBytes      int64         // per-sample traffic ignored as background
	MaxRestarts     int           // always-on self-heal budget per cell
	PreWake         time.Duration // wake a hibernated cell this long before NextDueAt
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
	// gives up after ThawSettle. Measured: 22 ms to ~800 ms.
	ThawSettle time.Duration
	// ReclaimWakeTimeout bounds readiness after unpausing a cell whose memory
	// was reclaimed to swap (measured 9–17 s on a laptop swap file; faster
	// on zram/NVMe). Larger than WakeTimeout, smaller than a full boot.
	ReclaimWakeTimeout time.Duration
	// ReclaimKeep is the resident floor left in RAM when reclaiming (bytes).
	// 0 reclaims everything reclaimable (max saving, slowest wake); a value
	// near the gateway's working set keeps wakes near pause speed.
	ReclaimKeep int64
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
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 2 // page-in is bandwidth bound: two in flight saturate a cloud volume, and sequencing gives earlier cells earlier wakes
	}
	if o.MemAvailable == nil {
		o.MemAvailable = memAvailable
	}
	if o.NoiseBytes == 0 {
		o.NoiseBytes = 2048
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
		o.ThawSettle = 3 * time.Second
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
	mu       sync.Mutex
	victims  map[string]bool        // cells chosen for reclaim under memory pressure, set per reconcile pass
	inflt    map[string]*wakeShare  // per-cell in-flight wake, so concurrent wakes coalesce and share the outcome
	cell     map[string]*sync.Mutex // per-cell lock: wake, hibernate, reclaim, pulse never interleave on one cell
	wakeWant map[string]bool        // a wake is waiting for this cell's lock; reclaim yields between chunks
	recon    chan struct{}          // reconcile pool, separate from the wake semaphore (a reconcile goroutine may itself wake)
	m        *Metrics
}

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
	return &Supervisor{opt: opt, reg: reg, rt: rt, sem: make(chan struct{}, opt.MaxConcurrent),
		recon: make(chan struct{}, opt.MaxConcurrent*4),
		inflt: map[string]*wakeShare{}, cell: map[string]*sync.Mutex{}, wakeWant: map[string]bool{}, m: newMetrics()}
}

// Run loops until ctx is done.
func (s *Supervisor) Run(ctx context.Context) error {
	t := time.NewTicker(s.opt.Interval)
	defer t.Stop()
	for {
		s.ReconcileOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// ReconcileOnce inspects every cell and acts, cells in parallel under a
// bounded pool, so one slow action (a 30 s reclaim, a 90 s stop-tier boot)
// never stalls the others. A cell already being acted on (e.g. a wake in
// flight from the ingress) is skipped this pass rather than waited for.
func (s *Supervisor) ReconcileOnce(ctx context.Context) {
	cells := s.reg.List()
	s.mu.Lock()
	s.victims = s.pressureVictims(ctx, cells)
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range cells {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := s.lockCell(c.Name)
			if !l.TryLock() {
				return // busy: wake/hibernate in progress
			}
			defer l.Unlock()
			select {
			case s.recon <- struct{}{}: // reconcile pool: never the wake semaphore, which a reconcile-driven wake needs
				defer func() { <-s.recon }()
			case <-ctx.Done():
				return
			}
			// A persisted Waking phase with no wake in flight is a crash
			// leftover; adopt the runtime's real state instead of honouring it.
			if c.Phase == registry.PhaseWaking && !s.wakeInFlight(c.Name) {
				c.Phase = registry.PhaseActive
				_ = s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseActive })
			}
			if err := s.reconcileCell(ctx, c); err != nil {
				s.opt.Logger.Warn("reconcile", "cell", c.Name, "err", err)
				_ = s.reg.Update(c.Name, func(x *registry.Cell) { x.LastError = err.Error() })
			}
		}()
	}
	wg.Wait()
}

func (s *Supervisor) reconcileCell(ctx context.Context, c registry.Cell) error {
	state, err := s.rt.Inspect(ctx, c.Container)
	if err != nil {
		return err
	}
	switch state {
	case runtime.StateMissing:
		return s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseFailed; x.LastError = "container missing" })
	case runtime.StateRunning:
		return s.observeRunning(ctx, c)
	case runtime.StatePaused:
		if s.dueSoon(c) {
			_ = s.advanceDue(c.Name)
			_, err := s.wakeLocked(ctx, c.Name)
			return err
		}
		if c.Phase == registry.PhaseHibernated && !c.PausedAt.IsZero() && s.opt.MaxPause > 0 && s.opt.Now().Sub(c.PausedAt) >= s.opt.MaxPause {
			return s.capPause(ctx, c)
		}
		if c.Phase == registry.PhaseHibernated && !c.PausedAt.IsZero() && c.ReclaimedAt.Before(c.PausedAt) {
			timed := s.opt.ReclaimAfter > 0 && s.opt.Now().Sub(c.PausedAt) >= s.opt.ReclaimAfter
			if timed || s.isVictim(c.Name) {
				return s.reclaimCell(ctx, c)
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
			x.Swapped = false
		})
	case runtime.StateExited, runtime.StateCreated:
		if c.Phase == registry.PhaseHibernated && s.dueSoon(c) {
			_ = s.advanceDue(c.Name)
			_, err := s.wakeLocked(ctx, c.Name)
			return err
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

func (s *Supervisor) observeRunning(ctx context.Context, c registry.Cell) error {
	st, err := s.rt.Stats(ctx, c.Container)
	if err != nil {
		return err
	}
	cur := idle.Sample{At: s.opt.Now(), RxBytes: st.Net.RxBytes, TxBytes: st.Net.TxBytes}
	prev := idle.Sample{RxBytes: c.RxBytes, TxBytes: c.TxBytes} // last persisted counters
	d := idle.Evaluate(prev, cur, c.LastActivity, c.IdleAfter, s.opt.NoiseBytes)
	if err := s.reg.Update(c.Name, func(x *registry.Cell) {
		x.Phase = registry.PhaseActive
		x.LastActivity = d.LastActivity
		x.RxBytes, x.TxBytes = cur.RxBytes, cur.TxBytes
		x.LastError = ""
	}); err != nil {
		return err
	}
	if d.ShouldSleep && c.Class == registry.ClassHibernate && !s.jobInsideIdleWindow(c) {
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
		if c.Phase == registry.PhaseHibernated && !c.PausedAt.IsZero() && c.ReclaimedAt.Before(c.PausedAt) {
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

func (s *Supervisor) isVictim(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.victims[name]
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
	st, err := s.opt.Reclaimer.Prefetch(pctx, id, 0)
	if err != nil {
		s.opt.Logger.Warn("prefetch", "cell", c.Name, "err", err, "mechanism", st.Mechanism)
		return
	}
	landed, wait := s.waitPagedIn(pctx, id, before.CurrentBytes, st.SwapBefore)
	s.opt.Logger.Info("prefetched", "cell", c.Name, "mechanism", st.Mechanism, "procs", st.Processes, "mappings", st.Mappings, "advised_mib", st.Bytes>>20, "swap_before_mib", st.SwapBefore>>20, "landed_mib", landed>>20, "advise_took", st.Took, "pagein_took", wait)
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
	if s.opt.PrefetchOnWake && reclaimed(c) {
		s.prefetch(ctx, c) // a pulse is a thaw: pay page-in in bulk, not fault by fault
	}
	if _, err := s.rt.Unpause(ctx, c.Container); err != nil {
		return err
	}
	if err := s.waitReady(ctx, c.Port, s.pauseWakeTimeout(c)); err != nil {
		// gateway did not come back healthy after the thaw: leave it running
		// and let the next reconcile observe it (self-exit shows as exited).
		s.opt.Logger.Warn("pause cap: pulse, gateway not ready", "cell", c.Name, "err", err)
		return s.reg.Update(c.Name, func(x *registry.Cell) { x.Phase = registry.PhaseActive; x.PausedAt = time.Time{} })
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
	// A pulse brings back only what it touched; the rest is still in swap.
	// Keep the reclaimed mark so the next wake prefetches and gets the
	// longer timeout, and do not re-arm a full reclaim for the same freeze:
	// ReclaimedAt stays >= PausedAt.
	now := s.opt.Now()
	return s.reg.Update(c.Name, func(x *registry.Cell) {
		x.PausedAt = now
		if x.Swapped {
			x.ReclaimedAt = now
		} else {
			x.ReclaimedAt = time.Time{}
		}
	})
}

// reclaimed reports whether the cell's memory was actually pushed to swap
// during the current pause. ReclaimedAt alone is not enough: it is also
// stamped when reclaim is impossible so the loop stops retrying. Only a
// reclaim that moved bytes marks the cell as swapped.
func reclaimed(c registry.Cell) bool {
	return c.Swapped && !c.ReclaimedAt.IsZero() && !c.ReclaimedAt.Before(c.PausedAt)
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
		if reclaimed(c) {
			kind = "reclaimed" // pages come back from the swap device: a different tier in any dashboard
			if s.opt.PrefetchOnWake {
				s.prefetch(ctx, c)
			}
		}
		timeout = s.pauseWakeTimeout(c)
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
		x.PausedAt = time.Time{}
		x.ReclaimedAt = time.Time{}
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
