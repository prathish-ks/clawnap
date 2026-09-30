//go:build linux

// ksmexec marks the process tree as mergeable by kernel samepage merging
// (prctl PR_SET_MEMORY_MERGE, inherited across fork and exec), drops to the
// cell's unprivileged user, then execs the cell's real entrypoint. Run it as
// the container entrypoint with --user 0:0 and only SYS_RESOURCE, SETUID and
// SETGID granted; the gateway itself runs as 1000:1000 with no capabilities.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ksmexec <entrypoint> [args...]")
		os.Exit(2)
	}
	if err := unix.Prctl(unix.PR_SET_MEMORY_MERGE, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "ksmexec: PR_SET_MEMORY_MERGE:", err) // continue: the cell must still start
	}
	uid, gid := envInt("KSMEXEC_UID", 1000), envInt("KSMEXEC_GID", 1000)
	if err := syscall.Setgroups([]int{gid}); err != nil {
		fail("setgroups", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		fail("setgid", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		fail("setuid", err)
	}
	path, err := exec.LookPath(os.Args[1])
	if err != nil {
		fail("lookpath", err)
	}
	fail("exec", syscall.Exec(path, os.Args[1:], os.Environ()))
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func fail(what string, err error) {
	fmt.Fprintf(os.Stderr, "ksmexec: %s: %v\n", what, err)
	os.Exit(1)
}
