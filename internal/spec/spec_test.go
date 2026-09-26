package spec

import (
	"strings"
	"testing"
)

func good() ContainerSpec {
	return ContainerSpec{Image: "openclaw:stable", UID: 1000, GID: 1000, UserSet: true, PidsLimit: 512, MemBytes: 1 << 30,
		Mounts: []Mount{{HostPath: "/srv/cells/a/state", ContainerPath: "/home/node/.openclaw"}}}
}

func TestValidateGood(t *testing.T) {
	if err := Validate(good()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*ContainerSpec){
		"root":       func(s *ContainerSpec) { s.UID = 0 },
		"privileged": func(s *ContainerSpec) { s.Privileged = true },
		"pids":       func(s *ContainerSpec) { s.PidsLimit = 0 },
		"mem":        func(s *ContainerSpec) { s.MemBytes = 0 },
		"docker.sock": func(s *ContainerSpec) {
			s.Mounts = append(s.Mounts, Mount{HostPath: "/var/run/docker.sock", ContainerPath: "/var/run/docker.sock"})
		},
		"etc": func(s *ContainerSpec) {
			s.Mounts = append(s.Mounts, Mount{HostPath: "/etc/passwd", ContainerPath: "/x"})
		},
		"root/":  func(s *ContainerSpec) { s.Mounts = append(s.Mounts, Mount{HostPath: "/", ContainerPath: "/host"}) },
		"secret": func(s *ContainerSpec) { s.Env = []string{"ANTHROPIC_API_KEY=sk-x"} },
	}
	for name, mut := range cases {
		s := good()
		mut(&s)
		if err := Validate(s); err == nil {
			t.Errorf("%s: expected rejection", name)
		} else if name == "docker.sock" && !strings.Contains(err.Error(), "blocked path") {
			t.Errorf("docker.sock: wrong message %v", err)
		}
	}
}
