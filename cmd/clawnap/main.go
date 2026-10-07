// Command clawnap is the fleet supervisor CLI and daemon.
//
//	clawnap cells add -name a -container openclaw-a -port 18801 [-class hibernate|always-on] [-tier pause|stop] [-idle 10m]
//	clawnap cells create -name a -port 18801 [-hook-port 18901 -ingress-url https://fleet.example] [-telegram-token-file f]
//	clawnap cells list | rm -name a
//	clawnap reconcile [-loop] [-interval 30s]
//	clawnap hibernate -name a | wake -name a | prefetch -name a   (page a reclaimed cell back in, Linux)
//	clawnap serve -listen 127.0.0.1:8080 [-token X]     (ingress + reconcile loop; POST /wake/{cell}, /hibernate/{cell}, /hook/{cell}/..., GET /metrics)
//	clawnap check [-label fleet.cell] [-json] [container...]   read-only host + cell security inspection
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prathish-ks/clawnap/internal/hostcheck"
	"github.com/prathish-ks/clawnap/internal/ingress"
	"github.com/prathish-ks/clawnap/internal/provision"
	"github.com/prathish-ks/clawnap/internal/reclaim"
	"github.com/prathish-ks/clawnap/internal/registry"
	"github.com/prathish-ks/clawnap/internal/runtime"
	"github.com/prathish-ks/clawnap/internal/supervisor"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "clawnap:", err)
		os.Exit(1)
	}
}

// dataDir holds the registry and, under cells/, every cell's state. As root
// it is the FHS service location /var/lib/clawnap: cell state is a bind mount
// into a tenant container and the launcher refuses mounts under /root, so a
// root-run daemon must not default to its own home. A registry left in the
// old /root/.fleetd location is read once and migrated.
func dataDir() string {
	if d := envOr("CLAWNAP_DATA", "FLEETD_DATA"); d != "" {
		return d
	}
	if os.Geteuid() == 0 {
		return dataRoot()
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".fleetd")
}

func cellsRoot() string { return filepath.Join(dataDir(), "cells") }

func newRunner() runtime.ExecRunner {
	return runtime.ExecRunner{Binary: envOr("CLAWNAP_RUNTIME", "FLEETD_RUNTIME")}
}

func openRegistry() (*registry.Store, error) {
	p := filepath.Join(dataDir(), "cells.json")
	if os.Geteuid() == 0 && envOr("CLAWNAP_DATA", "FLEETD_DATA") == "" {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			old := "/root/.fleetd/cells.json"
			if b, err := os.ReadFile(old); err == nil {
				_ = os.MkdirAll(filepath.Dir(p), 0o700)
				if os.WriteFile(p, b, 0o600) == nil {
					_ = os.Rename(old, old+".migrated")
				}
			}
		}
	}
	return registry.Open(p)
}

// viaDaemon sends a wake or hibernate to a running daemon's ingress when one
// is reachable, so the CLI never runs a second supervisor that the daemon's
// reclaim loop cannot see. Returns handled=false when no daemon answers.
func viaDaemon(ctx context.Context, verb, name string) (handled bool, out string, err error) {
	addr := envOr("CLAWNAP_INGRESS", "FLEETD_INGRESS")
	if addr == "" {
		addr = "http://127.0.0.1:8080"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/"+verb+"/"+name, nil)
	if err != nil {
		return false, "", err
	}
	if t := envOr("CLAWNAP_TOKEN", "FLEETD_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	res, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		// Only "nothing is listening" means there is no daemon. Any other
		// failure (Ctrl-C, a timeout, a reset mid-response) leaves a daemon
		// that may still be acting on the cell, and a second supervisor in
		// this process would race it on the same container.
		if ctx.Err() != nil {
			return true, "", ctx.Err()
		}
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" {
			return false, "", nil // no daemon: fall back to local
		}
		return true, "", fmt.Errorf("daemon %s: %w (not retrying locally: the daemon may still be acting on the cell)", verb, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		return true, "", fmt.Errorf("daemon %s: %s %s", verb, res.Status, strings.TrimSpace(string(b)))
	}
	return true, res.Header.Get("X-Wake-Ready-Ms"), nil
}

func open(opt supervisor.Options) (*registry.Store, *supervisor.Supervisor, error) {
	reg, err := openRegistry()
	if err != nil {
		return nil, nil, err
	}
	return reg, supervisor.New(reg, runtime.Client{R: newRunner()}, opt), nil
}

// loopFlags registers the reconcile-loop options once, for every subcommand
// that runs the loop, so they cannot drift apart.
func loopFlags(fs *flag.FlagSet) func() supervisor.Options {
	interval := fs.Duration("interval", 30*time.Second, "reconcile interval")
	reclaimAfter := fs.Duration("reclaim-after", 0, "push a paused cell's memory to swap after it has been paused this long (Linux cgroup v2; 0 = off)")
	maxPause := fs.Duration("max-pause", 20*time.Minute, "pulse (or stop) a paused cell frozen longer than this; negative = never")
	fallthrough_ := fs.String("pause-fallthrough", "pulse", "pulse|stop: what to do at -max-pause")
	reclaimKeep := fs.Int64("reclaim-keep-mib", 0, "resident floor kept in RAM when reclaiming (0 = reclaim everything; ~150 keeps OpenClaw's working set)")
	prefetch := fs.Bool("prefetch-on-wake", false, "page a reclaimed cell's memory back in bulk before unpausing it")
	warmKeep := fs.Int64("warm-keep-mib", 0, "trim a paused cell to this resident floor at once and record its hot set; the cold floor (-reclaim-keep-mib) applies later; 0 = single-stage reclaim")
	wakeConc := fs.Int("wake-concurrency", 4, "simultaneous page-ins/starts; readiness waits and the thaw settle run outside this bound")
	settle := fs.Duration("thaw-settle", time.Second, "bound on the hold after a pause wake while the gateway finishes its own post-thaw recovery (keyed on the cell log; measured window 22–800 ms)")
	maxRecov := fs.Int("max-recovering", 8, "cells allowed between unpause and ready at once (post-thaw recovery is CPU-bound)")
	idleCPU := fs.Float64("idle-cpu-pct", 10, "a running cell using more CPU than this (percent of one core) is not idle, whatever its traffic; 0 = default, negative = ignore CPU")
	minAwake := fs.Duration("min-awake", 3*time.Minute, "never hibernate a cell within this long of its last wake (OpenClaw's post-thaw maintenance must finish); 0 = default, negative = off")
	maintain := fs.Duration("maintain-every", 0, "maintenance rotation: wake the longest-unwoken hibernated cell on a pace derived from the fleet, so every cell is reached at least this often and runs its own internal schedule (memory consolidation, weekly review, one heartbeat turn) even when nothing is sent to it; yields to real wakes and to the headroom policy; 0 = off")
	maintainN := fs.Int("maintain-concurrent", 1, "cells the maintenance rotation may hold awake at once; a maintenance wake lasts until the cell's catch-up finishes and its idle timeout elapses, so this, not the pace, is what bounds the cost (~0.8 GB per cell)")
	maintainIdle := fs.Duration("maintain-idle", 30*time.Second, "idle timeout for a cell the rotation woke, instead of its own: a maintenance wake has no tenant to wait for, so the cell sleeps as soon as it goes quiet (-min-awake and the CPU gate still apply). A real message reverts it to the cell's own timeout; negative = off")
	readSched := fs.Bool("read-schedules", false, "read each cell's own scheduled jobs from its state directory when it hibernates, and wake it before the ones a person is waiting for (one-shots and jobs that deliver somewhere); the cell's internal maintenance is left to catch up on its next wake. Off by default: it reads the gateway's internal store, and an upstream schema change falls back to the operator-set -next-due")
	burst := fs.Int("burst-target", 0, "cold wakes to absorb at full speed at once; sets -headroom-mib to 800 MiB per wake when that flag is not given (measured: a woken cell holds ~800 MiB through its post-thaw window)")
	headroom := fs.Int64("headroom-mib", 0, "keep at least this much MemAvailable by reclaiming the longest-paused resident cells first (0 = timed reclaim only)")
	return func() supervisor.Options {
		head := *headroom
		if head == 0 && *burst > 0 {
			head = int64(*burst) * 800
		}
		return supervisor.Options{Interval: *interval, ReclaimAfter: *reclaimAfter, MaxPause: *maxPause, PauseFallthrough: *fallthrough_,
			ReclaimKeep: *reclaimKeep << 20, PrefetchOnWake: *prefetch, MaxConcurrent: *wakeConc, MaxRecovering: *maxRecov, IdleCPUPct: *idleCPU, MinAwake: *minAwake, MaintainEvery: *maintain, MaintainConcurrent: *maintainN, MaintainIdle: *maintainIdle, ReadSchedules: *readSched, Headroom: head << 20, ThawSettle: *settle, WarmKeep: *warmKeep << 20, HotSetDir: filepath.Join(dataDir(), "hotsets")}
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: clawnap <cells|reconcile|hibernate|wake|prefetch|serve|check|version> ...")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "version":
		fmt.Println("clawnap", version)
		return nil
	case "cells":
		return cells(args[1:])
	case "reconcile":
		fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
		loop := fs.Bool("loop", false, "run continuously")
		opts := loopFlags(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		_, sup, err := open(opts())
		if err != nil {
			return err
		}
		if !*loop {
			sup.ReconcileOnce(ctx)
			return nil
		}
		return sup.Run(ctx)
	case "prefetch":
		fs := flag.NewFlagSet("prefetch", flag.ContinueOnError)
		name := fs.String("name", "", "cell name")
		files := fs.Bool("files", false, "also prefetch file-backed mappings (binary, bundles); measured slower on a disk swap file, default off")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		reg, err := openRegistry()
		if err != nil {
			return err
		}
		c, err := reg.Get(*name)
		if err != nil {
			return err
		}
		id, err := (runtime.Client{R: newRunner()}).ID(ctx, c.Container)
		if err != nil {
			return err
		}
		st, err := (&reclaim.Reclaimer{}).PrefetchMappings(ctx, id, 0, *files)
		fmt.Printf("mechanism=%s procs=%d mappings=%d advised_mib=%d swap_before_mib=%d swap_after_mib=%d took=%s\n", st.Mechanism, st.Processes, st.Mappings, st.Bytes>>20, st.SwapBefore>>20, st.SwapAfter>>20, st.Took)
		return err
	case "hibernate", "wake":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		name := fs.String("name", "", "cell name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if handled, ready, err := viaDaemon(ctx, args[0], *name); handled {
			if err != nil {
				return err
			}
			if ready != "" {
				fmt.Printf("via daemon: ready_ms=%s\n", ready)
			} else {
				fmt.Println("via daemon: ok")
			}
			return nil
		}
		_, sup, err := open(supervisor.Options{})
		if err != nil {
			return err
		}
		if args[0] == "hibernate" {
			return sup.Hibernate(ctx, *name)
		}
		res, err := sup.Wake(ctx, *name)
		if err != nil {
			return err
		}
		fmt.Printf("already_running=%v start=%s ready=%s\n", res.AlreadyRunning, res.StartTook, res.ReadyTook)
		return nil
	case "check":
		fs := flag.NewFlagSet("check", flag.ContinueOnError)
		label := fs.String("label", "fleet.cell", "inspect containers carrying this label")
		jsonOut := fs.Bool("json", false, "machine-readable output")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rt := newRunner()
		hostExec := func(ctx context.Context, name string, a ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, a...).CombinedOutput()
			return string(out), err
		}
		res := hostcheck.Run(ctx, rt, hostcheck.Options{Label: *label, Containers: fs.Args(), HostExec: hostExec})
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(res)
		}
		for _, r := range res {
			mark := map[hostcheck.Level]string{hostcheck.LevelPass: "PASS", hostcheck.LevelWarn: "WARN", hostcheck.LevelFail: "FAIL"}[r.Level]
			scope := "host"
			if r.Cell != "" {
				scope = r.Cell
			}
			fmt.Printf("%-4s %-14s %-44s %s\n", mark, scope, r.Name, r.Detail)
			if r.Remediation != "" && r.Level != hostcheck.LevelPass {
				fmt.Printf("     %-14s %-44s fix: %s\n", "", "", r.Remediation)
			}
		}
		p, w, f := hostcheck.Summary(res)
		fmt.Printf("\n%d pass, %d warn, %d fail\n", p, w, f)
		if f > 0 {
			os.Exit(2)
		}
		return nil
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		listen := fs.String("listen", "127.0.0.1:8080", "ingress listen address")
		token := fs.String("token", envOr("CLAWNAP_TOKEN", "FLEETD_TOKEN"), "bearer token for /wake and /hibernate (required unless listening on loopback)")
		opts := loopFlags(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *token == "" && !isLoopback(*listen) {
			return fmt.Errorf("refusing to serve /wake and /hibernate unauthenticated on %s: pass -token or FLEETD_TOKEN", *listen)
		}
		reg, sup, err := open(opts())
		if err != nil {
			return err
		}
		go func() { _ = sup.Run(ctx) }()
		metrics := func(w http.ResponseWriter) { sup.Metrics().Write(w, sup.Cells()) }
		srv := &http.Server{Addr: *listen, Handler: (&ingress.Server{Reg: reg, Waker: wakeAdapter{sup}, Token: *token, Metrics: metrics}).Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
		slog.Info("clawnap serving", "listen", *listen)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		// Let wakes and reconcile actions already in flight finish before
		// the process goes: a cell left half-thawed is adopted on restart,
		// but the tenant whose message triggered the wake is waiting now.
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := sup.Drain(dctx); err != nil {
			slog.Warn("shutdown: actions still in flight", "err", err)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type wakeAdapter struct{ s *supervisor.Supervisor }

func (w wakeAdapter) Wake(ctx context.Context, cell string) (time.Duration, error) {
	r, err := w.s.Wake(ctx, cell)
	return r.ReadyTook, err
}

func (w wakeAdapter) Hibernate(ctx context.Context, cell string) error {
	return w.s.Hibernate(ctx, cell)
}

func cells(args []string) error {
	ctx := context.Background()
	if len(args) == 0 {
		return fmt.Errorf("usage: clawnap cells <add|create|list|rm>")
	}
	reg, err := openRegistry()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("add", flag.ContinueOnError)
		name := fs.String("name", "", "cell name")
		container := fs.String("container", "", "container name")
		port := fs.Int("port", 0, "loopback gateway port (readiness probe)")
		hookPort := fs.Int("hook-port", 0, "loopback webhook listener port (0 = same as -port)")
		hookVerifier := fs.String("hook-verifier", "", "telegram|slack|github|whatsapp|bearer, or 'none' to accept unverified hooks; unset = /hook refused for this cell")
		hookSecretFile := fs.String("hook-secret-file", "", "file holding the verify-only webhook secret (never a bot token)")
		class := fs.String("class", "hibernate", "hibernate|always-on")
		tier := fs.String("tier", "pause", "pause|stop (how a hibernate-class cell sleeps)")
		idle := fs.Duration("idle", 10*time.Minute, "idle timeout before hibernation")
		due := fs.String("next-due", "", "RFC3339 time of the cell's next scheduled job (interim cron-aware wake)")
		dueEvery := fs.Duration("next-due-every", 0, "recurrence for -next-due (0 = one-shot, cleared after it fires)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var nextDue time.Time
		if *due != "" {
			t, err := time.Parse(time.RFC3339, *due)
			if err != nil {
				return fmt.Errorf("-next-due: %w", err)
			}
			nextDue = t
		}
		return reg.Put(registry.Cell{Name: *name, Container: *container, Port: *port, HookPort: *hookPort, HookVerifier: *hookVerifier, HookSecretFile: *hookSecretFile, Class: registry.Class(*class), Tier: registry.Tier(*tier), IdleAfter: *idle, NextDueAt: nextDue, NextDueEvery: *dueEvery})
	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		name := fs.String("name", "", "cell name (plain identifier)")
		image := fs.String("image", "ghcr.io/openclaw/openclaw:latest", "cell image")
		root := fs.String("state-root", cellsRoot(), "directory holding <name>/{state,auth,secrets} (must not be under /root, /etc or other system trees)")
		port := fs.Int("port", 0, "host loopback port for the gateway")
		hookPort := fs.Int("hook-port", 0, "host loopback port for the webhook listener (needs -ingress-url)")
		ingressURL := fs.String("ingress-url", "", "public base URL of the ingress; empty = polling mode")
		tokenFile := fs.String("telegram-token-file", "", "file containing the Telegram bot token (written into the cell config only)")
		tier := fs.String("tier", "pause", "pause|stop")
		idle := fs.Duration("idle", 10*time.Minute, "idle timeout before hibernation")
		mem := fs.Int("memory-mib", 1024, "memory limit")
		pids := fs.Int("pids", 512, "pids limit")
		user := fs.String("user", "1000:1000", "uid:gid the cell runs as; state dirs are owned by it")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var tok string
		if *tokenFile != "" {
			b, err := os.ReadFile(*tokenFile)
			if err != nil {
				return err
			}
			tok = strings.TrimSpace(string(b))
		}
		res, err := provision.Create(ctx, provision.Spec{Name: *name, Image: *image, StateRoot: *root, Port: *port, HookPort: *hookPort,
			IngressURL: *ingressURL, TelegramToken: tok, Tier: registry.Tier(*tier), IdleAfter: *idle, MemMiB: *mem, Pids: *pids, User: *user}, newRunner(), reg)
		if err != nil {
			return err
		}
		fmt.Printf("created cell %s\n  state:   %s\n  config:  %s\n", *name, res.StateDir, res.ConfigPath)
		if res.WebhookURL != "" {
			fmt.Printf("  webhook: %s (verifier telegram, secret %s)\n", res.WebhookURL, res.SecretPath)
		} else {
			fmt.Println("  channel: polling mode (no -ingress-url)")
		}
		fmt.Printf("  gateway: http://127.0.0.1:%d  token: %s\n", *port, res.GatewayToken)
		return nil
	case "rm":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		name := fs.String("name", "", "cell name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return reg.Delete(*name)
	case "list":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reg.List())
	}
	return fmt.Errorf("unknown cells command %q", args[0])
}

// version is stamped by the release workflow (-X main.version=vX.Y.Z).
var version = "dev"

// envOr reads the first set variable: the clawnap name, then the pre-rename
// FLEETD_* name so an existing host keeps working until it is redeployed.
func envOr(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// dataRoot is /var/lib/clawnap, or /var/lib/fleetd when only that exists
// (hosts set up before the rename).
func dataRoot() string {
	if _, err := os.Stat("/var/lib/clawnap"); err == nil {
		return "/var/lib/clawnap"
	}
	if _, err := os.Stat("/var/lib/fleetd"); err == nil {
		return "/var/lib/fleetd"
	}
	return "/var/lib/clawnap"
}
