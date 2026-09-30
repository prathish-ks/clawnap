package reclaim

import "testing"

func TestHotRangesCoalescesPresentPages(t *testing.T) {
	const present = uint64(1) << 63
	const swapped = uint64(1) << 62
	entries := []uint64{present, present, swapped, 0, present, present | 0x1234, swapped}
	rs, bytes := hotRanges(entries, 0x10000, 4096)
	want := []Range{{0x10000, 0x16000}} // the two-page gap is bridged
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
	far := make([]uint64, 40)
	far[0], far[39] = present, present
	if rs, _ := hotRanges(far, 0, 4096); len(rs) != 2 {
		t.Fatalf("a 38-page gap must not be bridged: %+v", rs)
	}
	if rs, b := hotRanges(nil, 0, 4096); rs != nil || b != 0 {
		t.Fatal("empty input must yield nothing")
	}
}
