package cache

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aigateway/models"
)

func newTestResp(content string) *models.ChatCompletionResponse {
	c, _ := json.Marshal(content)
	return &models.ChatCompletionResponse{
		ID:      "test-id",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "test-model",
		Choices: []models.Choice{
			{Index: 0, Message: &models.Message{Role: "assistant", Content: c}},
		},
	}
}

func TestNewCache(t *testing.T) {
	c := New(100, 5*time.Second)
	if c == nil {
		t.Fatal("New returned nil")
	}
	hits, misses, size := c.Stats()
	if hits != 0 || misses != 0 || size != 0 {
		t.Errorf("expected empty cache, got hits=%d misses=%d size=%d", hits, misses, size)
	}
}

func TestSetAndGet(t *testing.T) {
	c := New(10, 5*time.Second)
	resp := newTestResp("hello")

	c.Set("key1", resp)
	got := c.Get("key1")
	if got == nil {
		t.Fatal("Get returned nil for existing key")
	}
	if got.ID != "test-id" {
		t.Errorf("expected ID 'test-id', got '%s'", got.ID)
	}
}

func TestGetMiss(t *testing.T) {
	c := New(10, 5*time.Second)
	got := c.Get("nonexistent")
	if got != nil {
		t.Error("expected nil for missing key")
	}
	hits, misses, _ := c.Stats()
	if hits != 0 || misses != 1 {
		t.Errorf("expected hits=0 misses=1, got hits=%d misses=%d", hits, misses)
	}
}

func TestGetEmptyKey(t *testing.T) {
	c := New(10, 5*time.Second)
	got := c.Get("")
	if got != nil {
		t.Error("expected nil for empty key")
	}
}

func TestSetEmptyKey(t *testing.T) {
	c := New(10, 5*time.Second)
	resp := newTestResp("hello")
	c.Set("", resp) // should be no-op
	_, _, size := c.Stats()
	if size != 0 {
		t.Errorf("expected size=0, got %d", size)
	}
}

func TestSetNilResponse(t *testing.T) {
	c := New(10, 5*time.Second)
	c.Set("key1", nil) // should be no-op
	_, _, size := c.Stats()
	if size != 0 {
		t.Errorf("expected size=0, got %d", size)
	}
}

func TestCacheHitStats(t *testing.T) {
	c := New(10, 5*time.Second)
	resp := newTestResp("hello")
	c.Set("key1", resp)

	c.Get("key1") // hit
	c.Get("key1") // hit
	c.Get("key2") // miss

	hits, misses, _ := c.Stats()
	if hits != 2 {
		t.Errorf("expected 2 hits, got %d", hits)
	}
	if misses != 1 {
		t.Errorf("expected 1 miss, got %d", misses)
	}
}

func TestCacheUpdate(t *testing.T) {
	c := New(10, 5*time.Second)
	c.Set("key1", newTestResp("first"))
	c.Set("key1", newTestResp("second"))

	got := c.Get("key1")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	var content string
	json.Unmarshal(got.Choices[0].Message.Content, &content)
	if content != "second" {
		t.Errorf("expected 'second', got '%s'", content)
	}
	_, _, size := c.Stats()
	if size != 1 {
		t.Errorf("expected size=1 after update, got %d", size)
	}
}

func TestEviction(t *testing.T) {
	c := New(3, 5*time.Second) // capacity 3
	c.Set("a", newTestResp("a"))
	c.Set("b", newTestResp("b"))
	c.Set("c", newTestResp("c"))

	// Access 'a' to make it recently used
	c.Get("a")

	// Add 'd' — should evict 'b' (least recently used)
	c.Set("d", newTestResp("d"))

	if c.Get("b") != nil {
		t.Error("expected 'b' to be evicted")
	}
	if c.Get("a") == nil {
		t.Error("expected 'a' to still exist (was recently used)")
	}
	if c.Get("c") == nil {
		t.Error("expected 'c' to still exist")
	}
	if c.Get("d") == nil {
		t.Error("expected 'd' to exist")
	}
}

func TestTTLExpiration(t *testing.T) {
	c := New(10, 100*time.Millisecond) // 100ms TTL
	c.Set("key1", newTestResp("hello"))

	// Should exist immediately
	if c.Get("key1") == nil {
		t.Fatal("expected key to exist immediately after set")
	}

	// Wait for expiration
	time.Sleep(150 * time.Millisecond)

	if c.Get("key1") != nil {
		t.Error("expected key to be expired")
	}
}

func TestClear(t *testing.T) {
	c := New(10, 5*time.Second)
	c.Set("a", newTestResp("a"))
	c.Set("b", newTestResp("b"))

	c.Clear()
	_, _, size := c.Stats()
	if size != 0 {
		t.Errorf("expected size=0 after clear, got %d", size)
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(100, 5*time.Second)
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key" + string(rune('A'+i%26))
			c.Set(key, newTestResp("value"))
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key" + string(rune('A'+i%26))
			c.Get(key)
		}(i)
	}

	wg.Wait()
	// Should not panic
}

func TestHashRequest(t *testing.T) {
	temp := 0.7
	maxTok := 100

	req1 := &models.ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []models.Message{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	req2 := &models.ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []models.Message{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	// Same request should produce same hash
	hash1 := HashRequest(req1)
	hash2 := HashRequest(req2)
	if hash1 != hash2 {
		t.Error("identical requests should produce same hash")
	}
	if hash1 == "" {
		t.Error("hash should not be empty for non-streaming request")
	}

	// Streaming request should produce empty hash
	streamReq := &models.ChatCompletionRequest{Stream: true}
	if HashRequest(streamReq) != "" {
		t.Error("streaming request should produce empty hash")
	}

	// Different model should produce different hash
	req3 := &models.ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: req1.Messages,
	}
	if HashRequest(req1) == HashRequest(req3) {
		t.Error("different models should produce different hashes")
	}
}
