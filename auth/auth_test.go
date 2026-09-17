package auth

import (
	"sync"
	"testing"

	"github.com/aigateway/config"
)

func TestKeyStore_Basic(t *testing.T) {
	configs := []config.APIKeyConfig{
		{
			Key:           "sk-test-1",
			Name:          "Test Key 1",
			AllowedModels: []string{"gpt-4", "claude-3"},
			RateLimit:     100,
			Disabled:      false,
		},
		{
			Key:           "sk-test-disabled",
			Name:          "Disabled Key",
			AllowedModels: []string{"*"},
			Disabled:      true,
		},
	}

	ks := NewKeyStore(configs)

	// Test valid key
	info, ok := ks.Validate("sk-test-1")
	if !ok || info == nil {
		t.Fatalf("expected key to be valid")
	}
	if info.Name != "Test Key 1" {
		t.Errorf("expected name 'Test Key 1', got %s", info.Name)
	}
	if !info.IsModelAllowed("gpt-4") {
		t.Errorf("expected gpt-4 to be allowed")
	}
	if info.IsModelAllowed("gemini") {
		t.Errorf("expected gemini not to be allowed")
	}

	// Test disabled key
	_, okDisabled := ks.Validate("sk-test-disabled")
	if okDisabled {
		t.Errorf("expected disabled key to be invalid")
	}

	// Test non-existent key
	_, okNone := ks.Validate("sk-non-existent")
	if okNone {
		t.Errorf("expected non-existent key to be invalid")
	}
}

func TestKeyStore_AddAndDelete(t *testing.T) {
	ks := NewKeyStore(nil)

	info := ks.AddKey("New Key", []string{"*"}, 60, nil)
	if info == nil || info.Key == "" {
		t.Fatalf("expected AddKey to return valid key info")
	}

	retrieved, ok := ks.Validate(info.Key)
	if !ok || retrieved == nil {
		t.Fatalf("failed to validate newly added key")
	}

	hash := HashKey(info.Key)
	deleted := ks.DeleteKey(hash)
	if !deleted {
		t.Errorf("expected DeleteKey to return true")
	}

	_, okAfterDelete := ks.Validate(info.Key)
	if okAfterDelete {
		t.Errorf("key still valid after deletion")
	}
}

func TestKeyStore_UpdateKey(t *testing.T) {
	ks := NewKeyStore(nil)
	info := ks.AddKey("Original Name", []string{"gpt-4"}, 10, nil)
	hash := HashKey(info.Key)

	updated := ks.UpdateKey(hash, "Updated Name", []string{"*"}, 50, nil, false)
	if !updated {
		t.Fatalf("expected UpdateKey to return true")
	}

	retrieved, ok := ks.Validate(info.Key)
	if !ok || retrieved.Name != "Updated Name" {
		t.Errorf("update did not reflect, got name: %v", retrieved.Name)
	}
	if retrieved.RateLimit != 50 {
		t.Errorf("expected rate limit 50, got %d", retrieved.RateLimit)
	}
	if !retrieved.IsModelAllowed("any-model") {
		t.Errorf("expected wildcard allowed model")
	}
}

func TestKeyInfo_SlidingWindowRateLimit(t *testing.T) {
	key := &KeyInfo{
		Key:       "sk-limit-test",
		RateLimit: 5,
	}

	// First 5 requests should pass
	for i := 0; i < 5; i++ {
		if !key.CheckRateLimit() {
			t.Fatalf("request %d should have passed rate limit", i+1)
		}
	}

	// 6th request should fail
	if key.CheckRateLimit() {
		t.Fatalf("request 6 should have exceeded rate limit of 5")
	}
}

func TestKeyInfo_ConcurrentRateLimit(t *testing.T) {
	limit := 50
	key := &KeyInfo{
		Key:       "sk-concurrent-test",
		RateLimit: limit,
	}

	var passed sync.WaitGroup
	var passedCount int
	var mu sync.Mutex

	totalGoroutines := 100
	passed.Add(totalGoroutines)

	for i := 0; i < totalGoroutines; i++ {
		go func() {
			defer passed.Done()
			if key.CheckRateLimit() {
				mu.Lock()
				passedCount++
				mu.Unlock()
			}
		}()
	}

	passed.Wait()

	if passedCount > limit {
		t.Errorf("expected at most %d requests to pass, got %d", limit, passedCount)
	}
}

func TestKeyInfo_TokenSaverEnabled(t *testing.T) {
	keyDefault := &KeyInfo{}
	if !keyDefault.IsTokenSaverEnabled(true) {
		t.Errorf("expected key to follow global true")
	}
	if keyDefault.IsTokenSaverEnabled(false) {
		t.Errorf("expected key to follow global false")
	}

	bTrue := true
	keyOverrideTrue := &KeyInfo{TokenSaver: &bTrue}
	if !keyOverrideTrue.IsTokenSaverEnabled(false) {
		t.Errorf("expected override true to beat global false")
	}

	bFalse := false
	keyOverrideFalse := &KeyInfo{TokenSaver: &bFalse}
	if keyOverrideFalse.IsTokenSaverEnabled(true) {
		t.Errorf("expected override false to beat global true")
	}
}
