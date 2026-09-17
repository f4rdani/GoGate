package router

import (
	"context"
	"strings"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

// The mock provider in router_test does not expose an upstream endpoint, so an
// auto route with an empty cache must fail with a catalog error — and must
// never silently fall back to a hardcoded model list.
func TestAutoRouteWithoutCatalogErrors(t *testing.T) {
	registry := provider.NewRegistry()
	mp := newMockProvider("oc-mock")
	registry.Register("oc-mock", mp)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 1, MaxBackoff: 1},
		Models: []config.ModelConfig{
			{Name: "oc/auto", Provider: "oc-mock", Model: "oc/auto"},
		},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}

	req := &models.ChatCompletionRequest{
		Model:    "oc/auto",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
	_, _, err = r.ChatCompletion(context.Background(), "oc/auto", req)
	if err == nil || !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("expected catalog error, got %v", err)
	}
	if mp.callCount.Load() != 0 {
		t.Fatalf("no upstream call should happen without a catalog, got %d", mp.callCount.Load())
	}
}

// A cached catalog (as filled by live prefetch or /v1/models listing) is used verbatim.
func TestAutoRouteUsesCachedCatalog(t *testing.T) {
	registry := provider.NewRegistry()
	mp := newMockProvider("oc-mock")
	mp.responses["dyn-model-7"] = &models.ChatCompletionResponse{ID: "resp-dyn"}
	registry.Register("oc-mock", mp)

	provider.SetCachedDynamicModels("oc-mock", []string{"dyn-model-7"})
	defer provider.SetCachedDynamicModels("oc-mock", nil)

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 0, InitialBackoff: 1, MaxBackoff: 1},
		Models: []config.ModelConfig{
			{Name: "oc/auto", Provider: "oc-mock", Model: "oc/auto"},
		},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}

	req := &models.ChatCompletionRequest{
		Model:    "oc/auto",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
	resp, provName, err := r.ChatCompletion(context.Background(), "oc/auto", req)
	if err != nil {
		t.Fatalf("expected success from cached catalog, got %v", err)
	}
	if resp.ID != "resp-dyn" || provName != "oc-mock" {
		t.Fatalf("unexpected routing result: %+v %q", resp, provName)
	}
}
