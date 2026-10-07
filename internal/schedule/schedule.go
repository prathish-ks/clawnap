// Package schedule reads a cell's own scheduled jobs from the host, so a
// hibernated cell can be woken before work that a person is waiting for.
//
// A cell's OpenClaw home is a host bind mount, so this needs no credentials,
// no exec into the container and no call to the gateway, and it works while
// the cell is frozen. A frozen cell cannot change its own schedule, so a value
// read at pause time stays true for the whole sleep.
//
// Privacy: a job row carries the prompt text that will be sent to the model,
// along with the job's name and description. None of that is any of the
// host's business. The query below extracts only scheduling metadata, so the
// payload never crosses into this process; Job deliberately has no field that
// could hold it.
//
// Verified against OpenClaw 2026.9.7 on 2026-10-05: jobs live in
// state/openclaw.sqlite, table cron_jobs, with the next run time already
// computed as an absolute epoch-millisecond value, so no cron parser and no
// timezone handling are needed here.
package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DBPath is the gateway's state database, relative to the state directory.
const DBPath = "state/openclaw.sqlite"

// LegacyJobsPath is where older builds kept jobs instead. Reading it is not
// implemented: its shape has not been verified against a real cell, and
// guessing at a schema that governs when a tenant's reminder fires is worse
// than saying so. A cell with only this file reports ErrUnsupportedStore.
const LegacyJobsPath = "cron/jobs.json"

// ErrUnsupportedStore means the cell keeps its schedule somewhere this package
// cannot read. The caller should fall back to an operator-set due time.
var ErrUnsupportedStore = errors.New("schedule: cell uses a job store this build cannot read")

// Job is the scheduling metadata of one enabled job. It holds no prompt text,
// name or description, by design.
type Job struct {
	Kind    string // schedule kind: "at" (one-shot), "cron", "every"
	Payload string // payload kind: "agentTurn", "heartbeat", ...
	// Delivery is the delivery mode: "none", a channel, or "" when unset. It
	// is the main signal that a job's result reaches a person.
	Delivery string
	// WakeMode ("now" or "next-heartbeat") and Session ("main" or "isolated")
	// are recorded for diagnostics but deliberately not used by UserFacing.
	// Their meaning is inferred from the field names rather than from anything
	// upstream states, and a rule that decides when a tenant's reminder fires
	// should not rest on a guess. Deferring to them could only ever skip a
	// wake, which is the unsafe direction here.
	WakeMode string
	Session  string
	NextRun  time.Time // absolute, already computed by the gateway
}

// UserFacing reports whether a person is waiting on this job running at its
// scheduled time, which is the only reason a host should spend a wake on it.
//
// The rule errs toward waking. Over-waking costs density, which is visible and
// tunable; under-waking loses a tenant's reminder, which is silent. So an
// unrecognised job is treated as time-critical rather than ignored.
func (j Job) UserFacing() bool {
	switch {
	case j.NextRun.IsZero():
		return false // nothing scheduled
	case j.Payload == "heartbeat":
		// Proactivity, not a deadline. Whether to wake for it is a tier
		// decision, not this function's.
		return false
	case j.Kind == "at":
		// One-shot: "remind me at nine". Always catches up rather than being
		// coalesced, so missing its time means it arrives late, not never.
		return true
	case j.Delivery == "none":
		// Nothing leaves the cell: internal maintenance whose value is
		// cumulative, and which the gateway coalesces on the next wake.
		return false
	default:
		return true
	}
}

// Next returns the earliest time this cell has work a person is waiting for,
// or the zero time if it has none. Times already in the past are ignored: the
// gateway runs its own catch-up for those.
func Next(ctx context.Context, stateDir string, now time.Time) (time.Time, error) {
	jobs, err := Read(ctx, stateDir)
	if err != nil {
		return time.Time{}, err
	}
	var next time.Time
	for _, j := range jobs {
		if !j.UserFacing() || !j.NextRun.After(now) {
			continue
		}
		if next.IsZero() || j.NextRun.Before(next) {
			next = j.NextRun
		}
	}
	return next, nil
}

// Read returns the scheduling metadata of every enabled job in the cell.
func Read(ctx context.Context, stateDir string) ([]Job, error) {
	p := filepath.Join(stateDir, DBPath)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			if _, lerr := os.Stat(filepath.Join(stateDir, LegacyJobsPath)); lerr == nil {
				return nil, ErrUnsupportedStore
			}
			return nil, nil // no schedule store at all: not an error
		}
		return nil, err
	}
	// Read-only. immutable is deliberately left at its default of 0: a frozen
	// cell can have committed data still in its write-ahead log, and
	// immutable=1 would skip the log and read a stale due time.
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// Only scheduling fields: job_json also holds the prompt, the job name and
	// its description, which stay inside the cell.
	const q = `SELECT
	  COALESCE(json_extract(job_json,'$.schedule.kind'),''),
	  COALESCE(json_extract(job_json,'$.wakeMode'),''),
	  COALESCE(json_extract(job_json,'$.payload.kind'),''),
	  COALESCE(json_extract(job_json,'$.delivery.mode'),''),
	  COALESCE(json_extract(job_json,'$.sessionTarget'),''),
	  COALESCE(json_extract(state_json,'$.nextRunAtMs'),0)
	FROM cron_jobs WHERE enabled = 1`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		// An older or newer gateway may not have this table at all.
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedStore, err)
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		var ms int64
		if err := rows.Scan(&j.Kind, &j.WakeMode, &j.Payload, &j.Delivery, &j.Session, &ms); err != nil {
			return nil, err
		}
		if ms > 0 {
			j.NextRun = time.UnixMilli(ms).UTC()
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
