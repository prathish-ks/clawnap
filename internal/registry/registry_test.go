package registry

import (
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
