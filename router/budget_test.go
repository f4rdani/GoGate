package router

import (
	"context"
	"strings"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

// An over-budget direct backend must fail with a budget error (429 class)
// without touching upstream.
func TestDirectRouteOverBudgetBlocked(t *testing.T) {
	registry := provider.NewRegistry()
	mp := newMockProvider("p1")
	mp.responses["m"] = &models.ChatCompletionResponse{ID: "resp-1"}
	registry.Register("p1", mp)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 1, MaxBackoff: 1},
		Models: []config.ModelConfig{
			{Name: "m", Provider: "p1", Model: "m"},
		},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	r.SetBudgetChecker(func(string) bool { return true })

	req := &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
	_, _, err = r.ChatCompletion(context.Background(), "m", req)
	if err == nil || !provider.IsBudgetExceeded(err) {
		t.Fatalf("expected budget error, got %v", err)
	}
	if mp.callCount.Load() != 0 {
		t.Fatalf("over-budget backend must not be called, got %d", mp.callCount.Load())
	}
}

// A fallback combo must skip the over-budget backend and serve from the next.
func TestFallbackSkipsOverBudgetBackend(t *testing.T) {
	registry := provider.NewRegistry()
	p1 := newMockProvider("p1")
	p2 := newMockProvider("p2")
	p2.responses["m2"] = &models.ChatCompletionResponse{ID: "resp-p2"}
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 1, MaxBackoff: 1},
		Models: []config.ModelConfig{
			{Name: "m", Strategy: "fallback", Backends: []config.BackendConfig{
				{Provider: "p1", Model: "m1", Tier: 1},
				{Provider: "p2", Model: "m2", Tier: 2},
			}},
		},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	r.SetBudgetChecker(func(name string) bool { return name == "p1" })

	req := &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
	resp, provName, err := r.ChatCompletion(context.Background(), "m", req)
	if err != nil {
		t.Fatalf("expected failover success, got %v", err)
	}
	if resp.ID != "resp-p2" || provName != "p2" {
		t.Fatalf("expected p2 to serve, got %+v %q", resp, provName)
	}
	if p1.callCount.Load() != 0 {
		t.Fatalf("over-budget p1 must not be called: %d", p1.callCount.Load())
	}
	if !strings.Contains(resp.ID, "resp-p2") {
		t.Fatalf("unexpected response %+v", resp)
	}
}
