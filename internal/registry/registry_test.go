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
