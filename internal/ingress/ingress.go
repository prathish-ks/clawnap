// Package ingress is the shared, always-on front door for hibernated cells.
// Chat platforms that deliver by webhook (Telegram, Slack Events API,
// generic HTTP) point at this server instead of at the cell; the server
// wakes the cell, then forwards the request to the cell's loopback port.
// Platforms that need a persistent socket (Discord gateway, WhatsApp) are
// handled by keepers or the always-on class, not here.
package ingress

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prathish-ks/clawnap/internal/registry"
)

// Waker is what the ingress needs from the supervisor.
type Waker interface {
	Wake(ctx context.Context, cell string) (readyTook time.Duration, err error)
}

// Hibernator is optional: when the Waker also implements it, the ingress
// serves POST /hibernate/{cell} (token-protected) for operators and tests.
type Hibernator interface {
	Hibernate(ctx context.Context, cell string) error
}

// hookReadyTimeout bounds the wait for a cell's webhook listener after a
// wake; the platform's own delivery timeout is the real ceiling.
const hookReadyTimeout = 20 * time.Second

// hookRetryWindow bounds our own retries of a forward the cell answered
// with a 5xx while finishing its post-thaw channel restart.
const hookRetryWindow = 3 * time.Second

// waitServing polls the cell's hook path with an empty unsigned POST until
// the listener answers any HTTP status at all (a 4xx is fine: it proves the
// application is serving, and the platform's real request follows). A
// connection reset, EOF or refusal means the listener is not up yet.
func waitServing(ctx context.Context, port int, fullPath, cell string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	path := strings.TrimPrefix(fullPath, "/hook/"+cell)
	if path == "" {
		path = "/"
	}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + path
	client := &http.Client{Timeout: 700 * time.Millisecond}
	var last error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return last
}

// Server routes /wake/{cell}, /hook/{cell}/... and /metrics.
type Server struct {
	Reg     *registry.Store
	Waker   Waker
	Token   string                      // optional shared bearer token for /wake
	Metrics func(w http.ResponseWriter) // optional Prometheus exposition writer
	Logger  *slog.Logger
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /wake/{cell}", s.handleWake)
	mux.HandleFunc("POST /hibernate/{cell}", s.handleHibernate)
	mux.HandleFunc("/hook/{cell}/", s.handleHook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		if s.Metrics == nil {
			http.Error(w, "metrics not configured", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.Metrics(w)
	})
	return mux
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("cell")
	took, err := s.Waker.Wake(r.Context(), name)
	if err != nil {
		s.log().Warn("wake failed", "cell", name, "err", err)
		http.Error(w, "wake failed", http.StatusBadGateway) // generic: never leak cell existence or runtime errors
		return
	}
	w.Header().Set("X-Wake-Ready-Ms", strconv.FormatInt(took.Milliseconds(), 10))
	_, _ = w.Write([]byte("awake\n"))
}

func (s *Server) handleHibernate(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h, ok := s.Waker.(Hibernator)
	if !ok {
		http.Error(w, "not supported", http.StatusNotImplemented)
		return
	}
	t := time.Now()
	if err := h.Hibernate(r.Context(), r.PathValue("cell")); err != nil {
		s.log().Warn("hibernate failed", "cell", r.PathValue("cell"), "err", err)
		http.Error(w, "hibernate failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("X-Hibernate-Ms", strconv.FormatInt(time.Since(t).Milliseconds(), 10))
	_, _ = w.Write([]byte("hibernated\n"))
}

// handleHook wakes the cell then reverse-proxies the webhook to it, keeping
// the path after /hook/{cell}. Cells must be configured to expect their
// webhook at that suffix path.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("cell")
	c, err := s.Reg.Get(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if c.Port == 0 {
		http.Error(w, "cell misconfigured", http.StatusBadGateway)
		return
	}
	// Verify before waking: an unverified request must not cost a wake.
	v, err := verifierFor(c.HookVerifier)
	if err != nil {
		s.log().Warn("hook", "cell", name, "err", err)
		http.Error(w, "cell misconfigured", http.StatusBadGateway)
		return
	}
	if v != nil {
		secret, err := readSecret(c.HookSecretFile)
		if err != nil {
			s.log().Warn("hook secret", "cell", name, "err", err)
			http.Error(w, "cell misconfigured", http.StatusBadGateway)
			return
		}
		body, err := bufferBody(r)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if v.Challenge(w, r, body, secret) {
			return // registration handshake answered on the sleeping cell's behalf
		}
		if err := v.Verify(r, body, secret); err != nil {
			s.log().Warn("hook rejected", "cell", name, "verifier", c.HookVerifier)
			http.Error(w, "unverified", http.StatusUnauthorized)
			return
		}
	}
	if _, err := s.Waker.Wake(r.Context(), name); err != nil {
		s.log().Warn("hook wake failed", "cell", name, "err", err)
		http.Error(w, "cell unavailable", http.StatusBadGateway)
		return
	}
	hookPort := c.HookPort
	if hookPort == 0 {
		hookPort = c.Port
	}
	// The gateway's /health answering does not mean its webhook listener
	// is serving: on thaw OpenClaw restarts the channel, tearing the old
	// listener down and bringing a new one up ~1 s later, and the container
	// port mapping accepts connections throughout, so a TCP accept proves
	// nothing (measured: accept passed, proxy got a reset). Probe with a
	// request instead: an unsigned POST to the hook path is answered (401)
	// only once the listener is really serving, and has no side effect.
	if hookPort != c.Port {
		if err := waitServing(r.Context(), hookPort, r.URL.Path, name, hookReadyTimeout); err != nil {
			s.log().Warn("hook listener not serving after wake", "cell", name, "port", hookPort, "err", err)
			http.Error(w, "cell unavailable", http.StatusBadGateway)
			return
		}
	}
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(hookPort)}
	prefix := "/hook/" + name
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.Host = target.Host
		},
		// After a thaw OpenClaw's channel answers its first update with a
		// 5xx "webhook ingress is not ready" for a few hundred ms while it
		// finishes restarting (its listener is up before its channel is).
		// Retry the forward ourselves within a short window instead of
		// handing the retry to the platform, which costs ~2 s per attempt.
		ModifyResponse: func(res *http.Response) error {
			if res.StatusCode >= 500 && res.Request != nil && res.Request.Context().Value(retryKey{}) == nil {
				return errNotReady
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if err == errNotReady {
				// caller (below) handles the retry by re-serving; signal it
				w.Header().Set("X-Fleet-Retry", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "cell unavailable", http.StatusBadGateway)
		},
	}
	// Serve with a retry window: the body is already buffered (verifiers
	// need it) or small, so it can be replayed.
	body, _ := bufferBody(r)
	deadline := time.Now().Add(hookRetryWindow)
	for attempt := 0; ; attempt++ {
		rec := &retryRecorder{ResponseWriter: w, header: http.Header{}}
		rr := r.Clone(r.Context())
		rr.Body = io.NopCloser(bytes.NewReader(body))
		rr.ContentLength = int64(len(body))
		if time.Now().After(deadline) {
			rr = rr.WithContext(context.WithValue(rr.Context(), retryKey{}, true)) // final attempt: pass the cell's answer through
		}
		rp.ServeHTTP(rec, rr)
		if rec.header.Get("X-Fleet-Retry") == "" || time.Now().After(deadline) {
			rec.flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// retryKey marks the final attempt, whose response is passed through as-is.
type retryKey struct{}

var errNotReady = errors.New("cell not ready")

// retryRecorder buffers one attempt's response so a retried attempt can
// replace it; flush writes the last attempt to the real writer.
type retryRecorder struct {
	http.ResponseWriter
	header http.Header
	status int
	body   bytes.Buffer
}

func (rr *retryRecorder) Header() http.Header         { return rr.header }
func (rr *retryRecorder) WriteHeader(code int)        { rr.status = code }
func (rr *retryRecorder) Write(b []byte) (int, error) { return rr.body.Write(b) }
func (rr *retryRecorder) flush() {
	for k, v := range rr.header {
		if k == "X-Fleet-Retry" {
			continue
		}
		rr.ResponseWriter.Header()[k] = v
	}
	if rr.status == 0 {
		rr.status = http.StatusOK
	}
	rr.ResponseWriter.WriteHeader(rr.status)
	_, _ = rr.ResponseWriter.Write(rr.body.Bytes())
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}
