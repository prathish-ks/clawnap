// Package hostcheck is the read-only host and cell inspector behind
// `fleetd check` ("Release 0"). It reports, never fixes. The checks and their
// remediation text are ported from Isthmus (github.com/prathish-ks/isthmus,
// go-host/internal/{doctor,securitycheck,egress,mount}, MIT), generalised
// from one NanoClaw host to any set of labelled OpenClaw cells.
//
// Checks:
//   - container runtime reachable and its version
//   - runtime class: gVisor / Kata / Sysbox vs shared-kernel runc (ADR-021)
//   - cloud-metadata egress block on the DOCKER-USER chain (ADR-013 D3)
//   - per cell: privileged, root user, missing cap-drop ALL, missing
//     no-new-privileges, missing pids limit, missing memory limit,
//     Docker socket mounted, dangerous host paths / secret-shaped mounts,
//     secret-shaped environment variables, published non-loopback ports
package hostcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Level mirrors Isthmus doctor levels: pass, warn, fail. Checks that cannot
// run report pass with a detail saying so, never silence.
type Level string

const (
	LevelPass Level = "pass"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// Result is one named check outcome.
type Result struct {
	Cell        string `json:"cell,omitempty"` // empty for host-level checks
	Name        string `json:"name"`
	Level       Level  `json:"level"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// Runner runs the container runtime CLI (docker/podman).
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Options configure a run.
type Options struct {
	Label      string // cells are containers carrying this label (e.g. "fleet.cell")
	Containers []string
	HostExec   func(ctx context.Context, name string, args ...string) (string, error) // for iptables/nft; nil = skip
}

// hardened runtime classes (ADR-021).
var hardened = []struct{ match, name string }{
	{"runsc", "gVisor"}, {"kata", "Kata Containers"}, {"sysbox", "Sysbox"},
}

func hardenedName(rt string) string {
	l := strings.ToLower(rt)
	for _, h := range hardened {
		if strings.Contains(l, h.match) {
			return h.name
		}
	}
	return ""
}

// Dangerous host paths for a tenant cell (Isthmus securitycheck.dangerousRoots
// plus the Docker/Podman sockets).
var dangerousRoots = []string{"/", "/etc", "/root", "/home", "/var", "/usr", "/bin", "/sbin", "/boot", "/proc", "/sys", "/dev"}
var socketPaths = []string{"/var/run/docker.sock", "/run/docker.sock", "/run/podman/podman.sock", "/var/run/podman/podman.sock"}

// Secret-shaped path fragments (Isthmus mount defaultBlockedPatterns).
var secretPathPatterns = []string{
	".ssh", ".gnupg", ".gpg", ".aws", ".azure", ".gcloud", ".kube", ".docker",
	"credentials", ".env", ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_ed25519", "private_key", ".secret",
}

var secretEnvWords = []string{"SECRET", "TOKEN", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "PRIVATE_KEY", "ACCESS_KEY"}

// Run executes every check and returns results in a stable order.
func Run(ctx context.Context, r Runner, opt Options) []Result {
	var out []Result
	out = append(out, checkRuntime(ctx, r))
	out = append(out, checkRuntimeClass(ctx, r))
	out = append(out, checkMetadataEgress(ctx, opt))
	names := opt.Containers
	if len(names) == 0 && opt.Label != "" {
		if lst, err := r.Run(ctx, "ps", "-a", "--filter", "label="+opt.Label, "--format", "{{.Names}}"); err == nil {
			for _, l := range strings.Split(strings.TrimSpace(lst), "\n") {
				if l = strings.TrimSpace(l); l != "" {
					names = append(names, l)
				}
			}
		}
	}
	if len(names) == 0 {
		out = append(out, Result{Name: "cells", Level: LevelPass, Detail: "no cells found (label " + opt.Label + "); nothing to inspect"})
		return out
	}
	for _, n := range names {
		out = append(out, checkCell(ctx, r, n)...)
	}
	return out
}

func checkRuntime(ctx context.Context, r Runner) Result {
	const name = "container runtime"
	out, err := r.Run(ctx, "info", "--format", "{{.ServerVersion}} {{.OperatingSystem}} cgroup={{.CgroupVersion}}")
	if err != nil {
		return Result{Name: name, Level: LevelFail, Detail: "daemon not reachable: " + firstLine(err.Error()), Remediation: "start Docker/Podman or point FLEETD_RUNTIME at the right binary"}
	}
	return Result{Name: name, Level: LevelPass, Detail: strings.TrimSpace(out)}
}

func checkRuntimeClass(ctx context.Context, r Runner) Result {
	const name = "container runtime class (hardened isolation)"
	out, err := r.Run(ctx, "info", "--format", "{{.DefaultRuntime}};{{range $n, $_ := .Runtimes}}{{$n}} {{end}}")
	def, list, ok := strings.Cut(strings.TrimSpace(out), ";")
	def = strings.TrimSpace(def)
	if err != nil || !ok || def == "" {
		return Result{Name: name, Level: LevelPass, Detail: "not determined: daemon did not answer with a parseable runtime list"}
	}
	if h := hardenedName(def); h != "" {
		return Result{Name: name, Level: LevelPass, Detail: "default runtime " + def + " (" + h + "): cells do not share the host kernel"}
	}
	var avail []string
	for _, n := range strings.Fields(list) {
		if h := hardenedName(n); h != "" {
			avail = append(avail, n+" ("+h+")")
		}
	}
	if len(avail) > 0 {
		return Result{Name: name, Level: LevelWarn, Detail: "hardened runtime installed (" + strings.Join(avail, ", ") + ") but default is " + def + "; cells share the host kernel",
			Remediation: "set default-runtime in daemon.json or run cells with --runtime; the checker reports, it never selects a runtime"}
	}
	return Result{Name: name, Level: LevelPass, Detail: "no hardened runtime (gVisor, Kata, Sysbox) available; cells run under " + def + " and share the host kernel. One kernel bug is a cross-tenant boundary on this host."}
}

// checkMetadataEgress looks for a DOCKER-USER rule dropping 169.254.0.0/16.
// Linux only; elsewhere it reports the gap honestly (Isthmus ADR-013).
func checkMetadataEgress(ctx context.Context, opt Options) Result {
	const name = "cloud-metadata egress block (169.254.0.0/16)"
	if runtime.GOOS != "linux" {
		return Result{Name: name, Level: LevelWarn, Detail: "not verifiable on " + runtime.GOOS + ": Docker Desktop's VM has its own netfilter tables the host cannot see",
			Remediation: "run this check on the Linux host that runs the cells"}
	}
	if opt.HostExec == nil {
		return Result{Name: name, Level: LevelPass, Detail: "skipped: no host exec configured"}
	}
	if out, err := opt.HostExec(ctx, "nft", "list", "chain", "ip", "filter", "DOCKER-USER"); err == nil {
		if strings.Contains(out, "169.254.0.0/16") && strings.Contains(out, "drop") {
			return Result{Name: name, Level: LevelPass, Detail: "nft DOCKER-USER rule dropping 169.254.0.0/16 is present"}
		}
	}
	if _, err := opt.HostExec(ctx, "iptables", "-C", "DOCKER-USER", "-d", "169.254.0.0/16", "-j", "DROP"); err == nil {
		return Result{Name: name, Level: LevelPass, Detail: "iptables DOCKER-USER rule dropping 169.254.0.0/16 is present"}
	}
	return Result{Name: name, Level: LevelWarn, Detail: "no DOCKER-USER rule blocks the cloud instance-metadata range; any cell can read the host's cloud IAM credentials at 169.254.169.254",
		Remediation: "iptables -I DOCKER-USER -d 169.254.0.0/16 -j DROP (or the nft equivalent); no legitimate agent workload needs that range"}
}

type inspect struct {
	Config struct {
		User string   `json:"User"`
		Env  []string `json:"Env"`
	} `json:"Config"`
	HostConfig struct {
		Privileged   bool                                           `json:"Privileged"`
		CapDrop      []string                                       `json:"CapDrop"`
		SecurityOpt  []string                                       `json:"SecurityOpt"`
		PidsLimit    *int64                                         `json:"PidsLimit"`
		Memory       int64                                          `json:"Memory"`
		Runtime      string                                         `json:"Runtime"`
		PortBindings map[string][]struct{ HostIp, HostPort string } `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type, Source, Destination string
		RW                        bool
	} `json:"Mounts"`
}

func checkCell(ctx context.Context, r Runner, name string) []Result {
	raw, err := r.Run(ctx, "inspect", name)
	if err != nil {
		return []Result{{Cell: name, Name: "inspect", Level: LevelFail, Detail: firstLine(err.Error())}}
	}
	var arr []inspect
	if err := json.Unmarshal([]byte(raw), &arr); err != nil || len(arr) == 0 {
		return []Result{{Cell: name, Name: "inspect", Level: LevelFail, Detail: "could not parse inspect output"}}
	}
	c := arr[0]
	var out []Result
	add := func(n string, lvl Level, d, rem string) {
		out = append(out, Result{Cell: name, Name: n, Level: lvl, Detail: d, Remediation: rem})
	}

	if c.HostConfig.Privileged {
		add("privileged", LevelFail, "container runs privileged: full host access", "remove --privileged")
	} else {
		add("privileged", LevelPass, "not privileged", "")
	}
	u := strings.TrimSpace(c.Config.User)
	if u == "" || u == "0" || u == "root" || strings.HasPrefix(u, "0:") || strings.HasPrefix(u, "root:") {
		add("non-root user", LevelWarn, "runs as root inside the container (User="+u+")", "run with --user <uid>:<gid> (the official image supports the node user)")
	} else {
		add("non-root user", LevelPass, "User="+u, "")
	}
	capAll := false
	for _, cd := range c.HostConfig.CapDrop {
		if strings.EqualFold(cd, "ALL") {
			capAll = true
		}
	}
	if capAll {
		add("cap-drop ALL", LevelPass, "all capabilities dropped", "")
	} else {
		add("cap-drop ALL", LevelWarn, "capabilities not dropped", "add --cap-drop ALL (re-add only what the cell needs)")
	}
	nnp := false
	for _, so := range c.HostConfig.SecurityOpt {
		if strings.Contains(so, "no-new-privileges") {
			nnp = true
		}
	}
	if nnp {
		add("no-new-privileges", LevelPass, "set", "")
	} else {
		add("no-new-privileges", LevelWarn, "not set", "add --security-opt no-new-privileges")
	}
	if c.HostConfig.PidsLimit == nil || *c.HostConfig.PidsLimit <= 0 {
		add("pids limit", LevelWarn, "no pids limit: one cell can fork-bomb the host", "add --pids-limit 512")
	} else {
		add("pids limit", LevelPass, fmt.Sprintf("%d", *c.HostConfig.PidsLimit), "")
	}
	if c.HostConfig.Memory <= 0 {
		add("memory limit", LevelWarn, "no memory limit: one cell can OOM the host and every other tenant", "add --memory (OpenClaw idles at 500-700 MiB; its own critical threshold is 768 MiB)")
	} else {
		add("memory limit", LevelPass, fmt.Sprintf("%d MiB", c.HostConfig.Memory>>20), "")
	}
	if h := hardenedName(c.HostConfig.Runtime); h != "" {
		add("runtime", LevelPass, c.HostConfig.Runtime+" ("+h+")", "")
	}
	// mounts
	sockHit, dangerHit, secretHit := []string{}, []string{}, []string{}
	for _, m := range c.Mounts {
		src := filepath.Clean(m.Source)
		for _, sp := range socketPaths {
			if src == sp || m.Destination == sp {
				sockHit = append(sockHit, src)
			}
		}
		for _, dr := range dangerousRoots {
			if src == dr {
				dangerHit = append(dangerHit, src)
			}
		}
		for _, p := range secretPathPatterns {
			if strings.Contains(strings.ToLower(src), p) {
				secretHit = append(secretHit, src)
				break
			}
		}
	}
	if len(sockHit) > 0 {
		add("docker socket", LevelFail, "container runtime socket mounted: "+strings.Join(sockHit, ", ")+" (root on the host; CVE-2026-27002 class)", "remove the socket mount; if the cell needs containers, give the supervisor that job")
	} else {
		add("docker socket", LevelPass, "no runtime socket mounted", "")
	}
	if len(dangerHit) > 0 {
		add("dangerous mounts", LevelFail, "host root paths mounted: "+strings.Join(dangerHit, ", "), "mount only the cell's own state directory")
	} else {
		add("dangerous mounts", LevelPass, "no host root paths mounted", "")
	}
	if len(secretHit) > 0 {
		add("secret-shaped mounts", LevelWarn, "mounts look like credential material: "+strings.Join(secretHit, ", "), "keep host credentials out of tenant cells; inject per-request via a broker")
	} else {
		add("secret-shaped mounts", LevelPass, "none", "")
	}
	// env
	var secretEnv []string
	for _, e := range c.Config.Env {
		k := strings.ToUpper(strings.SplitN(e, "=", 2)[0])
		for _, w := range secretEnvWords {
			if strings.Contains(k, w) {
				secretEnv = append(secretEnv, k)
				break
			}
		}
	}
	if len(secretEnv) > 0 {
		add("secrets in env", LevelWarn, "secret-shaped env vars visible to every process in the cell: "+strings.Join(secretEnv, ", "), "prefer token files or a broker; env is readable by any tool the agent runs (OPENCLAW_GATEWAY_TOKEN is expected)")
	} else {
		add("secrets in env", LevelPass, "none", "")
	}
	// ports
	var public []string
	for port, binds := range c.HostConfig.PortBindings {
		for _, b := range binds {
			if b.HostIp == "" || b.HostIp == "0.0.0.0" || b.HostIp == "::" {
				public = append(public, port+"->"+b.HostPort)
			}
		}
	}
	if len(public) > 0 {
		add("published ports", LevelWarn, "bound on all interfaces: "+strings.Join(public, ", ")+" (135k exposed OpenClaw instances were found on the internet in 2026)", "bind to 127.0.0.1 and front with the ingress/reverse proxy")
	} else {
		add("published ports", LevelPass, "loopback only or none", "")
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Summary counts levels.
func Summary(rs []Result) (pass, warn, fail int) {
	for _, r := range rs {
		switch r.Level {
		case LevelPass:
			pass++
		case LevelWarn:
			warn++
		case LevelFail:
			fail++
		}
	}
	return
}
