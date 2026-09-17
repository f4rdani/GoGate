package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

// stubPool is a scripted EgressPool: checkouts follow the script,
// reports are recorded for assertions.
type stubPool struct {
	script  []string
	reports []stubReport
}

type stubReport struct {
	url    string
	failed bool
}

func (s *stubPool) Checkout() string {
	if len(s.script) == 0 {
		return ""
	}
	u := s.script[0]
	s.script = s.script[1:]
	return u
}

func (s *stubPool) Report(proxyURL string, failed bool) {
	s.reports = append(s.reports, stubReport{url: proxyURL, failed: failed})
}

// ctxAwareTransport honors per-attempt proxy checkouts like the server wiring.
func ctxAwareTransport() *http.Transport {
	tr := &http.Transport{}
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		if u := EgressProxyFromContext(req.Context()); u != "" {
			return url.Parse(u)
		}
		return nil, nil
	}
	return tr
}

// A dead checked-out proxy must fail over to the next key (direct) and be
// reported — proving the ctx plumbing reaches the transport.
func TestEgressDeadProxyFailsOver(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "p1", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"k1", "k2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := p.(UpstreamConfigProvider)
	up.Client().Transport = ctxAwareTransport()

	stub := &stubPool{script: []string{"http://127.0.0.1:1", ""}}
	p.(interface{ SetEgressPool(EgressPool) }).SetEgressPool(stub)

	req := &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
	resp, err := p.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("expected failover success, got %v", err)
	}
	if resp == nil || hits.Load() != 1 {
		t.Fatalf("expected 1 direct upstream hit, got %d", hits.Load())
	}
	if len(stub.reports) != 1 || stub.reports[0].url != "http://127.0.0.1:1" || !stub.reports[0].failed {
		t.Fatalf("expected one failure report for the dead proxy, got %+v", stub.reports)
	}
}

// A completed exchange (even an upstream 429) proves the proxy works.
func TestEgressUpstreamErrorIsNotProxyFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"limit"}`))
	}))
	defer srv.Close()

	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "p1", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"k1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := p.(UpstreamConfigProvider)
	up.Client().Transport = ctxAwareTransport()
	stub := &stubPool{script: []string{""}}
	p.(interface{ SetEgressPool(EgressPool) }).SetEgressPool(stub)

	req := &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
	_, err = p.ChatCompletion(context.Background(), req)
	if _, ok := err.(*ProviderError); !ok {
		t.Fatalf("expected ProviderError, got %v", err)
	}
	for _, rep := range stub.reports {
		if rep.failed {
			t.Fatalf("upstream HTTP errors must not count as proxy failures: %+v", stub.reports)
		}
	}
}
