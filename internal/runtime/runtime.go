// Package runtime is the narrow seam between the supervisor and a container
// runtime (Docker or Podman CLI). Everything the supervisor needs from the
// runtime goes through Runner so tests can substitute a fake and so the
// supervisor stays the only process on the host that issues these commands
// for managed cells — the same boundary discipline as Isthmus's kernel.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// Run implements Runner. Only stdout is returned as data: runtime warnings
// on stderr (Podman banners, Docker deprecations) must never be parsed as
// a status word, JSON, or a container id. stderr goes into the error only.
func (e ExecRunner) Run(ctx context.Context, args ...string) (string, error) {
	bin := e.Binary
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", bin, strings.Join(redactArgs(args), " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// redactArgs hides the value of -e/--env KEY=VALUE pairs so a failed
// `docker run` never prints a token into logs.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := range out {
		if i > 0 && (out[i-1] == "-e" || out[i-1] == "--env") {
			if k, _, ok := strings.Cut(out[i], "="); ok {
				out[i] = k + "=<redacted>"
			}
		}
	}
	return out
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

// Client wraps a Runner with typed operations. ProcRoot is where the host's
// procfs is mounted (default /proc); tests point it at a fixture tree.
type Client struct {
	R        Runner
	ProcRoot string
}

// Info is what one inspect call yields: the lifecycle state and, for a
// running container, the host pid of its init process.
type Info struct {
	State State
	Pid   int
}

// Inspect returns the container's state.
func (c Client) Inspect(ctx context.Context, name string) (State, error) {
	i, err := c.Info(ctx, name)
	return i.State, err
}

// Info returns the container's state and init pid in one runtime call. The
// pid lets the idle detector read the container's network counters from
// procfs instead of the rounded figures `docker stats` prints.
func (c Client) Info(ctx context.Context, name string) (Info, error) {
	out, err := c.R.Run(ctx, "inspect", "-f", "{{.State.Status}} {{.State.Pid}}", name)
	if err != nil {
		if IsDaemonUnreachable(err) {
			return Info{State: StateUnknown}, fmt.Errorf("runtime unreachable: %w", err)
		}
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "no such") {
			return Info{State: StateMissing}, nil
		}
		return Info{State: StateUnknown}, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return Info{State: StateUnknown}, nil
	}
	i := Info{State: StateUnknown}
	switch s := State(fields[0]); s {
	case StateRunning, StateExited, StateCreated, StatePaused:
		i.State = s
	}
	if len(fields) > 1 {
		i.Pid, _ = strconv.Atoi(fields[1])
	}
	return i, nil
}

// Snapshot returns the state of every named container in two runtime calls
// (one `ps -a` for states, one inspect for the running ones' pids) instead
// of one inspect per cell. Measured 2026-10-02 on the 8-vCPU host: the
// per-cell form at 103 cells on a 5 s interval cost about one core of
// docker CLI start-up time, continuously. Names not listed are StateMissing.
func (c Client) Snapshot(ctx context.Context, names []string) (map[string]Info, error) {
	out, err := c.R.Run(ctx, "ps", "-a", "--no-trunc", "--format", "{{.Names}}\t{{.State}}")
	if err != nil {
		if IsDaemonUnreachable(err) {
			return nil, fmt.Errorf("runtime unreachable: %w", err)
		}
		return nil, err
	}
	res := make(map[string]Info, len(names))
	for _, n := range names {
		res[n] = Info{State: StateMissing}
	}
	var running []string
	for _, l := range strings.Split(out, "\n") {
		name, st, ok := strings.Cut(strings.TrimSpace(l), "\t")
		if !ok {
			continue
		}
		if _, wanted := res[name]; !wanted {
			continue
		}
		i := Info{State: StateUnknown}
		switch s := State(strings.TrimSpace(st)); s {
		case StateRunning, StateExited, StateCreated, StatePaused:
			i.State = s
		}
		if i.State == StateRunning {
			running = append(running, name)
		}
		res[name] = i
	}
	if len(running) > 0 {
		args := append([]string{"inspect", "-f", "{{.Name}} {{.State.Pid}}"}, running...)
		if out, err := c.R.Run(ctx, args...); err == nil {
			for _, l := range strings.Split(out, "\n") {
				f := strings.Fields(l)
				if len(f) != 2 {
					continue
				}
				n := strings.TrimPrefix(f[0], "/")
				if i, ok := res[n]; ok {
					i.Pid, _ = strconv.Atoi(f[1])
					res[n] = i
				}
			}
		}
	}
	return res, nil
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
	NetExact bool // counters read from procfs (byte-exact), not docker's rounded NetIO
	MemBytes int64
	CPUPct   float64 // docker's CPU %: 100 = one core busy over the sampling window
}

type statsJSON struct {
	NetIO    string `json:"NetIO"`
	MemUsage string `json:"MemUsage"`
	CPUPerc  string `json:"CPUPerc"`
}

// Stats samples a running container's network counters, memory and CPU.
// The network counters come from the container's own /proc/<pid>/net/dev
// when pid is known and the host's procfs is readable: `docker stats`
// prints NetIO rounded to three significant digits, so once a cell has
// moved a few hundred megabytes a whole conversation's traffic is invisible
// in the difference between two samples. The rounded figure is the fallback.
func (c Client) Stats(ctx context.Context, name string, pid int) (Stats, error) {
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
	exact := false
	if pid > 0 {
		if erx, etx, err := ReadNetDev(filepath.Join(c.procRoot(), strconv.Itoa(pid), "net", "dev")); err == nil {
			rx, tx, exact = erx, etx, true
		}
	}
	mem, _ := parseMemUsage(sj.MemUsage)
	cpu, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(sj.CPUPerc), "%"), 64)
	return Stats{Net: NetIO{RxBytes: rx, TxBytes: tx}, NetExact: exact, MemBytes: mem, CPUPct: cpu}, nil
}

func (c Client) procRoot() string {
	if c.ProcRoot != "" {
		return c.ProcRoot
	}
	return "/proc"
}

// ReadNetDev sums the byte counters of every interface except loopback in
// a /proc/<pid>/net/dev file (the network namespace of that process).
func ReadNetDev(path string) (rx, tx int64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 3 {
		return 0, 0, fmt.Errorf("%s: no interfaces", path)
	}
	seen := false
	for _, l := range lines[2:] {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseInt(f[0], 10, 64)
		t, err2 := strconv.ParseInt(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
		seen = true
	}
	if !seen {
		return 0, 0, fmt.Errorf("%s: no interfaces", path)
	}
	return rx, tx, nil
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

// LogsSince returns the container's log output since the given RFC3339
// timestamp (both streams). Used to read the gateway's own recovery
// markers after a thaw, which it does not expose on any endpoint.
func (c Client) LogsSince(ctx context.Context, name, since string) (string, error) {
	return c.R.Run(ctx, "logs", "--since", since, name)
}

// ID returns the container's full id.
func (c Client) ID(ctx context.Context, name string) (string, error) {
	out, err := c.R.Run(ctx, "inspect", "-f", "{{.Id}}", name)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if len(id) < 12 {
		return "", fmt.Errorf("unexpected container id %q for %s", id, name)
	}
	return id, nil
}

// Mount is one bind/volume mount of a container.
type Mount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// Mounts returns a container's mounts.
func (c Client) Mounts(ctx context.Context, name string) ([]Mount, error) {
	out, err := c.R.Run(ctx, "inspect", "-f", "{{json .Mounts}}", name)
	if err != nil {
		return nil, err
	}
	var ms []Mount
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ms); err != nil {
		return nil, fmt.Errorf("parse mounts: %w", err)
	}
	return ms, nil
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
