package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aigateway/config"
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

	prices   map[string]config.ModelPrice // model -> USD per 1M tokens (user-configured)
	budgets  map[string]int64             // provider -> monthly token cap (0 = unlimited)
	costMic  map[string]*atomic.Int64     // provider -> estimated micro-dollars
	monthly  map[string]*atomic.Int64     // "2006-01|provider" -> tokens this month
	totalMic atomic.Int64                 // total estimated micro-dollars
}

// NewTracker creates a new usage tracker.
func NewTracker() *Tracker {
	return &Tracker{
		byModel: make(map[string]*ModelStats),
		byKey:   make(map[string]*ModelStats),
		prices:  make(map[string]config.ModelPrice),
		budgets: make(map[string]int64),
		costMic: make(map[string]*atomic.Int64),
		monthly: make(map[string]*atomic.Int64),
		started: time.Now(),
	}
}

// SetPrices replaces the model price table used for cost estimation.
func (t *Tracker) SetPrices(prices map[string]config.ModelPrice) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prices == nil {
		prices = make(map[string]config.ModelPrice)
	}
	t.prices = prices
}

// SetBudgets replaces the per-provider monthly token budgets (0 = unlimited).
func (t *Tracker) SetBudgets(budgets []config.BudgetConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := make(map[string]int64, len(budgets))
	for _, b := range budgets {
		if b.Provider != "" && b.MonthlyTokens > 0 {
			m[b.Provider] = b.MonthlyTokens
		}
	}
	t.budgets = m
}

// monthKey returns the monthly bucket key for a provider.
func monthKey(provider string) string {
	return time.Now().Format("2006-01") + "|" + provider
}

// pruneMonthly drops buckets older than the previous month (keeps memory bounded).
func (t *Tracker) pruneMonthly() {
	now := time.Now()
	keep := map[string]bool{
		now.Format("2006-01"):                   true,
		now.AddDate(0, -1, 0).Format("2006-01"): true,
	}
	for k := range t.monthly {
		if len(k) < 7 || !keep[k[:7]] {
			delete(t.monthly, k)
		}
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

	t.mu.RLock()
	price, hasPrice := t.prices[model]
	t.mu.RUnlock()

	t.mu.Lock()
	mk := monthKey(provider)
	ctr, ok := t.monthly[mk]
	if !ok {
		ctr = &atomic.Int64{}
		t.monthly[mk] = ctr
		t.pruneMonthly()
	}
	ctr.Add(int64(promptTokens + outputTokens))
	if hasPrice && (promptTokens+outputTokens) > 0 {
		micros := int64(float64(promptTokens)*price.InputPer1M/1e6*1e6 + float64(outputTokens)*price.OutputPer1M/1e6*1e6)
		cc, ok := t.costMic[provider]
		if !ok {
			cc = &atomic.Int64{}
			t.costMic[provider] = cc
		}
		cc.Add(micros)
		t.totalMic.Add(micros)
	}
	t.mu.Unlock()
}

// MonthlyUsage returns tokens consumed by a provider in the current month.
func (t *Tracker) MonthlyUsage(provider string) int64 {
	t.mu.RLock()
	ctr, ok := t.monthly[monthKey(provider)]
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	return ctr.Load()
}

// OverBudget reports whether a provider exhausted its monthly token budget.
func (t *Tracker) OverBudget(provider string) bool {
	t.mu.RLock()
	budget, ok := t.budgets[provider]
	t.mu.RUnlock()
	if !ok || budget <= 0 {
		return false
	}
	return t.MonthlyUsage(provider) >= budget
}

// BudgetStatus describes budget consumption per provider.
type BudgetStatus struct {
	Budget    int64  `json:"budget"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	Over      bool   `json:"over"`
	ResetsIn  string `json:"resets_in"`
}

// monthEndsIn returns a human countdown until the monthly bucket rolls over.
func monthEndsIn() string {
	now := time.Now()
	monthEnd := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	d := monthEnd.Sub(now)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	mins := int(d.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// GetBudgetStatus returns budget consumption for every configured budget.
func (t *Tracker) GetBudgetStatus() map[string]BudgetStatus {
	t.mu.RLock()
	budgets := make(map[string]int64, len(t.budgets))
	for k, v := range t.budgets {
		budgets[k] = v
	}
	t.mu.RUnlock()

	resetsIn := monthEndsIn()
	out := make(map[string]BudgetStatus, len(budgets))
	for prov, budget := range budgets {
		used := t.MonthlyUsage(prov)
		out[prov] = BudgetStatus{
			Budget:    budget,
			Used:      used,
			Remaining: budget - used,
			Over:      used >= budget,
			ResetsIn:  resetsIn,
		}
	}
	return out
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
	Uptime           string                     `json:"uptime"`
	ByModel          map[string]ModelSnapshot   `json:"by_model"`
	ByAPIKey         map[string]ModelSnapshot   `json:"by_api_key"`
	TokenSaverSaved  int64                      `json:"token_saver_bytes_saved"`
	EstimatedCostUSD float64                    `json:"estimated_cost_usd"`
	CostByProvider   map[string]float64         `json:"cost_by_provider"`
	MonthlyTokens    map[string]int64           `json:"monthly_tokens"`
	Budgets          map[string]BudgetStatus    `json:"budgets"`
}

// GetStats returns a snapshot of all usage statistics.
func (t *Tracker) GetStats() StatsResponse {
	t.mu.RLock()
	byModel := make(map[string]ModelSnapshot, len(t.byModel))
	for k, v := range t.byModel {
		byModel[k] = v.Snapshot()
	}
	byKey := make(map[string]ModelSnapshot, len(t.byKey))
	for k, v := range t.byKey {
		byKey[k] = v.Snapshot()
	}
	costByProvider := make(map[string]float64, len(t.costMic))
	for k, v := range t.costMic {
		costByProvider[k] = float64(v.Load()) / 1e6
	}
	monthly := make(map[string]int64, len(t.monthly))
	for k, v := range t.monthly {
		monthly[k] = v.Load()
	}
	t.mu.RUnlock()

	return StatsResponse{
		Uptime:           time.Since(t.started).Round(time.Second).String(),
		ByModel:          byModel,
		ByAPIKey:         byKey,
		TokenSaverSaved:  t.totalBytesSaved.Load(),
		EstimatedCostUSD: float64(t.totalMic.Load()) / 1e6,
		CostByProvider:   costByProvider,
		MonthlyTokens:    monthly,
		Budgets:          t.GetBudgetStatus(),
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

// LoadUsage loads saved usage stats from a JSON file to restore stats across restarts.
func (t *Tracker) LoadUsage(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var stats StatsResponse
	if err := json.Unmarshal(data, &stats); err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for k, snap := range stats.ByModel {
		s := &ModelStats{}
		s.TotalRequests.Store(snap.TotalRequests)
		s.TotalPromptTokens.Store(snap.TotalPromptTokens)
		s.TotalOutputTokens.Store(snap.TotalOutputTokens)
		s.TotalTokens.Store(snap.TotalTokens)
		s.TotalErrors.Store(snap.TotalErrors)
		s.TotalCacheHits.Store(snap.TotalCacheHits)
		t.byModel[k] = s
	}

	for k, snap := range stats.ByAPIKey {
		s := &ModelStats{}
		s.TotalRequests.Store(snap.TotalRequests)
		s.TotalPromptTokens.Store(snap.TotalPromptTokens)
		s.TotalOutputTokens.Store(snap.TotalOutputTokens)
		s.TotalTokens.Store(snap.TotalTokens)
		s.TotalErrors.Store(snap.TotalErrors)
		s.TotalCacheHits.Store(snap.TotalCacheHits)
		t.byKey[k] = s
	}

	t.totalBytesSaved.Store(stats.TokenSaverSaved)

	for k, usd := range stats.CostByProvider {
		c := &atomic.Int64{}
		micros := int64(usd * 1e6)
		c.Store(micros)
		t.costMic[k] = c
		t.totalMic.Add(micros)
	}

	for k, n := range stats.MonthlyTokens {
		c := &atomic.Int64{}
		c.Store(n)
		t.monthly[k] = c
	}
	return nil
}

