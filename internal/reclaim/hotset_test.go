package reclaim

import "testing"

func TestHotRangesCoalescesPresentPages(t *testing.T) {
	const present = uint64(1) << 63
	const swapped = uint64(1) << 62
	entries := []uint64{present, present, swapped, 0, present, present | 0x1234, swapped}
	rs, bytes := hotRanges(entries, 0x10000, 4096)
	want := []Range{{0x10000, 0x12000}, {0x14000, 0x16000}}
	if len(rs) != len(want) {
		t.Fatalf("ranges %+v, want %+v", rs, want)
	}
	for i := range want {
		if rs[i] != want[i] {
			t.Fatalf("range %d = %+v, want %+v", i, rs[i], want[i])
		}
	}
	if bytes != 4*4096 {
		t.Fatalf("bytes %d, want %d", bytes, 4*4096)
	}
	if rs, b := hotRanges(nil, 0, 4096); rs != nil || b != 0 {
		t.Fatal("empty input must yield nothing")
	}
}
