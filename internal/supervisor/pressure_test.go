package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prathish-ks/clawnap/internal/reclaim"
	"github.com/prathish-ks/clawnap/internal/registry"
	"github.com/prathish-ks/clawnap/internal/runtime"
)

func fakeCgroup(t *testing.T, cg, container string) string {
	t.Helper()
	d := filepath.Join(cg, "docker", "cid-"+container+"-0000000000000000")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(d, "memory.current"), []byte("800000000"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.swap.current"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.reclaim"), []byte(""), 0o644)
	return d
}

func reclaimWritten(t *testing.T, d string) bool {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(d, "memory.reclaim"))
	return string(b) != ""
}

// Below the headroom target the longest-paused resident cell is reclaimed
// first, before its timed clock; with memory plentiful nothing is reclaimed
// early.
func TestReclaimOnPressureTakesLongestPausedFirst(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-r1": runtime.StatePaused, "oc-r2": runtime.StatePaused}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	cg := t.TempDir()
	d1, d2 := fakeCgroup(t, cg, "oc-r1"), fakeCgroup(t, cg, "oc-r2")
	avail := int64(600 << 20)
	s := New(reg, runtime.Client{R: fr}, Options{
		Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil },
		ReclaimAfter: 10 * time.Minute, MaxPause: 24 * time.Hour, Headroom: 1 << 30,
		MemAvailable: func() (int64, bool) { return avail, true }, Reclaimer: &reclaim.Reclaimer{FS: cg}})
	_ = reg.Put(registry.Cell{Name: "r1", Container: "oc-r1", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-3 * time.Minute), IdleAfter: time.Minute})
	_ = reg.Put(registry.Cell{Name: "r2", Container: "oc-r2", Port: 2, Phase: registry.PhaseHibernated, PausedAt: now.Add(-1 * time.Minute), IdleAfter: time.Minute})

	s.ReconcileOnce(context.Background()) // deficit 424 MiB < r1's 800 MB: one victim, the older cell
	if !reclaimWritten(t, d1) || reclaimWritten(t, d2) {
		t.Fatalf("expected only r1 reclaimed under pressure: r1=%v r2=%v", reclaimWritten(t, d1), reclaimWritten(t, d2))
	}
	c1, _ := reg.Get("r1")
	if c1.ReclaimedAt.IsZero() {
		t.Fatal("r1 ReclaimedAt not stamped")
	}
	avail = 2 << 30 // pressure gone
	now = now.Add(time.Minute)
	s.ReconcileOnce(context.Background())
	if reclaimWritten(t, d2) {
		t.Fatal("r2 must stay resident: headroom met and ReclaimAfter not reached")
	}
	now = now.Add(10 * time.Minute) // r2 now past ReclaimAfter: timed reclaim still applies
	s.ReconcileOnce(context.Background())
	if !reclaimWritten(t, d2) {
		t.Fatal("r2 should be reclaimed by the timed policy")
	}
}

// The concurrency slot is released after the unpause, so a second cell's
// wake proceeds while the first is still waiting for readiness.
func TestWakeSlotReleasedBeforeReadinessWait(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-a": runtime.StatePaused, "oc-b": runtime.StatePaused}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	release := make(chan struct{})
	probe := func(ctx context.Context, port int) error {
		if port != 1 {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: probe, WakeTimeout: 10 * time.Second, MaxConcurrent: 1})
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Minute), IdleAfter: time.Minute})
	_ = reg.Put(registry.Cell{Name: "b", Container: "oc-b", Port: 2, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Minute), IdleAfter: time.Minute})

	aDone := make(chan error, 1)
	go func() { _, err := s.Wake(context.Background(), "a"); aDone <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for !fr.has("unpause oc-a") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.Wake(ctx, "b"); err != nil {
		t.Fatalf("b must wake while a waits for readiness (slot not released): %v", err)
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("a: %v", err)
	}
}

// Pressure reclaim must not run while a wake is in flight.
func TestPressureReclaimYieldsToWakes(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
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
		ReclaimAfter: 10 * time.Minute, MaxPause: 24 * time.Hour, Headroom: 1 << 30,
		MemAvailable: func() (int64, bool) { return 100 << 20, true }, Reclaimer: &reclaim.Reclaimer{FS: cg}})
	_ = reg.Put(registry.Cell{Name: "r1", Container: "oc-r1", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-3 * time.Minute), IdleAfter: time.Minute})
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 9, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Minute), IdleAfter: time.Minute})
	done := make(chan struct{})
	go func() { _, _ = s.Wake(context.Background(), "a"); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !fr.has("unpause oc-a") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s.ReconcileOnce(context.Background()) // a wake is waiting for readiness: no pressure reclaim
	if reclaimWritten(t, d1) {
		t.Fatal("pressure reclaim ran while a wake was in flight")
	}
	close(release)
	<-done
	s.ReconcileOnce(context.Background()) // quiet pass: the target is restored
	if !reclaimWritten(t, d1) {
		t.Fatal("pressure reclaim should run once the wake has finished")
	}
}

// A cell that is busy on CPU is not idle, even with no traffic.
func TestCPUBusyCellIsNotIdle(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-c": runtime.StateRunning}, netio: map[string]string{"oc-c": "1kB / 1kB"}, cpu: map[string]string{"oc-c": "35.5%"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "c", Container: "oc-c", Port: 1, IdleAfter: time.Minute})
	s.ReconcileOnce(context.Background())
	now = now.Add(5 * time.Minute)
	s.ReconcileOnce(context.Background())
	if fr.has("pause oc-c") {
		t.Fatal("must not pause a CPU-busy cell")
	}
	fr.mu.Lock()
	fr.cpu["oc-c"] = "0.8%"
	fr.mu.Unlock()
	s.ReconcileOnce(context.Background()) // quiet now: the idle clock starts here
	now = now.Add(2 * time.Minute)
	s.ReconcileOnce(context.Background())
	if !fr.has("pause oc-c") {
		t.Fatalf("expected pause once quiet and idle: %v", fr.calls)
	}
}

// A freshly woken cell is not hibernated again inside MinAwake, even if idle.
func TestMinAwakeAfterWake(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-m": runtime.StatePaused}, netio: map[string]string{"oc-m": "1kB / 1kB"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, WakeTimeout: time.Second, MinAwake: 3 * time.Minute})
	_ = reg.Put(registry.Cell{Name: "m", Container: "oc-m", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Minute), IdleAfter: 30 * time.Second})
	if _, err := s.Wake(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	s.ReconcileOnce(context.Background()) // baseline sample
	now = now.Add(2 * time.Minute)        // idle for 2 min, but only 2 min awake
	s.ReconcileOnce(context.Background())
	if fr.has("pause oc-m") {
		t.Fatal("paused inside MinAwake")
	}
	now = now.Add(2 * time.Minute) // 4 min awake
	s.ReconcileOnce(context.Background())
	if !fr.has("pause oc-m") {
		t.Fatalf("expected pause after MinAwake: %v", fr.calls)
	}
}

// A CPU burst must delay hibernation, not restart the idle countdown. An idle
// OpenClaw cell bursts to ~47 % of a core for a few seconds every couple of
// minutes; when that reset LastActivity, any cell whose idle timeout was longer
// than the gap between bursts could never hibernate at all.
func TestCPUBurstDelaysSleepWithoutRestartingTheIdleClock(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-b": runtime.StateRunning},
		netio: map[string]string{"oc-b": "1kB / 1kB"}, cpu: map[string]string{"oc-b": "2.00%"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, IdleCPUPct: 10, MaxPause: -1})
	_ = reg.Put(registry.Cell{Name: "b", Container: "oc-b", Port: 1, IdleAfter: 10 * time.Minute})

	s.ReconcileOnce(context.Background()) // baseline
	// Nine quiet minutes with a burst every two, exactly the real pattern.
	for i := 1; i <= 9; i++ {
		now = now.Add(time.Minute)
		fr.mu.Lock()
		fr.cpu["oc-b"] = map[bool]string{true: "47.00%", false: "2.00%"}[i%2 == 0]
		fr.mu.Unlock()
		s.ReconcileOnce(context.Background())
		if fr.has("pause oc-b") {
			t.Fatalf("hibernated after %d min, inside the 10 min idle window", i)
		}
	}
	// Past the window and quiet: it must sleep, despite the bursts along the way.
	now = now.Add(2 * time.Minute)
	fr.mu.Lock()
	fr.cpu["oc-b"] = "2.00%"
	fr.mu.Unlock()
	s.ReconcileOnce(context.Background())
	if !fr.has("pause oc-b") {
		t.Fatalf("CPU bursts restarted the idle clock: cell never hibernated. calls=%v", fr.calls)
	}
}

// The gate still holds a cell awake while work is actually running.
func TestSustainedCPUStillBlocksHibernation(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-c": runtime.StateRunning},
		netio: map[string]string{"oc-c": "1kB / 1kB"}, cpu: map[string]string{"oc-c": "40.00%"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, IdleCPUPct: 10, MaxPause: -1})
	_ = reg.Put(registry.Cell{Name: "c", Container: "oc-c", Port: 1, IdleAfter: time.Minute})

	s.ReconcileOnce(context.Background())
	now = now.Add(5 * time.Minute) // long past the idle window, but still working
	s.ReconcileOnce(context.Background())
	if fr.has("pause oc-c") {
		t.Fatal("hibernated a cell that was still busy on CPU")
	}
	fr.mu.Lock()
	fr.cpu["oc-c"] = "2.00%" // work finishes
	fr.mu.Unlock()
	now = now.Add(10 * time.Second)
	s.ReconcileOnce(context.Background())
	if !fr.has("pause oc-c") {
		t.Fatalf("did not hibernate once the work stopped: %v", fr.calls)
	}
}
