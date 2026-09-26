package walcheck

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointTruncatesWAL(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "state"), 0o700)
	p := filepath.Join(dir, "state", "openclaw.sqlite")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE t(x BLOB);"); err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("x", 64*1024)
	for i := 0; i < 50; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES(?)", blob); err != nil {
			t.Fatal(err)
		}
	}
	// keep connection open like a crashed writer would have; WAL should be non-trivial
	before, _ := os.Stat(p + "-wal")
	if before == nil || before.Size() == 0 {
		t.Fatal("expected a non-empty WAL before checkpoint")
	}
	db.Close()
	// closing checkpoints on its own; re-grow the WAL with a second writer that does not close cleanly
	db2, _ := sql.Open("sqlite", "file:"+p)
	for i := 0; i < 50; i++ {
		_, _ = db2.Exec("INSERT INTO t VALUES(?)", blob)
	}
	// simulate unclean exit: do not Close db2 before checkpointing from a fresh handle
	res, err := Checkpoint(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("checkpoint: %v (%+v)", err, res)
	}
	if len(res) != 1 || res[0].Skipped != "" {
		t.Fatalf("unexpected result %+v", res)
	}
	if res[0].WALAfter != 0 && res[0].WALAfter != -1 {
		t.Fatalf("WAL not truncated: before=%d after=%d", res[0].WALBefore, res[0].WALAfter)
	}
	db2.Close()
}

func TestCheckpointSkipsMissingDB(t *testing.T) {
	res, err := Checkpoint(context.Background(), t.TempDir(), nil)
	if err != nil || len(res) != 1 || res[0].Skipped == "" {
		t.Fatalf("expected skip, got %v %+v", err, res)
	}
}
