package supervisor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/registry"
	"github.com/prathish-ks/fleet-supervisor/internal/runtime"
)

// fakeRunner scripts a runtime per container.
type fakeRunner struct {
	mu     sync.Mutex
	state  map[string]runtime.State
	netio  map[string]string
	calls  []string
	failOn string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	name := args[len(args)-1]
	switch args[0] {
	case "inspect":
		st, ok := f.state[name]
		if !ok {
			return "", errors.New("Error: No such object: " + name)
		}
		return string(st) + "\n", nil
	case "stats":
		return `{"NetIO":"` + f.netio[name] + `","MemUsage":"300MiB / 4GiB"}`, nil
	case "stop":
		if f.failOn == "stop" {
			return "", errors.New("boom")
		}
		f.state[name] = runtime.StateExited
		return "", nil
	case "start":
		if f.failOn == "start" {
			return "", errors.New("boom")
		}
		f.state[name] = runtime.StateRunning
		return "", nil
	case "pause":
		f.state[name] = runtime.StatePaused
		return "", nil
	case "unpause":
		f.state[name] = runtime.StateRunning
		return "", nil
	}
	return "", errors.New("unexpected " + args[0])
}

func (f *fakeRunner) has(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func newSup(t *testing.T, fr *fakeRunner, now *time.Time) (*Supervisor, *registry.Store) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	if err != nil {
		t.Fatal(err)
	}
	okProbe := func(context.Context, int) error { return nil }
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return *now }, Probe: okProbe, WakeTimeout: time.Second, MaxConcurrent: 2})
	return s, reg
}

func TestIdleCellHibernatesAfterPolicy(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-a": runtime.StateRunning}, netio: map[string]string{"oc-a": "1kB / 1kB"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 1, IdleAfter: 10 * time.Minute})

	s.ReconcileOnce(context.Background()) // first sample establishes baseline
	now = now.Add(5 * time.Minute)
	s.ReconcileOnce(context.Background())
	if fr.has("stop") {
		t.Fatal("stopped too early")
	}
	now = now.Add(6 * time.Minute) // 11m idle
	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("a")
	if !fr.has("pause oc-a") || fr.has("stop") || c.Phase != registry.PhaseHibernated {
		t.Fatalf("expected pause-tier hibernation, got phase=%s calls=%v", c.Phase, fr.calls)
	}
	// wake from paused must unpause, not start
	res, err := s.Wake(context.Background(), "a")
	if err != nil || !fr.has("unpause oc-a") || fr.has("start oc-a") || res.AlreadyRunning {
		t.Fatalf("expected unpause wake, err=%v calls=%v res=%+v", err, fr.calls, res)
	}
}

func TestStopTierUsesStopAndStart(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StateRunning}, netio: map[string]string{"oc-s": "1kB / 1kB"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1, Tier: registry.TierStop, IdleAfter: time.Minute})
	s.ReconcileOnce(context.Background())
	now = now.Add(2 * time.Minute)
	s.ReconcileOnce(context.Background())
	if !fr.has("stop -t 10 oc-s") || fr.has("pause") {
		t.Fatalf("expected stop-tier, calls=%v", fr.calls)
	}
	if _, err := s.Wake(context.Background(), "s"); err != nil || !fr.has("start oc-s") {
		t.Fatalf("expected start wake, err=%v calls=%v", err, fr.calls)
	}
}

func TestHTTPHealthProbeRequires200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	if err := HTTPHealthProbe(context.Background(), port); err != nil {
		t.Fatalf("expected ready, got %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	bport, _ := strconv.Atoi(strings.TrimPrefix(bad.URL, "http://127.0.0.1:"))
	if err := HTTPHealthProbe(context.Background(), bport); err == nil {
		t.Fatal("503 must not count as ready")
	}
}

func TestTrafficKeepsCellAwake(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-a": runtime.StateRunning}, netio: map[string]string{"oc-a": "1kB / 1kB"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", IdleAfter: 10 * time.Minute})
	s.ReconcileOnce(context.Background())
	now = now.Add(11 * time.Minute)
	fr.netio["oc-a"] = "900kB / 1kB" // real traffic in the window
	s.ReconcileOnce(context.Background())
	if fr.has("stop") {
		t.Fatal("cell with traffic must not hibernate")
	}
}

func TestAlwaysOnNeverHibernatesAndSelfHeals(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-w": runtime.StateRunning}, netio: map[string]string{"oc-w": "1kB / 1kB"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "w", Container: "oc-w", Class: registry.ClassAlwaysOn, IdleAfter: time.Minute, Port: 9})
	s.ReconcileOnce(context.Background())
	now = now.Add(time.Hour)
	s.ReconcileOnce(context.Background())
	if fr.has("stop") {
		t.Fatal("always-on must never be stopped")
	}
	fr.state["oc-w"] = runtime.StateExited // crashed
	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("w")
	if !fr.has("start oc-w") || c.Phase != registry.PhaseActive || c.Restarts != 1 {
		t.Fatalf("expected self-heal, got %+v calls=%v", c, fr.calls)
	}
}

func TestWakeCoalescesAndReportsTimings(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-a": runtime.StateExited}, netio: map[string]string{}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 1, Phase: registry.PhaseHibernated})
	var wg sync.WaitGroup
	results := make([]WakeResult, 3)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], _ = s.Wake(context.Background(), "a") }(i)
	}
	wg.Wait()
	starts := 0
	fr.mu.Lock()
	for _, c := range fr.calls {
		if c == "start oc-a" {
			starts++
		}
	}
	fr.mu.Unlock()
	if starts != 1 {
		t.Fatalf("expected exactly one start, got %d", starts)
	}
	c, _ := reg.Get("a")
	if c.Phase != registry.PhaseActive {
		t.Fatalf("phase=%s", c.Phase)
	}
}

func TestWakeTimeoutMarksFailed(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-a": runtime.StateExited}, netio: map[string]string{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 1, Phase: registry.PhaseHibernated})
	badProbe := func(context.Context, int) error { return errors.New("refused") }
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: badProbe, WakeTimeout: 300 * time.Millisecond, StopWakeTimeout: 300 * time.Millisecond})
	if _, err := s.Wake(context.Background(), "a"); err == nil {
		t.Fatal("expected timeout")
	}
	c, _ := reg.Get("a")
	if c.Phase != registry.PhaseFailed {
		t.Fatalf("phase=%s", c.Phase)
	}
}

func TestCronAwareNoSleepWhenJobDueInsideWindow(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-j": runtime.StateRunning}, netio: map[string]string{"oc-j": "1kB / 1kB"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "j", Container: "oc-j", Port: 1, IdleAfter: 10 * time.Minute, NextDueAt: now.Add(15 * time.Minute)})
	s.ReconcileOnce(context.Background())
	now = now.Add(11 * time.Minute) // idle, but the job is 4 min away (< 10 min window)
	s.ReconcileOnce(context.Background())
	if fr.has("pause") || fr.has("stop") {
		t.Fatalf("must not hibernate with a job inside the idle window: %v", fr.calls)
	}
}

func TestCronAwarePreWakeOfHibernatedCell(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-p": runtime.StatePaused}, netio: map[string]string{}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "p", Container: "oc-p", Port: 1, Phase: registry.PhaseHibernated, IdleAfter: time.Minute, NextDueAt: now.Add(90 * time.Second)})
	s.ReconcileOnce(context.Background()) // 90 s away < 2 min PreWake => wake now
	c, _ := reg.Get("p")
	if !fr.has("unpause oc-p") || c.Phase != registry.PhaseActive {
		t.Fatalf("expected pre-wake, phase=%s calls=%v", c.Phase, fr.calls)
	}
}

func TestDaemonRestartReconcilesExternalPause(t *testing.T) {
	// Registry says active (daemon died mid-flight), runtime says paused:
	// on restart the supervisor must adopt the real state, not fight it.
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-r": runtime.StatePaused}, netio: map[string]string{}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "r", Container: "oc-r", Port: 1, Phase: registry.PhaseActive, IdleAfter: time.Hour})
	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("r")
	if c.Phase != registry.PhaseHibernated || fr.has("unpause") || fr.has("start") {
		t.Fatalf("expected adoption as hibernated without action, phase=%s calls=%v", c.Phase, fr.calls)
	}
}
