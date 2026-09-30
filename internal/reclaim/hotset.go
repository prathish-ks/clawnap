package reclaim

import "time"

// Range is a half-open virtual address range [Start, End) in one process.
type Range struct {
	Start uint64 `json:"s"`
	End   uint64 `json:"e"`
}

// HotSet records which anonymous pages of a cell were still resident when
// the cell sat at its warm floor: by the kernel's own LRU, the pages the
// gateway used most recently. A later wake prefetches only these, leaving
// the cold heap on disk where the idle gateway never touches it (measured:
// an idle gateway runs on ~300 MiB of a ~700 MiB set). Keyed by pid so a
// restarted cell, whose pids differ, falls back to a full prefetch.
type HotSet struct {
	TakenAt time.Time       `json:"taken_at"`
	Bytes   int64           `json:"bytes"`
	PIDs    map[int][]Range `json:"pids"`
}

// hotRanges turns pagemap entries for the pages of [base, base+len(entries)*page)
// into ranges of pages that are present in RAM (bit 63), coalescing
// neighbours. Swapped pages (bit 62) and unmapped pages are cold.
func hotRanges(entries []uint64, base uint64, page uint64) ([]Range, int64) {
	var out []Range
	var bytes int64
	for i, e := range entries {
		if e&(1<<63) == 0 {
			continue
		}
		start := base + uint64(i)*page
		if n := len(out); n > 0 && out[n-1].End == start {
			out[n-1].End = start + page
		} else {
			out = append(out, Range{start, start + page})
		}
		bytes += int64(page)
	}
	return out, bytes
}
