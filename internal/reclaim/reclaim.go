// Package reclaim pushes a paused cell's memory out of RAM by writing to the
// container cgroup's memory.reclaim (cgroup v2, Linux ≥ 5.19). Measured
// locally: a paused OpenClaw cell drops from ~790 MiB to ~20 MiB resident;
// the pages return lazily on wake, at the speed of the swap device (zram or
// NVMe on a fleet host). The supervisor calls this some time after a pause,
// not immediately, so recently active cells keep sub-second wakes.
package reclaim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Stats is a cell's memory accounting from its cgroup.
type Stats struct {
	CurrentBytes int64
	SwapBytes    int64
}

// Reclaimer finds a container's cgroup and reclaims it. FS defaults to
// /sys/fs/cgroup; tests point it at a temp tree.
type Reclaimer struct {
	FS string
}

// ErrUnsupported is returned off Linux or without cgroup v2 memory.reclaim.
var ErrUnsupported = errors.New("memory reclaim unsupported on this host")

// Dir locates the cgroup directory for a container id under the common
// layouts: cgroupfs driver (docker/<id>), systemd driver
// (system.slice/docker-<id>.scope), and podman's slice variants.
func (r Reclaimer) Dir(containerID string) (string, error) {
	fs := r.FS
	if fs == "" {
		fs = "/sys/fs/cgroup"
	}
	candidates := []string{
		filepath.Join(fs, "docker", containerID),
		filepath.Join(fs, "system.slice", "docker-"+containerID+".scope"),
		filepath.Join(fs, "machine.slice", "libpod-"+containerID+".scope"),
		filepath.Join(fs, "user.slice", "libpod-"+containerID+".scope"),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("cgroup for container %s not found under %s", containerID, fs)
}

// Stats reads memory.current and memory.swap.current.
func (r Reclaimer) Stats(containerID string) (Stats, error) {
	d, err := r.Dir(containerID)
	if err != nil {
		return Stats{}, err
	}
	cur, err := readInt(filepath.Join(d, "memory.current"))
	if err != nil {
		return Stats{}, err
	}
	sw, _ := readInt(filepath.Join(d, "memory.swap.current")) // absent when swap is off
	return Stats{CurrentBytes: cur, SwapBytes: sw}, nil
}

// Reclaim pushes the cell's memory to swap in chunks, stopping when a chunk
// no longer reduces memory.current by at least a quarter of its size. Chunked
// requests return promptly; a single request for "everything" makes the
// kernel spin toward pages it cannot reclaim (observed: 60 s stalls). `max`
// bounds the total. Returns stats after the last chunk.
func (r Reclaimer) Reclaim(ctx context.Context, containerID string, max int64) (Stats, error) {
	return r.ReclaimUntil(ctx, containerID, max, nil)
}

// ReclaimUntil is Reclaim with a stop predicate checked between chunks, so a
// pending wake interrupts a long reclaim within one chunk.
func (r Reclaimer) ReclaimUntil(ctx context.Context, containerID string, max int64, stop func() bool) (Stats, error) {
	if runtime.GOOS != "linux" && r.FS == "" {
		return Stats{}, ErrUnsupported
	}
	d, err := r.Dir(containerID)
	if err != nil {
		return Stats{}, err
	}
	p := filepath.Join(d, "memory.reclaim")
	if _, err := os.Stat(p); err != nil {
		return Stats{}, ErrUnsupported
	}
	const chunk = int64(64 << 20)
	var total int64
	for total < max {
		if stop != nil && stop() {
			break
		}
		before, err := readInt(filepath.Join(d, "memory.current"))
		if err != nil {
			return Stats{}, err
		}
		if before <= chunk/2 {
			break
		}
		if err := writeReclaim(ctx, p, chunk); err != nil {
			return Stats{}, err
		}
		after, err := readInt(filepath.Join(d, "memory.current"))
		if err != nil {
			return Stats{}, err
		}
		freed := before - after
		total += chunk
		if freed < chunk/4 {
			break // stalled: the rest is unreclaimable
		}
		if err := ctx.Err(); err != nil {
			return Stats{}, err
		}
	}
	return r.Stats(containerID)
}

// writeReclaim writes one request and bounds the wait; the syscall cannot be
// cancelled, so a slow write is abandoned to its goroutine.
func writeReclaim(ctx context.Context, p string, bytes int64) error {
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			done <- err
			return
		}
		_, werr := f.WriteString(strconv.FormatInt(bytes, 10))
		_ = f.Close()
		done <- werr
	}()
	wait := 15 * time.Second
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
		wait = time.Until(dl)
	}
	select {
	case werr := <-done:
		if werr != nil && !strings.Contains(strings.ToLower(werr.Error()), "resource temporarily unavailable") {
			return werr
		}
		return nil
	case <-time.After(wait):
		return nil // treat as a stalled chunk; caller re-reads memory.current
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readInt(p string) (int64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}
