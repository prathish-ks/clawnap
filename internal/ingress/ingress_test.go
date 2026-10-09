package ingress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prathish-ks/clawnap/internal/registry"
)

type fakeWaker struct{ woke, slept []string }

func (f *fakeWaker) Hibernate(_ context.Context, cell string) error {
	f.slept = append(f.slept, cell)
	return nil
}

func (f *fakeWaker) Wake(_ context.Context, cell string) (time.Duration, error) {
	f.woke = append(f.woke, cell)
	return 42 * time.Millisecond, nil
}

func TestWakeRequiresTokenAndReportsTiming(t *testing.T) {
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	fw := &fakeWaker{}
	srv := httptest.NewServer((&Server{Reg: reg, Waker: fw, Token: "s3cret"}).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/wake/a", nil)
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 200 || res.Header.Get("X-Wake-Ready-Ms") != "42" || len(fw.woke) != 1 {
		t.Fatalf("bad wake response: %d %v %v", res.StatusCode, res.Header, fw.woke)
	}
}

func TestHookWakesThenProxiesWithStrippedPrefix(t *testing.T) {
	// backend standing in for the cell's gateway
	var gotPath, gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(204)
	}))
	defer backend.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(backend.URL, "http://127.0.0.1:"))

	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: 1, HookPort: port, HookVerifier: "none"}) // explicit none; gateway port differs from hook port
	fw := &fakeWaker{}
	srv := httptest.NewServer((&Server{Reg: reg, Waker: fw}).Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL+"/hook/a/telegram/webhook", "application/json", strings.NewReader(`{"update_id":1}`))
	if err != nil || res.StatusCode != 204 {
		t.Fatalf("proxy failed: %v %v", err, res)
	}
	if gotPath != "/telegram/webhook" || gotBody != `{"update_id":1}` || len(fw.woke) != 1 || fw.woke[0] != "a" {
		t.Fatalf("path=%q body=%q woke=%v", gotPath, gotBody, fw.woke)
	}
	res, _ = http.Post(srv.URL+"/hook/nope/x", "text/plain", nil)
	if res.StatusCode != 404 {
		t.Fatalf("unknown cell should 404, got %d", res.StatusCode)
	}
}

func TestUnsetVerifierFailsClosed(t *testing.T) {
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	_ = reg.Put(registry.Cell{Name: "u", Container: "oc-u", Port: 1}) // no verifier set
	fw := &fakeWaker{}
	srv := httptest.NewServer((&Server{Reg: reg, Waker: fw}).Handler())
	defer srv.Close()
	res, _ := http.Post(srv.URL+"/hook/u/x", "application/json", strings.NewReader("{}"))
	if res.StatusCode != 502 || len(fw.woke) != 0 {
		t.Fatalf("unset verifier must not wake or proxy: %d woke=%v", res.StatusCode, fw.woke)
	}
}

func TestHibernateEndpoint(t *testing.T) {
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	fw := &fakeWaker{}
	srv := httptest.NewServer((&Server{Reg: reg, Waker: fw, Token: "s3cret"}).Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/hibernate/a", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 || len(fw.slept) != 1 || fw.slept[0] != "a" {
		t.Fatalf("hibernate endpoint: %d %v", res.StatusCode, fw.slept)
	}
}

func TestHookWaitsForListenerAfterWake(t *testing.T) {
	// Like a thawing OpenClaw: the port ACCEPTS connections immediately (a
	// raw listener that resets them) and only starts SERVING 700 ms after
	// the wake. A TCP-accept probe would pass and the proxy would get a
	// reset; the request probe must wait for the real server.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	go func() { // accept-and-reset until the real server takes over
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	_ = reg.Put(registry.Cell{Name: "l", Container: "oc-l", Port: 1, HookPort: port, HookVerifier: "none"})
	var srvBackend *httptest.Server
	fw := &lateWaker{after: 700 * time.Millisecond, start: func() {
		_ = ln.Close()
		time.Sleep(50 * time.Millisecond)
		l2, _ := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		srvBackend = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		srvBackend.Listener = l2
		srvBackend.Start()
	}}
	srv := httptest.NewServer((&Server{Reg: reg, Waker: fw}).Handler())
	defer srv.Close()
	res, err := http.Post(srv.URL+"/hook/l/telegram-webhook", "application/json", strings.NewReader(`{"update_id":1}`))
	if err != nil || res.StatusCode != 204 {
		t.Fatalf("hook must wait for the listener and then proxy: %v %v", err, res)
	}
	srvBackend.Close()
}

// lateWaker "wakes" instantly but the listener appears only after a delay.
type lateWaker struct {
	after time.Duration
	start func()
}

func (l *lateWaker) Wake(context.Context, string) (time.Duration, error) {
	go func() { time.Sleep(l.after); l.start() }()
	return time.Millisecond, nil
}

func TestHookRetriesCellNotReadyThenSucceeds(t *testing.T) {
	// the cell answers 500 "not ready" to the first two forwards, then 204:
	// the platform must see a single 204, not a 500 it has to retry
	var calls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // the ingress's readiness probe: not a delivery
			w.WriteHeader(405)
			return
		}
		b, _ := io.ReadAll(r.Body)
		n := atomic.AddInt32(&calls, 1)
		if string(b) != `{"update_id":7}` {
			t.Errorf("body not replayed on attempt %d: %q", n, b)
		}
		if n <= 2 {
			http.Error(w, "Telegram webhook ingress is not ready.", 500)
			return
		}
		w.WriteHeader(204)
	}))
	defer backend.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(backend.URL, "http://127.0.0.1:"))
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	_ = reg.Put(registry.Cell{Name: "n", Container: "oc-n", Port: 1, HookPort: port, HookVerifier: "none"})
	srv := httptest.NewServer((&Server{Reg: reg, Waker: &fakeWaker{}}).Handler())
	defer srv.Close()
	res, err := http.Post(srv.URL+"/hook/n/telegram-webhook", "application/json", strings.NewReader(`{"update_id":7}`))
	if err != nil || res.StatusCode != 204 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("want a single 204 after 3 forwards, got %v %v calls=%d", err, res, calls)
	}
}

// An oversized body is refused before the cell is woken, for the "none"
// verifier too; it must never be forwarded as an empty request.
func TestHookRefusesOversizedBodyBeforeWaking(t *testing.T) {
	var forwarded int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&forwarded, 1)
		w.WriteHeader(204)
	}))
	defer backend.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(backend.URL, "http://127.0.0.1:"))
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	_ = reg.Put(registry.Cell{Name: "big", Container: "oc-big", Port: port, HookVerifier: "none"})
	fw := &fakeWaker{}
	srv := httptest.NewServer((&Server{Reg: reg, Waker: fw}).Handler())
	defer srv.Close()
	body := strings.NewReader(strings.Repeat("x", maxBody+1))
	res, err := http.Post(srv.URL+"/hook/big/", "application/json", body)
	if err != nil || res.StatusCode != http.StatusRequestEntityTooLarge || len(fw.woke) != 0 || atomic.LoadInt32(&forwarded) != 0 {
		t.Fatalf("want 413 with no wake and no forward, got err=%v status=%v woke=%v forwarded=%d", err, res, fw.woke, forwarded)
	}
}

// Only the channel's own "not ready" answer is retried. A 500 from the
// bot's handler is an application error: forwarded once, passed through.
func TestHookDoesNotReplayApplicationErrors(t *testing.T) {
	var calls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // readiness probe: not a delivery
			w.WriteHeader(405)
			return
		}
		atomic.AddInt32(&calls, 1)
		http.Error(w, "handler crashed", 500)
	}))
	defer backend.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(backend.URL, "http://127.0.0.1:"))
	reg, _ := registry.Open(filepath.Join(t.TempDir(), "c.json"))
	_ = reg.Put(registry.Cell{Name: "e", Container: "oc-e", Port: 1, HookPort: port, HookVerifier: "none"})
	srv := httptest.NewServer((&Server{Reg: reg, Waker: &fakeWaker{}}).Handler())
	defer srv.Close()
	res, err := http.Post(srv.URL+"/hook/e/telegram-webhook", "application/json", strings.NewReader(`{"update_id":8}`))
	if err != nil || res.StatusCode != 500 {
		t.Fatalf("application error must pass through, got %v %v", err, res)
	}
	b, _ := io.ReadAll(res.Body)
	if atomic.LoadInt32(&calls) != 1 || !strings.Contains(string(b), "handler crashed") {
		t.Fatalf("want exactly one forward with the cell's body passed through, got calls=%d body=%q", calls, b)
	}
}

// /metrics names every cell, its phase and its wake counts: the host's tenant
// list and their activity pattern. It must need the token like the control
// endpoints, because the quickstart puts a reverse proxy in front of this
// listener and a proxy forwarding every path would otherwise publish it.
func TestMetricsRequiresTheToken(t *testing.T) {
	srv := &Server{Token: "sekret", Metrics: func(w http.ResponseWriter) {
		_, _ = w.Write([]byte("clawnap_cells{phase=\"active\"} 1\n"))
	}}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /metrics returned %d, body %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "clawnap_cells") {
		t.Fatalf("authorised /metrics returned %d, body %q", rec.Code, rec.Body.String())
	}

	// /healthz stays open: it reveals nothing and load balancers need it.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz should not need a token, got %d", rec.Code)
	}
}

// The readiness probe runs against the tenant's own handler, so it must not be
// able to do anything: a cell using the "none" verifier would process a POST.
func TestReadinessProbeIsSideEffectFree(t *testing.T) {
	var methods []string
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed) // a GET-less hook handler
	}))
	defer cell.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(cell.URL, "http://127.0.0.1:"))

	if err := waitServing(context.Background(), port, "/hook/c/telegram", "c", time.Second); err != nil {
		t.Fatalf("a 405 still proves the listener is serving: %v", err)
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Fatalf("probe used %s; it must not be able to deliver anything", m)
		}
	}
}
