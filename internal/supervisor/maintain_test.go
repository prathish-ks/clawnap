package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/prathish-ks/clawnap/internal/registry"
	"github.com/prathish-ks/clawnap/internal/runtime"
)

// maintainFixture: three hibernated cells, all paused, with distinct last-wake
// times so the rotation's ordering is unambiguous.
func maintainFixture(t *testing.T, now *time.Time, opt Options) (*Supervisor, *registry.Store, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{state: map[string]runtime.State{
		"oc-m1": runtime.StatePaused, "oc-m2": runtime.StatePaused, "oc-m3": runtime.StatePaused}}
	reg, err := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	if err != nil {
		t.Fatal(err)
	}
	opt.Now = func() time.Time { return *now }
	opt.MaxPause = -1 // the pause cap is a different policy; keep it out of these tests
	if opt.Probe == nil {
		opt.Probe = func(context.Context, int) error { return nil }
	}
	s := New(reg, runtime.Client{R: fr}, opt)
	// m1 longest asleep, m3 most recently awake.
	for i, ago := range []time.Duration{9 * time.Hour, 6 * time.Hour, 1 * time.Hour} {
		name := []string{"m1", "m2", "m3"}[i]
		if err := reg.Put(registry.Cell{Name: name, Container: "oc-" + name, Port: 9000 + i,
			Phase: registry.PhaseHibernated, PausedAt: now.Add(-ago), WokeAt: now.Add(-ago),
			IdleAfter: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	return s, reg, fr
}

// The rotation wakes the longest-unwoken cell, and only one per pace: with
// three candidates and a 3 h target the pace is 1 h, so a second pass a minute
// later must wake nobody.
func TestMaintenanceRotationWakesLongestUnwokenOnePerPace(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, reg, fr := maintainFixture(t, &now, Options{MaintainEvery: 3 * time.Hour, MinAwake: time.Minute})
	// New() starts the clock at boot; move past one pace so the first pass is due.
	s.lastMaintain = now.Add(-2 * time.Hour)

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if got := fr.state["oc-m1"]; got != runtime.StateRunning {
		t.Fatalf("expected the longest-unwoken cell m1 to be woken, state=%v", got)
	}
	if fr.state["oc-m2"] != runtime.StatePaused || fr.state["oc-m3"] != runtime.StatePaused {
		t.Fatal("rotation woke more than one cell in a single pace")
	}
	if c, _ := reg.Get("m1"); c.WokeAt.Equal(now.Add(-9 * time.Hour)) {
		t.Fatal("WokeAt not advanced by the maintenance wake")
	}

	// Same instant again: inside the pace, so nothing more wakes.
	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if fr.state["oc-m2"] != runtime.StatePaused {
		t.Fatal("rotation woke a second cell inside one pace")
	}
}

// Off by default: with MaintainEvery unset no cell is ever woken for maintenance.
func TestMaintenanceRotationOffByDefault(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, _, fr := maintainFixture(t, &now, Options{MinAwake: time.Minute})
	s.lastMaintain = now.Add(-24 * time.Hour)

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	for _, c := range []string{"oc-m1", "oc-m2", "oc-m3"} {
		if fr.state[c] != runtime.StatePaused {
			t.Fatalf("%s woke with the rotation off", c)
		}
	}
}

// Maintenance is the lowest-priority work on the host: below the headroom
// target it stands aside rather than taking memory the policy is trying to free.
func TestMaintenanceRotationYieldsUnderMemoryPressure(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	avail := int64(500 << 20)
	// 24 h target against a 9 h oldest candidate: ahead of schedule, so the
	// rotation is free to stand aside. Pace is 24h/3 = 8 h.
	s, _, fr := maintainFixture(t, &now, Options{MaintainEvery: 24 * time.Hour, MinAwake: time.Minute,
		Headroom: 1 << 30, MemAvailable: func() (int64, bool) { return avail, true }})
	s.lastMaintain = now.Add(-9 * time.Hour)

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if fr.state["oc-m1"] != runtime.StatePaused {
		t.Fatal("rotation woke a cell while the host was below its headroom target")
	}

	avail = 4 << 30 // pressure gone: the same pass now proceeds
	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if fr.state["oc-m1"] != runtime.StateRunning {
		t.Fatal("rotation did not resume once headroom was restored")
	}
}

// A cell that cannot wake must not sit at the head of the rotation forever:
// after a failure it is held out for one interval and the next cell is reached.
func TestMaintenanceRotationSkipsACellThatFailedToWake(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, _, fr := maintainFixture(t, &now, Options{MaintainEvery: 3 * time.Hour, MinAwake: time.Minute,
		WakeTimeout: 10 * time.Millisecond, ReclaimWakeTimeout: 10 * time.Millisecond,
		Probe: func(_ context.Context, port int) error {
			if port == 9000 { // m1 never becomes ready
				return errors.New("not ready")
			}
			return nil
		}})
	s.lastMaintain = now.Add(-2 * time.Hour)

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	s.mu.Lock()
	_, held := s.maintainSkip["m1"]
	s.mu.Unlock()
	if !held {
		t.Fatal("a cell whose maintenance wake failed was not held out of the rotation")
	}

	now = now.Add(2 * time.Hour) // past the pace, still inside m1's hold
	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if fr.state["oc-m2"] != runtime.StateRunning {
		t.Fatal("rotation did not move on to the next cell after a failure")
	}
}

// Starvation guard: a host that is always busy, or always at its headroom
// target, must still maintain a cell once that cell is actually overdue.
// Yielding without a bound is how the pressure path once starved (maxPressureDefer).
func TestMaintenanceRotationStopsYieldingOnceACellIsOverdue(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// Oldest candidate is 9 h asleep against a 3 h target: overdue.
	s, _, fr := maintainFixture(t, &now, Options{MaintainEvery: 3 * time.Hour, MinAwake: time.Minute,
		Headroom: 1 << 30, MemAvailable: func() (int64, bool) { return 100 << 20, true }})
	s.lastMaintain = now.Add(-2 * time.Hour)

	// Below the headroom target and with a wake registered in flight: both
	// yield conditions hold, and the rotation must proceed anyway.
	s.mu.Lock()
	s.inflt["someone-else"] = &wakeShare{done: make(chan struct{})}
	s.mu.Unlock()

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if fr.state["oc-m1"] != runtime.StateRunning {
		t.Fatal("rotation starved: an overdue cell was never maintained on a busy, memory-tight host")
	}
}

// A maintenance wake has no tenant waiting on it, so it must not start once
// shutdown has begun and hold up the drain.
func TestMaintenanceRotationDoesNotStartDuringShutdown(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, _, fr := maintainFixture(t, &now, Options{MaintainEvery: 3 * time.Hour, MinAwake: time.Minute})
	s.lastMaintain = now.Add(-2 * time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.maintenanceWake(ctx, s.reg.List())
	s.Drain(context.Background())
	if fr.state["oc-m1"] != runtime.StatePaused {
		t.Fatal("rotation started a wake after the context was cancelled")
	}
}

// With -min-awake disabled the pace floor must not vanish, or rotation wakes
// stack up on a large fleet.
func TestMaintenanceRotationFloorsThePaceWithMinAwakeOff(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// MinAwake negative is normalised to 0 by defaults(); pace would be
	// 30s/3 = 10 s without a floor, so 30 s since the last wake must not be
	// enough to fire another.
	s, _, fr := maintainFixture(t, &now, Options{MaintainEvery: 30 * time.Second, MinAwake: -1})
	s.lastMaintain = now.Add(-30 * time.Second)

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if fr.state["oc-m1"] == runtime.StateRunning {
		t.Fatalf("pace floor did not apply with MinAwake off (floor is %v)", minMaintainPace)
	}
}

// Holds are dropped once they expire or once the cell they name is gone, so
// the map tracks the fleet instead of growing with it.
func TestMaintenanceRotationPrunesStaleHolds(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, _, _ := maintainFixture(t, &now, Options{MaintainEvery: 3 * time.Hour, MinAwake: time.Minute})
	s.mu.Lock()
	s.maintainSkip["removed-cell"] = now.Add(time.Hour) // names a cell that no longer exists
	s.maintainSkip["m2"] = now.Add(-time.Minute)        // hold that has expired
	s.maintainSkip["m3"] = now.Add(time.Hour)           // live hold, must survive
	s.mu.Unlock()

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.maintainSkip["removed-cell"]; ok {
		t.Error("hold for a removed cell was not pruned")
	}
	if _, ok := s.maintainSkip["m2"]; ok {
		t.Error("expired hold was not pruned")
	}
	if _, ok := s.maintainSkip["m3"]; !ok {
		t.Error("live hold was pruned")
	}
}
