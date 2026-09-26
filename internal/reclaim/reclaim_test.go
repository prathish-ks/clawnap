package reclaim

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDirStatsAndReclaimAcrossLayouts(t *testing.T) {
	fs := t.TempDir()
	id := "abc123"
	d := filepath.Join(fs, "system.slice", "docker-"+id+".scope")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "memory.current"), []byte("827326464\n"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.swap.current"), []byte("0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "memory.reclaim"), []byte(""), 0o644)
	r := Reclaimer{FS: fs}
	got, err := r.Dir(id)
	if err != nil || got != d {
		t.Fatalf("Dir: %q %v", got, err)
	}
	st, err := r.Stats(id)
	if err != nil || st.CurrentBytes != 827326464 || st.SwapBytes != 0 {
		t.Fatalf("Stats: %+v %v", st, err)
	}
	if _, err := r.Reclaim(context.Background(), id, 900<<20); err != nil {
		t.Fatal(err)
	}
	// request is clamped to memory.current minus the 48 MiB floor
	if b, _ := os.ReadFile(filepath.Join(d, "memory.reclaim")); string(b) != "776994816" {
		t.Fatalf("wrote %q", b)
	}
	if _, err := r.Dir("nope"); err == nil {
		t.Fatal("unknown id must error")
	}
}

func TestReclaimUnsupportedWithoutFile(t *testing.T) {
	fs := t.TempDir()
	d := filepath.Join(fs, "docker", "id1")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "memory.current"), []byte("1\n"), 0o644)
	if _, err := (Reclaimer{FS: fs}).Reclaim(context.Background(), "id1", 1); err != ErrUnsupported {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}
