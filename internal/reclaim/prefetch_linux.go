//go:build linux

package reclaim

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
// Both need the same privilege the reclaim path already needs (root or
// CAP_SYS_PTRACE over the cell's processes). Works on a paused cell: the
// freezer stops the processes, not the kernel's page-in on their behalf.

const (
	sysPidfdOpen      = 434 // x86_64 and arm64 share these numbers
	sysProcessMadvise = 440
	madvWillNeed      = 3
)

type mapping struct{ start, end uintptr }

// Prefetch pages the cell's anonymous memory back in. maxBytes bounds the
// amount advised (0 = everything mapped).
func (r Reclaimer) Prefetch(ctx context.Context, containerID string, maxBytes int64) (PrefetchStats, error) {
	return r.PrefetchMappings(ctx, containerID, maxBytes, true)
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
	for _, pid := range pids {
		maps, err := mappings(pid, includeFiles)
		if err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", pid, err))
			continue
		}
		st.Processes++
		st.Mappings += len(maps)
		n, mech, err := advise(pid, maps, maxBytes-st.Bytes, maxBytes > 0)
		st.Bytes += n
		if mech != "" {
			st.Mechanism = mech
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", pid, err))
		}
		if maxBytes > 0 && st.Bytes >= maxBytes {
			break
		}
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
	}
	st.SwapAfter, _ = readInt(filepath.Join(d, "memory.swap.current"))
	st.Took = time.Since(t0)
	if st.Processes == 0 && len(errs) > 0 {
		return st, errors.Join(errs...)
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

// anonMappings lists the mappings worth prefetching: private writable
// anonymous ones (heap, V8 arenas, stacks: the pages that were swapped),
// and, when includeFiles is set, readable file-backed ones too (the node
// binary, bundles, shared libraries), which reclaim evicts from the page
// cache and which the gateway otherwise faults back on demand. Special
// kernel mappings are always skipped.
func anonMappings(pid int) ([]mapping, error) { return mappings(pid, false) }

func mappings(pid int, includeFiles bool) ([]mapping, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []mapping
	sc := bufio.NewScanner(f)
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

// advise tries process_madvise first, then the /proc/<pid>/mem fallback.
func advise(pid int, maps []mapping, budget int64, bounded bool) (int64, string, error) {
	iov := make([]syscall.Iovec, 0, len(maps))
	var total int64
	for _, m := range maps {
		l := int64(m.end - m.start)
		if bounded && total+l > budget {
			l = budget - total
			if l <= 0 {
				break
			}
		}
		// The address belongs to another process; it is never dereferenced
		// here, only handed to the kernel, so the uintptr→pointer conversion
		// vet warns about is the intended use.
		iov = append(iov, syscall.Iovec{Base: (*byte)(unsafe.Pointer(m.start)), Len: uint64(l)}) //nolint:govet
		total += l
	}
	if len(iov) == 0 {
		return 0, "", nil
	}
	pidfd, _, e := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if e == 0 {
		defer syscall.Close(int(pidfd))
		// process_madvise accepts at most IOV_MAX (1024) vectors per call
		var done int64
		var lastErr syscall.Errno
		for i := 0; i < len(iov); i += 1024 {
			j := i + 1024
			if j > len(iov) {
				j = len(iov)
			}
			n, _, e2 := syscall.Syscall6(sysProcessMadvise, pidfd, uintptr(unsafe.Pointer(&iov[i])), uintptr(j-i), madvWillNeed, 0, 0)
			if e2 != 0 {
				lastErr = e2
				break
			}
			done += int64(n)
		}
		if lastErr == 0 {
			return done, "process_madvise", nil
		}
		if lastErr != syscall.ENOSYS && lastErr != syscall.EINVAL {
			return done, "process_madvise", lastErr
		}
	}
	// fallback: sequential read of /proc/<pid>/mem faults the pages in
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var read int64
	for _, v := range iov {
		off := int64(uintptr(unsafe.Pointer(v.Base))) //nolint:govet
		for rem := int64(v.Len); rem > 0; {
			n := int64(len(buf))
			if rem < n {
				n = rem
			}
			k, err := f.ReadAt(buf[:n], off)
			read += int64(k)
			if err != nil {
				break // unreadable range (guard pages); move on
			}
			off += n
			rem -= n
		}
	}
	return read, "proc_mem", nil
}
