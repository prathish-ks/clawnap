//go:build linux

package reclaim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAnonMappingsParsesProcMapsFormat(t *testing.T) {
	// synthetic /proc/<pid>/maps content via a temp file is not possible
	// (anonMappings opens /proc directly), so test the parsing rules on the
	// current process, which always has a heap and a stack.
	maps, err := anonMappings(os.Getpid())
	if err != nil {
		t.Skip("no /proc on this platform:", err)
	}
	if len(maps) == 0 {
		t.Fatal("expected at least one anonymous mapping")
	}
	for _, m := range maps {
		if m.end <= m.start {
			t.Fatalf("bad range %x-%x", m.start, m.end)
		}
	}
}

func TestCgroupPIDs(t *testing.T) {
	d := t.TempDir()
	_ = os.WriteFile(filepath.Join(d, "cgroup.procs"), []byte("12\n345\n\n7\n"), 0o644)
	p, err := cgroupPIDs(d)
	if err != nil || len(p) != 3 || p[1] != 345 {
		t.Fatalf("%v %v", p, err)
	}
}
