package reclaim

import "time"

// PrefetchStats reports what a Prefetch did.
type PrefetchStats struct {
	Processes  int
	Mappings   int
	Bytes      int64 // bytes advised or read
	Mechanism  string
	SwapBefore int64
	SwapAfter  int64
	Took       time.Duration
}
