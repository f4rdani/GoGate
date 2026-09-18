package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Basic(t *testing.T) {
	yamlContent := `
server:
  host: "127.0.0.1"
  port: 9090
  admin_secret: "secret123"
concurrency:
  max_concurrent: 40
providers:
  - name: test-openai
    type: openai
    base_url: https://api.openai.com/v1
    api_keys: ["sk-test"]
models:
  - name: gpt-4
    provider: test-openai
    model: gpt-4
api_keys:
  - key: sk-client-1
    name: Client 1
    allowed_models: ["gpt-4"]
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 9090 {
		t.Errorf("unexpected server host/port: %s:%d", cfg.Server.Host, cfg.Server.Port)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "test-openai" {
		t.Errorf("unexpected providers: %+v", cfg.Providers)
	}
	if len(cfg.Models) != 1 || cfg.Models[0].Name != "gpt-4" {
		t.Errorf("unexpected models: %+v", cfg.Models)
	}
}

func TestLoadConfig_EnvExpansion(t *testing.T) {
	os.Setenv("TEST_GATEWAY_PORT", "9999")
	os.Setenv("TEST_GATEWAY_SECRET", "my-env-secret")
	defer os.Unsetenv("TEST_GATEWAY_PORT")
	defer os.Unsetenv("TEST_GATEWAY_SECRET")

	yamlContent := `
server:
  port: ${TEST_GATEWAY_PORT}
  admin_secret: ${TEST_GATEWAY_SECRET}
providers:
  - name: test-p
    type: openai
    base_url: https://api.openai.com/v1
    api_keys: ["sk-test"]
models:
  - name: m1
    provider: test-p
    model: m1
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Server.Port != 9999 {
		t.Errorf("expected expanded port 9999, got %d", cfg.Server.Port)
	}
	if cfg.Server.AdminSecret != "my-env-secret" {
		t.Errorf("expected expanded secret 'my-env-secret', got %s", cfg.Server.AdminSecret)
	}
}

func TestConfig_Validation(t *testing.T) {
	// No providers
	cfg := &Config{}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty providers")
	}

	// Duplicate provider
	cfg = &Config{
		Providers: []ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: "https://p1.com"},
			{Name: "p1", Type: "openai", BaseURL: "https://p1.com"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for duplicate provider name")
	}

	// Invalid model strategy
	cfg = &Config{
		Providers: []ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: "https://p1.com"},
		},
		Models: []ModelConfig{
			{Name: "m1", Strategy: "invalid-strat"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid strategy")
	}
}

func TestConfig_CascadeOperations(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "prov1", Type: "openai", BaseURL: "https://api1.com", Models: []string{"mod1"}},
			{Name: "prov2", Type: "openai", BaseURL: "https://api2.com", Models: []string{"mod2"}},
		},
		Models: []ModelConfig{
			{Name: "alias1", Provider: "prov1", Model: "mod1"},
			{
				Name:     "combo1",
				Strategy: "round-robin",
				Backends: []BackendConfig{
					{Provider: "prov1", Model: "mod1"},
					{Provider: "prov2", Model: "mod2"},
				},
			},
		},
		APIKeys: []APIKeyConfig{
			{Key: "sk-key1", Name: "Key 1", AllowedModels: []string{"alias1", "combo1"}},
		},
	}

	// Test DeleteModel
	err := cfg.DeleteModel("alias1")
	if err != nil {
		t.Fatalf("DeleteModel failed: %v", err)
	}
	if cfg.GetModel("alias1") != nil {
		t.Errorf("expected alias1 to be deleted")
	}
	// Check API key allowed models cascade
	if len(cfg.APIKeys[0].AllowedModels) != 1 || cfg.APIKeys[0].AllowedModels[0] != "combo1" {
		t.Errorf("cascade failed on APIKeys, got: %v", cfg.APIKeys[0].AllowedModels)
	}

	// Test RenameModelCascade
	cfg.RenameModelCascade("combo1", "renamed-combo")
	if cfg.APIKeys[0].AllowedModels[0] != "renamed-combo" {
		t.Errorf("expected renamed-combo in API key, got %s", cfg.APIKeys[0].AllowedModels[0])
	}

	// Test DeleteProvider
	routesRemoved, combosRemoved, err := cfg.DeleteProvider("prov1")
	if err != nil {
		t.Fatalf("DeleteProvider failed: %v", err)
	}
	if cfg.GetProvider("prov1") != nil {
		t.Errorf("provider prov1 still exists")
	}
	_ = routesRemoved
	_ = combosRemoved
}

func TestNormalizeProxyURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"   ", ""},
		{"1.2.3.4:8080", "http://1.2.3.4:8080"},
		{"http://1.2.3.4:8080", "http://1.2.3.4:8080"},
		{"socks5://1.2.3.4:1080", "socks5://1.2.3.4:1080"},
		{"31.59.20.176:6754:user:pass", "http://user:pass@31.59.20.176:6754"},
		{"socks5://31.59.20.176:6754:user:pass", "socks5://user:pass@31.59.20.176:6754"},
		{"http://user:pass@31.59.20.176:6754", "http://user:pass@31.59.20.176:6754"},
	}

	for _, tt := range tests {
		got := NormalizeProxyURL(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeProxyURL(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
