package ingress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/registry"
)

func setup(t *testing.T, verifier string) (*httptest.Server, *fakeWaker, *httptest.Server) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(backend.Close)
	port, _ := strconv.Atoi(strings.TrimPrefix(backend.URL, "http://127.0.0.1:"))
	sf := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(sf, []byte("s3cret\n"), 0o600)
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: port, HookVerifier: verifier, HookSecretFile: sf})
	fw := &fakeWaker{}
	srv := httptest.NewServer((&Server{Reg: reg, Waker: fw}).Handler())
	t.Cleanup(srv.Close)
	return srv, fw, backend
}

func TestTelegramHeaderSecretGatesWake(t *testing.T) {
	srv, fw, _ := setup(t, "telegram")
	req, _ := http.NewRequest("POST", srv.URL+"/hook/a/telegram-webhook", strings.NewReader(`{"update_id":1}`))
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 401 || len(fw.woke) != 0 {
		t.Fatalf("missing header must be rejected without wake: %d woke=%v", res.StatusCode, fw.woke)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/hook/a/telegram-webhook", strings.NewReader(`{"update_id":1}`))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 401 || len(fw.woke) != 0 {
		t.Fatalf("wrong secret must be rejected without wake")
	}
	req, _ = http.NewRequest("POST", srv.URL+"/hook/a/telegram-webhook", strings.NewReader(`{"update_id":1}`))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s3cret")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 204 || len(fw.woke) != 1 {
		t.Fatalf("correct secret must wake and proxy: %d woke=%v", res.StatusCode, fw.woke)
	}
}

func TestSlackSignatureAndChallenge(t *testing.T) {
	srv, fw, _ := setup(t, "slack")
	sign := func(ts, body string) string {
		m := hmac.New(sha256.New, []byte("s3cret"))
		m.Write([]byte("v0:" + ts + ":" + body))
		return "v0=" + hex.EncodeToString(m.Sum(nil))
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	// url_verification is answered without waking
	body := `{"type":"url_verification","challenge":"abc123"}`
	req, _ := http.NewRequest("POST", srv.URL+"/hook/a/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sign(ts, body))
	res, _ := http.DefaultClient.Do(req)
	b := make([]byte, 16)
	n, _ := res.Body.Read(b)
	if res.StatusCode != 200 || string(b[:n]) != "abc123" || len(fw.woke) != 0 {
		t.Fatalf("challenge: %d %q woke=%v", res.StatusCode, b[:n], fw.woke)
	}
	// a real event with a valid signature wakes
	body = `{"type":"event_callback","event":{"type":"message"}}`
	req, _ = http.NewRequest("POST", srv.URL+"/hook/a/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sign(ts, body))
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 204 || len(fw.woke) != 1 {
		t.Fatalf("valid event: %d woke=%v", res.StatusCode, fw.woke)
	}
	// stale timestamp is a replay
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	req, _ = http.NewRequest("POST", srv.URL+"/hook/a/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", old)
	req.Header.Set("X-Slack-Signature", sign(old, body))
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 401 || len(fw.woke) != 1 {
		t.Fatalf("replay must be rejected: %d woke=%v", res.StatusCode, fw.woke)
	}
}

func TestGitHubHubSignature(t *testing.T) {
	srv, fw, _ := setup(t, "github")
	body := `{"action":"opened"}`
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte(body))
	req, _ := http.NewRequest("POST", srv.URL+"/hook/a/github", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 204 || len(fw.woke) != 1 {
		t.Fatalf("valid signature: %d woke=%v", res.StatusCode, fw.woke)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/hook/a/github", strings.NewReader(body+" "))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 401 || len(fw.woke) != 1 {
		t.Fatalf("tampered body must be rejected")
	}
}

func TestWhatsAppVerifyHandshake(t *testing.T) {
	srv, fw, _ := setup(t, "whatsapp")
	res, _ := http.Get(srv.URL + "/hook/a/wa?hub.mode=subscribe&hub.verify_token=s3cret&hub.challenge=777")
	b := make([]byte, 8)
	n, _ := res.Body.Read(b)
	if res.StatusCode != 200 || string(b[:n]) != "777" || len(fw.woke) != 0 {
		t.Fatalf("handshake: %d %q woke=%v", res.StatusCode, b[:n], fw.woke)
	}
	res, _ = http.Get(srv.URL + "/hook/a/wa?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=777")
	if res.StatusCode != 403 {
		t.Fatalf("wrong verify token must be 403, got %d", res.StatusCode)
	}
}

func TestUnknownVerifierFailsClosed(t *testing.T) {
	srv, fw, _ := setup(t, "telegramm")
	res, _ := http.Post(srv.URL+"/hook/a/x", "application/json", strings.NewReader("{}"))
	if res.StatusCode != 502 || len(fw.woke) != 0 {
		t.Fatalf("misspelt verifier must not accept: %d woke=%v", res.StatusCode, fw.woke)
	}
}
