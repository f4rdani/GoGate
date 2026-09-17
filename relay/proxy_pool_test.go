package relay

import (
	"testing"
	"time"

	"github.com/aigateway/config"
)

func TestProxyPool_BasicRotation(t *testing.T) {
	cfg := config.ProxyPoolConfig{
		Enabled:    true,
		MaxProxies: 10,
	}
	pool := NewProxyPool(cfg)

	// Pool is initially empty
	if got := pool.Next(); got != nil {
		t.Errorf("expected nil for empty pool, got %v", got)
	}

	// Inject 3 dummy proxies
	pool.proxies = []*ProxyEntry{
		{URL: "http://1.1.1.1:8080", Latency: 50 * time.Millisecond},
		{URL: "http://2.2.2.2:8080", Latency: 100 * time.Millisecond},
		{URL: "http://3.3.3.3:8080", Latency: 150 * time.Millisecond},
	}

	// Should rotate 1 -> 2 -> 3 -> 1
	p1 := pool.Next()
	p2 := pool.Next()
	p3 := pool.Next()
	p4 := pool.Next()

	if p1.URL != "http://1.1.1.1:8080" || p2.URL != "http://2.2.2.2:8080" || p3.URL != "http://3.3.3.3:8080" {
		t.Errorf("round-robin sequence failed: %s, %s, %s", p1.URL, p2.URL, p3.URL)
	}
	if p4.URL != "http://1.1.1.1:8080" {
		t.Errorf("expected wrap-around to 1.1.1.1, got %s", p4.URL)
	}
}

func TestProxyPool_MarkFailureEviction(t *testing.T) {
	pool := NewProxyPool(config.ProxyPoolConfig{Enabled: true})
	pool.proxies = []*ProxyEntry{
		{URL: "http://failing-proxy:8080"},
		{URL: "http://good-proxy:8080"},
	}

	// 1st failure: not evicted
	pool.MarkFailure("http://failing-proxy:8080")
	if len(pool.proxies) != 2 {
		t.Errorf("expected proxy not to be evicted on 1st failure")
	}

	// 2nd failure: not evicted
	pool.MarkFailure("http://failing-proxy:8080")
	if len(pool.proxies) != 2 {
		t.Errorf("expected proxy not to be evicted on 2nd failure")
	}

	// 3rd failure: evicted
	pool.MarkFailure("http://failing-proxy:8080")
	if len(pool.proxies) != 1 || pool.proxies[0].URL != "http://good-proxy:8080" {
		t.Errorf("failing proxy should have been evicted on 3rd failure, remaining: %v", pool.proxies)
	}
}

func TestProxyPool_Stats(t *testing.T) {
	pool := NewProxyPool(config.ProxyPoolConfig{Enabled: true})
	pool.proxies = []*ProxyEntry{
		{URL: "http://p1:8080", Latency: 100 * time.Millisecond},
		{URL: "http://p2:8080", Latency: 200 * time.Millisecond},
	}
	pool.lastRefresh = time.Now()
	pool.totalFound = 50

	stats := pool.Stats()
	if !stats.Enabled {
		t.Errorf("expected stats.Enabled to be true")
	}
	if stats.ActiveCount != 2 {
		t.Errorf("expected active count 2, got %d", stats.ActiveCount)
	}
	if stats.AverageLatency != "150ms" {
		t.Errorf("expected avg latency 150ms, got %s", stats.AverageLatency)
	}
	if stats.TotalFound != 50 {
		t.Errorf("expected total found 50, got %d", stats.TotalFound)
	}
}

func TestProxyPool_DynamicProxyFunc(t *testing.T) {
	pool := NewProxyPool(config.ProxyPoolConfig{Enabled: true})
	proxyFunc := pool.DynamicProxyFunc()

	// Empty pool -> returns nil (direct)
	u, err := proxyFunc(nil)
	if err != nil || u != nil {
		t.Errorf("expected nil for empty pool, got %v, err %v", u, err)
	}

	// Populate pool
	pool.proxies = []*ProxyEntry{
		{URL: "http://10.0.0.1:8080"},
	}

	u, err = proxyFunc(nil)
	if err != nil || u == nil || u.Host != "10.0.0.1:8080" {
		t.Errorf("expected proxy URL http://10.0.0.1:8080, got %v, err %v", u, err)
	}
}
