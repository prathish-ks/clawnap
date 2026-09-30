package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/reclaim"
	"github.com/prathish-ks/fleet-supervisor/internal/registry"
	"github.com/prathish-ks/fleet-supervisor/internal/runtime"
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
