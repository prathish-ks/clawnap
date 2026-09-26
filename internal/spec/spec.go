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

// PathClass is what a host path looks like to a tenant cell.
type PathClass int

const (
	PathOK            PathClass = iota
	PathRuntimeSocket           // Docker/Podman control socket: root on the host
	PathDangerousRoot           // host root paths: /etc, /proc, /var/lib/docker, / ...
	PathSecretShaped            // credential material: .ssh, .aws, .env, id_rsa ...
)

var runtimeSockets = []string{"/var/run/docker.sock", "/run/docker.sock", "/run/podman", "/var/run/podman"}

// dangerousPrefixes are refused as a mount source together with everything
// under them: system trees and the runtime's own state.
var dangerousPrefixes = []string{"/etc", "/root", "/proc", "/sys", "/dev", "/boot", "/var/lib/docker", "/var/lib/containers", "/usr", "/bin", "/sbin"}

// dangerousExact are refused only when mounted as a whole (Isthmus
// securitycheck.dangerousRoots): /home/ops/cells/a is fine, /home is not.
var dangerousExact = []string{"/home", "/var", "/opt", "/srv"}

// secretPathPatterns are Isthmus mount defaultBlockedPatterns.
var secretPathPatterns = []string{
	".ssh", ".gnupg", ".gpg", ".aws", ".azure", ".gcloud", ".kube", ".docker",
	"credentials", ".env", ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_ed25519", "private_key", ".secret",
}

// ClassifyHostPath is the single rule set shared by the launcher (Validate)
// and the inspector (hostcheck), so the two can never disagree.
func ClassifyHostPath(p string) PathClass {
	hp := filepath.Clean(p)
	if hp == "/" {
		return PathDangerousRoot
	}
	for _, sp := range runtimeSockets {
		if hp == sp || strings.HasPrefix(hp, sp+"/") {
			return PathRuntimeSocket
		}
	}
	for _, dr := range dangerousPrefixes {
		if hp == dr || strings.HasPrefix(hp, dr+"/") {
			return PathDangerousRoot
		}
	}
	for _, dr := range dangerousExact {
		if hp == dr {
			return PathDangerousRoot
		}
	}
	lower := strings.ToLower(hp)
	for _, pat := range secretPathPatterns {
		if strings.Contains(lower, pat) {
			return PathSecretShaped
		}
	}
	return PathOK
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
		switch ClassifyHostPath(hp) {
		case PathRuntimeSocket:
			errs = append(errs, fmt.Errorf("mount %q is a container runtime socket (blocked path)", m.HostPath))
		case PathDangerousRoot:
			errs = append(errs, fmt.Errorf("mount %q is under a blocked host root path", m.HostPath))
		case PathSecretShaped:
			errs = append(errs, fmt.Errorf("mount %q looks like credential material; inject secrets via a broker instead", m.HostPath))
		}
	}
	for _, e := range s.Env {
		k := strings.SplitN(e, "=", 2)[0]
		if LooksSecretEnvKey(k) {
			errs = append(errs, fmt.Errorf("env %s looks like a secret; inject via broker, not env", k))
		}
	}
	return errors.Join(errs...)
}

var secretEnvWords = []string{"SECRET", "TOKEN", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "PRIVATE_KEY", "ACCESS_KEY"}

// LooksSecretEnvKey reports whether an env var name looks like a credential.
func LooksSecretEnvKey(k string) bool {
	k = strings.ToUpper(k)
	for _, w := range secretEnvWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}
