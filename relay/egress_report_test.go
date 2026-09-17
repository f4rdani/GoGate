package relay

import (
	"net/http/httptest"
	"testing"

	"github.com/aigateway/provider"
)

func testPoolWith(proxies ...string) *ProxyPool {
	p := &ProxyPool{enabled: true, proxies: make([]*ProxyEntry, 0, len(proxies))}
	for _, u := range proxies {
		p.proxies = append(p.proxies, &ProxyEntry{URL: u})
	}
	return p
}

func TestCheckoutRoundRobinAndDirect(t *testing.T) {
	p := testPoolWith("http://a:8080", "http://b:8080")
	if got := p.Checkout(); got != "http://a:8080" {
		t.Fatalf("expected first proxy, got %q", got)
	}
	if got := p.Checkout(); got != "http://b:8080" {
		t.Fatalf("expected rotation, got %q", got)
	}

	disabled := testPoolWith("http://a:8080")
	disabled.SetEnabled(false)
	if got := disabled.Checkout(); got != "" {
		t.Fatalf("disabled pool must checkout direct, got %q", got)
	}

	empty := testPoolWith()
	if got := empty.Checkout(); got != "" {
		t.Fatalf("empty pool must checkout direct, got %q", got)
	}
}

func TestReportEvictsAfterThreeFailures(t *testing.T) {
	p := testPoolWith("http://flaky:8080", "http://good:8080")
	p.Report("http://flaky:8080", true)
	p.Report("http://flaky:8080", true)
	if len(p.proxies) != 2 {
		t.Fatalf("must survive 2 failures, have %d", len(p.proxies))
	}
	p.Report("http://flaky:8080", true)
	if len(p.proxies) != 1 || p.proxies[0].URL != "http://good:8080" {
		t.Fatalf("flaky proxy must be evicted after 3 failures: %+v", p.proxies)
	}
}

func TestReportSuccessResetsCounter(t *testing.T) {
	p := testPoolWith("http://w:8080")
	p.Report("http://w:8080", true)
	p.Report("http://w:8080", true)
	p.Report("http://w:8080", false) // completed exchange proves the proxy works
	p.Report("http://w:8080", true)
	p.Report("http://w:8080", true)
	if len(p.proxies) != 1 {
		t.Fatal("success must reset the consecutive-failure counter")
	}
}

func TestContextProxyFuncPrefersCheckout(t *testing.T) {
	p := testPoolWith("http://rr:8080")
	fn := p.ContextProxyFunc()

	req := httptest.NewRequest("GET", "http://upstream/x", nil)
	req = req.WithContext(provider.WithEgressProxy(req.Context(), "http://checked:9090"))
	u, err := fn(req)
	if err != nil || u.String() != "http://checked:9090" {
		t.Fatalf("expected checked-out proxy, got %v %v", u, err)
	}

	plain := httptest.NewRequest("GET", "http://upstream/x", nil)
	u, err = fn(plain)
	if err != nil || u.String() != "http://rr:8080" {
		t.Fatalf("expected round-robin fallback, got %v %v", u, err)
	}
}
