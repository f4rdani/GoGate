package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aigateway/balancer"
	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

// upstreamMock is a mockProvider that also exposes upstream endpoint details
// so dynamic catalog fetching and header paths can be exercised.
type upstreamMock struct {
	*mockProvider
	ptype   string
	baseURL string
	client  *http.Client
	keys    []*provider.UpstreamKey
}

func (m *upstreamMock) ProviderType() string                { return m.ptype }
func (m *upstreamMock) BaseURL() string                     { return m.baseURL }
func (m *upstreamMock) Client() *http.Client                { return m.client }
func (m *upstreamMock) APIKeys() []*provider.UpstreamKey    { return m.keys }

func newUpstreamMock(name, baseURL string, keys ...string) *upstreamMock {
	uks := make([]*provider.UpstreamKey, 0, len(keys))
	for _, k := range keys {
		uks = append(uks, &provider.UpstreamKey{Key: k})
	}
	return &upstreamMock{
		mockProvider: newMockProvider(name),
		ptype:        "openai",
		baseURL:      baseURL,
		client:       &http.Client{},
		keys:         uks,
	}
}

func TestDynamicAutoLiveFetch(t *testing.T) {
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("catalog path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"dyn-live"}]}`))
	}))
	defer catalog.Close()

	registry := provider.NewRegistry()
	up := newUpstreamMock("oc", catalog.URL, "k1")
	up.responses["dyn-live"] = &models.ChatCompletionResponse{ID: "resp-dyn-live"}
	registry.Register("oc", up)

	cfg := &config.Config{
		Retry:  config.RetryConfig{MaxRetries: 0, InitialBackoff: 1, MaxBackoff: 1},
		Models: []config.ModelConfig{{Name: "oc/auto", Provider: "oc", Model: "oc/auto"}},
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
		t.Fatalf("live catalog routing failed: %v", err)
	}
	if resp.ID != "resp-dyn-live" || provName != "oc" {
		t.Fatalf("unexpected result: %+v %q", resp, provName)
	}
	if cached := provider.GetCachedDynamicModels("oc"); len(cached) != 1 || cached[0] != "dyn-live" {
		t.Fatalf("catalog must be cached: %v", cached)
	}
	provider.SetCachedDynamicModels("oc", nil)
}

func TestStreamRoundRobinFailover(t *testing.T) {
	registry := provider.NewRegistry()
	p1 := newMockProvider("p1")
	p1.errors["m1"] = errStreamBoom()
	p2 := newMockProvider("p2")
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "rr", Strategy: "round-robin",
			Backends: []config.BackendConfig{{Provider: "p1", Model: "m1"}, {Provider: "p2", Model: "m2"}},
		}},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	// Force starting backend to the failing one for determinism.
	r.routes["rr"].Balancer = balancer.New([]int{0, 1})

	w := httptest.NewRecorder()
	err = r.ChatCompletionStream(context.Background(), "rr",
		&models.ChatCompletionRequest{Model: "rr"}, w, w)
	if err != nil {
		t.Fatalf("failover must succeed: %v", err)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("fallback backend must stream: %q", w.Body.String())
	}
}

func TestStreamFallbackTiers(t *testing.T) {
	registry := provider.NewRegistry()
	p1 := newMockProvider("p1")
	p1.healthy = false
	p2 := newMockProvider("p2")
	registry.Register("p1", p1)
	registry.Register("p2", p2)

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "fb", Strategy: "fallback",
			Backends: []config.BackendConfig{{Provider: "p1", Model: "m1", Tier: 1}, {Provider: "p2", Model: "m2", Tier: 2}},
		}},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err := r.ChatCompletionStream(context.Background(), "fb",
		&models.ChatCompletionRequest{Model: "fb"}, w, w); err != nil {
		t.Fatalf("tier fallback must succeed: %v", err)
	}
	if p1.callCount.Load() != 0 || p2.callCount.Load() != 1 {
		t.Fatalf("unhealthy tier-1 must be skipped: p1=%d p2=%d", p1.callCount.Load(), p2.callCount.Load())
	}
}
