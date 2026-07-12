package usage

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ModelStats tracks usage for a single model.
type ModelStats struct {
	TotalRequests      atomic.Int64 `json:"-"`
	TotalPromptTokens  atomic.Int64 `json:"-"`
	TotalOutputTokens  atomic.Int64 `json:"-"`
	TotalTokens        atomic.Int64 `json:"-"`
	TotalErrors        atomic.Int64 `json:"-"`
	TotalCacheHits     atomic.Int64 `json:"-"`
}

// Record adds token usage to this model's stats.
func (s *ModelStats) Record(promptTokens, outputTokens int, cacheHit bool) {
	s.TotalRequests.Add(1)
	s.TotalPromptTokens.Add(int64(promptTokens))
	s.TotalOutputTokens.Add(int64(outputTokens))
	s.TotalTokens.Add(int64(promptTokens + outputTokens))
	if cacheHit {
		s.TotalCacheHits.Add(1)
	}
}

// RecordError increments the error counter.
func (s *ModelStats) RecordError() {
	s.TotalErrors.Add(1)
}

// Snapshot returns a JSON-serializable snapshot of the stats.
type ModelSnapshot struct {
	TotalRequests     int64 `json:"total_requests"`
	TotalPromptTokens int64 `json:"total_prompt_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	TotalErrors       int64 `json:"total_errors"`
	TotalCacheHits    int64 `json:"total_cache_hits"`
}

func (s *ModelStats) Snapshot() ModelSnapshot {
	return ModelSnapshot{
		TotalRequests:     s.TotalRequests.Load(),
		TotalPromptTokens: s.TotalPromptTokens.Load(),
		TotalOutputTokens: s.TotalOutputTokens.Load(),
		TotalTokens:       s.TotalTokens.Load(),
		TotalErrors:       s.TotalErrors.Load(),
		TotalCacheHits:    s.TotalCacheHits.Load(),
	}
}

// Tracker tracks usage across all providers, models, and API keys.
type Tracker struct {
	mu              sync.RWMutex
	byModel         map[string]*ModelStats // key: "provider/model"
	byKey           map[string]*ModelStats // key: apiKey prefix (first 8 chars)
	totalBytesSaved atomic.Int64           // total bytes saved by token saver
	started         time.Time
}

// NewTracker creates a new usage tracker.
func NewTracker() *Tracker {
	return &Tracker{
		byModel: make(map[string]*ModelStats),
		byKey:   make(map[string]*ModelStats),
		started: time.Now(),
	}
}

// getOrCreateModel returns (or creates) stats for a provider/model combo.
func (t *Tracker) getOrCreateModel(provider, model string) *ModelStats {
	key := provider + "/" + model
	t.mu.RLock()
	if s, ok := t.byModel[key]; ok {
		t.mu.RUnlock()
		return s
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	// Double-check after acquiring write lock
	if s, ok := t.byModel[key]; ok {
		return s
	}
	s := &ModelStats{}
	t.byModel[key] = s
	return s
}

// getOrCreateKey returns (or creates) stats for an API key.
func (t *Tracker) getOrCreateKey(apiKey string) *ModelStats {
	prefix := apiKey
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	t.mu.RLock()
	if s, ok := t.byKey[prefix]; ok {
		t.mu.RUnlock()
		return s
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.byKey[prefix]; ok {
		return s
	}
	s := &ModelStats{}
	t.byKey[prefix] = s
	return s
}

// RecordUsage records a completed request's token usage.
func (t *Tracker) RecordUsage(apiKey, provider, model string, promptTokens, outputTokens int, cacheHit bool) {
	t.getOrCreateModel(provider, model).Record(promptTokens, outputTokens, cacheHit)
	t.getOrCreateKey(apiKey).Record(promptTokens, outputTokens, cacheHit)
}

// RecordError records an error for a provider/model.
func (t *Tracker) RecordError(provider, model string) {
	t.getOrCreateModel(provider, model).RecordError()
}

// RecordTokenSaving records bytes saved by the token saver.
func (t *Tracker) RecordTokenSaving(bytesSaved int64) {
	if bytesSaved > 0 {
		t.totalBytesSaved.Add(bytesSaved)
	}
}

// StatsResponse is the JSON response for usage stats.
type StatsResponse struct {
	Uptime          string                     `json:"uptime"`
	ByModel         map[string]ModelSnapshot   `json:"by_model"`
	ByAPIKey        map[string]ModelSnapshot   `json:"by_api_key"`
	TokenSaverSaved int64                      `json:"token_saver_bytes_saved"`
}

// GetStats returns a snapshot of all usage statistics.
func (t *Tracker) GetStats() StatsResponse {
	t.mu.RLock()
	defer t.mu.RUnlock()

	byModel := make(map[string]ModelSnapshot, len(t.byModel))
	for k, v := range t.byModel {
		byModel[k] = v.Snapshot()
	}
	byKey := make(map[string]ModelSnapshot, len(t.byKey))
	for k, v := range t.byKey {
		byKey[k] = v.Snapshot()
	}

	return StatsResponse{
		Uptime:          time.Since(t.started).Round(time.Second).String(),
		ByModel:         byModel,
		ByAPIKey:        byKey,
		TokenSaverSaved: t.totalBytesSaved.Load(),
	}
}

// PersistUsage saves usage stats to a JSON file for persistence across restarts.
func (t *Tracker) PersistUsage(path string) error {
	stats := t.GetStats()
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
