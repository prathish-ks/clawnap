package supervisor

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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
