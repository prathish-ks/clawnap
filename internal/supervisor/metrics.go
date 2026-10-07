package supervisor

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/prathish-ks/clawnap/internal/registry"
)

// Metrics is a dependency-free Prometheus text exposition of what a fleet
// owner measures: wake and hibernate counts, wake latency, pulses, failures,
// and cells per phase. Written for Phase 0b so the density and wake numbers
// come from the supervisor, not from scripts.
type Metrics struct {
	mu           sync.Mutex
	wakes        map[string]int // by kind: pause, stop
	wakeFails    int
	hibernates   map[string]int // by tier
	pulses       int
	fellThru     int
	selfHeals    int
	maintains    int // maintenance-rotation wakes
	reclaims     int
	reclaimedB   int64
	readyBuckets []float64 // seconds
	readyCounts  []int     // cumulative per bucket
	readySum     float64
	readyN       int
}

func newMetrics() *Metrics {
	return &Metrics{
		wakes: map[string]int{}, hibernates: map[string]int{},
		readyBuckets: []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		readyCounts:  make([]int, 10),
	}
}

func (m *Metrics) wake(kind string, ready time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wakes[kind]++
	s := ready.Seconds()
	m.readySum += s
	m.readyN++
	for i, b := range m.readyBuckets {
		if s <= b {
			m.readyCounts[i]++
		}
	}
}

func (m *Metrics) wakeFail()             { m.mu.Lock(); m.wakeFails++; m.mu.Unlock() }
func (m *Metrics) hibernate(tier string) { m.mu.Lock(); m.hibernates[tier]++; m.mu.Unlock() }
func (m *Metrics) pulse()                { m.mu.Lock(); m.pulses++; m.mu.Unlock() }
func (m *Metrics) fellThrough()          { m.mu.Lock(); m.fellThru++; m.mu.Unlock() }
func (m *Metrics) selfHeal()             { m.mu.Lock(); m.selfHeals++; m.mu.Unlock() }
func (m *Metrics) maintain()             { m.mu.Lock(); m.maintains++; m.mu.Unlock() }
func (m *Metrics) reclaimed(b int64)     { m.mu.Lock(); m.reclaims++; m.reclaimedB += b; m.mu.Unlock() }

// Write renders the exposition. cells supplies the current phase gauges.
func (m *Metrics) Write(w io.Writer, cells []registry.Cell) {
	m.mu.Lock()
	defer m.mu.Unlock()
	phases := map[string]int{}
	tiers := map[string]int{}
	for _, c := range cells {
		phases[string(c.Phase)]++
		if c.Phase == registry.PhaseHibernated {
			tiers[string(c.Tier)]++
		}
	}
	fmt.Fprintln(w, "# HELP clawnap_cells Cells by supervisor phase.\n# TYPE clawnap_cells gauge")
	for _, p := range sortedKeys(phases) {
		fmt.Fprintf(w, "clawnap_cells{phase=%q} %d\n", p, phases[p])
	}
	fmt.Fprintln(w, "# HELP clawnap_hibernated_cells Hibernated cells by tier.\n# TYPE clawnap_hibernated_cells gauge")
	for _, t := range sortedKeys(tiers) {
		fmt.Fprintf(w, "clawnap_hibernated_cells{tier=%q} %d\n", t, tiers[t])
	}
	fmt.Fprintln(w, "# HELP clawnap_wakes_total Successful wakes by kind.\n# TYPE clawnap_wakes_total counter")
	for _, k := range sortedKeys(m.wakes) {
		fmt.Fprintf(w, "clawnap_wakes_total{kind=%q} %d\n", k, m.wakes[k])
	}
	fmt.Fprintf(w, "# HELP clawnap_wake_failures_total Wakes that did not reach readiness.\n# TYPE clawnap_wake_failures_total counter\nclawnap_wake_failures_total %d\n", m.wakeFails)
	fmt.Fprintf(w, "# HELP clawnap_maintenance_wakes_total Wakes started by the maintenance rotation so an unmessaged cell still runs its own schedule.\n# TYPE clawnap_maintenance_wakes_total counter\nclawnap_maintenance_wakes_total %d\n", m.maintains)
	fmt.Fprintln(w, "# HELP clawnap_hibernates_total Hibernations by tier.\n# TYPE clawnap_hibernates_total counter")
	for _, t := range sortedKeys(m.hibernates) {
		fmt.Fprintf(w, "clawnap_hibernates_total{tier=%q} %d\n", t, m.hibernates[t])
	}
	fmt.Fprintf(w, "# HELP clawnap_pulses_total Pause-cap pulses.\n# TYPE clawnap_pulses_total counter\nclawnap_pulses_total %d\n", m.pulses)
	fmt.Fprintf(w, "# HELP clawnap_pause_fallthrough_total Pause-cap fallthroughs to the stop tier.\n# TYPE clawnap_pause_fallthrough_total counter\nclawnap_pause_fallthrough_total %d\n", m.fellThru)
	fmt.Fprintf(w, "# HELP clawnap_self_heals_total Always-on cells restarted after exit.\n# TYPE clawnap_self_heals_total counter\nclawnap_self_heals_total %d\n", m.selfHeals)
	fmt.Fprintf(w, "# HELP clawnap_reclaims_total Paused cells whose memory was reclaimed to swap.\n# TYPE clawnap_reclaims_total counter\nclawnap_reclaims_total %d\n", m.reclaims)
	fmt.Fprintf(w, "# HELP clawnap_reclaimed_bytes_total Bytes moved out of RAM by reclaims.\n# TYPE clawnap_reclaimed_bytes_total counter\nclawnap_reclaimed_bytes_total %d\n", m.reclaimedB)
	fmt.Fprintln(w, "# HELP clawnap_wake_ready_seconds Time from wake to readiness.\n# TYPE clawnap_wake_ready_seconds histogram")
	for i, b := range m.readyBuckets {
		fmt.Fprintf(w, "clawnap_wake_ready_seconds_bucket{le=\"%g\"} %d\n", b, m.readyCounts[i])
	}
	fmt.Fprintf(w, "clawnap_wake_ready_seconds_bucket{le=\"+Inf\"} %d\nclawnap_wake_ready_seconds_sum %g\nclawnap_wake_ready_seconds_count %d\n", m.readyN, m.readySum, m.readyN)
}

func sortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
