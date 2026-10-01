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

// With WarmKeep set, a paused cell is trimmed to the warm floor at once
// (WarmAt stamped, ReclaimedAt untouched) and only reaches the cold floor
// when the timed policy or memory pressure says so.
func TestTwoStageReclaimWarmThenCold(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-w": runtime.StatePaused}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	cg := t.TempDir()
	d := fakeCgroup(t, cg, "oc-w")
	s := New(reg, runtime.Client{R: fr}, Options{
		Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil },
		ReclaimAfter: 10 * time.Minute, MaxPause: 24 * time.Hour, WarmKeep: 300 << 20, ReclaimKeep: 150 << 20,
		HotSetDir: filepath.Join(t.TempDir(), "hotsets"), Reclaimer: &reclaim.Reclaimer{FS: cg}})
	_ = reg.Put(registry.Cell{Name: "w", Container: "oc-w", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Minute), IdleAfter: time.Minute})

	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("w")
	if !reclaimWritten(t, d) || c.WarmAt.IsZero() || !c.ReclaimedAt.IsZero() {
		t.Fatalf("expected the warm stage only: reclaim=%v warm=%v cold=%v", reclaimWritten(t, d), c.WarmAt, c.ReclaimedAt)
	}
	_ = os.WriteFile(filepath.Join(d, "memory.reclaim"), []byte(""), 0o644)
	now = now.Add(2 * time.Minute)
	s.ReconcileOnce(context.Background())
	c, _ = reg.Get("w")
	if reclaimWritten(t, d) || !c.ReclaimedAt.IsZero() {
		t.Fatal("must not go cold before ReclaimAfter without pressure")
	}
	now = now.Add(10 * time.Minute)
	s.ReconcileOnce(context.Background())
	c, _ = reg.Get("w")
	if !reclaimWritten(t, d) || c.ReclaimedAt.IsZero() {
		t.Fatalf("expected the cold stage: %+v", c)
	}
	// wake: a cell that only reached the warm floor is woken as "warm" with no prefetch
	fr2 := &fakeRunner{state: map[string]runtime.State{"oc-x": runtime.StatePaused}}
	reg2, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s2 := New(reg2, runtime.Client{R: fr2}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, WakeTimeout: time.Second, PrefetchOnWake: true, Reclaimer: &reclaim.Reclaimer{FS: cg}})
	_ = reg2.Put(registry.Cell{Name: "x", Container: "oc-x", Port: 2, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Minute), WarmAt: now.Add(-50 * time.Second), Swapped: true})
	if _, err := s2.Wake(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if fr2.has("inspect -f {{.Id}}") { // the prefetch path resolves the container id; a warm wake must not
		t.Fatalf("warm wake must skip prefetch: %v", fr2.calls)
	}
}
