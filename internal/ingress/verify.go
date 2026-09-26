package ingress

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Verifier checks that an inbound webhook genuinely comes from the platform
// before the cell is woken. Verifiers hold verify-only secrets (a header
// value, a signing secret) — never a bot token that could act as the tenant.
// Challenge lets a verifier answer registration-time handshakes on behalf of
// a sleeping cell; it returns handled=true when it wrote the response.
type Verifier interface {
	Verify(r *http.Request, body []byte, secret string) error
	Challenge(w http.ResponseWriter, r *http.Request, body []byte, secret string) (handled bool)
}

// ErrUnverified is returned for requests that fail authentication.
var ErrUnverified = errors.New("webhook not verified")

// maxBody bounds what the ingress will buffer to verify a signature.
const maxBody = 1 << 20

func readSecret(path string) (string, error) {
	if path == "" {
		return "", errors.New("no hook secret file configured")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// verifierFor maps a registry name to an implementation. Unknown names fail
// closed: a misspelt verifier must never mean "accept everything".
func verifierFor(name string) (Verifier, error) {
	switch strings.ToLower(name) {
	case "", "none":
		return nil, nil
	case "telegram":
		return telegramVerifier{}, nil
	case "slack":
		return slackVerifier{now: time.Now}, nil
	case "github":
		return hubSignatureVerifier{}, nil
	case "whatsapp":
		return whatsappVerifier{}, nil
	case "bearer":
		return bearerVerifier{}, nil
	}
	return nil, errors.New("unknown hook verifier " + name)
}

// telegramVerifier: X-Telegram-Bot-Api-Secret-Token must equal the secret
// chosen at setWebhook time.
type telegramVerifier struct{}

func (telegramVerifier) Verify(r *http.Request, _ []byte, secret string) error {
	got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		return ErrUnverified
	}
	return nil
}
func (telegramVerifier) Challenge(http.ResponseWriter, *http.Request, []byte, string) bool {
	return false
}

// slackVerifier: v0=HMAC_SHA256(secret, "v0:"+ts+":"+body) in
// X-Slack-Signature, timestamp within 5 minutes; answers url_verification.
type slackVerifier struct{ now func() time.Time }

func (v slackVerifier) Verify(r *http.Request, body []byte, secret string) error {
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return ErrUnverified
	}
	tsec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrUnverified
	}
	if d := v.now().Unix() - tsec; d > 300 || d < -300 {
		return ErrUnverified // replay window
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return ErrUnverified
	}
	return nil
}
func (v slackVerifier) Challenge(w http.ResponseWriter, r *http.Request, body []byte, secret string) bool {
	var p struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(body, &p) != nil || p.Type != "url_verification" {
		return false
	}
	if v.Verify(r, body, secret) != nil {
		http.Error(w, "unverified", http.StatusUnauthorized)
		return true
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(p.Challenge))
	return true
}

// hubSignatureVerifier: GitHub and Meta style X-Hub-Signature-256: sha256=HMAC(secret, body).
type hubSignatureVerifier struct{}

func (hubSignatureVerifier) Verify(r *http.Request, body []byte, secret string) error {
	sig := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(sig, "sha256=") {
		return ErrUnverified
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return ErrUnverified
	}
	return nil
}
func (hubSignatureVerifier) Challenge(http.ResponseWriter, *http.Request, []byte, string) bool {
	return false
}

// whatsappVerifier: Meta Cloud API. GET with hub.mode=subscribe and
// hub.verify_token equal to the secret must be answered with hub.challenge;
// POSTs carry X-Hub-Signature-256 over the body using the app secret. Both
// share one secret file here; split later if a tenant needs distinct values.
type whatsappVerifier struct{}

func (whatsappVerifier) Verify(r *http.Request, body []byte, secret string) error {
	return hubSignatureVerifier{}.Verify(r, body, secret)
}
func (whatsappVerifier) Challenge(w http.ResponseWriter, r *http.Request, _ []byte, secret string) bool {
	if r.Method != http.MethodGet || r.URL.Query().Get("hub.mode") != "subscribe" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("hub.verify_token")), []byte(secret)) != 1 {
		http.Error(w, "unverified", http.StatusForbidden)
		return true
	}
	_, _ = w.Write([]byte(r.URL.Query().Get("hub.challenge")))
	return true
}

// bearerVerifier: Authorization: Bearer <secret>, for generic senders.
type bearerVerifier struct{}

func (bearerVerifier) Verify(r *http.Request, _ []byte, secret string) error {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		return ErrUnverified
	}
	return nil
}
func (bearerVerifier) Challenge(http.ResponseWriter, *http.Request, []byte, string) bool {
	return false
}

// bufferBody reads and restores the request body so it can be verified and
// then proxied.
func bufferBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, errors.New("body too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	return b, nil
}
