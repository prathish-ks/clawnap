// Package provision creates an OpenClaw cell the supervisor can manage:
// state directories, a minimal openclaw.json (gateway.mode local, token auth,
// optional Telegram channel in webhook mode pointing at the ingress), the
// verify-only webhook secret file, the hardened container, and the registry
// entry. It exists so the cell's webhook URL/secret and the ingress's
// verifier can never disagree: one command writes both.
package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prathish-ks/clawnap/internal/registry"
	"github.com/prathish-ks/clawnap/internal/spec"
)

// Spec describes the cell to create.
type Spec struct {
	Name          string
	Image         string // e.g. ghcr.io/openclaw/openclaw:latest
	StateRoot     string // cells live at <StateRoot>/<Name>/{state,auth,secrets}
	Port          int    // host loopback port -> container 18789
	HookPort      int    // host loopback port -> container webhook listener (0 = no webhook mode)
	IngressURL    string // public base URL of the ingress, e.g. https://fleet.example.com (empty = polling mode)
	TelegramToken string // bot token; written into the cell's config only, never the registry
	Tier          registry.Tier
	IdleAfter     time.Duration
	MemMiB        int
	Pids          int
	Workspace     string // container path; default /home/node/.openclaw/workspace
	// User is the uid:gid the cell runs as (the official image's node user
	// is 1000:1000). State directories are created for that uid so a
	// root-run fleetd on Linux does not hand the container an unwritable
	// 0700 root-owned mount.
	User string
}

// Result reports what was created.
type Result struct {
	StateDir      string
	AuthDir       string
	ConfigPath    string
	SecretPath    string // verify-only webhook secret (empty in polling mode)
	WebhookSecret string // the same value, for Create to write; never logged
	WebhookURL    string
	GatewayToken  string // written into the cell's openclaw.json (gateway.auth.token), returned once
	RunArgs       []string
	Cell          registry.Cell
}

const (
	gatewayPort = 18789
	webhookPort = 8787
	webhookPath = "/telegram-webhook"
	statePathIn = "/home/node/.openclaw"
	authPathIn  = "/home/node/.config/openclaw"
)

// Render computes everything without touching the host: config JSON, run
// args, registry entry. Create writes and runs it.
func Render(s Spec) (Result, map[string]any, error) {
	if s.Name == "" || s.Image == "" || s.StateRoot == "" || s.Port == 0 {
		return Result{}, nil, errors.New("name, image, state-root and port are required")
	}
	if strings.ContainsAny(s.Name, "/\\ .") {
		return Result{}, nil, errors.New("name must be a plain identifier")
	}
	if s.MemMiB == 0 {
		s.MemMiB = 1024
	}
	if s.Pids == 0 {
		s.Pids = 512
	}
	if s.Tier == "" {
		s.Tier = registry.TierPause
	}
	if s.Workspace == "" {
		s.Workspace = statePathIn + "/workspace"
	}
	if s.User == "" {
		s.User = "1000:1000"
	}
	webhook := s.IngressURL != "" && s.HookPort != 0
	if webhook && s.TelegramToken == "" {
		return Result{}, nil, errors.New("webhook mode (-ingress-url with -hook-port) needs a channel to register the webhook for: pass -telegram-token-file, or drop -hook-port for polling mode")
	}
	base := filepath.Join(s.StateRoot, s.Name)
	r := Result{StateDir: filepath.Join(base, "state"), AuthDir: filepath.Join(base, "auth")}
	r.ConfigPath = filepath.Join(r.StateDir, "openclaw.json")
	r.GatewayToken = randomHex(16)

	// The gateway token lives in the 0600 config inside the cell's own
	// state dir, never in argv or env, so `ps`, failed-run error strings
	// and `fleetd check` never see it.
	cfg := map[string]any{
		"gateway": map[string]any{"mode": "local", "bind": "auto", "auth": map[string]any{"mode": "token", "token": r.GatewayToken}},
		"agents":  map[string]any{"defaults": map[string]any{"workspace": s.Workspace}},
	}
	channels := map[string]any{}
	if s.TelegramToken != "" {
		tg := map[string]any{"botToken": s.TelegramToken}
		if webhook {
			r.WebhookSecret = randomHex(24)
			r.SecretPath = filepath.Join(base, "secrets", "telegram-webhook-secret")
			r.WebhookURL = strings.TrimRight(s.IngressURL, "/") + "/hook/" + s.Name + webhookPath
			tg["webhookUrl"] = r.WebhookURL
			tg["webhookSecret"] = r.WebhookSecret
			tg["webhookHost"] = "0.0.0.0"
			tg["webhookPort"] = webhookPort
			tg["webhookPath"] = webhookPath
		}
		channels["telegram"] = tg
	}
	if len(channels) > 0 {
		cfg["channels"] = channels
	}

	uid, gid, err := parseUser(s.User)
	if err != nil {
		return Result{}, nil, err
	}
	cs := spec.ContainerSpec{Image: s.Image, UID: uid, GID: gid, UserSet: true, PidsLimit: s.Pids, MemBytes: int64(s.MemMiB) << 20,
		Mounts: []spec.Mount{{HostPath: r.StateDir, ContainerPath: statePathIn}, {HostPath: r.AuthDir, ContainerPath: authPathIn}}}
	if err := spec.Validate(cs); err != nil {
		return Result{}, nil, err
	}
	r.RunArgs = []string{"run", "-d", "--name", s.Name, "--label", "fleet.cell=" + s.Name,
		"--user", s.User,
		"-v", r.StateDir + ":" + statePathIn, "-v", r.AuthDir + ":" + authPathIn,
		"-p", "127.0.0.1:" + strconv.Itoa(s.Port) + ":" + strconv.Itoa(gatewayPort),
		"--pids-limit", strconv.Itoa(s.Pids), "--memory", strconv.Itoa(s.MemMiB) + "m",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--restart", "no"}
	if webhook {
		r.RunArgs = append(r.RunArgs, "-p", "127.0.0.1:"+strconv.Itoa(s.HookPort)+":"+strconv.Itoa(webhookPort))
	}
	r.RunArgs = append(r.RunArgs, s.Image)

	r.Cell = registry.Cell{Name: s.Name, Container: s.Name, Port: s.Port, Tier: s.Tier, Class: registry.ClassHibernate, IdleAfter: s.IdleAfter}
	if webhook {
		r.Cell.HookPort = s.HookPort
		r.Cell.HookVerifier = "telegram"
		r.Cell.HookSecretFile = r.SecretPath
	}
	return r, cfg, nil
}

// Runner runs the container runtime CLI.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Create writes the directories, config and secret, runs the container and
// registers the cell. Refuses to overwrite an existing state dir.
func Create(ctx context.Context, s Spec, rt Runner, reg *registry.Store) (Result, error) {
	r, cfg, err := Render(s)
	if err != nil {
		return r, err
	}
	if _, err := os.Stat(r.ConfigPath); err == nil {
		return r, fmt.Errorf("cell %s already exists at %s", s.Name, r.StateDir)
	}
	uid, gid, _ := parseUser(s.User)
	for _, d := range []string{r.StateDir, r.AuthDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return r, err
		}
		if err := chownIfRoot(d, uid, gid); err != nil {
			return r, err
		}
	}
	if r.WebhookSecret != "" {
		if err := os.MkdirAll(filepath.Dir(r.SecretPath), 0o700); err != nil {
			return r, err
		}
		if err := os.WriteFile(r.SecretPath, []byte(r.WebhookSecret+"\n"), 0o600); err != nil {
			return r, err
		}
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(r.ConfigPath, append(b, '\n'), 0o600); err != nil {
		return r, err
	}
	if err := chownIfRoot(r.ConfigPath, uid, gid); err != nil {
		return r, err
	}
	if _, err := rt.Run(ctx, r.RunArgs...); err != nil {
		return r, fmt.Errorf("run container: %w", err)
	}
	return r, reg.Put(r.Cell)
}

func parseUser(u string) (int, int, error) {
	us, gs, ok := strings.Cut(u, ":")
	if !ok {
		return 0, 0, fmt.Errorf("user must be uid:gid, got %q", u)
	}
	uid, err1 := strconv.Atoi(us)
	gid, err2 := strconv.Atoi(gs)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("user must be numeric uid:gid, got %q", u)
	}
	return uid, gid, nil
}

// chownIfRoot gives the cell's uid ownership of a path when fleetd itself
// runs as root (Linux fleet hosts); as an unprivileged user the caller's
// own ownership is what the bind mount will carry, and Docker Desktop
// remaps it anyway.
func chownIfRoot(path string, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
