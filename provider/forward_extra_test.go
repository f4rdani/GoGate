package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

func forwardTestBase(t *testing.T, srv *httptest.Server, keys []string) *BaseProvider {
	t.Helper()
	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "fwd", Type: "openai", BaseURL: srv.URL, APIKeys: keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	up, ok := p.(UpstreamConfigProvider)
	if !ok {
		t.Fatal("must expose upstream config")
	}
	base, ok := p.(*OpenAIProvider)
	if !ok {
		t.Fatal("must be openai provider")
	}
	_ = up
	return base.BaseProvider
}

func TestForwardUpstreamPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			t.Errorf("path must pass through: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k1" {
			t.Errorf("auth must pass: %q", got)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(201)
		w.Write([]byte("AUD"))
	}))
	defer srv.Close()

	base := forwardTestBase(t, srv, []string{"k1"})
	resp, err := base.ForwardUpstream(context.Background(), "POST", "/audio/speech", []byte("{}"), "application/json")
	if err != nil {
		t.Fatalf("forward failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("status/headers must proxy: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestForwardUpstreamKeyRotation(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.Header.Get("Authorization"), "k1") {
			w.WriteHeader(500)
			w.Write([]byte("busy"))
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	base := forwardTestBase(t, srv, []string{"k1", "k2"})
	resp, err := base.ForwardUpstream(context.Background(), "POST", "/x", nil, "")
	if err != nil {
		t.Fatalf("rotation failed: %v", err)
	}
	resp.Body.Close()
	if hits.Load() != 2 || resp.StatusCode != 200 {
		t.Fatalf("must try both keys: hits=%d status=%d", hits.Load(), resp.StatusCode)
	}
}

func TestForwardUpstreamExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("down"))
	}))
	defer srv.Close()

	base := forwardTestBase(t, srv, []string{"k1"})
	_, err := base.ForwardUpstream(context.Background(), "POST", "/x", nil, "")
	pe, ok := err.(*ProviderError)
	if !ok || !pe.KeyExhausted {
		t.Fatalf("must be exhausted: %+v", err)
	}
}

func TestForwardUpstreamNonRetryableProxied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	base := forwardTestBase(t, srv, []string{"k1"})
	resp, err := base.ForwardUpstream(context.Background(), "POST", "/x", nil, "")
	if err != nil {
		t.Fatalf("4xx must proxy, not error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status must proxy: %d", resp.StatusCode)
	}
}

func TestProviderErrorHelpers(t *testing.T) {
	if (&ProviderError{StatusCode: 429, Body: "x concurrency saturated y", Provider: "p"}).IsRetryable() != true {
		t.Fatal("429 retryable")
	}
	if !IsSaturationError(&ProviderError{StatusCode: 429, Body: "provider p concurrency saturated, retry", Provider: "p"}) {
		t.Fatal("saturation detect")
	}
	if IsSaturationError(&ProviderError{StatusCode: 429, Body: "rate limit", Provider: "p"}) {
		t.Fatal("plain 429 is not saturation")
	}
	be := BudgetExceeded("p")
	if !IsBudgetExceeded(be) || !IsLocalLimit(be) {
		t.Fatalf("budget helpers: %+v", be)
	}
	if IsBudgetExceeded(&ProviderError{StatusCode: 429, Body: "nope", Provider: "p"}) {
		t.Fatal("plain 429 is not budget")
	}
	if IsLocalLimit(&ProviderError{StatusCode: 500, Body: "x", Provider: "p"}) {
		t.Fatal("500 is not a local limit")
	}
	if (&ProviderError{StatusCode: 1, Body: "b", Provider: "p"}).Error() == "" {
		t.Fatal("Error() must stringify")
	}
}

func TestBaseProviderGettersAndRegistry(t *testing.T) {
	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "g", Type: "openai", BaseURL: "http://u", APIKeys: []string{"k"}, Models: []string{"m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	up, ok := p.(UpstreamConfigProvider)
	if !ok {
		t.Fatal("must expose upstream config")
	}
	if up.ProviderType() != "openai" || up.BaseURL() != "http://u" || len(up.APIKeys()) != 1 {
		t.Fatal("getters must work")
	}
	base, ok := p.(*OpenAIProvider)
	if !ok {
		t.Fatal("type assert")
	}
	if !base.SupportsModel("m") || base.SupportsModel("nope") {
		t.Fatal("SupportsModel broken")
	}
	if !p.IsHealthy() {
		t.Fatal("starts healthy")
	}
	p.SetHealthy(false)
	if p.IsHealthy() {
		t.Fatal("SetHealthy broken")
	}

	reg := NewRegistry()
	reg.Register("g", p)
	if got, ok := reg.Get("g"); !ok || got != p {
		t.Fatal("registry get broken")
	}
	if _, ok := reg.Get("missing"); ok {
		t.Fatal("missing must fail")
	}
	if len(reg.All()) != 1 {
		t.Fatal("registry all broken")
	}

	SetCachedDynamicModels("g", []string{"a"})
	if got := GetCachedDynamicModels("g"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("dynamic cache broken: %v", got)
	}
}

func TestOpenAIEmbeddingsLoop(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/embeddings" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if strings.Contains(r.Header.Get("Authorization"), "k1") {
			w.WriteHeader(500)
			w.Write([]byte("busy"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"e","usage":{"prompt_tokens":2,"total_tokens":2}}`))
	}))
	defer srv.Close()

	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "e", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"k1", "k2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Embeddings(context.Background(), &models.EmbeddingsRequest{Model: "e", Input: "hi"})
	if err != nil {
		t.Fatalf("embeddings fallback failed: %v", err)
	}
	if resp == nil || hits.Load() != 2 {
		t.Fatalf("both keys must be tried: hits=%d", hits.Load())
	}
}
