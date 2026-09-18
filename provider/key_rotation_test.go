package provider

import (
	"testing"

	"github.com/aigateway/config"
)

func testProviderWithKeys(t *testing.T, rotation string) *BaseProvider {
	t.Helper()
	cfg := config.ProviderConfig{
		Name:        "rr-test",
		Type:        "openai",
		BaseURL:     "http://127.0.0.1:9/v1",
		APIKeys:     []string{"key-A-1234567890", "key-B-1234567890", "key-C-1234567890"},
		KeyRotation: rotation,
	}
	p, err := NewProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewProviderFromConfig failed: %v", err)
	}
	up, ok := p.(UpstreamConfigProvider)
	if !ok {
		t.Fatalf("provider does not expose upstream config")
	}
	bp, ok := p.(*OpenAIProvider)
	if !ok {
		t.Fatalf("expected *OpenAIProvider, got %T", p)
	}
	_ = up
	return bp.BaseProvider
}

// Round-robin (default) must rotate through keys on consecutive calls.
func TestKeyRotationRoundRobin(t *testing.T) {
	b := testProviderWithKeys(t, "")
	seen := map[int]bool{}
	for i := 0; i < 6; i++ {
		k, err := b.NextAPIKey()
		if err != nil {
			t.Fatalf("NextAPIKey failed: %v", err)
		}
		seen[k.Index] = true
	}
	if len(seen) != 3 {
		t.Fatalf("round-robin should cycle all 3 keys, saw indexes %v", seen)
	}
}

// Sticky must always serve the first healthy key.
func TestKeyRotationSticky(t *testing.T) {
	b := testProviderWithKeys(t, "sticky")
	for i := 0; i < 6; i++ {
		k, err := b.NextAPIKey()
		if err != nil {
			t.Fatalf("NextAPIKey failed: %v", err)
		}
		if k.Index != 0 {
			t.Fatalf("sticky should always serve key index 0, got %d", k.Index)
		}
	}
}

// Sticky must fail over when the primary key is circuit-broken.
func TestKeyRotationStickyFailover(t *testing.T) {
	b := testProviderWithKeys(t, "sticky")
	k0, _ := b.NextAPIKey()
	k0.DisabledUntil.Store(1 << 62) // circuit-break primary far into the future
	k, err := b.NextAPIKey()
	if err != nil {
		t.Fatalf("NextAPIKey failed: %v", err)
	}
	if k.Index != 1 {
		t.Fatalf("sticky should fail over to key index 1, got %d", k.Index)
	}
}
