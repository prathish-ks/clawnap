//go:build linux

package reclaim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleMaps = `55d1c0000000-55d1c0021000 r--p 00000000 fd:01 1234 /usr/local/bin/node
55d1c0021000-55d1c1000000 r-xp 00021000 fd:01 1234 /usr/local/bin/node
55d1c2000000-55d1c2400000 rw-p 00000000 00:00 0    [heap]
7f0000000000-7f0010000000 rw-p 00000000 00:00 0
7f0010000000-7f0010100000 rw-p 00000000 00:00 0    [anon:v8]
7f0020000000-7f0020001000 ---p 00000000 00:00 0
7f0030000000-7f0030100000 rw-s 00000000 00:1c 99   /dev/shm/x (deleted)
7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0    [stack]
7ffd00100000-7ffd00102000 r--p 00000000 00:00 0    [vvar]
7ffd00102000-7ffd00104000 r-xp 00000000 00:00 0    [vdso]
`

func TestParseMapsSelectsSwappablePrivateAnon(t *testing.T) {
	got, err := parseMaps(strings.NewReader(sampleMaps), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []mapping{{0x55d1c2000000, 0x55d1c2400000}, {0x7f0000000000, 0x7f0010000000}, {0x7f0010000000, 0x7f0010100000}, {0x7ffd00000000, 0x7ffd00021000}}
	if len(got) != len(want) {
		t.Fatalf("anon-only: got %d mappings %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mapping %d: got %x-%x want %x-%x", i, got[i].start, got[i].end, want[i].start, want[i].end)
		}
	}
	// guard page (---p), shared (rw-s), vvar/vdso, and file-backed must be excluded
	withFiles, _ := parseMaps(strings.NewReader(sampleMaps), true)
	if len(withFiles) != len(want)+2 {
		t.Fatalf("with files: got %d, want %d (the two node segments added)", len(withFiles), len(want)+2)
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
