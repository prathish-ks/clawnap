// Package walcheck truncates a stopped cell's SQLite write-ahead log from
// the host. OpenClaw keeps its state in <state>/state/openclaw.sqlite in WAL
// mode; an unclean exit (SIGKILL after the stop grace, an OOM kill) can leave
// a large WAL behind, and upstream issue #143524 documents WALs growing to
// gigabytes and blocking startup. Run only after the container has stopped:
// checkpointing a database another process holds open is not safe here.
package walcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// StatePathInContainer is where OpenClaw's state directory is mounted.
const StatePathInContainer = "/home/node/.openclaw"

// DefaultDBs lists SQLite files, relative to the state mount, to checkpoint.
var DefaultDBs = []string{"state/openclaw.sqlite"}

// Result reports what happened to one database.
type Result struct {
	Path      string
	WALBefore int64 // bytes, -1 if no WAL file
	WALAfter  int64
	Skipped   string // reason, if not checkpointed
}

// Checkpoint runs PRAGMA wal_checkpoint(TRUNCATE) on each database under
// stateDir. Missing databases are skipped, not errors.
func Checkpoint(ctx context.Context, stateDir string, dbs []string) ([]Result, error) {
	if len(dbs) == 0 {
		dbs = DefaultDBs
	}
	var results []Result
	var errs []error
	for _, rel := range dbs {
		p := filepath.Join(stateDir, rel)
		r := Result{Path: p, WALBefore: walSize(p), WALAfter: -1}
		if _, err := os.Stat(p); err != nil {
			r.Skipped = "database not found"
			results = append(results, r)
			continue
		}
		if err := checkpointOne(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			r.Skipped = err.Error()
		}
		r.WALAfter = walSize(p)
		results = append(results, r)
	}
	return results, errors.Join(errs...)
}

func checkpointOne(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	var busy, logPages, ckPages int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &ckPages); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("checkpoint blocked: database busy (is the cell still running?)")
	}
	return nil
}

func walSize(dbPath string) int64 {
	fi, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return -1
	}
	return fi.Size()
}
