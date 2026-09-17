package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aigateway/config"
)

func TestProviderError_IsRetryable(t *testing.T) {
	retryableCodes := []int{429, 500, 502, 503}
	for _, code := range retryableCodes {
		err := &ProviderError{StatusCode: code, Provider: "test"}
		if !err.IsRetryable() {
			t.Errorf("expected status %d to be retryable", code)
		}
	}

	nonRetryableCodes := []int{400, 401, 403, 404, 422}
	for _, code := range nonRetryableCodes {
		err := &ProviderError{StatusCode: code, Provider: "test"}
		if err.IsRetryable() {
			t.Errorf("expected status %d NOT to be retryable", code)
		}
	}
}

func TestResolveKeyAndURL(t *testing.T) {
	// Standard provider with trailing slash
	base := &BaseProvider{
		name:         "openai",
		providerType: "openai",
		baseURL:      "https://api.openai.com/v1/",
	}
	key, url := base.ResolveKeyAndURL("sk-123", "/chat/completions")
	if key != "sk-123" {
		t.Errorf("expected sk-123, got %s", key)
	}
	if url != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("unexpected URL: %s", url)
	}

	// Avoid duplicate path if baseURL already includes endpoint
	baseDuplicate := &BaseProvider{
		name:         "local",
		providerType: "custom",
		baseURL:      "http://localhost:11434/v1/chat/completions",
	}
	_, urlDup := baseDuplicate.ResolveKeyAndURL("sk-none", "/chat/completions")
	if urlDup != "http://localhost:11434/v1/chat/completions" {
		t.Errorf("duplicate path was not prevented: %s", urlDup)
	}

	// Cloudflare colon-separated accountID:apiKey
	baseCF := &BaseProvider{
		name:         "cf",
		providerType: "cloudflare",
		baseURL:      "",
	}
	cfKey, cfURL := baseCF.ResolveKeyAndURL("acc123:cf-token", "/chat/completions")
	if cfKey != "cf-token" {
		t.Errorf("expected extracted cf-token, got %s", cfKey)
	}
	if !strings.Contains(cfURL, "accounts/acc123/ai/v1/chat/completions") {
		t.Errorf("unexpected Cloudflare URL: %s", cfURL)
	}

	// RelayURL override
	baseRelay := &BaseProvider{
		name:         "openai-relay",
		providerType: "openai",
		baseURL:      "https://api.openai.com/v1",
		relayURL:     "https://my-relay.workers.dev",
	}
	_, relayURL := baseRelay.ResolveKeyAndURL("sk-123", "/chat/completions")
	if relayURL != "https://my-relay.workers.dev/chat/completions" {
		t.Errorf("expected relay URL, got %s", relayURL)
	}
}

func TestBaseProvider_NextAPIKeyCircuitBreaker(t *testing.T) {
	key1 := &UpstreamKey{Key: "key-1"}
	key2 := &UpstreamKey{Key: "key-2"}

	base := &BaseProvider{
		name:    "test",
		apiKeys: []*UpstreamKey{key1, key2},
	}

	// Normal round robin
	k, err := base.NextAPIKey()
	if err != nil || k == nil {
		t.Fatalf("NextAPIKey failed: %v", err)
	}

	// Disable key1 for 1 hour
	key1.DisabledUntil.Store(time.Now().Add(1 * time.Hour).UnixNano())

	// Next key should be key2
	for i := 0; i < 3; i++ {
		k, err = base.NextAPIKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if k.Key != "key-2" {
			t.Errorf("expected key-2 while key-1 is circuit broken, got %s", k.Key)
		}
	}

	// Disable key2 as well
	key2.DisabledUntil.Store(time.Now().Add(1 * time.Hour).UnixNano())

	// Now all keys are broken
	_, errAllBroken := base.NextAPIKey()
	if errAllBroken == nil {
		t.Errorf("expected error when all keys are circuit broken")
	}
}

func TestTranslateMessageContent_Base64(t *testing.T) {
	rawInput := json.RawMessage(`[
		{"type": "text", "text": "What is in this image?"},
		{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}
	]`)

	out, err := translateMessageContent(rawInput)
	if err != nil {
		t.Fatalf("translateMessageContent failed: %v", err)
	}

	outStr := string(out)
	if !strings.Contains(outStr, `"type":"image"`) || !strings.Contains(outStr, `"media_type":"image/png"`) {
		t.Errorf("expected Anthropic image block, got: %s", outStr)
	}
}

func TestIsSafeURL(t *testing.T) {
	if isSafeURL("http://127.0.0.1/admin") {
		t.Error("expected 127.0.0.1 to be blocked by SSRF check")
	}
	if isSafeURL("http://192.168.1.1/secret") {
		t.Error("expected private IP 192.168.1.1 to be blocked")
	}
	if isSafeURL("http://169.254.169.254/latest/meta-data") {
		t.Error("expected link-local IP to be blocked")
	}
	if isSafeURL("ftp://example.com/file") {
		t.Error("expected non-http(s) scheme to be blocked")
	}
	if !isSafeURL("https://www.google.com/images/branding/googlelogo/1x/googlelogo_color_272x92dp.png") {
		t.Error("expected public URL to be permitted")
	}
}

func TestNewProviderFromConfig(t *testing.T) {
	cfg := config.ProviderConfig{
		Name:     "openai-test",
		Type:     "openai",
		BaseURL:  "https://api.openai.com/v1",
		APIKeys:  []string{"sk-123"},
		Models:   []string{"gpt-4o"},
		RelayURL: "https://my-cf-worker.workers.dev",
	}

	p, err := NewProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewProviderFromConfig failed: %v", err)
	}
	if p.Name() != "openai-test" {
		t.Errorf("expected name openai-test, got %s", p.Name())
	}
	if !p.IsHealthy() {
		t.Errorf("expected new provider to start healthy")
	}
}
