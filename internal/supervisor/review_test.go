package supervisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prathish-ks/clawnap/internal/reclaim"
	"github.com/prathish-ks/clawnap/internal/registry"
	"github.com/prathish-ks/clawnap/internal/runtime"
)

// A cell that only reached the warm floor and is then pulsed must come out
// of the pulse reclaimable again: the stages restart from the new pause, so
// it stays a candidate for the headroom policy. (Before: the pulse stamped
// ReclaimedAt for any Swapped cell, closing every reclaim gate for good.)
func TestPulseKeepsWarmOnlyCellReclaimable(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-w": runtime.StatePaused}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	cg := t.TempDir()
	fakeCgroup(t, cg, "oc-w")
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil },
		MaxPause: 10 * time.Minute, PulseWindow: time.Millisecond, WarmKeep: 300 << 20, ReclaimKeep: 150 << 20, Headroom: 1 << 30,
		MemAvailable: func() (int64, bool) { return 100 << 20, true }, Reclaimer: &reclaim.Reclaimer{FS: cg}})
	_ = reg.Put(registry.Cell{Name: "w", Container: "oc-w", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-11 * time.Minute), WarmAt: now.Add(-10 * time.Minute), Swapped: true})
	c, _ := reg.Get("w")
	if floorOf(c) != floorWarm {
		t.Fatalf("precondition: warm floor, got %v", floorOf(c))
	}
	s.ReconcileOnce(context.Background()) // past the cap: pulse
	c, _ = reg.Get("w")
	if !fr.has("unpause oc-w") || c.Phase != registry.PhaseHibernated || !c.PausedAt.Equal(now) {
		t.Fatalf("expected a pulse: %+v calls=%v", c, fr.calls)
	}
	if floorOf(c) != floorResident || c.Swapped {
		t.Fatalf("after a pulse a warm-only cell must be resident again (stages restart), got floor=%v swapped=%v", floorOf(c), c.Swapped)
	}
	if v := s.pressureVictims(context.Background(), []registry.Cell{c}); !v["w"] {
		t.Fatalf("pulsed warm cell must stay a pressure candidate, victims=%v", v)
	}
	// and a cold cell keeps its cold mark across the pulse (no second full reclaim for the same freeze)
	_ = reg.Put(registry.Cell{Name: "k", Container: "oc-k", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-11 * time.Minute), WarmAt: now.Add(-10 * time.Minute), ReclaimedAt: now.Add(-9 * time.Minute), Swapped: true})
	fr.state["oc-k"] = runtime.StatePaused
	s.ReconcileOnce(context.Background())
	k, _ := reg.Get("k")
	if !fr.has("unpause oc-k") || floorOf(k) != floorCold || !reclaimed(k) || !hotSetCurrent(k) {
		t.Fatalf("pulsed cold cell must stay cold with its hot set current: %+v", k)
	}
}

// A reconcile goroutine waiting for a pool slot must not hold its cell's
// lock: an inbound wake for that cell has to proceed while unrelated cells
// occupy the pool.
func TestQueuedReconcileDoesNotBlockWakeOfSameCell(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	fr := &fakeRunner{state: map[string]runtime.State{"oc-x": runtime.StatePaused}, hold: map[string]chan struct{}{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, MaxConcurrent: 1, MaxPause: 24 * time.Hour})
	// pool = MaxConcurrent*4 = 4 slots; four slow cells fill it, so x's goroutine is the one queued
	for i := 0; i < 4; i++ {
		n := "slow-" + string(rune('a'+i))
		fr.state["oc-"+n] = runtime.StateRunning
		fr.hold["oc-"+n] = release
		_ = reg.Put(registry.Cell{Name: n, Container: "oc-" + n, Port: 1, IdleAfter: time.Hour})
	}
	_ = reg.Put(registry.Cell{Name: "x", Container: "oc-x", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now, IdleAfter: time.Hour})
	passDone := make(chan struct{})
	go func() { s.ReconcileOnce(context.Background()); close(passDone) }()
	deadline := time.Now().Add(3 * time.Second)
	for fr.holding.Load() < 4 && time.Now().Before(deadline) { // all four slots held inside a blocked inspect
		time.Sleep(5 * time.Millisecond)
	}
	if fr.holding.Load() < 4 {
		t.Fatal("pool never filled")
	}
	wctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Wake(wctx, "x"); err != nil {
		t.Fatalf("wake of x must not wait for unrelated cells to leave the pool: %v", err)
	}
	close(release)
	<-passDone
}

// Pressure reclaim yields to wakes in flight, but not for ever: after
// maxPressureDefer busy passes it runs anyway, so steady inbound traffic
// cannot leave the host below its headroom indefinitely.
func TestPressureReclaimRunsAfterBoundedDeferral(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-r1": runtime.StatePaused, "oc-a": runtime.StatePaused}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	cg := t.TempDir()
	d1 := fakeCgroup(t, cg, "oc-r1")
	release := make(chan struct{})
	probe := func(ctx context.Context, port int) error {
		if port != 9 {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := New(reg, runtime.Client{R: fr}, Options{
		Now: func() time.Time { return now }, Probe: probe, WakeTimeout: 10 * time.Second,
		MaxPause: 24 * time.Hour, Headroom: 1 << 30,
		MemAvailable: func() (int64, bool) { return 100 << 20, true }, Reclaimer: &reclaim.Reclaimer{FS: cg}})
	_ = reg.Put(registry.Cell{Name: "r1", Container: "oc-r1", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-3 * time.Minute), IdleAfter: time.Minute})
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 9, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Minute), IdleAfter: time.Minute})
	done := make(chan struct{})
	go func() { _, _ = s.Wake(context.Background(), "a"); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !fr.has("unpause oc-a") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < maxPressureDefer; i++ {
		s.ReconcileOnce(context.Background())
		if reclaimWritten(t, d1) {
			t.Fatalf("pass %d: pressure reclaim must still yield to the wake in flight", i+1)
		}
	}
	s.ReconcileOnce(context.Background()) // deferral budget spent: the headroom policy runs regardless
	if !reclaimWritten(t, d1) {
		t.Fatal("pressure reclaim must run once the deferral budget is spent")
	}
	close(release)
	<-done
}

// A scheduled wake that fails must not consume the due time: the next pass
// retries, and only a wake that succeeded moves the schedule on.
func TestDueTimeSurvivesFailedWake(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-d": runtime.StateExited}, failOn: "start"}
	s, reg := newSup(t, fr, &now)
	due := now.Add(time.Minute)
	_ = reg.Put(registry.Cell{Name: "d", Container: "oc-d", Port: 1, Tier: registry.TierStop, Phase: registry.PhaseHibernated, IdleAfter: time.Minute, NextDueAt: due})
	s.ReconcileOnce(context.Background()) // start fails
	c, _ := reg.Get("d")
	if !fr.has("start oc-d") || !c.NextDueAt.Equal(due) {
		t.Fatalf("a failed wake must leave the due time for a retry: %+v calls=%v", c, fr.calls)
	}
	fr.failOn = ""
	s.ReconcileOnce(context.Background()) // Failed -> Hibernated bookkeeping pass
	s.ReconcileOnce(context.Background()) // retry succeeds
	c, _ = reg.Get("d")
	if c.Phase != registry.PhaseActive || !c.NextDueAt.IsZero() {
		t.Fatalf("the retried wake must succeed and clear the one-shot: %+v calls=%v", c, fr.calls)
	}
	// a recurring schedule whose window was missed entirely is moved on, not lost
	_ = reg.Put(registry.Cell{Name: "r", Container: "oc-r", Port: 1, Phase: registry.PhaseHibernated, IdleAfter: time.Minute, NextDueAt: now.Add(-time.Hour), NextDueEvery: 30 * time.Minute})
	fr.state["oc-r"] = runtime.StatePaused
	s.ReconcileOnce(context.Background())
	r, _ := reg.Get("r")
	if !r.NextDueAt.After(now) {
		t.Fatalf("missed recurring due time must be advanced past now: %v", r.NextDueAt)
	}
}

// The self-heal budget bounds crash loops, not a cell's lifetime: restarts
// are forgiven once the cell has stayed up long enough.
func TestRestartBudgetForgivenAfterStableRun(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-w": runtime.StateExited}, netio: map[string]string{"oc-w": "1kB / 1kB"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "w", Container: "oc-w", Class: registry.ClassAlwaysOn, Port: 1, IdleAfter: time.Minute, Restarts: 4})
	s.ReconcileOnce(context.Background()) // self-heal: Restarts -> 5 (the cap)
	c, _ := reg.Get("w")
	if c.Restarts != 5 || c.Phase != registry.PhaseActive {
		t.Fatalf("expected one more self-heal: %+v", c)
	}
	now = now.Add(restartForgiveAfter)
	s.ReconcileOnce(context.Background()) // stable for long enough: forgiven
	c, _ = reg.Get("w")
	if c.Restarts != 0 {
		t.Fatalf("restart budget must be forgiven after a stable run: %+v", c)
	}
	fr.state["oc-w"] = runtime.StateExited
	s.ReconcileOnce(context.Background()) // a later crash is healed, not abandoned
	c, _ = reg.Get("w")
	if c.Phase != registry.PhaseActive || c.Restarts != 1 {
		t.Fatalf("expected self-heal after forgiveness: %+v", c)
	}
}

// Pulses are thaws and run under the same bounds as wakes: never more cells
// unpaused at once than MaxRecovering allows.
func TestPulsesAreBoundedLikeWakes(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil },
		MaxConcurrent: 1, MaxRecovering: 1, MaxPause: 10 * time.Minute, PulseWindow: 20 * time.Millisecond})
	for _, n := range []string{"a", "b", "c", "d"} {
		fr.state["oc-"+n] = runtime.StatePaused
		_ = reg.Put(registry.Cell{Name: n, Container: "oc-" + n, Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Hour), IdleAfter: time.Minute})
	}
	s.ReconcileOnce(context.Background())
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.maxThawed != 1 {
		t.Fatalf("pulses must respect MaxRecovering=1, saw %d cells thawed at once (calls=%v)", fr.maxThawed, fr.calls)
	}
}

// Cells frozen at the same instant reach the cap at different times.
func TestPauseCapIsSpreadPerCell(t *testing.T) {
	s := New(nil, runtime.Client{}, Options{MaxPause: 20 * time.Minute})
	seen := map[time.Duration]bool{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		cap := s.pauseCap(n)
		if cap > 20*time.Minute || cap <= 15*time.Minute {
			t.Fatalf("cap for %s out of [15m, 20m]: %v", n, cap)
		}
		seen[cap] = true
	}
	if len(seen) < 4 {
		t.Fatalf("caps should spread across cells, got %d distinct of 8", len(seen))
	}
}

// Negative concurrency flags fall back to the defaults instead of panicking
// in make(chan).
func TestNegativeConcurrencyUsesDefaults(t *testing.T) {
	s := New(nil, runtime.Client{}, Options{MaxConcurrent: -1, MaxRecovering: -3})
	if cap(s.sem) != 4 || cap(s.recov) != 8 {
		t.Fatalf("want defaults 4/8, got %d/%d", cap(s.sem), cap(s.recov))
	}
}

// A reconcile-driven wake cancelled before it committed anything puts the
// phase back, so a restart does not find a Waking cell nothing is waking.
func TestCancelledWakeRestoresPhaseBeforeCommit(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-a": runtime.StatePaused, "oc-b": runtime.StatePaused}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	release := make(chan struct{})
	probe := func(ctx context.Context, port int) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: probe, WakeTimeout: 5 * time.Second, MaxConcurrent: 2, MaxRecovering: 1})
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now})
	_ = reg.Put(registry.Cell{Name: "b", Container: "oc-b", Port: 2, Phase: registry.PhaseHibernated, PausedAt: now})
	done := make(chan struct{})
	go func() { _, _ = s.Wake(context.Background(), "a"); close(done) }() // holds the single recovery slot in the probe
	deadline := time.Now().Add(3 * time.Second)
	for !fr.has("unpause oc-a") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.wakeLocked(ctx, "b"); err == nil {
		t.Fatal("expected the cancelled wake to fail")
	}
	b, _ := reg.Get("b")
	if b.Phase != registry.PhaseHibernated {
		t.Fatalf("cancelled wake must restore the phase, got %s", b.Phase)
	}
	close(release)
	<-done
}

// The daemon's hot-set prefetch falls back to a full prefetch on any error,
// not only a stale set: a cold wake must never fault its pages in one at a
// time because one advise failed.
func TestHotSetUnusableFallsBackToFullPrefetch(t *testing.T) {
	c := registry.Cell{PausedAt: time.Unix(100, 0), WarmAt: time.Unix(110, 0), ReclaimedAt: time.Unix(120, 0), Swapped: true}
	if !hotSetCurrent(c) {
		t.Fatal("warm then cold in the same pause: hot set must be current")
	}
	c.WarmAt = time.Unix(50, 0) // recorded in an earlier pause
	if hotSetCurrent(c) {
		t.Fatal("a hot set from an earlier pause must not be used")
	}
	c.WarmAt = time.Time{}
	if hotSetCurrent(c) {
		t.Fatal("no warm stage: no hot set")
	}
}
