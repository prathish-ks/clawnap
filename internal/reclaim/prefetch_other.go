//go:build !linux

package reclaim

import "context"

// Prefetch needs /proc and process_madvise; off Linux it is unsupported.
func (r Reclaimer) Prefetch(ctx context.Context, containerID string, maxBytes int64) (PrefetchStats, error) {
	return PrefetchStats{}, ErrUnsupported
}
