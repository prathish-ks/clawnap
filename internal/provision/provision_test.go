package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prathish-ks/clawnap/internal/registry"
)

type fakeRT struct{ args []string }

func (f *fakeRT) Run(_ context.Context, a ...string) (string, error) { f.args = a; return "cid", nil }

func TestRenderWebhookModeAgreesWithIngress(t *testing.T) {
	r, cfg, err := Render(Spec{Name: "acme", Image: "ghcr.io/openclaw/openclaw:latest", StateRoot: "/srv/cells", Port: 18801, HookPort: 18901,
		IngressURL: "https://fleet.example.com/", TelegramToken: "123:abc"})
	if err != nil {
		t.Fatal(err)
	}
	tg := cfg["channels"].(map[string]any)["telegram"].(map[string]any)
	if tg["webhookUrl"] != "https://fleet.example.com/hook/acme/telegram-webhook" || r.WebhookURL != tg["webhookUrl"] {
		t.Fatalf("webhook url mismatch: %v vs %v", tg["webhookUrl"], r.WebhookURL)
	}
	if tg["webhookSecret"] != r.WebhookSecret || r.WebhookSecret == "" || r.Cell.HookVerifier != "telegram" || r.Cell.HookSecretFile != r.SecretPath || r.Cell.HookPort != 18901 {
		t.Fatalf("secret/verifier wiring wrong: %+v", r.Cell)
	}
	joined := strings.Join(r.RunArgs, " ")
	for _, want := range []string{"--cap-drop ALL", "--security-opt no-new-privileges", "--pids-limit 512", "--memory 1024m", "-p 127.0.0.1:18801:18789", "-p 127.0.0.1:18901:8787", "--label fleet.cell=acme", "--user 1000:1000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "123:abc") || strings.Contains(joined, r.GatewayToken) {
		t.Fatal("neither the bot token nor the gateway token may appear in run args")
	}
	auth := cfg["gateway"].(map[string]any)["auth"].(map[string]any)
	if auth["token"] != r.GatewayToken {
		t.Fatal("gateway token must be written into the config")
	}
}

func TestRenderWebhookWithoutTokenIsRefused(t *testing.T) {
	if _, _, err := Render(Spec{Name: "x", Image: "img", StateRoot: "/x", Port: 1, HookPort: 2, IngressURL: "https://f"}); err == nil {
		t.Fatal("webhook mode without a channel token must be refused, not registered with an empty secret")
	}
}

func TestRenderPollingModeHasNoWebhook(t *testing.T) {
	r, cfg, err := Render(Spec{Name: "p", Image: "img", StateRoot: "/x", Port: 1, TelegramToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	tg := cfg["channels"].(map[string]any)["telegram"].(map[string]any)
	if _, has := tg["webhookUrl"]; has || r.SecretPath != "" || r.Cell.HookVerifier != "" {
		t.Fatalf("polling mode must not configure webhook: %+v %+v", tg, r.Cell)
	}
}

func TestCreateWritesFilesAndRegisters(t *testing.T) {
	root := t.TempDir()
	reg, _ := registry.Open(filepath.Join(root, "cells.json"))
	rt := &fakeRT{}
	r, err := Create(context.Background(), Spec{Name: "c1", Image: "img", StateRoot: root, Port: 18801, HookPort: 18901, IngressURL: "https://f.example", TelegramToken: "tok", IdleAfter: 5 * time.Minute}, rt, reg)
	if err != nil {
		t.Fatal(err)
	}
	cfgB, _ := os.ReadFile(r.ConfigPath)
	if !strings.Contains(string(cfgB), `"webhookSecret"`) || !strings.Contains(string(cfgB), `"mode": "local"`) {
		t.Fatalf("config wrong:\n%s", cfgB)
	}
	sec, _ := os.ReadFile(r.SecretPath)
	if !strings.Contains(string(cfgB), strings.TrimSpace(string(sec))) {
		t.Fatal("secret file and config secret differ")
	}
	if fi, _ := os.Stat(r.SecretPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret perms %v", fi.Mode().Perm())
	}
	c, err := reg.Get("c1")
	if err != nil || c.HookVerifier != "telegram" || c.IdleAfter.Minutes() != 5 {
		t.Fatalf("registry: %+v %v", c, err)
	}
	if rt.args[0] != "run" {
		t.Fatalf("container not run: %v", rt.args)
	}
	if _, err := Create(context.Background(), Spec{Name: "c1", Image: "img", StateRoot: root, Port: 18801}, rt, reg); err == nil {
		t.Fatal("must refuse to overwrite an existing cell")
	}
}
