package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aigateway/config"
	"github.com/aigateway/middleware"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

// mockProvider implements provider.Provider for testing.
type mockProvider struct {
	name      string
	healthy   bool
	responses map[string]*models.ChatCompletionResponse
	errors    map[string]error
	callCount atomic.Int64
	lastReq   *models.ChatCompletionRequest
}

func newMockProvider(name string) *mockProvider {
	return &mockProvider{
		name:      name,
		healthy:   true,
		responses: make(map[string]*models.ChatCompletionResponse),
		errors:    make(map[string]error),
	}
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	m.callCount.Add(1)
	m.lastReq = req
	if err, ok := m.errors[req.Model]; ok {
		return nil, err
	}
	if resp, ok := m.responses[req.Model]; ok {
		return resp, nil
	}
	c, _ := json.Marshal("response from " + m.name)
	return &models.ChatCompletionResponse{
		ID:      "resp-" + m.name,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []models.Choice{
			{Index: 0, Message: &models.Message{Role: "assistant", Content: c}},
		},
	}, nil
}
func (m *mockProvider) ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	m.callCount.Add(1)
	if err, ok := m.errors[req.Model]; ok {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	w.Write([]byte("data: [DONE]\n\n"))
	return nil
}
func (m *mockProvider) IsHealthy() bool  { return m.healthy }
func (m *mockProvider) SetHealthy(h bool) { m.healthy = h }
func (m *mockProvider) Embeddings(ctx context.Context, req *models.EmbeddingsRequest) (*models.EmbeddingsResponse, error) {
	m.callCount.Add(1)
	if err, ok := m.errors[req.Model]; ok {
		return nil, err
	}
	return &models.EmbeddingsResponse{
		Object: "list",
		Data: []models.EmbeddingData{
			{Object: "embedding", Embedding: []float32{0.1, 0.2}, Index: 0},
		},
		Model: req.Model,
		Usage: models.EmbeddingUsage{PromptTokens: 5, TotalTokens: 5},
	}, nil
}

func TestDirectRouting(t *testing.T) {
	p := newMockProvider("openai")
	registry := provider.NewRegistry()
	registry.Register("openai", p)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{Name: "gpt-4o", Provider: "openai", Model: "gpt-4o"},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.ChatCompletionRequest{Model: "gpt-4o"}
	resp, _, err := r.ChatCompletion(context.Background(), "gpt-4o", req)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}
	if resp.ID != "resp-openai" {
		t.Errorf("expected 'resp-openai', got '%s'", resp.ID)
	}
}

func TestRoundRobinRouting(t *testing.T) {
	p1 := newMockProvider("provider1")
	p2 := newMockProvider("provider2")
	registry := provider.NewRegistry()
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "fast-mix",
				Strategy: "round-robin",
				Backends: []config.BackendConfig{
					{Provider: "p1", Model: "model-a"},
					{Provider: "p2", Model: "model-b"},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	// First request should go to one provider, second to the other
	req1 := &models.ChatCompletionRequest{Model: "fast-mix"}
	resp1, _, err := r.ChatCompletion(context.Background(), "fast-mix", req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	req2 := &models.ChatCompletionRequest{Model: "fast-mix"}
	resp2, _, err := r.ChatCompletion(context.Background(), "fast-mix", req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	// Both should succeed, potentially from different providers
	if resp1 == nil || resp2 == nil {
		t.Error("responses should not be nil")
	}
}

func TestRoundRobinSkipsUnhealthyProvider(t *testing.T) {
	p1 := newMockProvider("provider1")
	p2 := newMockProvider("provider2")
	p1.SetHealthy(false) // simulates a disabled provider
	registry := provider.NewRegistry()
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "fast-mix",
				Strategy: "round-robin",
				Backends: []config.BackendConfig{
					{Provider: "p1", Model: "model-a"},
					{Provider: "p2", Model: "model-b"},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	for i := 0; i < 4; i++ {
		req := &models.ChatCompletionRequest{Model: "fast-mix"}
		resp, _, err := r.ChatCompletion(context.Background(), "fast-mix", req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		if resp.ID != "resp-provider2" {
			t.Errorf("request %d: expected 'resp-provider2', got '%s'", i, resp.ID)
		}
	}
	if p1.callCount.Load() != 0 {
		t.Errorf("disabled provider should never receive traffic, got %d calls", p1.callCount.Load())
	}
	if p2.callCount.Load() != 4 {
		t.Errorf("healthy provider should serve all 4 requests, got %d", p2.callCount.Load())
	}
}

func TestRoundRobinAllUnhealthyErrors(t *testing.T) {
	p1 := newMockProvider("provider1")
	p2 := newMockProvider("provider2")
	p1.SetHealthy(false)
	p2.SetHealthy(false)
	registry := provider.NewRegistry()
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "fast-mix",
				Strategy: "round-robin",
				Backends: []config.BackendConfig{
					{Provider: "p1", Model: "model-a"},
					{Provider: "p2", Model: "model-b"},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.ChatCompletionRequest{Model: "fast-mix"}
	_, _, err = r.ChatCompletion(context.Background(), "fast-mix", req)
	if err == nil {
		t.Fatal("expected error when all backends unavailable, got nil")
	}
	if p1.callCount.Load() != 0 || p2.callCount.Load() != 0 {
		t.Errorf("no provider should receive traffic, got p1=%d p2=%d", p1.callCount.Load(), p2.callCount.Load())
	}
}

func TestFallbackRouting(t *testing.T) {
	p1 := newMockProvider("primary")
	p2 := newMockProvider("fallback")
	// Primary fails
	p1.errors["model-a"] = &provider.ProviderError{StatusCode: 500, Body: "error", Provider: "primary"}

	registry := provider.NewRegistry()
	registry.Register("primary", p1)
	registry.Register("fallback", p2)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "test-model",
				Strategy: "fallback",
				Backends: []config.BackendConfig{
					{Provider: "primary", Model: "model-a", Tier: 1},
					{Provider: "fallback", Model: "model-a", Tier: 2},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.ChatCompletionRequest{Model: "test-model"}
	resp, _, err := r.ChatCompletion(context.Background(), "test-model", req)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if resp.ID != "resp-fallback" {
		t.Errorf("expected response from fallback, got '%s'", resp.ID)
	}
	if p2.callCount.Load() != 1 {
		t.Errorf("expected fallback to be called once, got %d", p2.callCount.Load())
	}
}

func TestFallbackTieredRouting(t *testing.T) {
	p1 := newMockProvider("tier1-sub")
	p2 := newMockProvider("tier2-cheap")
	p3 := newMockProvider("tier3-free")

	// Tier 1 fails with 429 (rate limit)
	p1.errors["model-a"] = &provider.ProviderError{StatusCode: 429, Body: "rate limited", Provider: "tier1-sub"}

	registry := provider.NewRegistry()
	registry.Register("tier1", p1)
	registry.Register("tier2", p2)
	registry.Register("tier3", p3)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "smart-fallback",
				Strategy: "fallback",
				Backends: []config.BackendConfig{
					{Provider: "tier1", Model: "model-a", Tier: 1},
					{Provider: "tier2", Model: "model-a", Tier: 2},
					{Provider: "tier3", Model: "model-a", Tier: 3},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.ChatCompletionRequest{Model: "smart-fallback"}
	resp, _, err := r.ChatCompletion(context.Background(), "smart-fallback", req)
	if err != nil {
		t.Fatalf("expected tiered fallback to succeed: %v", err)
	}
	// Should fall through to tier2 since tier1 is rate limited
	if resp.ID != "resp-tier2-cheap" {
		t.Errorf("expected response from tier2, got '%s'", resp.ID)
	}
}

func TestFallbackSkipUnhealthy(t *testing.T) {
	p1 := newMockProvider("unhealthy")
	p2 := newMockProvider("healthy")
	p1.SetHealthy(false)

	registry := provider.NewRegistry()
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "test",
				Strategy: "fallback",
				Backends: []config.BackendConfig{
					{Provider: "p1", Model: "m1", Tier: 1},
					{Provider: "p2", Model: "m2", Tier: 1},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.ChatCompletionRequest{Model: "test"}
	resp, _, err := r.ChatCompletion(context.Background(), "test", req)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if resp.ID != "resp-healthy" {
		t.Errorf("expected response from healthy provider, got '%s'", resp.ID)
	}
	// Unhealthy provider should not have been called
	if p1.callCount.Load() != 0 {
		t.Errorf("unhealthy provider should not be called, got %d calls", p1.callCount.Load())
	}
}

func TestRetryWithBackoff(t *testing.T) {
	p := newMockProvider("retry-test")
	// Always fail with retryable error
	p.errors["m"] = &provider.ProviderError{StatusCode: 429, Body: "rate limited", Provider: "retry-test"}

	registry := provider.NewRegistry()
	registry.Register("p", p)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 2, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "test",
				Strategy: "fallback",
				Backends: []config.BackendConfig{
					{Provider: "p", Model: "m", Tier: 1},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.ChatCompletionRequest{Model: "test"}
	_, _, err = r.ChatCompletion(context.Background(), "test", req)
	if err == nil {
		t.Error("expected error after all retries exhausted")
	}
	// Should have been called maxRetries+1 times (1 initial + 2 retries = 3)
	if p.callCount.Load() != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", p.callCount.Load())
	}
}

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	// Test retryWithBackoff directly via a router with retry config
	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 2, InitialBackoff: 10, MaxBackoff: 50},
	}

	r := &Router{
		retryCfg: RetryConfig{
			MaxRetries:     cfg.Retry.MaxRetries,
			InitialBackoff: 10 * time.Millisecond,
			MaxBackoff:     50 * time.Millisecond,
		},
	}

	// Test retryWithBackoff directly
	var callCount int
	err := r.retryWithBackoff(context.Background(), func() (bool, error) {
		callCount++
		if callCount == 1 {
			return true, fmt.Errorf("transient error") // retryable
		}
		return false, nil // success
	})

	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls, got %d", callCount)
	}

	// Test with non-retryable error
	callCount = 0
	err = r.retryWithBackoff(context.Background(), func() (bool, error) {
		callCount++
		return false, fmt.Errorf("permanent error") // not retryable
	})
	if err == nil {
		t.Error("expected error for non-retryable")
	}
	if callCount != 1 {
		t.Errorf("expected 1 call for non-retryable, got %d", callCount)
	}
}

func TestRetryContextCancellation(t *testing.T) {
	r := &Router{
		retryCfg: RetryConfig{
			MaxRetries:     10,
			InitialBackoff: 100 * time.Millisecond,
			MaxBackoff:     1 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	callCount := 0
	err := r.retryWithBackoff(ctx, func() (bool, error) {
		callCount++
		return true, fmt.Errorf("always fail")
	})

	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got: %v", err)
	}
}

func TestAllBackendsFail(t *testing.T) {
	p1 := newMockProvider("fail1")
	p2 := newMockProvider("fail2")
	p1.errors["m"] = &provider.ProviderError{StatusCode: 500, Body: "error", Provider: "fail1"}
	p2.errors["m"] = &provider.ProviderError{StatusCode: 500, Body: "error", Provider: "fail2"}

	registry := provider.NewRegistry()
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "test",
				Strategy: "fallback",
				Backends: []config.BackendConfig{
					{Provider: "p1", Model: "m", Tier: 1},
					{Provider: "p2", Model: "m", Tier: 2},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.ChatCompletionRequest{Model: "test"}
	_, _, err = r.ChatCompletion(context.Background(), "test", req)
	if err == nil {
		t.Error("expected error when all backends fail")
	}
}

func TestGetModelNames(t *testing.T) {
	p := newMockProvider("p")
	registry := provider.NewRegistry()
	registry.Register("p", p)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{Name: "model-a", Provider: "p", Model: "m1"},
			{Name: "model-b", Provider: "p", Model: "m2", Disabled: true},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	names := r.GetModelNames()
	if len(names) != 1 {
		t.Errorf("expected 1 active model name, got %d", len(names))
	}
	if names[0] != "model-a" {
		t.Errorf("expected model-a, got %s", names[0])
	}
}

func TestModelNotFound(t *testing.T) {
	r := &Router{routes: make(map[string]*ModelRoute)}
	_, _, err := r.ChatCompletion(context.Background(), "nonexistent", &models.ChatCompletionRequest{})
	if err == nil {
		t.Error("expected error for nonexistent model")
	}
}

func TestRoundRobinDistribution(t *testing.T) {
	p1 := newMockProvider("rr1")
	p2 := newMockProvider("rr2")
	p3 := newMockProvider("rr3")

	registry := provider.NewRegistry()
	registry.Register("p1", p1)
	registry.Register("p2", p2)
	registry.Register("p3", p3)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "rr-model",
				Strategy: "round-robin",
				Backends: []config.BackendConfig{
					{Provider: "p1", Model: "m"},
					{Provider: "p2", Model: "m"},
					{Provider: "p3", Model: "m"},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	// Send 6 requests — each provider should get 2
	for i := 0; i < 6; i++ {
		req := &models.ChatCompletionRequest{Model: "rr-model"}
		_, _, err := r.ChatCompletion(context.Background(), "rr-model", req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
	}

	if p1.callCount.Load() != 2 {
		t.Errorf("p1 expected 2 calls, got %d", p1.callCount.Load())
	}
	if p2.callCount.Load() != 2 {
		t.Errorf("p2 expected 2 calls, got %d", p2.callCount.Load())
	}
	if p3.callCount.Load() != 2 {
		t.Errorf("p3 expected 2 calls, got %d", p3.callCount.Load())
	}
}

func TestBackendTierSorting(t *testing.T) {
	p1 := newMockProvider("cheap")
	p2 := newMockProvider("expensive")
	p3 := newMockProvider("free")

	registry := provider.NewRegistry()
	registry.Register("cheap", p1)
	registry.Register("expensive", p2)
	registry.Register("free", p3)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{
				Name:     "tiered",
				Strategy: "fallback",
				Backends: []config.BackendConfig{
					{Provider: "free", Model: "m", Tier: 3},
					{Provider: "expensive", Model: "m", Tier: 1},
					{Provider: "cheap", Model: "m", Tier: 2},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	route := r.routes["tiered"]
	if route.Backends[0].Tier != 1 {
		t.Errorf("first backend should be tier 1, got %d", route.Backends[0].Tier)
	}
	if route.Backends[1].Tier != 2 {
		t.Errorf("second backend should be tier 2, got %d", route.Backends[1].Tier)
	}
	if route.Backends[2].Tier != 3 {
		t.Errorf("third backend should be tier 3, got %d", route.Backends[2].Tier)
	}
}

func TestEmbeddingsRouting(t *testing.T) {
	p := newMockProvider("openai")
	registry := provider.NewRegistry()
	registry.Register("openai", p)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1, InitialBackoff: 10, MaxBackoff: 50},
		Models: []config.ModelConfig{
			{Name: "text-embedding-3", Provider: "openai", Model: "text-embedding-3-small"},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	req := &models.EmbeddingsRequest{Model: "text-embedding-3", Input: "hello"}
	resp, err := r.Embeddings(context.Background(), "text-embedding-3", req)
	if err != nil {
		t.Fatalf("Embeddings routing failed: %v", err)
	}

	if resp.Model != "text-embedding-3-small" {
		t.Errorf("expected model text-embedding-3-small, got %s", resp.Model)
	}
	if len(resp.Data) != 1 || resp.Data[0].Embedding[0] != 0.1 {
		t.Errorf("unexpected embedding data: %+v", resp.Data)
	}
}

func TestSmartVisionRouting(t *testing.T) {
	pText := newMockProvider("openai-text")
	pVision := newMockProvider("openai-vision")

	registry := provider.NewRegistry()
	registry.Register("openai-text", pText)
	registry.Register("openai-vision", pVision)

	cfg := &config.Config{
		Models: []config.ModelConfig{
			{Name: "text-model", Provider: "openai-text", Model: "gpt-3.5-turbo", Vision: false},
			{Name: "vision-model", Provider: "openai-vision", Model: "gpt-4o", Vision: true},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	// Request with image targeting text-model
	req := &models.ChatCompletionRequest{
		Model: "text-model",
		Messages: []models.Message{
			{
				Role:    "user",
				Content: []byte(`[{"type": "image_url", "image_url": {"url": "data:image/png;base64,123"}}]`),
			},
		},
	}

	// Route it
	resp, _, err := r.ChatCompletion(context.Background(), "text-model", req)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	// Verify that it routed to the vision-model instead of the text-model!
	if pVision.callCount.Load() != 1 {
		t.Errorf("expected 1 call to vision provider, got %d", pVision.callCount.Load())
	}
	if pText.callCount.Load() != 0 {
		t.Errorf("expected 0 calls to text provider, got %d", pText.callCount.Load())
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("expected response model gpt-4o, got %s", resp.Model)
	}
}

// ServeHTTP implements http.ResponseWriter for the mock provider's stream test.
// This is already handled by httptest.ResponseRecorder in tests.

func TestRouter_PrivacyFilter_FreeOnlyScope(t *testing.T) {
	registry := provider.NewRegistry()
	pPaid := newMockProvider("paid-prov")
	pFree := newMockProvider("free-prov")
	registry.Register("paid-prov", pPaid)
	registry.Register("free-prov", pFree)

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "paid-prov", Type: "openai", Tier: 1},
			{Name: "free-prov", Type: "opencode", Tier: 3},
		},
		Models: []config.ModelConfig{
			{Name: "direct-paid", Provider: "paid-prov", Model: "gpt-4o"},
			{Name: "direct-free", Provider: "free-prov", Model: "oc-free"},
			{
				Name:     "combo-fallback",
				Strategy: "fallback",
				Backends: []config.BackendConfig{
					{Provider: "paid-prov", Model: "gpt-4o", Tier: 1},
					{Provider: "free-prov", Model: "oc-free", Tier: 3},
				},
			},
		},
	}

	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	privCfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "redact",
		Scope:       "free-only",
		MaskSecrets: true,
		MaskPII:     true,
	}
	ctx := middleware.WithPrivacyConfig(context.Background(), privCfg)

	secretPrompt := "My secret is sk-proj-1234567890abcdef1234567890"
	contentBytes, _ := json.Marshal(secretPrompt)

	// 1. Direct call to paid provider: should NOT be sanitized (secret remains intact)
	req1 := &models.ChatCompletionRequest{
		Model:    "direct-paid",
		Messages: []models.Message{{Role: "user", Content: contentBytes}},
	}
	_, _, err = r.ChatCompletion(ctx, "direct-paid", req1)
	if err != nil {
		t.Fatalf("call to paid provider failed: %v", err)
	}
	if pPaid.lastReq == nil {
		t.Fatal("expected paid provider to receive request")
	}
	var paidReceived string
	json.Unmarshal(pPaid.lastReq.Messages[0].Content, &paidReceived)
	if paidReceived != secretPrompt {
		t.Fatalf("expected paid provider to receive original prompt, got: %s", paidReceived)
	}

	// 2. Direct call to free provider: MUST be sanitized
	req2 := &models.ChatCompletionRequest{
		Model:    "direct-free",
		Messages: []models.Message{{Role: "user", Content: contentBytes}},
	}
	_, _, err = r.ChatCompletion(ctx, "direct-free", req2)
	if err != nil {
		t.Fatalf("call to free provider failed: %v", err)
	}
	if pFree.lastReq == nil {
		t.Fatal("expected free provider to receive request")
	}
	var freeReceived string
	json.Unmarshal(pFree.lastReq.Messages[0].Content, &freeReceived)
	if !strings.Contains(freeReceived, "[REDACTED_API_KEY]") {
		t.Fatalf("expected free provider to receive redacted prompt, got: %s", freeReceived)
	}
	if strings.Contains(freeReceived, "sk-proj-") {
		t.Fatalf("sensitive key leaked to free provider: %s", freeReceived)
	}

	// 3. Fallback scenario: paid provider fails -> fallback to free provider gets sanitized
	pPaid.errors["gpt-4o"] = &provider.ProviderError{StatusCode: 500, Body: "paid server error"}
	req3 := &models.ChatCompletionRequest{
		Model:    "combo-fallback",
		Messages: []models.Message{{Role: "user", Content: contentBytes}},
	}
	_, _, err = r.ChatCompletion(ctx, "combo-fallback", req3)
	if err != nil {
		t.Fatalf("fallback call failed: %v", err)
	}
	var fallbackReceived string
	json.Unmarshal(pFree.lastReq.Messages[0].Content, &fallbackReceived)
	if !strings.Contains(fallbackReceived, "[REDACTED_API_KEY]") {
		t.Fatalf("expected fallback free provider to receive redacted prompt, got: %s", fallbackReceived)
	}
}
