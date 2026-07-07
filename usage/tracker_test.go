package usage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNewTracker(t *testing.T) {
	tr := NewTracker()
	if tr == nil {
		t.Fatal("NewTracker returned nil")
	}
	stats := tr.GetStats()
	if len(stats.ByModel) != 0 {
		t.Errorf("expected 0 models, got %d", len(stats.ByModel))
	}
	if len(stats.ByAPIKey) != 0 {
		t.Errorf("expected 0 API keys, got %d", len(stats.ByAPIKey))
	}
	if stats.Uptime == "" {
		t.Error("uptime should not be empty")
	}
}

func TestRecordUsage(t *testing.T) {
	tr := NewTracker()
	tr.RecordUsage("sk-gw-test-key", "openai", "gpt-4o", 100, 50, false)

	stats := tr.GetStats()

	// Check by model
	modelKey := "openai/gpt-4o"
	snapshot, ok := stats.ByModel[modelKey]
	if !ok {
		t.Fatalf("expected model key '%s' in stats", modelKey)
	}
	if snapshot.TotalRequests != 1 {
		t.Errorf("expected 1 request, got %d", snapshot.TotalRequests)
	}
	if snapshot.TotalPromptTokens != 100 {
		t.Errorf("expected 100 prompt tokens, got %d", snapshot.TotalPromptTokens)
	}
	if snapshot.TotalOutputTokens != 50 {
		t.Errorf("expected 50 output tokens, got %d", snapshot.TotalOutputTokens)
	}
	if snapshot.TotalTokens != 150 {
		t.Errorf("expected 150 total tokens, got %d", snapshot.TotalTokens)
	}
	if snapshot.TotalCacheHits != 0 {
		t.Errorf("expected 0 cache hits, got %d", snapshot.TotalCacheHits)
	}
}

func TestRecordUsageCacheHit(t *testing.T) {
	tr := NewTracker()
	tr.RecordUsage("sk-gw-test-key", "cache", "gpt-4o", 0, 0, true)

	stats := tr.GetStats()
	modelKey := "cache/gpt-4o"
	snapshot, ok := stats.ByModel[modelKey]
	if !ok {
		t.Fatalf("expected model key '%s'", modelKey)
	}
	if snapshot.TotalCacheHits != 1 {
		t.Errorf("expected 1 cache hit, got %d", snapshot.TotalCacheHits)
	}
}

func TestRecordError(t *testing.T) {
	tr := NewTracker()
	tr.RecordError("openai", "gpt-4o")
	tr.RecordError("openai", "gpt-4o")

	stats := tr.GetStats()
	snapshot := stats.ByModel["openai/gpt-4o"]
	if snapshot.TotalErrors != 2 {
		t.Errorf("expected 2 errors, got %d", snapshot.TotalErrors)
	}
}

func TestMultipleModels(t *testing.T) {
	tr := NewTracker()
	tr.RecordUsage("key1", "openai", "gpt-4o", 100, 50, false)
	tr.RecordUsage("key2", "anthropic", "claude-sonnet", 200, 100, false)
	tr.RecordUsage("key1", "groq", "llama-70b", 50, 30, false)

	stats := tr.GetStats()
	if len(stats.ByModel) != 3 {
		t.Errorf("expected 3 models, got %d", len(stats.ByModel))
	}
}

func TestAPIKeyTracking(t *testing.T) {
	tr := NewTracker()
	tr.RecordUsage("sk-gw-admin-key-12345", "openai", "gpt-4o", 100, 50, false)

	stats := tr.GetStats()
	// Key prefix is first 8 chars
	keyPrefix := "sk-gw-ad"
	snapshot, ok := stats.ByAPIKey[keyPrefix]
	if !ok {
		t.Fatalf("expected API key prefix '%s' in stats", keyPrefix)
	}
	if snapshot.TotalRequests != 1 {
		t.Errorf("expected 1 request for key, got %d", snapshot.TotalRequests)
	}
}

func TestAccumulation(t *testing.T) {
	tr := NewTracker()
	for i := 0; i < 100; i++ {
		tr.RecordUsage("key", "openai", "gpt-4o", 10, 5, false)
	}

	stats := tr.GetStats()
	snapshot := stats.ByModel["openai/gpt-4o"]
	if snapshot.TotalRequests != 100 {
		t.Errorf("expected 100 requests, got %d", snapshot.TotalRequests)
	}
	if snapshot.TotalTokens != 1500 {
		t.Errorf("expected 1500 total tokens, got %d", snapshot.TotalTokens)
	}
}

func TestConcurrentUsage(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.RecordUsage("key", "openai", "gpt-4o", 10, 5, false)
		}()
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.RecordError("openai", "gpt-4o")
		}()
	}
	wg.Wait()

	stats := tr.GetStats()
	snapshot := stats.ByModel["openai/gpt-4o"]
	if snapshot.TotalRequests != 100 {
		t.Errorf("expected 100 requests, got %d", snapshot.TotalRequests)
	}
	if snapshot.TotalErrors != 50 {
		t.Errorf("expected 50 errors, got %d", snapshot.TotalErrors)
	}
}

func TestPersistUsage(t *testing.T) {
	tr := NewTracker()
	tr.RecordUsage("key", "openai", "gpt-4o", 100, 50, false)

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "usage.json")

	if err := tr.PersistUsage(path); err != nil {
		t.Fatalf("PersistUsage failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read persisted file: %v", err)
	}

	if len(data) == 0 {
		t.Error("persisted file is empty")
	}

	// Verify it's valid JSON containing our data
	content := string(data)
	if !contains(content, "openai/gpt-4o") {
		t.Error("persisted JSON should contain model key")
	}
}

func TestModelStatsSnapshot(t *testing.T) {
	ms := &ModelStats{}
	ms.Record(100, 50, true)
	ms.Record(200, 100, false)
	ms.RecordError()

	snap := ms.Snapshot()
	if snap.TotalRequests != 2 {
		t.Errorf("expected 2 requests, got %d", snap.TotalRequests)
	}
	if snap.TotalPromptTokens != 300 {
		t.Errorf("expected 300 prompt tokens, got %d", snap.TotalPromptTokens)
	}
	if snap.TotalOutputTokens != 150 {
		t.Errorf("expected 150 output tokens, got %d", snap.TotalOutputTokens)
	}
	if snap.TotalTokens != 450 {
		t.Errorf("expected 450 total tokens, got %d", snap.TotalTokens)
	}
	if snap.TotalErrors != 1 {
		t.Errorf("expected 1 error, got %d", snap.TotalErrors)
	}
	if snap.TotalCacheHits != 1 {
		t.Errorf("expected 1 cache hit, got %d", snap.TotalCacheHits)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
