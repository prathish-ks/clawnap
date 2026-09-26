package idle

import (
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	prev := Sample{At: t0, RxBytes: 1000, TxBytes: 1000}

	// traffic above noise => active, not sleeping
	d := Evaluate(prev, Sample{At: t0.Add(time.Minute), RxBytes: 5000, TxBytes: 1000}, t0, 10*time.Minute, 512)
	if !d.Active || d.ShouldSleep || !d.LastActivity.Equal(t0.Add(time.Minute)) {
		t.Fatalf("active case wrong: %+v", d)
	}
	// noise-only traffic, idle 9m => not yet
	d = Evaluate(prev, Sample{At: t0.Add(9 * time.Minute), RxBytes: 1100, TxBytes: 1000}, t0, 10*time.Minute, 512)
	if d.Active || d.ShouldSleep {
		t.Fatalf("noise case wrong: %+v", d)
	}
	// idle 10m => sleep
	d = Evaluate(prev, Sample{At: t0.Add(10 * time.Minute), RxBytes: 1000, TxBytes: 1000}, t0, 10*time.Minute, 512)
	if !d.ShouldSleep || d.IdleFor != 10*time.Minute {
		t.Fatalf("sleep case wrong: %+v", d)
	}
	// idleAfter 0 => never sleep
	d = Evaluate(prev, Sample{At: t0.Add(time.Hour), RxBytes: 1000, TxBytes: 1000}, t0, 0, 512)
	if d.ShouldSleep {
		t.Fatal("idleAfter=0 must never sleep")
	}
	// counter reset (restart) => active
	d = Evaluate(prev, Sample{At: t0.Add(time.Minute), RxBytes: 10, TxBytes: 10}, t0, 10*time.Minute, 512)
	if !d.Active {
		t.Fatal("counter reset should count as activity")
	}
}
