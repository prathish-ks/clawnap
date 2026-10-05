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
	s, _, fr := maintainFixture(t, &now, Options{MaintainEvery: 3 * time.Hour, MinAwake: time.Minute,
		Headroom: 1 << 30, MemAvailable: func() (int64, bool) { return avail, true }})
	s.lastMaintain = now.Add(-2 * time.Hour)

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
