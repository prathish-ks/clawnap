// Command fleetd is the fleet supervisor CLI and daemon.
//
//	fleetd cells add -name a -container openclaw-a -port 18801 [-class hibernate|always-on] [-tier pause|stop] [-idle 10m]
//	fleetd cells list | rm -name a
//	fleetd reconcile [-loop] [-interval 30s]
//	fleetd hibernate -name a | wake -name a
//	fleetd serve -listen 127.0.0.1:8080 [-token X]     (ingress + reconcile loop)
//	fleetd check [-label fleet.cell] [-json] [container...]   read-only host + cell security inspection
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/hostcheck"
	"github.com/prathish-ks/fleet-supervisor/internal/ingress"
	"github.com/prathish-ks/fleet-supervisor/internal/registry"
	"github.com/prathish-ks/fleet-supervisor/internal/runtime"
	"github.com/prathish-ks/fleet-supervisor/internal/supervisor"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fleetd:", err)
		os.Exit(1)
	}
}

func dataDir() string {
	if d := os.Getenv("FLEETD_DATA"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".fleetd")
}

func open(opt supervisor.Options) (*registry.Store, *supervisor.Supervisor, error) {
	reg, err := registry.Open(filepath.Join(dataDir(), "cells.json"))
	if err != nil {
		return nil, nil, err
	}
	bin := os.Getenv("FLEETD_RUNTIME")
	rt := runtime.Client{R: runtime.ExecRunner{Binary: bin}}
	return reg, supervisor.New(reg, rt, opt), nil
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleetd <cells|reconcile|hibernate|wake|serve> ...")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "cells":
		return cells(args[1:])
	case "reconcile":
		fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
		loop := fs.Bool("loop", false, "run continuously")
		interval := fs.Duration("interval", 30*time.Second, "sampling interval")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		_, sup, err := open(supervisor.Options{Interval: *interval})
		if err != nil {
			return err
		}
		if !*loop {
			sup.ReconcileOnce(ctx)
			return nil
		}
		return sup.Run(ctx)
	case "hibernate", "wake":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		name := fs.String("name", "", "cell name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
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
		rt := runtime.ExecRunner{Binary: os.Getenv("FLEETD_RUNTIME")}
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
		token := fs.String("token", os.Getenv("FLEETD_TOKEN"), "bearer token for /wake")
		maxPause := fs.Duration("max-pause", 20*time.Minute, "pulse (or stop) a paused cell frozen longer than this")
		fallthrough_ := fs.String("pause-fallthrough", "pulse", "pulse|stop: what to do at -max-pause")
		interval := fs.Duration("interval", 30*time.Second, "reconcile interval")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		reg, sup, err := open(supervisor.Options{MaxPause: *maxPause, PauseFallthrough: *fallthrough_, Interval: *interval})
		if err != nil {
			return err
		}
		go func() { _ = sup.Run(ctx) }()
		metrics := func(w http.ResponseWriter) { sup.Metrics().Write(w, sup.Cells()) }
		srv := &http.Server{Addr: *listen, Handler: (&ingress.Server{Reg: reg, Waker: wakeAdapter{sup}, Token: *token, Metrics: metrics}).Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
		slog.Info("fleetd serving", "listen", *listen)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

type wakeAdapter struct{ s *supervisor.Supervisor }

func (w wakeAdapter) Wake(ctx context.Context, cell string) (time.Duration, error) {
	r, err := w.s.Wake(ctx, cell)
	return r.ReadyTook, err
}

func cells(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleetd cells <add|list|rm>")
	}
	reg, _, err := open(supervisor.Options{})
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
		hookVerifier := fs.String("hook-verifier", "", "none|telegram|slack|github|whatsapp|bearer (verify inbound webhooks before waking)")
		hookSecretFile := fs.String("hook-secret-file", "", "file holding the verify-only webhook secret (never a bot token)")
		class := fs.String("class", "hibernate", "hibernate|always-on")
		tier := fs.String("tier", "pause", "pause|stop (how a hibernate-class cell sleeps)")
		idle := fs.Duration("idle", 10*time.Minute, "idle timeout before hibernation")
		due := fs.String("next-due", "", "RFC3339 time of the cell's next scheduled job (interim cron-aware wake)")
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
		return reg.Put(registry.Cell{Name: *name, Container: *container, Port: *port, HookPort: *hookPort, HookVerifier: *hookVerifier, HookSecretFile: *hookSecretFile, Class: registry.Class(*class), Tier: registry.Tier(*tier), IdleAfter: *idle, NextDueAt: nextDue})
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
