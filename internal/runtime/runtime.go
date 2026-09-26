// Package runtime is the narrow seam between the supervisor and a container
// runtime (Docker or Podman CLI). Everything the supervisor needs from the
// runtime goes through Runner so tests can substitute a fake and so the
// supervisor stays the only process on the host that issues these commands
// for managed cells — the same boundary discipline as Isthmus's kernel.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner executes one runtime CLI invocation and returns stdout.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// ExecRunner shells out to a real binary ("docker" or "podman").
type ExecRunner struct{ Binary string }

// Run implements Runner.
func (e ExecRunner) Run(ctx context.Context, args ...string) (string, error) {
	bin := e.Binary
	if bin == "" {
		bin = "docker"
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// State is a container's lifecycle state as reported by the runtime.
type State string

const (
	StateRunning State = "running"
	StateExited  State = "exited"
	StateCreated State = "created"
	StatePaused  State = "paused"
	StateMissing State = "missing" // no such container
	StateUnknown State = "unknown"
)

// Client wraps a Runner with typed operations.
type Client struct{ R Runner }

// Inspect returns the container's state.
func (c Client) Inspect(ctx context.Context, name string) (State, error) {
	out, err := c.R.Run(ctx, "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		if IsDaemonUnreachable(err) {
			return StateUnknown, fmt.Errorf("runtime unreachable: %w", err)
		}
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "no such") {
			return StateMissing, nil
		}
		return StateUnknown, err
	}
	switch s := State(strings.TrimSpace(out)); s {
	case StateRunning, StateExited, StateCreated, StatePaused:
		return s, nil
	default:
		return StateUnknown, nil
	}
}

// IsDaemonUnreachable reports whether an error is the runtime daemon being
// down or unreachable, which must never be read as "container missing".
func IsDaemonUnreachable(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "cannot connect to the docker daemon") ||
		strings.Contains(m, "is the docker daemon running") ||
		strings.Contains(m, "connection refused") ||
		strings.Contains(m, "no such file or directory") && strings.Contains(m, ".sock") ||
		strings.Contains(m, "executable file not found")
}

// Stop hibernates a container (graceful stop with a timeout). Returns wall time.
func (c Client) Stop(ctx context.Context, name string, grace time.Duration) (time.Duration, error) {
	t := time.Now()
	_, err := c.R.Run(ctx, "stop", "-t", strconv.Itoa(int(grace.Seconds())), name)
	return time.Since(t), err
}

// Start restores a stopped container. Returns wall time to the runtime's
// "started" (not application readiness — see supervisor.waitReady).
func (c Client) Start(ctx context.Context, name string) (time.Duration, error) {
	t := time.Now()
	_, err := c.R.Run(ctx, "start", name)
	return time.Since(t), err
}

// Pause freezes a running container's processes (cgroup freezer). Memory
// stays resident; wake is near-instant. Returns wall time.
func (c Client) Pause(ctx context.Context, name string) (time.Duration, error) {
	t := time.Now()
	_, err := c.R.Run(ctx, "pause", name)
	return time.Since(t), err
}

// Unpause thaws a paused container. Returns wall time.
func (c Client) Unpause(ctx context.Context, name string) (time.Duration, error) {
	t := time.Now()
	_, err := c.R.Run(ctx, "unpause", name)
	return time.Since(t), err
}

// NetIO is cumulative bytes since container start.
type NetIO struct{ RxBytes, TxBytes int64 }

// Stats holds the one-shot sample the idle detector consumes.
type Stats struct {
	Net      NetIO
	MemBytes int64
}

type statsJSON struct {
	NetIO    string `json:"NetIO"`
	MemUsage string `json:"MemUsage"`
}

// Stats samples a running container's network counters and memory.
func (c Client) Stats(ctx context.Context, name string) (Stats, error) {
	out, err := c.R.Run(ctx, "stats", "--no-stream", "--format", "{{json .}}", name)
	if err != nil {
		return Stats{}, err
	}
	var sj statsJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sj); err != nil {
		return Stats{}, fmt.Errorf("parse stats: %w", err)
	}
	rx, tx, err := ParseNetIO(sj.NetIO)
	if err != nil {
		return Stats{}, err
	}
	mem, _ := parseMemUsage(sj.MemUsage)
	return Stats{Net: NetIO{RxBytes: rx, TxBytes: tx}, MemBytes: mem}, nil
}

// ParseNetIO parses docker's "1.2kB / 3.4MB" form into bytes.
func ParseNetIO(s string) (rx, tx int64, err error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad NetIO %q", s)
	}
	rx, err = ParseSize(strings.TrimSpace(parts[0]))
	if err != nil {
		return
	}
	tx, err = ParseSize(strings.TrimSpace(parts[1]))
	return
}

func parseMemUsage(s string) (int64, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 1 {
		return 0, errors.New("bad MemUsage")
	}
	return ParseSize(strings.TrimSpace(parts[0]))
}

var units = map[string]float64{
	"B": 1, "kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
}

// ParseSize parses "656B", "1.23kB", "512MiB" into bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') && s[i-1] != '.' {
		i--
	}
	num, unit := s[:i], strings.TrimSpace(s[i:])
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	m, ok := units[unit]
	if !ok {
		return 0, fmt.Errorf("unknown unit in %q", s)
	}
	return int64(f * m), nil
}

// ListManaged returns container names carrying the supervisor's label.
func (c Client) ListManaged(ctx context.Context, label string) ([]string, error) {
	out, err := c.R.Run(ctx, "ps", "-a", "--filter", "label="+label, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}
