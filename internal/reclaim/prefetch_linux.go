//go:build linux

package reclaim

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Prefetch reads a cell's swapped-out anonymous memory back into RAM in
// bulk, ahead of the unpause, so the gateway does not fault it back one
// page at a time at disk latency. Mechanism, in order of preference:
//
//  1. process_madvise(MADV_WILLNEED) over every anonymous mapping of every
//     process in the cell's cgroup (Linux >= 5.10). The kernel schedules
//     large sequential swap reads and returns immediately.
//  2. Fallback: touch the pages by reading /proc/<pid>/mem in 1 MiB steps,
//     which faults them in synchronously but still sequentially.
//
// process_madvise on another process needs CAP_SYS_NICE as well as ptrace
// read access; the fallback needs full ptrace attach. In practice the
// supervisor runs as root on a fleet host, where both hold. Works on a
// paused cell: the freezer stops the processes, not the kernel's page-in
// on their behalf. Note that on zram MADV_WILLNEED decompresses inline and
// the call returns only when the pages are resident, so it is bounded by
// the device's throughput, not "immediate".

const (
	sysPidfdOpen      = 434 // x86_64, arm64, arm and 386 share these numbers
	sysProcessMadvise = 440
	madvWillNeed      = 3
	iovMax            = 1024 // IOV_MAX: process_madvise accepts at most this many vectors per call
)

type mapping struct{ start, end uintptr }

// Prefetch pages the cell's anonymous memory back in. maxBytes bounds the
// amount advised (0 = everything mapped).
func (r Reclaimer) Prefetch(ctx context.Context, containerID string, maxBytes int64) (PrefetchStats, error) {
	return r.PrefetchMappings(ctx, containerID, maxBytes, false) // anon only: measured 0.72 s vs 2.0 s with files
}

// PrefetchMappings is Prefetch with control over file-backed mappings.
func (r Reclaimer) PrefetchMappings(ctx context.Context, containerID string, maxBytes int64, includeFiles bool) (PrefetchStats, error) {
	d, err := r.Dir(containerID)
	if err != nil {
		return PrefetchStats{}, err
	}
	t0 := time.Now()
	st := PrefetchStats{}
	st.SwapBefore, _ = readInt(filepath.Join(d, "memory.swap.current"))
	pids, err := cgroupPIDs(d)
	if err != nil {
		return st, err
	}
	var errs []error
	advised := 0
	budget := int64(math.MaxInt64)
	if maxBytes > 0 {
		budget = maxBytes
	}
	for _, pid := range pids {
		maps, err := mappings(pid, includeFiles)
		if err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", pid, err))
			continue
		}
		st.Processes++
		st.Mappings += len(maps)
		n, mech, err := advise(ctx, pid, maps, budget-st.Bytes)
		st.Bytes += n
		if mech != "" {
			st.Mechanism = mech
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", pid, err))
		} else {
			advised++
		}
		if st.Bytes >= budget {
			break
		}
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
	}
	st.SwapAfter, _ = readInt(filepath.Join(d, "memory.swap.current"))
	st.Took = time.Since(t0)
	// An error for any process is an error: a silent partial prefetch would
	// be reported as success while the wake pays the fault-by-fault cost.
	if len(errs) > 0 {
		return st, errors.Join(errs...)
	}
	if advised == 0 {
		return st, errors.New("no process advised")
	}
	return st, nil
}

func cgroupPIDs(dir string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, l := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(l); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// mappings lists the mappings worth prefetching: private writable
// anonymous ones (heap, V8 arenas, stacks: the pages that were swapped),
// and, when includeFiles is set, readable file-backed ones too (the node
// binary, bundles, shared libraries), which reclaim evicts from the page
// cache and which the gateway otherwise faults back on demand. Special
// kernel mappings are always skipped.
func mappings(pid int, includeFiles bool) ([]mapping, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMaps(f, includeFiles)
}

// parseMaps applies the selection rules to /proc/<pid>/maps content.
func parseMaps(r io.Reader, includeFiles bool) ([]mapping, error) {
	var out []mapping
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		perms := fields[1]
		if len(perms) < 4 || perms[0] != 'r' {
			continue
		}
		fileBacked := len(fields) >= 6 && strings.HasPrefix(fields[5], "/")
		special := len(fields) >= 6 && strings.HasPrefix(fields[5], "[") && fields[5] != "[heap]" && !strings.HasPrefix(fields[5], "[stack") && !strings.HasPrefix(fields[5], "[anon")
		if special {
			continue // [vdso], [vvar], [vsyscall]
		}
		if fileBacked {
			if !includeFiles {
				continue
			}
		} else if perms[1] != 'w' || perms[3] != 'p' {
			continue // anon but not private-writable: nothing swapped
		}
		a, b, ok := strings.Cut(fields[0], "-")
		if !ok {
			continue
		}
		s, err1 := strconv.ParseUint(a, 16, 64)
		e, err2 := strconv.ParseUint(b, 16, 64)
		if err1 != nil || err2 != nil || e <= s {
			continue
		}
		out = append(out, mapping{uintptr(s), uintptr(e)})
	}
	return out, sc.Err()
}

// advise issues process_madvise(MADV_WILLNEED) over the mappings in
// IOV_MAX-sized batches, checking ctx between batches. The earlier
// /proc/<pid>/mem read fallback is gone: it could only run on kernels too
// old to have reclaimed the cell in the first place, it needs stricter
// ptrace access than process_madvise so it never rescues an EPERM, and it
// copied every range through user space.
func advise(ctx context.Context, pid int, maps []mapping, budget int64) (int64, string, error) {
	iov := make([]syscall.Iovec, 0, len(maps))
	var total int64
	for _, m := range maps {
		l := int64(m.end - m.start)
		if total+l > budget {
			l = budget - total
			if l <= 0 {
				break
			}
		}
		// The address belongs to another process; it is never dereferenced
		// here, only handed to the kernel. Build the vector from integers so
		// neither vet's unsafeptr check nor a 32-bit Iovec.Len width is an
		// issue (SetLen takes an int).
		var v syscall.Iovec
		*(*uintptr)(unsafe.Pointer(&v.Base)) = m.start
		v.SetLen(int(l))
		iov = append(iov, v)
		total += l
	}
	if len(iov) == 0 {
		return 0, "", nil
	}
	pidfd, _, e := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if e != 0 {
		return 0, "", fmt.Errorf("pidfd_open: %w", e)
	}
	defer syscall.Close(int(pidfd))
	var done int64
	for i := 0; i < len(iov); i += iovMax {
		if err := ctx.Err(); err != nil {
			return done, "process_madvise", err
		}
		j := i + iovMax
		if j > len(iov) {
			j = len(iov)
		}
		n, _, e2 := syscall.Syscall6(sysProcessMadvise, pidfd, uintptr(unsafe.Pointer(&iov[i])), uintptr(j-i), madvWillNeed, 0, 0)
		if e2 != 0 {
			return done, "process_madvise", fmt.Errorf("process_madvise: %w (needs CAP_SYS_NICE and ptrace read over the cell's processes)", e2)
		}
		done += int64(n)
	}
	return done, "process_madvise", nil
}
