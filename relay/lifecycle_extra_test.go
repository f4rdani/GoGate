package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aigateway/config"
)

func TestStartDisabledIsNoop(t *testing.T) {
	p := NewProxyPool(config.ProxyPoolConfig{Enabled: false})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx) // must return immediately without goroutines
	time.Sleep(50 * time.Millisecond)
}

func TestTestProxyAgainstFakeProxy(t *testing.T) {
	// Fake forward proxy: answers any proxied request with 204.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fake.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	p := NewProxyPool(config.ProxyPoolConfig{Enabled: true, CheckTimeout: 5 * time.Second})
	p.testURL = target.URL

	entry, ok := p.testProxy(context.Background(), fake.URL)
	if !ok || entry == nil {
		t.Fatal("working proxy must validate")
	}
	if entry.URL != fake.URL || entry.Latency <= 0 {
		t.Fatalf("entry must carry URL+latency: %+v", entry)
	}

	// Dead proxy must fail fast (connection refused).
	if _, ok := p.testProxy(context.Background(), "http://127.0.0.1:1"); ok {
		t.Fatal("dead proxy must fail")
	}
	// Garbage URL must fail.
	if _, ok := p.testProxy(context.Background(), "://bad"); ok {
		t.Fatal("garbage URL must fail")
	}
}

func TestCheckProxiesFilters(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fake.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	p := NewProxyPool(config.ProxyPoolConfig{Enabled: true, CheckTimeout: 5 * time.Second, MaxProxies: 10})
	p.testURL = target.URL

	got := p.checkProxies(context.Background(), []string{fake.URL, "http://127.0.0.1:1", "not-a-url"})
	if len(got) != 1 || got[0].URL != fake.URL {
		t.Fatalf("only the working proxy must survive: %+v", got)
	}
}
