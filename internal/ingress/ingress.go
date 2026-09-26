// Package ingress is the shared, always-on front door for hibernated cells.
// Chat platforms that deliver by webhook (Telegram, Slack Events API,
// generic HTTP) point at this server instead of at the cell; the server
// wakes the cell, then forwards the request to the cell's loopback port.
// Platforms that need a persistent socket (Discord gateway, WhatsApp) are
// handled by keepers or the always-on class, not here.
package ingress

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/registry"
)

// Waker is what the ingress needs from the supervisor.
type Waker interface {
	Wake(ctx context.Context, cell string) (readyTook time.Duration, err error)
}

// Server routes /wake/{cell} and /hook/{cell}/... .
type Server struct {
	Reg    *registry.Store
	Waker  Waker
	Token  string // optional shared bearer token for /wake
	Logger *slog.Logger
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /wake/{cell}", s.handleWake)
	mux.HandleFunc("/hook/{cell}/", s.handleHook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
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
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("X-Wake-Ready-Ms", strconv.FormatInt(took.Milliseconds(), 10))
	_, _ = w.Write([]byte("awake\n"))
}

// handleHook wakes the cell then reverse-proxies the webhook to it, keeping
// the path after /hook/{cell}. Cells must be configured to expect their
// webhook at that suffix path.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("cell")
	c, err := s.Reg.Get(name)
	if err != nil {
		http.Error(w, "unknown cell", http.StatusNotFound)
		return
	}
	if c.Port == 0 {
		http.Error(w, "cell has no port", http.StatusBadGateway)
		return
	}
	if _, err := s.Waker.Wake(r.Context(), name); err != nil {
		s.log().Warn("hook wake failed", "cell", name, "err", err)
		http.Error(w, "cell unavailable", http.StatusBadGateway)
		return
	}
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(c.Port)}
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
	}
	rp.ServeHTTP(w, r)
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}
