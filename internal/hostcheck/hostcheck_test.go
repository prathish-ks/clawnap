package hostcheck

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fake struct{ inspect map[string]string }

func (f fake) Run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "info":
		if strings.Contains(args[2], "DefaultRuntime") {
			return "runc;io.containerd.runc.v2 runc runsc \n", nil
		}
		return "27.3.1 Docker Desktop cgroup=2", nil
	case "inspect":
		return f.inspect[args[1]], nil
	}
	return "", nil
}

func mk(privileged bool, user string, capDrop []string, secOpt []string, pids int64, mem int64, mounts []map[string]any, env []string, hostIP string) string {
	m := map[string]any{
		"Config": map[string]any{"User": user, "Env": env},
		"HostConfig": map[string]any{"Privileged": privileged, "CapDrop": capDrop, "SecurityOpt": secOpt, "PidsLimit": pids, "Memory": mem, "Runtime": "runc",
			"PortBindings": map[string]any{"18789/tcp": []map[string]string{{"HostIp": hostIP, "HostPort": "18801"}}}},
		"Mounts": mounts,
	}
	b, _ := json.Marshal([]any{m})
	return string(b)
}

func find(rs []Result, cell, name string) Result {
	for _, r := range rs {
		if r.Cell == cell && r.Name == name {
			return r
		}
	}
	return Result{}
}

func TestHardenedCellPassesAndSloppyCellFails(t *testing.T) {
	good := mk(false, "node", []string{"ALL"}, []string{"no-new-privileges"}, 512, 1<<30,
		[]map[string]any{{"Type": "bind", "Source": "/srv/cells/a/state", "Destination": "/home/node/.openclaw", "RW": true}},
		[]string{"PATH=/usr/bin", "OPENCLAW_GATEWAY_TOKEN=x"}, "127.0.0.1")
	bad := mk(true, "", nil, nil, 0, 0,
		[]map[string]any{{"Type": "bind", "Source": "/var/run/docker.sock", "Destination": "/var/run/docker.sock"}, {"Type": "bind", "Source": "/home/ops/.ssh", "Destination": "/keys"}, {"Type": "bind", "Source": "/", "Destination": "/host"}},
		[]string{"ANTHROPIC_API_KEY=sk"}, "0.0.0.0")
	rs := Run(context.Background(), fake{inspect: map[string]string{"good": good, "bad": bad}}, Options{Containers: []string{"good", "bad"}})

	if r := find(rs, "", "container runtime class (hardened isolation)"); r.Level != LevelWarn || !strings.Contains(r.Detail, "gVisor") {
		t.Fatalf("runsc installed but not default should warn: %+v", r)
	}
	for _, n := range []string{"privileged", "docker socket", "dangerous mounts"} {
		if r := find(rs, "bad", n); r.Level != LevelFail {
			t.Errorf("bad/%s should fail: %+v", n, r)
		}
		if r := find(rs, "good", n); r.Level != LevelPass {
			t.Errorf("good/%s should pass: %+v", n, r)
		}
	}
	for _, n := range []string{"non-root user", "cap-drop ALL", "no-new-privileges", "pids limit", "memory limit", "secret-shaped mounts", "published ports"} {
		if r := find(rs, "bad", n); r.Level != LevelWarn {
			t.Errorf("bad/%s should warn: %+v", n, r)
		}
		if r := find(rs, "good", n); r.Level != LevelPass {
			t.Errorf("good/%s should pass: %+v", n, r)
		}
	}
	// OPENCLAW_GATEWAY_TOKEN is secret-shaped and flagged as warn (expected but visible)
	if r := find(rs, "good", "secrets in env"); r.Level != LevelWarn {
		t.Errorf("gateway token in env should warn: %+v", r)
	}
	p, w, f := Summary(rs)
	if f != 3 || p == 0 || w == 0 {
		t.Fatalf("summary pass=%d warn=%d fail=%d", p, w, f)
	}
}

// The swap check exists because the failure it catches is silent: a full
// swapfile does not error, it removes the host's ability to reclaim.
func TestSwapCapacityCheck(t *testing.T) {
	const cell = 784 << 20
	for _, tc := range []struct {
		name  string
		cells int
		total int64
		free  int64
		want  Level
	}{
		{"room for the fleet", 100, 110 << 30, 40 << 30, LevelPass},
		{"cannot hold the fleet", 100, 80 << 30, 1 << 30, LevelWarn},
		{"holds it but nearly full", 100, 100 << 30, 2 << 30, LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := swapVerdict(tc.cells, tc.total, tc.free)
			if got.Level != tc.want {
				t.Fatalf("want %s, got %s (%s)", tc.want, got.Level, got.Detail)
			}
			if tc.want == LevelWarn && got.Remediation == "" {
				t.Fatal("a warning with no remediation tells an operator nothing")
			}
		})
	}
	// Off Linux there is no /proc/meminfo: say so rather than warn falsely.
	if r := checkSwapForFleet(100); r.Level == LevelFail {
		t.Fatalf("an undeterminable check must not fail: %s", r.Detail)
	}
}
