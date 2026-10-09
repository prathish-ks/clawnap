package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prathish-ks/clawnap/internal/registry"
	"github.com/prathish-ks/clawnap/internal/runtime"
	"github.com/prathish-ks/clawnap/internal/schedule"
	_ "modernc.org/sqlite"
)

// cellWithSchedule builds a host-side state directory holding the gateway's
// job store, with one job that delivers to a person and one that does not.
func cellWithSchedule(t *testing.T, due time.Time) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, schedule.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cron_jobs (store_key TEXT NOT NULL, job_id TEXT NOT NULL,
		name TEXT NOT NULL, enabled INTEGER NOT NULL, payload_kind TEXT NOT NULL,
		job_json TEXT NOT NULL, state_json TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL,
		PRIMARY KEY (store_key, job_id)) STRICT`); err != nil {
		t.Fatal(err)
	}
	ins := func(id, jj, sj string) {
		if _, err := db.Exec(`INSERT INTO cron_jobs (store_key,job_id,name,enabled,payload_kind,job_json,state_json,updated_at)
			VALUES ('default',?,'n',1,'x',?,?,0)`, id, jj, sj); err != nil {
			t.Fatal(err)
		}
	}
	// internal maintenance, sooner but nobody is waiting on it
	ins("dream", `{"schedule":{"kind":"cron"},"payload":{"kind":"agentTurn"},"delivery":{"mode":"none"},"sessionTarget":"isolated"}`,
		`{"nextRunAtMs":`+itoaMs(due.Add(-time.Hour))+`}`)
	// the tenant's digest
	ins("digest", `{"schedule":{"kind":"cron"},"payload":{"kind":"agentTurn"},"delivery":{"mode":"telegram"},"sessionTarget":"main"}`,
		`{"nextRunAtMs":`+itoaMs(due)+`}`)
	return dir
}

func itoaMs(t time.Time) string {
	n := t.UnixMilli()
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// Hibernating a cell reads its own schedule, so the operator no longer has to
// register each due time by hand. The maintenance job, due sooner, is ignored.
func TestHibernateReadsTheCellsOwnSchedule(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	due := now.Add(90 * time.Minute)
	dir := cellWithSchedule(t, due)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StateRunning},
		netio:  map[string]string{"oc-s": "1kB / 1kB"},
		mounts: map[string]string{"oc-s": dir}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, ReadSchedules: true})
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1,
		Phase: registry.PhaseActive, IdleAfter: time.Minute})

	if err := s.Hibernate(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("s")
	if !c.NextDueAt.Equal(due) {
		t.Fatalf("NextDueAt not taken from the cell: want %v, got %v", due, c.NextDueAt)
	}
}

// With the feature off, nothing is read and an operator's own due time stands.
func TestHibernateLeavesTheOperatorsDueTimeWhenReadingIsOff(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	operator := now.Add(5 * time.Minute)
	dir := cellWithSchedule(t, now.Add(90*time.Minute))
	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StateRunning},
		netio:  map[string]string{"oc-s": "1kB / 1kB"},
		mounts: map[string]string{"oc-s": dir}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }}) // ReadSchedules off
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1,
		Phase: registry.PhaseActive, IdleAfter: time.Minute, NextDueAt: operator})

	if err := s.Hibernate(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("s")
	if !c.NextDueAt.Equal(operator) {
		t.Fatalf("operator-set due time was overwritten with the reader off: %v", c.NextDueAt)
	}
}

// A cell with no store at all must not lose the operator's due time.
func TestMissingStoreKeepsTheOperatorsDueTime(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	operator := now.Add(20 * time.Minute)
	empty := t.TempDir() // no job store at all
	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StateRunning},
		netio:  map[string]string{"oc-s": "1kB / 1kB"},
		mounts: map[string]string{"oc-s": empty}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, ReadSchedules: true})
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1,
		Phase: registry.PhaseActive, IdleAfter: time.Minute, NextDueAt: operator})

	if err := s.Hibernate(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("s")
	if !c.NextDueAt.Equal(operator) {
		t.Fatalf("a missing store wiped the operator's due time: %v", c.NextDueAt)
	}
}

// The failure that actually matters in the field: the store is there but
// cannot be read, because the gateway's schema moved under us. The feature is
// supposed to fall back to the operator's due time, and the only signal is a
// log line, so if this regressed nobody would notice until a reminder was late.
func TestUnreadableStoreKeepsTheOperatorsDueTime(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	operator := now.Add(20 * time.Minute)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a real database, but not the one this build knows how to read
	db, err := sql.Open("sqlite", filepath.Join(dir, schedule.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE automations_v2 (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StateRunning},
		netio:  map[string]string{"oc-s": "1kB / 1kB"},
		mounts: map[string]string{"oc-s": dir}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, ReadSchedules: true})
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1,
		Phase: registry.PhaseActive, IdleAfter: time.Minute, NextDueAt: operator})

	if err := s.Hibernate(context.Background(), "s"); err != nil {
		t.Fatalf("a store it cannot read must not fail the hibernate: %v", err)
	}
	c, _ := reg.Get("s")
	if !c.NextDueAt.Equal(operator) {
		t.Fatalf("an unreadable store wiped the operator's due time: %v", c.NextDueAt)
	}
}

// A scheduled job the host missed must still wake the cell. Overdue used to be
// ignored on the assumption that a past due time meant the job had fired; once
// due times come from the cell's own store, "in the past" means the host missed
// it and a tenant is waiting. Waking late beats never waking.
func TestOverdueJobStillWakesTheCell(t *testing.T) {
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-o": runtime.StatePaused},
		netio: map[string]string{"oc-o": "1kB / 1kB"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, WakeTimeout: time.Second, MaxPause: -1})
	// Due an hour ago: the daemon was down across its window.
	_ = reg.Put(registry.Cell{Name: "o", Container: "oc-o", Port: 1, IdleAfter: time.Minute,
		Phase: registry.PhaseHibernated, PausedAt: now.Add(-2 * time.Hour),
		NextDueAt: now.Add(-time.Hour)})

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	if !fr.has("unpause oc-o") {
		t.Fatalf("an overdue job never woke its cell: %v", fr.calls)
	}
	// One-shot: cleared after the wake, so it cannot wake again on every pass.
	c, _ := reg.Get("o")
	if !c.NextDueAt.IsZero() {
		t.Fatalf("one-shot due time survived the wake: %v", c.NextDueAt)
	}
}

// Reading the cell and finding nothing due must clear a due time we are still
// holding; otherwise, now that overdue wakes, a served job would wake its cell
// on every pass forever.
func TestReaderClearsADueTimeTheCellNoLongerHas(t *testing.T) {
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	dir := t.TempDir() // a store with no user-facing job at all
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, schedule.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cron_jobs (store_key TEXT NOT NULL, job_id TEXT NOT NULL,
		name TEXT NOT NULL, enabled INTEGER NOT NULL, payload_kind TEXT NOT NULL,
		job_json TEXT NOT NULL, state_json TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL,
		PRIMARY KEY (store_key, job_id)) STRICT`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	fr := &fakeRunner{state: map[string]runtime.State{"oc-z": runtime.StateRunning},
		netio: map[string]string{"oc-z": "1kB / 1kB"}, mounts: map[string]string{"oc-z": dir}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, ReadSchedules: true})
	_ = reg.Put(registry.Cell{Name: "z", Container: "oc-z", Port: 1, Phase: registry.PhaseActive,
		IdleAfter: time.Minute, NextDueAt: now.Add(-time.Hour)}) // a job that has been served

	if err := s.Hibernate(context.Background(), "z"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("z")
	if !c.NextDueAt.IsZero() {
		t.Fatalf("a served due time survived a successful read: %v", c.NextDueAt)
	}
}

// A recurring job whose wake fails must keep its due time and be retried, and
// a wake that succeeds must advance it exactly one occurrence. Both were wrong
// while reconcileCell advanced an overdue recurring time before the attempt:
// a failure discarded the occurrence, and because the caller held a stale copy
// of the cell, a success advanced it twice and skipped one.
func TestRecurringJobIsRetriedAfterAFailedWakeThenAdvancesOnce(t *testing.T) {
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	ready := false
	fr := &fakeRunner{state: map[string]runtime.State{"oc-r": runtime.StatePaused},
		netio: map[string]string{"oc-r": "1kB / 1kB"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		WakeTimeout: 10 * time.Millisecond, ReclaimWakeTimeout: 10 * time.Millisecond,
		MaxPause: -1,
		Probe: func(context.Context, int) error {
			if !ready {
				return errors.New("gateway not up")
			}
			return nil
		}})
	due := now.Add(-30 * time.Minute) // overdue, well inside its hourly interval
	_ = reg.Put(registry.Cell{Name: "r", Container: "oc-r", Port: 1, IdleAfter: time.Minute,
		Phase: registry.PhaseHibernated, PausedAt: now.Add(-2 * time.Hour),
		NextDueAt: due, NextDueEvery: time.Hour})

	s.ReconcileOnce(context.Background()) // wake fails
	s.Drain(context.Background())
	c, _ := reg.Get("r")
	if !c.NextDueAt.Equal(due) {
		t.Fatalf("a failed wake consumed the missed occurrence: want %v, got %v", due, c.NextDueAt)
	}

	ready = true
	fr.mu.Lock()
	fr.state["oc-r"] = runtime.StatePaused // it is still asleep, ready to retry
	fr.mu.Unlock()
	s.ReconcileOnce(context.Background()) // retry succeeds
	s.Drain(context.Background())
	if !fr.has("unpause oc-r") {
		t.Fatalf("the missed occurrence was never retried: %v", fr.calls)
	}
	c, _ = reg.Get("r")
	// Exactly one interval on from the first occurrence after the pre-wake
	// horizon: advancing twice would land an hour further out.
	want := due
	for !want.After(now.Add(2 * time.Minute)) {
		want = want.Add(time.Hour)
	}
	if !c.NextDueAt.Equal(want) {
		t.Fatalf("schedule did not advance exactly one occurrence: want %v, got %v", want, c.NextDueAt)
	}
}

// Retrying must be bounded: once an occurrence has been missed for longer than
// its own interval, the next one is closer than the one that keeps failing.
func TestRecurringJobStopsRetryingAfterItsIntervalHasPassed(t *testing.T) {
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	fr := &fakeRunner{state: map[string]runtime.State{"oc-s": runtime.StatePaused},
		netio: map[string]string{"oc-s": "1kB / 1kB"}}
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "cells.json"))
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		WakeTimeout: 10 * time.Millisecond, ReclaimWakeTimeout: 10 * time.Millisecond,
		MaxPause: -1,
		Probe:    func(context.Context, int) error { return errors.New("never ready") }})
	due := now.Add(-90 * time.Minute) // missed by more than its hourly interval
	_ = reg.Put(registry.Cell{Name: "s", Container: "oc-s", Port: 1, IdleAfter: time.Minute,
		Phase: registry.PhaseHibernated, PausedAt: now.Add(-3 * time.Hour),
		NextDueAt: due, NextDueEvery: time.Hour})

	s.ReconcileOnce(context.Background())
	s.Drain(context.Background())
	c, _ := reg.Get("s")
	if c.NextDueAt.Equal(due) {
		t.Fatal("kept retrying an occurrence missed by more than a full interval")
	}
	if !c.NextDueAt.After(now) {
		t.Fatalf("advanced to a time that is still in the past: %v", c.NextDueAt)
	}
}

// readOnlyDir makes a directory unwritable and reports whether that actually
// prevents writes: running as root it does not, and the test cannot be run.
func readOnlyDir(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(probe)
		return false
	}
	return true
}

// A wake that succeeds but cannot persist the new due time is not a success:
// the old due time is still on disk, so the next pass wakes the cell again for
// a job already served. The failure has to reach the caller rather than being
// dropped.
func TestFailureToPersistTheDueTimeIsReported(t *testing.T) {
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	// Already running: a message woke this cell moments before its scheduled
	// time, so the wake is a no-op success that writes nothing. The schedule
	// still has to move, which leaves the advance as the only thing that can
	// fail here.
	fr := &fakeRunner{state: map[string]runtime.State{"oc-p": runtime.StateRunning},
		netio: map[string]string{"oc-p": "1kB / 1kB"}}
	reg, err := registry.Open(filepath.Join(dir, "cells.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		Probe: func(context.Context, int) error { return nil }, WakeTimeout: time.Second, MaxPause: -1})
	if err := reg.Put(registry.Cell{Name: "p", Container: "oc-p", Port: 1, IdleAfter: time.Minute,
		Phase: registry.PhaseActive, NextDueAt: now, NextDueEvery: time.Hour}); err != nil {
		t.Fatal(err)
	}
	cell := mustGet(t, reg, "p")
	if !readOnlyDir(t, dir) {
		t.Skip("cannot make the registry unwritable (running as root?)")
	}

	err = s.wakeDue(context.Background(), cell)
	if err == nil {
		t.Fatal("a wake that could not persist its due time reported success")
	}
	if !strings.Contains(err.Error(), "could not advance") {
		t.Fatalf("error does not say what went wrong: %v", err)
	}
}

// When the wake itself fails and the advance also fails, the wake failure is
// the one worth reporting: it is the cause, and an occurrence that did not
// move is retried next pass anyway.
func TestWakeFailureOutranksAFailedAdvance(t *testing.T) {
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	fr := &fakeRunner{state: map[string]runtime.State{"oc-w": runtime.StatePaused},
		netio: map[string]string{"oc-w": "1kB / 1kB"}}
	reg, err := registry.Open(filepath.Join(dir, "cells.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(reg, runtime.Client{R: fr}, Options{Now: func() time.Time { return now },
		WakeTimeout: 10 * time.Millisecond, ReclaimWakeTimeout: 10 * time.Millisecond, MaxPause: -1,
		Probe: func(context.Context, int) error { return errors.New("gateway never came up") }})
	if err := reg.Put(registry.Cell{Name: "w", Container: "oc-w", Port: 1, IdleAfter: time.Minute,
		Phase: registry.PhaseHibernated, PausedAt: now.Add(-3 * time.Hour),
		NextDueAt: now.Add(-90 * time.Minute), NextDueEvery: time.Hour}); err != nil { // past its interval
		t.Fatal(err)
	}
	if !readOnlyDir(t, dir) {
		t.Skip("cannot make the registry unwritable (running as root?)")
	}

	err = s.wakeDue(context.Background(), mustGet(t, reg, "w"))
	if err == nil {
		t.Fatal("expected the wake failure to be reported")
	}
	if strings.Contains(err.Error(), "could not advance") {
		t.Fatalf("the bookkeeping failure masked the real cause: %v", err)
	}
}

func mustGet(t *testing.T, reg *registry.Store, name string) registry.Cell {
	t.Helper()
	c, err := reg.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
