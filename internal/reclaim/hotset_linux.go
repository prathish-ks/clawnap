//go:build linux

package reclaim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

// pageSize is the kernel's: /proc/<pid>/pagemap holds one entry per page,
// and process_madvise wants page-aligned ranges, so a 16 or 64 KiB arm64
// kernel must not be read with x86's 4 KiB stride.
var pageSize = uint64(os.Getpagesize())

// HotSet snapshots which anonymous pages of every process in the cell's
// cgroup are resident right now, from /proc/<pid>/pagemap. Meant to be
// called after a reclaim to the warm floor and before the reclaim to the
// cold floor. Needs no PFNs, only the present bit, so it works with the
// daemon's normal privileges.
func (r Reclaimer) HotSet(containerID string) (HotSet, error) {
	d, err := r.Dir(containerID)
	if err != nil {
		return HotSet{}, err
	}
	pids, err := cgroupPIDs(d)
	if err != nil {
		return HotSet{}, err
	}
	hs := HotSet{TakenAt: time.Now(), PIDs: map[int][]Range{}}
	var errs []error
	for _, pid := range pids {
		maps, err := mappings(pid, false)
		if err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", pid, err))
			continue
		}
		f, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", pid))
		if err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", pid, err))
			continue
		}
		buf := make([]byte, 8*65536) // 64k pages = 256 MiB of address space per read
		for _, m := range maps {
			for off := uint64(m.start); off < uint64(m.end); {
				n := (uint64(m.end) - off) / pageSize
				if n > 65536 {
					n = 65536
				}
				b := buf[:8*n]
				if _, err := f.ReadAt(b, int64(off/pageSize)*8); err != nil {
					break // a mapping that vanished: skip the rest of it
				}
				entries := make([]uint64, n)
				for i := range entries {
					entries[i] = binary.LittleEndian.Uint64(b[8*i:])
				}
				rs, bytes := hotRanges(entries, off, pageSize)
				hs.PIDs[pid] = append(hs.PIDs[pid], rs...)
				hs.Bytes += bytes
				off += n * pageSize
			}
		}
		f.Close()
	}
	if len(hs.PIDs) == 0 {
		return hs, errors.Join(append(errs, errors.New("no process read"))...)
	}
	return hs, nil
}

// PrefetchHot advises only the recorded hot ranges. A pid in the set that is
// no longer in the cgroup means the set is stale (the cell was restarted):
// the caller should fall back to a full prefetch.
func (r Reclaimer) PrefetchHot(ctx context.Context, containerID string, hs HotSet) (PrefetchStats, error) {
	d, err := r.Dir(containerID)
	if err != nil {
		return PrefetchStats{}, err
	}
	t0 := time.Now()
	st := PrefetchStats{}
	st.SwapBefore, _ = readInt(d + "/memory.swap.current")
	pids, err := cgroupPIDs(d)
	if err != nil {
		return st, err
	}
	live := map[int]bool{}
	for _, p := range pids {
		live[p] = true
	}
	var errs []error
	for pid, rs := range hs.PIDs {
		if !live[pid] {
			return st, ErrStaleHotSet
		}
		maps := make([]mapping, 0, len(rs))
		for _, x := range rs {
			maps = append(maps, mapping{uintptr(x.Start), uintptr(x.End)})
		}
		st.Processes++
		st.Mappings += len(maps)
		n, mech, err := advise(ctx, pid, maps, 1<<62)
		st.Bytes += n
		if mech != "" {
			st.Mechanism = mech
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", pid, err))
		}
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
	}
	st.SwapAfter, _ = readInt(d + "/memory.swap.current")
	st.Took = time.Since(t0)
	if len(errs) > 0 {
		return st, errors.Join(errs...)
	}
	return st, nil
}
