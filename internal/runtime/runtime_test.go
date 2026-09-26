package runtime

import (
	"context"
	"errors"
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
