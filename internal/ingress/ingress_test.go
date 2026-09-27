package ingress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/registry"
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
