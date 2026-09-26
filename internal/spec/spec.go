// Package spec is the guest-neutral container specification a cell is created
// from, plus the invariants the supervisor refuses to launch without. The
// rules mirror Isthmus's internal/containerdefaults and internal/mount
// (non-root identity, positive pids limit, no Docker socket or dangerous host
// paths). Once those Isthmus packages are extracted into an importable
// module with this input shape, this file becomes a thin adapter over them.
package spec

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Mount is one host→container bind.
type Mount struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
	ReadOnly      bool   `json:"read_only"`
}

// ContainerSpec is what a driver turns into a runtime create command.
type ContainerSpec struct {
	Image      string   `json:"image"`
	UID        int      `json:"uid"`
	GID        int      `json:"gid"`
	UserSet    bool     `json:"user_set"`
	PidsLimit  int      `json:"pids_limit"`
	MemBytes   int64    `json:"mem_bytes"`
	Mounts     []Mount  `json:"mounts"`
	Env        []string `json:"env"`
	Privileged bool     `json:"privileged"`
}

// Dangerous host paths that must never be bind-mounted into a tenant cell.
var blockedPrefixes = []string{
	"/var/run/docker.sock", "/run/docker.sock", "/run/podman", "/var/run/podman",
	"/etc", "/root", "/proc", "/sys", "/dev", "/boot", "/var/lib/docker",
}

// Validate returns every violated invariant, or nil.
func Validate(s ContainerSpec) error {
	var errs []error
	if s.Image == "" {
		errs = append(errs, errors.New("image is required"))
	}
	if s.Privileged {
		errs = append(errs, errors.New("privileged cells are refused"))
	}
	if s.UserSet && (s.UID == 0 || s.GID == 0) {
		errs = append(errs, errors.New("root uid/gid is refused"))
	}
	if s.PidsLimit <= 0 {
		errs = append(errs, errors.New("pids_limit must be > 0 (fork-bomb guard)"))
	}
	if s.MemBytes <= 0 {
		errs = append(errs, errors.New("mem_bytes must be > 0 (density needs a ceiling)"))
	}
	for _, m := range s.Mounts {
		hp := filepath.Clean(m.HostPath)
		if !filepath.IsAbs(hp) {
			errs = append(errs, fmt.Errorf("mount %q must be absolute", m.HostPath))
			continue
		}
		if hp == "/" {
			errs = append(errs, errors.New("mounting / is refused"))
		}
		for _, b := range blockedPrefixes {
			if hp == b || strings.HasPrefix(hp, b+"/") {
				errs = append(errs, fmt.Errorf("mount %q is under blocked path %s", m.HostPath, b))
			}
		}
	}
	for _, e := range s.Env {
		k := strings.SplitN(e, "=", 2)[0]
		if looksSecret(k) {
			errs = append(errs, fmt.Errorf("env %s looks like a secret; inject via broker, not env", k))
		}
	}
	return errors.Join(errs...)
}

func looksSecret(k string) bool {
	k = strings.ToUpper(k)
	for _, w := range []string{"SECRET", "TOKEN", "PASSWORD", "API_KEY", "APIKEY", "PRIVATE_KEY"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}
