package runtime

import (
	"context"
	"errors"
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
