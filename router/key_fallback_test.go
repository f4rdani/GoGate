package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

// TestDirectRouteFallsBackAcrossKeys is the headline regression test: a direct
// route whose first upstream key hits 429 must transparently succeed with the
// next key instead of failing the client request.
func TestDirectRouteFallsBackAcrossKeys(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.Header.Get("Authorization"), "key-1") {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"rate limit"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"r1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 2, InitialBackoff: 1, MaxBackoff: 5},
		Providers: []config.ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"key-1", "key-2"}, Tier: 1},
		},
		Models: []config.ModelConfig{
			{Name: "m", Provider: "p1", Model: "m"},
		},
	}
	reg := provider.NewRegistry()
	p, err := provider.NewProviderFromConfig(cfg.Providers[0])
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("p1", p)
	r, err := NewRouter(cfg, reg)
	if err != nil {
		t.Fatal(err)
	}

	req := &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
	resp, provName, err := r.ChatCompletion(context.Background(), "m", req)
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if provName != "p1" {
		t.Fatalf("expected provider name p1, got %q", provName)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits (key-1 then key-2), got %d", hits.Load())
	}
}
