//go:build !linux

package reclaim

import "context"

// Prefetch needs /proc and process_madvise; off Linux it is unsupported.
func (r Reclaimer) Prefetch(ctx context.Context, containerID string, maxBytes int64) (PrefetchStats, error) {
	return PrefetchStats{}, ErrUnsupported
}

// PrefetchMappings is unsupported off Linux.
func (r Reclaimer) PrefetchMappings(ctx context.Context, containerID string, maxBytes int64, includeFiles bool) (PrefetchStats, error) {
	return PrefetchStats{}, ErrUnsupported
}

// HotSet is unsupported off Linux.
func (r Reclaimer) HotSet(containerID string) (HotSet, error) { return HotSet{}, ErrUnsupported }

// PrefetchHot is unsupported off Linux.
func (r Reclaimer) PrefetchHot(ctx context.Context, containerID string, hs HotSet) (PrefetchStats, error) {
	return PrefetchStats{}, ErrUnsupported
}
