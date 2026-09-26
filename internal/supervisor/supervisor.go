// Package supervisor is the reconcile loop: it samples every registered
// cell, hibernates the ones that have been idle past their policy, restores
// always-on cells that died, and serves wake requests with a bounded
// concurrency so a thundering herd cannot OOM the host.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
	MaxConcurrent   int           // simultaneous wakes/restarts
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
		o.MaxConcurrent = 4
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
	if o.PulseWindow == 0 {
		o.PulseWindow = 5 * time.Second
	}
	if o.PauseFallthrough == "" {
		o.PauseFallthrough = "pulse"
	}
	if o.Reclaimer == nil {
		o.Reclaimer = &reclaim.Reclaimer{}
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
	inflt    map[string]chan struct{} // per-cell in-flight wake, so concurrent wakes coalesce
	last     map[string]idle.Sample
	cell     map[string]*sync.Mutex // per-cell lock: wake, hibernate, reclaim, pulse never interleave on one cell
	wakeWant map[string]bool        // a wake is waiting for this cell's lock; reclaim yields between chunks
	m        *Metrics
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

func (s *Supervisor) sample(name string) (idle.Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.last[name]
	return p, ok
}

func (s *Supervisor) setSample(name string, v idle.Sample) {
	s.mu.Lock()
	s.last[name] = v
	s.mu.Unlock()
}

func (s *Supervisor) dropSample(name string) {
	s.mu.Lock()
	delete(s.last, name)
	s.mu.Unlock()
}

// Metrics exposes the supervisor's counters (for the ingress /metrics handler).
func (s *Supervisor) Metrics() *Metrics { return s.m }

// Cells lists the registry (for metrics gauges).
func (s *Supervisor) Cells() []registry.Cell { return s.reg.List() }

// New builds a Supervisor.
func New(reg *registry.Store, rt runtime.Client, opt Options) *Supervisor {
	opt.defaults()
	return &Supervisor{opt: opt, reg: reg, rt: rt, sem: make(chan struct{}, opt.MaxConcurrent),
		inflt: map[string]chan struct{}{}, last: map[string]idle.Sample{}, cell: map[string]*sync.Mutex{}, wakeWant: map[string]bool{}, m: newMetrics()}
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
	var wg sync.WaitGroup
	for _, c := range s.reg.List() {
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
			case s.sem <- struct{}{}:
				defer func() { <-s.sem }()
			case <-ctx.Done():
				return
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
			_, err := s.wakeLocked(ctx, c.Name)
			return err
		}
		if c.Phase == registry.PhaseHibernated && !c.PausedAt.IsZero() && s.opt.Now().Sub(c.PausedAt) >= s.opt.MaxPause {
			return s.capPause(ctx, c)
		}
		if c.Phase == registry.PhaseHibernated && s.opt.ReclaimAfter > 0 && !c.PausedAt.IsZero() &&
			c.ReclaimedAt.Before(c.PausedAt) && s.opt.Now().Sub(c.PausedAt) >= s.opt.ReclaimAfter {
			return s.reclaimCell(ctx, c)
		}
		if c.Phase == registry.PhaseHibernated || c.Phase == registry.PhaseWaking {
			return nil // expected
		}
		// paused by someone else: adopt it as hibernated from now, so the
		// pause cap and reclaim rules apply to it too; wake on demand
		return s.reg.Update(c.Name, func(x *registry.Cell) {
			x.Phase = registry.PhaseHibernated
			x.PausedAt = s.opt.Now()
			x.ReclaimedAt = time.Time{}
		})
	case runtime.StateExited, runtime.StateCreated:
		if c.Phase == registry.PhaseHibernated && s.dueSoon(c) {
			_, err := s.wakeLocked(ctx, c.Name)
			return err
		}
		if c.Phase == registry.PhaseHibernated || c.Phase == registry.PhaseWaking {
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
	prev, seen := s.sample(c.Name)
	s.setSample(c.Name, cur)
	if !seen {
		prev = idle.Sample{At: cur.At, RxBytes: c.RxBytes, TxBytes: c.TxBytes}
	}
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

// reclaimCell pushes a paused cell's memory to swap. Unsupported hosts are
// logged once per cell and never retried until the next pause.
func (s *Supervisor) reclaimCell(ctx context.Context, c registry.Cell) error {
	id, err := s.rt.R.Run(ctx, "inspect", "-f", "{{.Id}}", c.Container)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	before, _ := s.opt.Reclaimer.Stats(id)
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	stop := func() bool { return s.wakeWanted(c.Name) }
	after, err := s.opt.Reclaimer.ReclaimUntil(rctx, id, 8<<30, stop)
	if err != nil {
		if err == reclaim.ErrUnsupported {
			s.opt.Logger.Info("reclaim unsupported on this host; leaving cell resident", "cell", c.Name)
			return s.reg.Update(c.Name, func(x *registry.Cell) { x.ReclaimedAt = s.opt.Now() })
		}
		return err
	}
	s.m.reclaimed(before.CurrentBytes - after.CurrentBytes)
	s.opt.Logger.Info("reclaimed", "cell", c.Name, "before_mib", before.CurrentBytes>>20, "after_mib", after.CurrentBytes>>20, "swap_mib", after.SwapBytes>>20)
	return s.reg.Update(c.Name, func(x *registry.Cell) { x.ReclaimedAt = s.opt.Now() })
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
	if _, err := s.rt.Unpause(ctx, c.Container); err != nil {
		return err
	}
	if err := s.waitReady(ctx, c.Port, s.opt.WakeTimeout); err != nil {
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
	return s.reg.Update(c.Name, func(x *registry.Cell) { x.PausedAt = s.opt.Now(); x.ReclaimedAt = time.Time{} })
}

// dueSoon: a hibernated cell whose next job is within PreWake should wake now.
func (s *Supervisor) dueSoon(c registry.Cell) bool {
	return !c.NextDueAt.IsZero() && !c.NextDueAt.After(s.opt.Now().Add(s.opt.PreWake))
}

// jobInsideIdleWindow: never hibernate a cell that would only have to be
// woken again before its idle timeout elapsed (interim rule until cron
// schedules are read from the cell itself).
func (s *Supervisor) jobInsideIdleWindow(c registry.Cell) bool {
	if c.NextDueAt.IsZero() {
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
	s.dropSample(name)
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
	// coalesce
	s.mu.Lock()
	if ch, ok := s.inflt[name]; ok {
		s.mu.Unlock()
		select {
		case <-ch:
			return WakeResult{AlreadyRunning: true}, nil
		case <-ctx.Done():
			return WakeResult{}, ctx.Err()
		}
	}
	ch := make(chan struct{})
	s.inflt[name] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflt, name)
		s.mu.Unlock()
		close(ch)
	}()
	s.setWakeWanted(name, true)
	l := s.lockCell(name)
	l.Lock()
	defer l.Unlock()
	s.setWakeWanted(name, false)
	return s.wakeLocked(ctx, name)
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

	select { // bounded concurrency
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return WakeResult{}, ctx.Err()
	}
	_ = s.reg.Update(name, func(x *registry.Cell) { x.Phase = registry.PhaseWaking })
	t0 := s.opt.Now()
	var startTook time.Duration
	timeout := s.opt.StopWakeTimeout
	kind := "stop"
	if state == runtime.StatePaused {
		kind = "pause"
		timeout = s.opt.WakeTimeout
		startTook, err = s.rt.Unpause(ctx, c.Container)
	} else {
		startTook, err = s.rt.Start(ctx, c.Container)
	}
	if err != nil {
		s.m.wakeFail()
		_ = s.reg.Update(name, func(x *registry.Cell) { x.Phase = registry.PhaseFailed; x.LastError = err.Error() })
		return WakeResult{}, err
	}
	if err := s.waitReady(ctx, c.Port, timeout); err != nil {
		s.m.wakeFail()
		_ = s.reg.Update(name, func(x *registry.Cell) { x.Phase = registry.PhaseFailed; x.LastError = err.Error() })
		return WakeResult{StartTook: startTook}, err
	}
	res := WakeResult{StartTook: startTook, ReadyTook: s.opt.Now().Sub(t0)}
	s.m.wake(kind, res.ReadyTook)
	s.opt.Logger.Info("woke", "cell", name, "start", res.StartTook, "ready", res.ReadyTook)
	return res, s.reg.Update(name, func(x *registry.Cell) {
		x.Phase = registry.PhaseActive
		x.LastActivity = s.opt.Now()
		x.PausedAt = time.Time{}
		x.LastError = ""
	})
}

func (s *Supervisor) waitReady(ctx context.Context, port int, timeout time.Duration) error {
	if port == 0 {
		return nil // no readiness probe configured
	}
	deadline := time.After(timeout)
	var last error
	for {
		if last = s.opt.Probe(ctx, port); last == nil {
			return nil
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
