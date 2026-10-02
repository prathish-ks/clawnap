package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type errRunner struct{ msg string }

func (e errRunner) Run(context.Context, ...string) (string, error) { return "", errors.New(e.msg) }

func TestInspect_DaemonDownIsAnErrorNotMissing(t *testing.T) {
	c := Client{R: errRunner{msg: "docker inspect x: exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\nError: No such object: x"}}
	st, err := c.Inspect(context.Background(), "x")
	if err == nil || st != StateUnknown {
		t.Fatalf("daemon down must be an error with StateUnknown, got %s %v", st, err)
	}
	c = Client{R: errRunner{msg: "docker inspect x: exit status 1: Error: No such object: x"}}
	st, err = c.Inspect(context.Background(), "x")
	if err != nil || st != StateMissing {
		t.Fatalf("genuinely missing container should be StateMissing, got %s %v", st, err)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"656B": 656, "1.5kB": 1500, "2MB": 2000000, "512MiB": 512 << 20, "0B": 0}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("12 parsecs"); err == nil {
		t.Error("expected error for unknown unit")
	}
}

func TestParseNetIO(t *testing.T) {
	rx, tx, err := ParseNetIO("1.2kB / 3.4MB")
	if err != nil || rx != 1200 || tx != 3400000 {
		t.Fatalf("got %d %d %v", rx, tx, err)
	}
}

func TestRedactArgs(t *testing.T) {
	got := redactArgs([]string{"run", "-e", "OPENCLAW_GATEWAY_TOKEN=abc", "--env", "X=1", "img"})
	if got[2] != "OPENCLAW_GATEWAY_TOKEN=<redacted>" || got[4] != "X=<redacted>" || got[5] != "img" {
		t.Fatalf("redaction wrong: %v", got)
	}
}

func TestExecRunnerSeparatesStderr(t *testing.T) {
	// sh -c prints a warning on stderr and the real answer on stdout
	r := ExecRunner{Binary: "sh"}
	out, err := r.Run(context.Background(), "-c", "echo WARN >&2; echo running")
	if err != nil || out != "running\n" {
		t.Fatalf("stdout must be clean: %q %v", out, err)
	}
	_, err = r.Run(context.Background(), "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stderr must reach the error: %v", err)
	}
}

type scriptRunner struct{ out map[string]string }

func (s scriptRunner) Run(_ context.Context, args ...string) (string, error) {
	return s.out[args[0]], nil
}

func TestInfoParsesStateAndPid(t *testing.T) {
	c := Client{R: scriptRunner{out: map[string]string{"inspect": "running 4242\n"}}}
	i, err := c.Info(context.Background(), "x")
	if err != nil || i.State != StateRunning || i.Pid != 4242 {
		t.Fatalf("got %+v %v", i, err)
	}
	c = Client{R: scriptRunner{out: map[string]string{"inspect": "paused\n"}}} // older format: no pid
	if i, _ := c.Info(context.Background(), "x"); i.State != StatePaused || i.Pid != 0 {
		t.Fatalf("got %+v", i)
	}
}

const netDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999    1000    0    0    0     0          0         0  9999999    1000    0    0    0     0       0          0
  eth0: 250123456  200000    0    0    0     0          0         0  180654321  150000    0    0    0     0       0          0
`

// docker stats rounds NetIO to three significant digits ("250MB"), which
// hides a conversation's worth of traffic once a cell has moved a few
// hundred megabytes; Stats must read the exact counters from procfs when
// the pid is known, and fall back to the rounded figure when it is not.
func TestStatsPrefersExactProcfsCounters(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "4242", "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "4242", "net", "dev"), []byte(netDev), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Client{R: scriptRunner{out: map[string]string{"stats": `{"NetIO":"250MB / 181MB","MemUsage":"300MiB / 4GiB","CPUPerc":"1.00%"}`}}, ProcRoot: proc}
	st, err := c.Stats(context.Background(), "x", 4242)
	if err != nil || !st.NetExact || st.Net.RxBytes != 250123456 || st.Net.TxBytes != 180654321 {
		t.Fatalf("want exact counters excluding lo, got %+v %v", st, err)
	}
	st, err = c.Stats(context.Background(), "x", 0)
	if err != nil || st.NetExact || st.Net.RxBytes != 250000000 {
		t.Fatalf("without a pid the rounded figure is the fallback, got %+v %v", st, err)
	}
}

func TestSnapshotListsStatesInOnePsAndPidsInOneInspect(t *testing.T) {
	r := &countingRunner{out: map[string]string{"ps": "a\trunning\nb\tpaused\nstray\texited\n", "inspect": "/a 4242\n"}}
	c := Client{R: r}
	snap, err := c.Snapshot(context.Background(), []string{"a", "b", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if snap["a"].State != StateRunning || snap["a"].Pid != 4242 || snap["b"].State != StatePaused || snap["gone"].State != StateMissing {
		t.Fatalf("got %+v", snap)
	}
	if _, listed := snap["stray"]; listed {
		t.Fatal("containers outside the registry must not be reported")
	}
	if r.calls != 2 {
		t.Fatalf("want exactly two runtime calls (ps + inspect), got %d", r.calls)
	}
}

type countingRunner struct {
	out   map[string]string
	calls int
}

func (r *countingRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls++
	return r.out[args[0]], nil
}
