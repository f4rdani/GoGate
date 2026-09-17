package config

import (
	"os"
	"path/filepath"
	"testing"
)

func testManagerCfg() *Config {
	return &Config{
		Server:    ServerConfig{AdminSecret: "x"},
		Providers: []ProviderConfig{{Name: "p1", Type: "openai", BaseURL: "http://x", APIKeys: []string{"k"}}},
		Models:    []ModelConfig{{Name: "m1", Provider: "p1", Model: "m1"}},
		APIKeys:   []APIKeyConfig{{Key: "sk-1", Name: "n", AllowedModels: []string{"*"}}},
	}
}

func TestManagerProviderCRUD(t *testing.T) {
	c := testManagerCfg()
	if err := c.AddProvider(ProviderConfig{Name: "p2", Type: "groq", BaseURL: "http://g", APIKeys: []string{"k"}}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := c.AddProvider(ProviderConfig{Name: "p2"}); err == nil {
		t.Fatal("duplicate add must fail")
	}
	if err := c.UpdateProvider(ProviderConfig{Name: "p2", Type: "groq", BaseURL: "http://g2", APIKeys: []string{"k"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := c.GetProvider("p2"); got == nil || got.BaseURL != "http://g2" {
		t.Fatalf("update must apply: %+v", got)
	}
	if err := c.UpdateProvider(ProviderConfig{Name: "nope"}); err == nil {
		t.Fatal("update missing must fail")
	}
}

func TestManagerModelCRUD(t *testing.T) {
	c := testManagerCfg()
	if err := c.AddModel(ModelConfig{Name: "m2", Provider: "p1", Model: "m2"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := c.AddModel(ModelConfig{Name: "m2", Provider: "p1", Model: "m2"}); err == nil {
		t.Fatal("duplicate add must fail")
	}
	if c.GetModel("nope") != nil {
		t.Fatal("missing model must be nil")
	}
}

func TestManagerAPIKeyCRUD(t *testing.T) {
	c := testManagerCfg()
	if err := c.AddAPIKey(APIKeyConfig{Key: "sk-2", Name: "n2"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := c.AddAPIKey(APIKeyConfig{Key: "sk-2"}); err == nil {
		t.Fatal("duplicate add must fail")
	}
	if got := c.GetAPIKey("sk-2"); got == nil || got.Name != "n2" {
		t.Fatalf("get must find: %+v", got)
	}
	if c.GetAPIKey("nope") != nil {
		t.Fatal("missing key must be nil")
	}
	if err := c.DeleteAPIKey("sk-2"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteAPIKey("sk-2"); err == nil {
		t.Fatal("double delete must fail")
	}
}

func TestSaveConfigRoundtrip(t *testing.T) {
	c := testManagerCfg()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := c.SaveConfig(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.GetProvider("p1") == nil || loaded.GetModel("m1") == nil || loaded.GetAPIKey("sk-1") == nil {
		t.Fatalf("roundtrip lost data: %+v", loaded)
	}
	if err := c.SaveConfig(filepath.Join(t.TempDir(), "nope", "c.yaml")); err == nil {
		t.Fatal("save to bad path must fail")
	}
	_ = os.Getenv("")
}

func TestIsDashboardEnabled(t *testing.T) {
	var s ServerConfig
	if !s.IsDashboardEnabled() {
		t.Fatal("nil must default true")
	}
	off := false
	s.DashboardEnabled = &off
	if s.IsDashboardEnabled() {
		t.Fatal("false must disable")
	}
}
