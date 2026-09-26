package ingress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prathish-ks/fleet-supervisor/internal/registry"
)

type fakeWaker struct{ woke []string }

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
	_ = reg.Put(registry.Cell{Name: "a", Container: "oc-a", Port: port})
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
