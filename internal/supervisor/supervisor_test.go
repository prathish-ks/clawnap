package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/reclaim"
	"github.com/prathish-ks/fleet-supervisor/internal/registry"
	"github.com/prathish-ks/fleet-supervisor/internal/runtime"
)

// fakeRunner scripts a runtime per container.
// keepExited makes "start" leave the container exited, simulating a gateway
// that dies immediately after the runtime reports it started.
var keepExited = map[*fakeRunner]bool{}

type fakeRunner struct {
	logs   map[string]string // container -> log text returned by "logs"
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
		if len(args) > 2 && args[2] == "{{json .Mounts}}" {
			return "[]", nil
		}
		if len(args) > 2 && args[2] == "{{.Id}}" {
			return "cid-" + name + "-0000000000000000", nil // full-length id like docker prints
		}
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
		if !keepExited[f] {
			f.state[name] = runtime.StateRunning
		}
		return "", nil
	case "pause":
		f.state[name] = runtime.StatePaused
		return "", nil
	case "inspect-mounts":
		return "[]", nil
	case "logs":
		return f.logs[args[len(args)-1]], nil // f.mu already held by Run
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
	// unknown-age pauses are adopted as already at the cap, so the next pass pulses rather than trusting a clock we never saw start
	if c.Phase != registry.PhaseHibernated || fr.has("unpause") || fr.has("start") || !c.PausedAt.Equal(now.Add(-20*time.Minute)) {
		t.Fatalf("expected adoption as hibernated (PausedAt=now-MaxPause) without action, cell=%+v calls=%v", c, fr.calls)
	}
}

func TestPauseCapPulsesLongFrozenCell(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-z": runtime.StateRunning}, netio: map[string]string{"oc-z": "1kB / 1kB"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	okProbe := func(context.Context, int) error { return nil }
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: okProbe, MaxPause: 10 * time.Minute, PulseWindow: time.Millisecond})
	_ = reg.Put(registry.Cell{Name: "z", Container: "oc-z", Port: 1, IdleAfter: time.Minute})
	s.ReconcileOnce(context.Background())
	now = now.Add(2 * time.Minute)
	s.ReconcileOnce(context.Background()) // hibernates (pause)
	c, _ := reg.Get("z")
	if c.Phase != registry.PhaseHibernated || c.PausedAt.IsZero() {
		t.Fatalf("expected paused with PausedAt, got %+v", c)
	}
	now = now.Add(5 * time.Minute) // under cap
	s.ReconcileOnce(context.Background())
	if fr.has("unpause") {
		t.Fatal("must not pulse before the cap")
	}
	now = now.Add(6 * time.Minute) // 11 min frozen > 10 min cap
	s.ReconcileOnce(context.Background())
	c, _ = reg.Get("z")
	pauses := 0
	fr.mu.Lock()
	for _, x := range fr.calls {
		if x == "pause oc-z" {
			pauses++
		}
	}
	fr.mu.Unlock()
	if !fr.has("unpause oc-z") || pauses != 2 || c.Phase != registry.PhaseHibernated || !c.PausedAt.Equal(now) {
		t.Fatalf("expected pulse (unpause, re-pause, fresh PausedAt): calls=%v cell=%+v", fr.calls, c)
	}
}

func TestPauseCapStopFallthrough(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-y": runtime.StatePaused}, netio: map[string]string{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, MaxPause: 10 * time.Minute, PauseFallthrough: "stop"})
	_ = reg.Put(registry.Cell{Name: "y", Container: "oc-y", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Hour), IdleAfter: time.Minute})
	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("y")
	if !fr.has("unpause oc-y") || !fr.has("stop -t 10 oc-y") || c.Phase != registry.PhaseHibernated || !c.PausedAt.IsZero() {
		t.Fatalf("expected unpause+stop fallthrough: calls=%v cell=%+v", fr.calls, c)
	}
}

func TestMetricsExposition(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-m": runtime.StateExited}, netio: map[string]string{}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "m", Container: "oc-m", Port: 1, Phase: registry.PhaseHibernated, Tier: registry.TierStop})
	if _, err := s.Wake(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	s.Metrics().Write(&b, s.Cells())
	out := b.String()
	for _, want := range []string{`fleetd_wakes_total{kind="stop"} 1`, `fleetd_cells{phase="active"} 1`, `fleetd_wake_ready_seconds_count 1`, `fleetd_wake_failures_total 0`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestReclaimAfterPauseWritesCgroup(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-r": runtime.StatePaused}, netio: map[string]string{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	cg := t.TempDir()
	d := filepath.Join(cg, "docker", "cid-oc-r-0000000000000000")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "memory.current"), []byte("800000000"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.swap.current"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.reclaim"), []byte(""), 0o644)
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, ReclaimAfter: 5 * time.Minute, MaxPause: time.Hour, Reclaimer: &reclaim.Reclaimer{FS: cg}})
	_ = reg.Put(registry.Cell{Name: "r", Container: "oc-r", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-2 * time.Minute), IdleAfter: time.Minute})
	s.ReconcileOnce(context.Background())
	if b, _ := os.ReadFile(filepath.Join(d, "memory.reclaim")); string(b) != "" {
		t.Fatal("must not reclaim before ReclaimAfter")
	}
	now = now.Add(4 * time.Minute) // 6 min paused
	s.ReconcileOnce(context.Background())
	b, _ := os.ReadFile(filepath.Join(d, "memory.reclaim"))
	c, _ := reg.Get("r")
	if string(b) == "" || c.ReclaimedAt.IsZero() {
		t.Fatalf("expected reclaim write and ReclaimedAt: %q %+v", b, c)
	}
	_ = os.WriteFile(filepath.Join(d, "memory.reclaim"), []byte(""), 0o644)
	now = now.Add(time.Minute)
	s.ReconcileOnce(context.Background())
	if b, _ := os.ReadFile(filepath.Join(d, "memory.reclaim")); string(b) != "" {
		t.Fatal("must not reclaim twice for the same pause")
	}
}

// slowRunner delays one operation so concurrency can be observed.
type slowRunner struct {
	fakeRunner
	slowOp string
	delay  time.Duration
}

func (r *slowRunner) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == r.slowOp {
		time.Sleep(r.delay)
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReconcileDoesNotSerialiseCells(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &slowRunner{fakeRunner: fakeRunner{state: map[string]runtime.State{}, netio: map[string]string{}}, slowOp: "stats", delay: 300 * time.Millisecond}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	for _, n := range []string{"a", "b", "c", "d"} {
		fr.state["oc-"+n] = runtime.StateRunning
		fr.netio["oc-"+n] = "1kB / 1kB"
		_ = reg.Put(registry.Cell{Name: n, Container: "oc-" + n, Port: 1, IdleAfter: time.Hour})
	}
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, MaxConcurrent: 4})
	t0 := time.Now()
	s.ReconcileOnce(context.Background())
	if el := time.Since(t0); el > 900*time.Millisecond {
		t.Fatalf("4 cells x 300 ms stats took %v; expected parallel (~300 ms)", el)
	}
}

func TestWakeWaitsForHibernateOnSameCellAndSkipsBusyReconcile(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &slowRunner{fakeRunner: fakeRunner{state: map[string]runtime.State{"oc-h": runtime.StateRunning}, netio: map[string]string{"oc-h": "1kB / 1kB"}}, slowOp: "pause", delay: 400 * time.Millisecond}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "h", Container: "oc-h", Port: 1, IdleAfter: time.Minute})
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }})
	done := make(chan error, 1)
	go func() { done <- s.Hibernate(context.Background(), "h") }()
	time.Sleep(50 * time.Millisecond) // hibernate holds the cell lock, inside the slow pause
	t0 := time.Now()
	res, err := s.Wake(context.Background(), "h") // must wait, then unpause
	if err != nil || res.AlreadyRunning || time.Since(t0) < 300*time.Millisecond {
		t.Fatalf("wake should have waited for the hibernate: %+v %v after %v", res, err, time.Since(t0))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("h")
	if c.Phase != registry.PhaseActive || !fr.has("unpause oc-h") {
		t.Fatalf("expected final state active via unpause: %+v calls=%v", c, fr.calls)
	}
}

func TestReclaimYieldsToPendingWake(t *testing.T) {
	// cgroup fake whose memory.current shrinks 32 MiB per 64 MiB request (never stalls)
	cg := t.TempDir()
	d := filepath.Join(cg, "docker", "cid-oc-w-0000000000000000")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "memory.current"), []byte("800000000"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.swap.current"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.reclaim"), []byte(""), 0o644)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-w": runtime.StatePaused}, netio: map[string]string{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "w", Container: "oc-w", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Hour), IdleAfter: time.Minute})
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, ReclaimAfter: time.Minute, MaxPause: 24 * time.Hour, Reclaimer: &reclaim.Reclaimer{FS: cg}})
	// drive the fake cgroup: each write shrinks current by 32 MiB, slowly
	stopDrv := make(chan struct{})
	go func() {
		last := ""
		for {
			select {
			case <-stopDrv:
				return
			default:
			}
			b, _ := os.ReadFile(filepath.Join(d, "memory.reclaim"))
			if string(b) != "" && string(b) != last {
				last = string(b)
				cur, _ := os.ReadFile(filepath.Join(d, "memory.current"))
				var n int64
				fmt.Sscan(string(cur), &n)
				time.Sleep(60 * time.Millisecond)
				_ = os.WriteFile(filepath.Join(d, "memory.current"), []byte(fmt.Sprint(n-32<<20)), 0o644)
				_ = os.WriteFile(filepath.Join(d, "memory.reclaim"), []byte(""), 0o644)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	defer close(stopDrv)
	go s.ReconcileOnce(context.Background()) // starts a long chunked reclaim
	time.Sleep(150 * time.Millisecond)
	t0 := time.Now()
	res, err := s.Wake(context.Background(), "w")
	if err != nil || res.AlreadyRunning {
		t.Fatalf("wake failed: %+v %v", res, err)
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("wake should interrupt reclaim within a chunk, took %v", el)
	}
	cur, _ := os.ReadFile(filepath.Join(d, "memory.current"))
	var n int64
	fmt.Sscan(string(cur), &n)
	if n <= 800000000-20*32<<20 {
		t.Fatalf("reclaim should have stopped early, current=%d", n)
	}
}

// Regression for the semaphore double-acquire: N always-on cells exited with
// MaxConcurrent=N and a slow inspect must all be restarted, not deadlock.
func TestReconcileWakesDoNotDeadlockOnSemaphore(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &slowRunner{fakeRunner: fakeRunner{state: map[string]runtime.State{}, netio: map[string]string{}}, slowOp: "inspect", delay: 100 * time.Millisecond}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	for _, n := range []string{"a", "b", "c", "d"} {
		fr.state["oc-"+n] = runtime.StateExited
		_ = reg.Put(registry.Cell{Name: n, Container: "oc-" + n, Port: 1, Class: registry.ClassAlwaysOn, IdleAfter: time.Hour})
	}
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, MaxConcurrent: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.ReconcileOnce(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("ReconcileOnce deadlocked")
	}
	starts := 0
	fr.mu.Lock()
	for _, c := range fr.calls {
		if strings.HasPrefix(c, "start oc-") {
			starts++
		}
	}
	fr.mu.Unlock()
	if starts != 4 {
		t.Fatalf("expected 4 starts, got %d (calls=%v)", starts, fr.calls)
	}
}

func TestCoalescedFollowersSeeLeaderFailure(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &slowRunner{fakeRunner: fakeRunner{state: map[string]runtime.State{"oc-f": runtime.StateExited}, netio: map[string]string{}, failOn: "start"}, slowOp: "start", delay: 200 * time.Millisecond}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "f", Container: "oc-f", Port: 1, Phase: registry.PhaseHibernated, Tier: registry.TierStop})
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }})
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() { _, err := s.Wake(context.Background(), "f"); errs <- err }()
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		if err := <-errs; err == nil {
			t.Fatal("a follower reported success although the leader's start failed")
		}
	}
}

func TestWakeSurvivesCallerCancellation(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &slowRunner{fakeRunner: fakeRunner{state: map[string]runtime.State{"oc-c": runtime.StateExited}, netio: map[string]string{}}, slowOp: "start", delay: 300 * time.Millisecond}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "c", Container: "oc-c", Port: 1, Phase: registry.PhaseHibernated, Tier: registry.TierStop})
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }() // the webhook client gives up mid-boot
	res, err := s.Wake(ctx, "c")
	if err != nil || res.AlreadyRunning {
		t.Fatalf("wake must complete despite caller cancellation: %+v %v", res, err)
	}
	c, _ := reg.Get("c")
	if c.Phase != registry.PhaseActive {
		t.Fatalf("phase=%s", c.Phase)
	}
}

func TestOneShotDueTimeIsClearedAfterWake(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-o": runtime.StatePaused}, netio: map[string]string{"oc-o": "1kB / 1kB"}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "o", Container: "oc-o", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now, IdleAfter: time.Minute, NextDueAt: now.Add(time.Minute)})
	s.ReconcileOnce(context.Background()) // pre-wake fires
	c, _ := reg.Get("o")
	if c.Phase != registry.PhaseActive || !c.NextDueAt.IsZero() {
		t.Fatalf("one-shot due time must be cleared after the wake: %+v", c)
	}
	// and a stale past due time never blocks hibernation
	_ = reg.Update("o", func(x *registry.Cell) { x.NextDueAt = now.Add(-time.Hour) })
	fr.state["oc-o"] = runtime.StateRunning
	s.ReconcileOnce(context.Background())
	now = now.Add(2 * time.Minute)
	s.ReconcileOnce(context.Background())
	if !fr.has("pause oc-o") {
		t.Fatalf("stale due time must not keep the cell awake: %v", fr.calls)
	}
}

func TestRecurringDueTimeAdvances(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-q": runtime.StatePaused}, netio: map[string]string{}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "q", Container: "oc-q", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now, IdleAfter: time.Minute, NextDueAt: now.Add(time.Minute), NextDueEvery: 24 * time.Hour})
	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("q")
	if !c.NextDueAt.Equal(now.Add(time.Minute + 24*time.Hour)) {
		t.Fatalf("recurring due time must advance by one period: %v", c.NextDueAt)
	}
}

func TestStaleWakingPhaseIsAdoptedAfterRestart(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StateExited}, netio: map[string]string{}}
	s, reg := newSup(t, fr, &now)
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1, Class: registry.ClassAlwaysOn, Phase: registry.PhaseWaking, IdleAfter: time.Hour})
	s.ReconcileOnce(context.Background()) // no wake in flight: Waking is a crash leftover
	c, _ := reg.Get("s")
	if !fr.has("start oc-s") || c.Phase != registry.PhaseActive || c.Restarts != 1 {
		t.Fatalf("stale Waking must be adopted and the always-on cell self-healed: %+v calls=%v", c, fr.calls)
	}
}

func TestFailedWakeKeepsPauseClock(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-k": runtime.StatePaused}, netio: map[string]string{}}
	s, reg := newSup(t, fr, &now)
	paused := now.Add(-19 * time.Minute)
	_ = reg.Put(registry.Cell{Name: "k", Container: "oc-k", Port: 1, Phase: registry.PhaseFailed, PausedAt: paused, ReclaimedAt: now.Add(-10 * time.Minute), IdleAfter: time.Minute})
	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("k")
	if c.Phase != registry.PhaseHibernated || !c.PausedAt.Equal(paused) || c.ReclaimedAt.IsZero() {
		t.Fatalf("failed wake must keep the original pause clock and reclaim mark: %+v", c)
	}
}

func TestReclaimedCellGetsLongerWakeTimeout(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-t": runtime.StatePaused}, netio: map[string]string{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "t", Container: "oc-t", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Hour), ReclaimedAt: now.Add(-30 * time.Minute), Swapped: true})
	calls := 0
	slowProbe := func(context.Context, int) error {
		calls++
		if calls < 4 {
			return errors.New("paging in")
		}
		return nil
	}
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: slowProbe, WakeTimeout: 300 * time.Millisecond, ReclaimWakeTimeout: 5 * time.Second})
	if _, err := s.Wake(context.Background(), "t"); err != nil {
		t.Fatalf("reclaimed cell must get the longer timeout: %v", err)
	}
	var b strings.Builder
	s.Metrics().Write(&b, s.Cells())
	if !strings.Contains(b.String(), `fleetd_wakes_total{kind="reclaimed"} 1`) {
		t.Fatalf("reclaimed wakes must be labelled separately:\n%s", b.String())
	}
}

func TestUnsupportedReclaimDoesNotMarkCellReclaimed(t *testing.T) {
	// a host without memory.reclaim stamps ReclaimedAt to stop retrying, but
	// the cell must not be treated as swapped: no long timeout, no prefetch,
	// no "reclaimed" wake kind
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-u": runtime.StatePaused}, netio: map[string]string{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "u", Container: "oc-u", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-time.Hour), IdleAfter: time.Minute})
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, ReclaimAfter: time.Minute, MaxPause: 24 * time.Hour, Reclaimer: &reclaim.Reclaimer{FS: t.TempDir()}}) // empty cgroup tree: ErrNoCgroup
	s.ReconcileOnce(context.Background())
	c, _ := reg.Get("u")
	if c.ReclaimedAt.IsZero() || c.Swapped || reclaimed(c) {
		t.Fatalf("unsupported reclaim must stamp but not mark swapped: %+v", c)
	}
	if _, err := s.Wake(context.Background(), "u"); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	s.Metrics().Write(&b, s.Cells())
	if strings.Contains(b.String(), `kind="reclaimed"`) {
		t.Fatalf("never-reclaimed cell must not be counted as a reclaimed wake:\n%s", b.String())
	}
}

func TestWakeFailsFastWhenContainerExitsDuringStartup(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	// start "succeeds" but the container is exited on every later inspect
	fr := &fakeRunner{state: map[string]runtime.State{"oc-d": runtime.StateExited}, netio: map[string]string{}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "d", Container: "oc-d", Port: 1, Phase: registry.PhaseHibernated, Tier: registry.TierStop})
	neverReady := func(context.Context, int) error { return errors.New("connection refused") }
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: neverReady, StopWakeTimeout: 30 * time.Second})
	keepExited[fr] = true
	t0 := time.Now()
	_, err := s.Wake(context.Background(), "d")
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("expected fail-fast on an exited container, got %v", err)
	}
	if el := time.Since(t0); el > 5*time.Second {
		t.Fatalf("fail-fast took %v; must not wait the 30 s timeout", el)
	}
}

func TestThawSettleWaitsForRecoveryMarker(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StatePaused}, netio: map[string]string{}, logs: map[string]string{"oc-s": "[health] host timing gap detected: process was frozen ~90000ms; restarting channels when idle"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-2 * time.Minute)})
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, ThawSettle: 2 * time.Second})
	go func() { // recovery completes 300 ms after the thaw
		time.Sleep(300 * time.Millisecond)
		fr.mu.Lock()
		fr.logs["oc-s"] += "\n[admission] reopened: suspend phase"
		fr.mu.Unlock()
	}()
	t0 := time.Now()
	if _, err := s.Wake(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	el := time.Since(t0)
	if el < 250*time.Millisecond || el > 1500*time.Millisecond {
		t.Fatalf("wake should settle on the marker (~300 ms), took %v", el)
	}
}

func TestThawSettleReturnsFastWhenNoRecoveryTriggered(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-q": runtime.StatePaused}, netio: map[string]string{}, logs: map[string]string{"oc-q": "[gateway] ordinary line"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	_ = reg.Put(registry.Cell{Name: "q", Container: "oc-q", Port: 1, Phase: registry.PhaseHibernated, PausedAt: now.Add(-10 * time.Second)})
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now }, Probe: func(context.Context, int) error { return nil }, ThawSettle: 3 * time.Second})
	t0 := time.Now()
	if _, err := s.Wake(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el > time.Second {
		t.Fatalf("short freeze must not wait the full settle bound, took %v", el)
	}
}
