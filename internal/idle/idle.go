// Package idle decides whether a cell has been quiet long enough to hibernate.
// "Activity" is any change in the container's cumulative network counters
// between samples — a deliberately crude, guest-agnostic signal that needs no
// hook inside OpenClaw. Channel-level wake signals (webhooks) are the precise
// complement and live in package ingress.
package idle

import "time"

// Sample is one observation of a cell's counters.
type Sample struct {
	At      time.Time
	RxBytes int64
	TxBytes int64
}

// Decision describes what the detector concluded.
type Decision struct {
	Active       bool          // counters moved since last sample
	IdleFor      time.Duration // time since last observed activity
	ShouldSleep  bool          // idle for at least IdleAfter
	LastActivity time.Time
}

// Evaluate compares the previous and current samples. prevActivity is the
// last time activity was seen (zero if never). idleAfter <= 0 means never
// sleep. Background chatter below noiseBytes per sample is ignored so a
// heartbeat ping does not keep a cell awake forever.
func Evaluate(prev, cur Sample, prevActivity time.Time, idleAfter time.Duration, noiseBytes int64) Decision {
	delta := (cur.RxBytes - prev.RxBytes) + (cur.TxBytes - prev.TxBytes)
	if delta < 0 { // container restarted: counters reset — treat as activity
		delta = noiseBytes + 1
	}
	d := Decision{Active: delta > noiseBytes, LastActivity: prevActivity}
	if d.Active || prevActivity.IsZero() {
		d.LastActivity = cur.At
	}
	d.IdleFor = cur.At.Sub(d.LastActivity)
	d.ShouldSleep = idleAfter > 0 && !d.Active && d.IdleFor >= idleAfter
	return d
}
