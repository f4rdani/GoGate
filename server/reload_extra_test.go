package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aigateway/config"
	"github.com/aigateway/provider"
	"github.com/aigateway/relay"
)

func newTestProxyPool() *relay.ProxyPool {
	return relay.NewProxyPool(config.ProxyPoolConfig{Enabled: false})
}

func testServerCfg(upstream string) *config.Config {
	return &config.Config{
		Server:      config.ServerConfig{AdminSecret: "x"},
		Concurrency: config.ConcurrencyConfig{MaxConcurrent: 10, QueueDepth: 10, QueueTimeout: 1000000000},
		Cache:       config.CacheConfig{MaxSize: 10, TTL: 60},
		Retry:       config.RetryConfig{MaxRetries: 1, InitialBackoff: 1, MaxBackoff: 5},
		Providers: []config.ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: upstream, APIKeys: []string{"k1"}},
		},
		Models:  []config.ModelConfig{{Name: "m", Provider: "p1", Model: "m"}},
		APIKeys: []config.APIKeyConfig{{Key: "sk-1", Name: "n", AllowedModels: []string{"*"}}},
	}
}

func writeCfgFile(t *testing.T, cfg *config.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.SaveConfig(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	*cfg = *loaded
	return path
}

func TestReloadConfigSwapsStack(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv1.Close()

	cfg := testServerCfg(srv1.URL)
	path := writeCfgFile(t, cfg)

	srv, err := New(cfg, path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	before := srv.handler.GetLimiter().Capacity()
	// Change limits + add a provider via a FRESH struct (never mutate the
	// live server config — ReloadConfig diffs disk state against it).
	fresh, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Concurrency.MaxConcurrent = 5
	fresh.Providers = append(fresh.Providers, config.ProviderConfig{
		Name: "p2", Type: "openai", BaseURL: srv1.URL, APIKeys: []string{"k2"},
	})
	if err := fresh.SaveConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := srv.ReloadConfig(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if srv.handler.GetLimiter().Capacity() == before {
		t.Fatal("limiter must rebuild on limit change")
	}
	if _, ok := srv.registry.Get("p2"); !ok {
		t.Fatal("new provider must register")
	}
	if srv.handler.GetTracker() == nil {
		t.Fatal("tracker must survive reload")
	}

	// Broken config file must fail without mutating state.
	if err := os.WriteFile(path, []byte(":\tbad yaml ["), 0600); err != nil {
		t.Fatal(err)
	}
	if err := srv.ReloadConfig(); err == nil {
		t.Fatal("broken config must fail reload")
	}
	if _, ok := srv.registry.Get("p2"); !ok {
		t.Fatal("failed reload must keep old state")
	}
}

func TestWireEgressPool(t *testing.T) {
	p, err := provider.NewProviderFromConfig(config.ProviderConfig{
		Name: "oc", Type: "opencode", BaseURL: "https://opencode.ai/zen/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	up, ok := p.(provider.UpstreamConfigProvider)
	if !ok || up.Client() == nil {
		t.Fatal("needs upstream client")
	}
	pool := newTestProxyPool()
	wireEgressPool(p, config.ProviderConfig{Name: "oc", Type: "opencode"}, pool)
	if up.Client().Transport == nil {
		t.Fatal("transport must exist")
	}
	// Non-pool provider types must be left alone.
	p2, _ := provider.NewProviderFromConfig(config.ProviderConfig{
		Name: "o", Type: "openai", BaseURL: "https://api.openai.com/v1", APIKeys: []string{"k"},
	})
	up2 := p2.(provider.UpstreamConfigProvider)
	before := up2.Client().Transport
	wireEgressPool(p2, config.ProviderConfig{Name: "o", Type: "openai"}, pool)
	if up2.Client().Transport != before {
		t.Fatal("direct provider transport must not change")
	}
}

func TestHandleUsageStats(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv1.Close()
	cfg := testServerCfg(srv1.URL)
	srv, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}

	// Unauthorized.
	w := httptest.NewRecorder()
	handleUsageStats(w, httptest.NewRequest("GET", "/admin/usage", nil), srv.admin, srv.handler)
	if w.Code != 401 {
		t.Fatalf("must 401: %d", w.Code)
	}

	// Authorized with admin secret.
	req := httptest.NewRequest("GET", "/admin/usage", nil)
	req.Header.Set("X-Admin-Secret", "x")
	w2 := httptest.NewRecorder()
	handleUsageStats(w2, req, srv.admin, srv.handler)
	if w2.Code != 200 {
		t.Fatalf("must 200: %d %s", w2.Code, w2.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(w2.Body.Bytes(), &payload); err != nil {
		t.Fatalf("usage must be JSON: %v", err)
	}
	for _, key := range []string{"uptime", "by_model", "by_api_key", "cache", "estimated_cost_usd", "cost_by_provider", "monthly_tokens", "budgets", "token_saver_bytes_saved"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("usage response must include %q: %v", key, payload)
		}
	}
}

func TestPrefetchDynamicCatalogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"dyn-1"}]}`))
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	p, err := provider.NewProviderFromConfig(config.ProviderConfig{
		Name: "ocx", Type: "opencode", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("ocx", p)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefetchDynamicCatalogs(ctx, reg)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := provider.GetCachedDynamicModels("ocx"); len(got) == 1 && got[0] == "dyn-1" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog must prefetch: %v", provider.GetCachedDynamicModels("ocx"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
