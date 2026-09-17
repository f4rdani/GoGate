package config

import (
	"os"
	"strings"
	"testing"
)

func TestValidateShortDuplicateKeyNoPanic(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{{Name: "p", Type: "openai", BaseURL: "http://x", APIKeys: []string{"k"}}},
		Models:    []ModelConfig{{Name: "m", Provider: "p", Model: "m"}},
		APIKeys:   []APIKeyConfig{{Key: "abc", Name: "a"}, {Key: "abc", Name: "b"}},
	}
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Validate panicked on short duplicate key: %v", rec)
		}
	}()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate api_key") {
		t.Fatalf("expected duplicate api_key error, got %v", err)
	}
}

func TestServerUsageFileDefault(t *testing.T) {
	path := t.TempDir() + "/cfg.yaml"
	content := "server:\n  admin_secret: x\nproviders:\n  - {name: p, type: openai, base_url: http://x, api_keys: [k]}\nmodels:\n  - {name: m, provider: p, model: m}\napi_keys:\n  - {key: sk-1, name: n}\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.UsageFile != "usage.json" {
		t.Fatalf("expected default usage.json, got %q", cfg.Server.UsageFile)
	}
}

func TestValidateTieredStrategyAccepted(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: "http://x", APIKeys: []string{"k"}},
			{Name: "p2", Type: "openai", BaseURL: "http://y", APIKeys: []string{"k"}},
		},
		Models: []ModelConfig{
			{Name: "m", Strategy: "tiered", Backends: []BackendConfig{
				{Provider: "p1", Model: "a", Tier: 1},
				{Provider: "p2", Model: "b", Tier: 3},
			}},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("tiered must be a valid strategy, got %v", err)
	}
}

func TestValidateOAuthRequiresTokenFields(t *testing.T) {
	base := []ProviderConfig{{Name: "oa", Type: "oauth", BaseURL: "http://u", TokenURL: "http://t", RefreshToken: "rt"}}
	models := []ModelConfig{{Name: "m", Provider: "oa", Model: "m"}}
	if err := (&Config{Providers: base, Models: models}).Validate(); err != nil {
		t.Fatalf("valid oauth must pass: %v", err)
	}
	noToken := []ProviderConfig{{Name: "oa", Type: "oauth", BaseURL: "http://u", RefreshToken: "rt"}}
	if err := (&Config{Providers: noToken, Models: models}).Validate(); err == nil {
		t.Fatal("expected error without token_url")
	}
	noRefresh := []ProviderConfig{{Name: "oa", Type: "oauth", BaseURL: "http://u", TokenURL: "http://t"}}
	if err := (&Config{Providers: noRefresh, Models: models}).Validate(); err == nil {
		t.Fatal("expected error without refresh_token")
	}
}

func TestValidateKiroCredentials(t *testing.T) {
	models := []ModelConfig{{Name: "m", Provider: "kr", Model: "claude-sonnet-4.5"}}
	keyed := []ProviderConfig{{Name: "kr", Type: "kiro", BaseURL: "http://x", APIKeys: []string{"k"}}}
	if err := (&Config{Providers: keyed, Models: models}).Validate(); err != nil {
		t.Fatalf("api-key kiro must pass: %v", err)
	}
	rt := []ProviderConfig{{Name: "kr", Type: "kiro", BaseURL: "http://x", RefreshToken: "aorAAAAAGx"}}
	if err := (&Config{Providers: rt, Models: models}).Validate(); err != nil {
		t.Fatalf("refresh-token kiro must pass: %v", err)
	}
	none := []ProviderConfig{{Name: "kr", Type: "kiro", BaseURL: "http://x"}}
	if err := (&Config{Providers: none, Models: models}).Validate(); err == nil {
		t.Fatal("expected error for credential-less kiro")
	}
	if !keyed[0].HasCredentials() || !rt[0].HasCredentials() || none[0].HasCredentials() {
		t.Fatal("HasCredentials mismatch")
	}
}

func TestValidateDuplicateModelName(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{{Name: "p", Type: "openai", BaseURL: "http://x", APIKeys: []string{"k"}}},
		Models: []ModelConfig{
			{Name: "m", Provider: "p", Model: "m"},
			{Name: "m", Provider: "p", Model: "m2"},
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate model name") {
		t.Fatalf("expected duplicate model name error, got %v", err)
	}
}
