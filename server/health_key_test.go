package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/provider"
)

// catalog server: first key is dead (401), second key works.
func TestCheckProviderTriesAllKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "dead-key") {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"dead-key", "good-key"}},
		},
	}
	reg := provider.NewRegistry()
	p, err := provider.NewProviderFromConfig(cfg.Providers[0])
	if err != nil {
		t.Fatal(err)
	}
	p.SetHealthy(false) // start unhealthy; a good sibling key must revive it
	reg.Register("p1", p)

	s := &Server{cfg: cfg, registry: reg}
	s.checkProvider("p1")
	if got, ok := reg.Get("p1"); !ok || !got.IsHealthy() {
		t.Fatal("provider must be healthy when any key answers")
	}
}

func TestOAuthProviderWithoutKeysRegisters(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{AdminSecret: "x"},
		Concurrency: config.ConcurrencyConfig{
			MaxConcurrent: 10, QueueDepth: 10, QueueTimeout: 1000000000,
		},
		Cache: config.CacheConfig{MaxSize: 10, TTL: 60},
		Retry: config.RetryConfig{MaxRetries: 1, InitialBackoff: 1, MaxBackoff: 5},
		Providers: []config.ProviderConfig{
			{Name: "oa", Type: "oauth", BaseURL: "http://127.0.0.1:9", TokenURL: "http://127.0.0.1:9/t", RefreshToken: "rt"},
		},
		Models:  []config.ModelConfig{{Name: "m", Provider: "oa", Model: "m"}},
		APIKeys: []config.APIKeyConfig{{Key: "sk-1", Name: "n"}},
	}
	srv, err := New(cfg, "")
	if err != nil {
		t.Fatalf("server with keyless oauth provider must init: %v", err)
	}
	if _, ok := srv.registry.Get("oa"); !ok {
		t.Fatal("oauth provider must be registered despite having no static keys")
	}
}

func TestKiroRefreshOnlyRegisters(t *testing.T) {
	cfg := &config.Config{
		Server:      config.ServerConfig{AdminSecret: "x"},
		Concurrency: config.ConcurrencyConfig{MaxConcurrent: 10, QueueDepth: 10, QueueTimeout: 1000000000},
		Cache:       config.CacheConfig{MaxSize: 10, TTL: 60},
		Retry:       config.RetryConfig{MaxRetries: 1, InitialBackoff: 1, MaxBackoff: 5},
		Providers: []config.ProviderConfig{
			{Name: "kr", Type: "kiro", BaseURL: "http://127.0.0.1:9", RefreshToken: "aorAAAAAGx"},
		},
		Models:  []config.ModelConfig{{Name: "m", Provider: "kr", Model: "claude-sonnet-4.5"}},
		APIKeys: []config.APIKeyConfig{{Key: "sk-1", Name: "n"}},
	}
	srv, err := New(cfg, "")
	if err != nil {
		t.Fatalf("keyless refresh-token kiro must init: %v", err)
	}
	if _, ok := srv.registry.Get("kr"); !ok {
		t.Fatal("kiro provider must be registered")
	}
}

func TestCheckProviderUnhealthyWhenAllKeysDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"k1", "k2"}},
		},
	}
	reg := provider.NewRegistry()
	p, err := provider.NewProviderFromConfig(cfg.Providers[0])
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("p1", p)

	s := &Server{cfg: cfg, registry: reg}
	s.checkProvider("p1")
	if got, ok := reg.Get("p1"); !ok || got.IsHealthy() {
		t.Fatal("provider must be unhealthy when no key answers")
	}
}
