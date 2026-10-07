package schedule

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The schema and the job shapes are copied from a real OpenClaw 2026.9.7 cell
// (read on the fleet host, 2026-10-05).
const schema = `CREATE TABLE cron_jobs (
  store_key TEXT NOT NULL, job_id TEXT NOT NULL, declaration_key TEXT, owner_agent_id TEXT,
  name TEXT NOT NULL, description TEXT, enabled INTEGER NOT NULL, agent_id TEXT,
  payload_kind TEXT NOT NULL, job_json TEXT NOT NULL, state_json TEXT NOT NULL DEFAULT '{}',
  schedule_identity TEXT, sort_order INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL,
  PRIMARY KEY (store_key, job_id)) STRICT`

func newDB(t *testing.T, rows [][3]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows { // r = {name, job_json, state_json}
		if _, err := db.Exec(`INSERT INTO cron_jobs
			(store_key, job_id, name, enabled, payload_kind, job_json, state_json, updated_at)
			VALUES ('default', ?, ?, 1, 'x', ?, ?, 0)`,
			"job"+string(rune('a'+i)), r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ms(t time.Time) string { return `{"nextRunAtMs":` + itoa(t.UnixMilli()) + `}` }

func itoa(n int64) string {
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// Next picks the earliest job a person is actually waiting for, and ignores
// the three jobs a stock cell ships with.
func TestNextPicksUserFacingWorkAndIgnoresStockJobs(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	reminder := now.Add(2 * time.Hour)
	dir := newDB(t, [][3]string{
		// the three a stock cell ships with
		{"heartbeat-main", `{"schedule":{"kind":"every","everyMs":1800000},"wakeMode":"next-heartbeat","payload":{"kind":"heartbeat"},"sessionTarget":"main"}`, ms(now.Add(10 * time.Minute))},
		{"Memory Dreaming Promotion", `{"schedule":{"kind":"cron","expr":"0 3 * * *"},"wakeMode":"now","payload":{"kind":"agentTurn"},"delivery":{"mode":"none"},"sessionTarget":"isolated"}`, ms(now.Add(30 * time.Minute))},
		{"skill-collection-review-main", `{"schedule":{"kind":"every","everyMs":604800000},"wakeMode":"next-heartbeat","payload":{"kind":"agentTurn"},"delivery":{"mode":"none"},"sessionTarget":"isolated"}`, ms(now.Add(time.Hour))},
		// what the tenant actually cares about
		{"morning digest", `{"schedule":{"kind":"cron","expr":"0 11 * * *"},"wakeMode":"now","payload":{"kind":"agentTurn"},"delivery":{"mode":"telegram"},"sessionTarget":"main"}`, ms(reminder)},
	})
	got, err := Next(context.Background(), dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(reminder) {
		t.Fatalf("want the delivering job at %v, got %v (a stock job was treated as user-facing)", reminder, got)
	}
}

// A one-shot always counts: it catches up rather than coalescing, so missing
// its time means the tenant's reminder arrives late.
func TestOneShotCountsEvenWithoutADeliveryTarget(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	at := now.Add(30 * time.Minute)
	dir := newDB(t, [][3]string{
		{"remind me", `{"schedule":{"kind":"at"},"wakeMode":"now","payload":{"kind":"agentTurn"},"sessionTarget":"main"}`, ms(at)},
	})
	got, err := Next(context.Background(), dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(at) {
		t.Fatalf("one-shot not treated as user-facing: got %v", got)
	}
}

// Fail toward waking: a job this build does not recognise is woken for, because
// over-waking is visible and under-waking is silent.
func TestUnrecognisedJobIsTreatedAsUserFacing(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	due := now.Add(15 * time.Minute)
	dir := newDB(t, [][3]string{
		{"something new", `{"schedule":{"kind":"quantum"},"payload":{"kind":"somethingUpstreamAdded"}}`, ms(due)},
	})
	got, err := Next(context.Background(), dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(due) {
		t.Fatalf("unknown job was ignored instead of woken for: got %v", got)
	}
}

// Past due times are the gateway's own catch-up, not something to wake for.
func TestPastDueTimesAreIgnored(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	dir := newDB(t, [][3]string{
		{"overdue", `{"schedule":{"kind":"at"},"delivery":{"mode":"telegram"}}`, ms(now.Add(-time.Hour))},
	})
	got, err := Next(context.Background(), dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("woke for a time already past: %v", got)
	}
}

// The host must never see what a job will say. The query extracts scheduling
// fields only, so prompt text cannot reach this process.
func TestPayloadTextNeverLeavesTheCell(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	const secret = "TENANT-PRIVATE-PROMPT-DO-NOT-READ"
	dir := newDB(t, [][3]string{
		{"name " + secret, `{"schedule":{"kind":"at"},"description":"` + secret + `","payload":{"kind":"agentTurn","message":"` + secret + `"}}`, ms(now.Add(time.Hour))},
	})
	jobs, err := Read(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	for _, f := range []string{jobs[0].Kind, jobs[0].WakeMode, jobs[0].Payload, jobs[0].Delivery, jobs[0].Session} {
		if strings.Contains(f, secret) {
			t.Fatalf("payload text reached the host in %q", f)
		}
	}
}

// A cell with no store at all is not an error; one with only the legacy file
// says so, rather than guessing at a schema nobody has verified.
func TestMissingAndLegacyStores(t *testing.T) {
	empty := t.TempDir()
	got, err := Next(context.Background(), empty, time.Now())
	if err != nil || !got.IsZero() {
		t.Fatalf("missing store should be silent: got %v, %v", got, err)
	}

	legacy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacy, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, LegacyJobsPath), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Next(context.Background(), legacy, time.Now()); err == nil {
		t.Fatal("legacy-only store should report that it cannot be read")
	}
}

// A file that is not a database, and a database without the table, are the two
// ways this can genuinely fail in the field: a store the daemon cannot open,
// and a schema that moved under us. Both must report rather than look empty,
// so the caller keeps the operator-set due time instead of silently losing it.
func TestUnreadableStoresReportRatherThanLookEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{"not a database", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, DBPath), []byte("this is not sqlite"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"schema moved: no cron_jobs table", func(t *testing.T, dir string) {
			db, err := sql.Open("sqlite", filepath.Join(dir, DBPath))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE something_else (x TEXT)`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.write(t, dir)
			got, err := Next(context.Background(), dir, time.Now())
			if err == nil {
				t.Fatal("an unreadable store returned no error, so a caller cannot tell it apart from a cell with nothing scheduled")
			}
			if !got.IsZero() {
				t.Fatalf("returned a due time from an unreadable store: %v", got)
			}
		})
	}
}
