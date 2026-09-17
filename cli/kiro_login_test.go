package cli

import (
	"testing"

	"github.com/aigateway/config"
)

func testKiroCfg() *config.Config {
	return &config.Config{}
}

func TestUpsertKiroProviderCreate(t *testing.T) {
	cfg := testKiroCfg()
	upsertKiroProvider(cfg, "kiro", KiroLoginResult{RefreshToken: "aorAAAAAGx", AuthMethod: "import"})
	p := cfg.GetProvider("kiro")
	if p == nil {
		t.Fatal("provider must be created")
	}
	if p.Type != "kiro" || p.RefreshToken != "aorAAAAAGx" {
		t.Fatalf("bad entry: %+v", p)
	}
	if p.BaseURL == "" {
		t.Fatal("base URL must be set")
	}
}

func TestUpsertKiroProviderMerge(t *testing.T) {
	cfg := testKiroCfg()
	upsertKiroProvider(cfg, "kiro", KiroLoginResult{APIKey: "k1", AuthMethod: "api_key"})
	upsertKiroProvider(cfg, "kiro", KiroLoginResult{RefreshToken: "aorAAAAAGy", AuthMethod: "google"})
	p := cfg.GetProvider("kiro")
	if p == nil {
		t.Fatal("provider must exist")
	}
	if len(p.APIKeys) != 1 || p.APIKeys[0] != "k1" {
		t.Fatalf("existing key must be kept: %+v", p.APIKeys)
	}
	if p.RefreshToken != "aorAAAAAGy" {
		t.Fatalf("refresh token must update: %+v", p)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("merged config must validate: %v", err)
	}
}

func TestKiroExtractCode(t *testing.T) {
	code, err := kiroExtractCode("kiro://kiro.kiroAgent/authenticate-success?code=CODE1&state=ST1", "ST1")
	if err != nil || code != "CODE1" {
		t.Fatalf("expected CODE1, got %q %v", code, err)
	}
	if _, err := kiroExtractCode("kiro://x/?code=C&state=WRONG", "ST1"); err == nil {
		t.Fatal("expected state mismatch error")
	}
	code, err = kiroExtractCode("  BARECODE  ", "ST1")
	if err != nil || code != "BARECODE" {
		t.Fatalf("bare code must pass through: %q %v", code, err)
	}
	if _, err := kiroExtractCode("kiro://x/?state=ST1", "ST1"); err == nil {
		t.Fatal("expected error without code")
	}
}

func TestKiroPKCE(t *testing.T) {
	v, c, s, err := kiroPKCE()
	if err != nil || v == "" || c == "" || s == "" {
		t.Fatalf("pkce failed: %v", err)
	}
	v2, c2, _, err := kiroPKCE()
	if err != nil || v2 == v || c2 == c {
		t.Fatal("pkce must be random")
	}
}
