package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPutGetListPersist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cells.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Cell{Name: "b", Container: "oc-b", Port: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Cell{Name: "a", Container: "oc-a", Port: 1, IdleAfter: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("a", func(c *Cell) { c.Phase = PhaseHibernated }); err != nil {
		t.Fatal(err)
	}
	// reopen
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	l := s2.List()
	if len(l) != 2 || l[0].Name != "a" || l[0].Phase != PhaseHibernated || l[0].Class != ClassHibernate {
		t.Fatalf("unexpected list: %+v", l)
	}
	if _, err := s2.Get("zzz"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Put(Cell{Name: "", Container: "x"}); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestTwoStoresOnOneFileDoNotClobber(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cells.json")
	daemon, _ := Open(p)
	_ = daemon.Put(Cell{Name: "a", Container: "oc-a"})
	cli, _ := Open(p) // a second process
	_ = cli.Put(Cell{Name: "b", Container: "oc-b"})
	// the daemon updates a from its stale map; b must survive
	if err := daemon.Update("a", func(c *Cell) { c.Restarts++ }); err != nil {
		t.Fatal(err)
	}
	fresh, _ := Open(p)
	l := fresh.List()
	if len(l) != 2 || l[0].Name != "a" || l[0].Restarts != 1 || l[1].Name != "b" {
		t.Fatalf("lost update: %+v", l)
	}
	// the daemon's List sees the CLI's cell without restart
	if got := daemon.List(); len(got) != 2 {
		t.Fatalf("daemon should see b: %+v", got)
	}
	// and a CLI delete is not resurrected by the daemon
	_ = cli.Delete("b")
	_ = daemon.Update("a", func(c *Cell) { c.Restarts++ })
	if got, _ := Open(p); len(got.List()) != 1 {
		t.Fatalf("delete resurrected: %+v", got.List())
	}
}

// A daemon's Get must see a cell another process just created or removed,
// not the map it loaded at startup.
func TestGetSeesOtherProcessWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cells.json")
	daemon, _ := Open(p)
	cli, _ := Open(p)
	if err := cli.Put(Cell{Name: "n", Container: "oc-n", Port: 1}); err != nil {
		t.Fatal(err)
	}
	if c, err := daemon.Get("n"); err != nil || c.Container != "oc-n" {
		t.Fatalf("daemon must see the CLI's new cell: %+v %v", c, err)
	}
	if err := cli.Delete("n"); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Get("n"); err != ErrNotFound {
		t.Fatalf("daemon must see the CLI's removal, got %v", err)
	}
}

// refresh() skips reloading when the file's mtime and size both match what it
// last saw. An external review asked whether that can hide a cross-process
// write. These are the three shapes that matter in practice: another process
// replacing the file atomically, a same-size edit, and a reader opened before
// any of it happened.
func TestCrossProcessWritesAreSeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cells.json")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Put(Cell{Name: "a", Container: "oc-a", Port: 1}); err != nil {
		t.Fatal(err)
	}

	// A second store on the same file, standing in for another process.
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Get("a"); err != nil {
		t.Fatalf("reader cannot see the first write: %v", err)
	}

	// Growing write: a new cell appears.
	if err := writer.Put(Cell{Name: "b", Container: "oc-b", Port: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Get("b"); err != nil {
		t.Fatalf("reader missed a cell added by another process: %v", err)
	}

	// Same-size write: a field changes without the file length moving.
	if err := writer.Update("a", func(c *Cell) { c.Port = 9 }); err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 9 {
		t.Fatalf("reader kept a stale copy after a same-size write: port=%d", got.Port)
	}

	// The edge case the skip could actually miss, forced rather than hoped for:
	// a same-size write whose mtime is pinned to the value already recorded, so
	// neither half of the check moves. On the nanosecond-mtime filesystems this
	// runs on, two writes colliding like this is not reachable in practice, but
	// a coarser clock could produce it, and a reader must not be able to serve
	// a stale cell indefinitely.
	// Update() also rewrites a timestamp, so it never produces a same-size
	// write; force the collision by editing the bytes and pinning the mtime.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte(`"port": 9`), []byte(`"port": 8`), 1)
	if bytes.Equal(edited, raw) || len(edited) != len(raw) {
		t.Fatalf("could not build a same-size edit; the on-disk shape has changed: %s", firstLineOf(raw))
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Immediately afterwards the reader may still serve its cached copy: both
	// halves of the check match, which is the whole point of the optimisation.
	// What must not happen is that it serves it for ever.
	time.Sleep(staleAfter + 250*time.Millisecond)
	got, err = reader.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 8 {
		t.Fatalf("a reader served a stale cell past the staleness bound (port=%d, want 8): "+
			"with mtime and size pinned, nothing else would ever invalidate its cache", got.Port)
	}

	// Restart recovery: a store opened fresh reads the file itself, so it sees
	// the latest write regardless of what any live reader cached.
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := restarted.Get("a"); err != nil || c.Port != 8 {
		t.Fatalf("a newly opened store did not recover the file: %+v err=%v", c, err)
	}
	if len(restarted.List()) != 2 {
		t.Fatalf("want 2 cells after restart, got %d", len(restarted.List()))
	}
}

func firstLineOf(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
