package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/aigateway/config"
	"github.com/google/uuid"
)

// KeyInfo holds metadata and state for a single API key.
type KeyInfo struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	RateLimit     int      `json:"rate_limit"`      // requests per minute, 0 = unlimited
	TokenSaver    *bool    `json:"token_saver,omitempty"` // nil=follow global, true/false=override
	Disabled      bool     `json:"disabled"`

	// Rate limiting state (sliding window counter, not serialized)
	currentWindow time.Time
	prevCount     int64
	currCount     int64
	mu            sync.Mutex
}

// IsTokenSaverEnabled checks if token saver is enabled for this key.
// Returns the per-key override if set, otherwise defers to the global setting.
func (k *KeyInfo) IsTokenSaverEnabled(globalEnabled bool) bool {
	if k.TokenSaver != nil {
		return *k.TokenSaver
	}
	return globalEnabled
}

// IsModelAllowed checks if the given model is permitted for this key.
func (k *KeyInfo) IsModelAllowed(model string) bool {
	for _, m := range k.AllowedModels {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}

// CheckRateLimit returns true if the request is within rate limits.
// Uses a weighted sliding window per-minute counter to eliminate boundary burst spikes.
func (k *KeyInfo) CheckRateLimit() bool {
	if k.RateLimit <= 0 {
		return true // No rate limit
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	now := time.Now()
	nowWindow := now.Truncate(time.Minute)

	if nowWindow != k.currentWindow {
		if nowWindow == k.currentWindow.Add(time.Minute) {
			k.prevCount = k.currCount
		} else {
			k.prevCount = 0
		}
		k.currCount = 0
		k.currentWindow = nowWindow
	}

	// Calculate weighted requests from previous and current window
	weight := float64(time.Minute-now.Sub(nowWindow)) / float64(time.Minute)
	estimatedCount := int64(float64(k.prevCount)*weight) + k.currCount

	if estimatedCount < int64(k.RateLimit) {
		k.currCount++
		return true
	}

	return false
}

// HashKey returns the SHA-256 hash of an API key as a hex string.
func HashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// KeyStore manages API keys with thread-safe access.
type KeyStore struct {
	mu   sync.RWMutex
	keys map[string]*KeyInfo
}

// NewKeyStore creates a KeyStore from configuration.
func NewKeyStore(configs []config.APIKeyConfig) *KeyStore {
	store := &KeyStore{
		keys: make(map[string]*KeyInfo),
	}
	for _, cfg := range configs {
		h := HashKey(cfg.Key)
		store.keys[h] = &KeyInfo{
			Key:           cfg.Key,
			Name:          cfg.Name,
			AllowedModels: cfg.AllowedModels,
			RateLimit:     cfg.RateLimit,
			TokenSaver:    cfg.TokenSaver,
			Disabled:      cfg.Disabled,
		}
	}
	return store
}

// Validate checks if an API key is valid and returns its info.
func (s *KeyStore) Validate(key string) (*KeyInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := HashKey(key)
	info, ok := s.keys[h]
	if ok && info.Disabled {
		return nil, false
	}
	return info, ok
}

// AddKey creates a new API key and returns its info.
// The key is generated with format: sk-gw-{uuid}
func (s *KeyStore) AddKey(name string, allowedModels []string, rateLimit int, tokenSaver *bool) *KeyInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := "sk-gw-" + uuid.New().String()
	info := &KeyInfo{
		Key:           key,
		Name:          name,
		AllowedModels: allowedModels,
		RateLimit:     rateLimit,
		TokenSaver:    tokenSaver,
	}
	h := HashKey(key)
	s.keys[h] = info
	return info
}

// DeleteKey removes an API key by its hash. Returns true if the key existed.
func (s *KeyStore) DeleteKey(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[hash]; ok {
		delete(s.keys, hash)
		return true
	}
	return false
}

// ListKeys returns all API keys.
func (s *KeyStore) ListKeys() []*KeyInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*KeyInfo, 0, len(s.keys))
	for _, info := range s.keys {
		result = append(result, info)
	}
	return result
}

// UpdateKey updates an existing API key's metadata by its hash. Returns true if the key existed.
func (s *KeyStore) UpdateKey(hash string, name string, allowedModels []string, rateLimit int, tokenSaver *bool, disabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.keys[hash]
	if !ok {
		return false
	}
	if name != "" {
		info.Name = name
	}
	if len(allowedModels) > 0 {
		info.AllowedModels = allowedModels
	}
	info.RateLimit = rateLimit
	info.TokenSaver = tokenSaver
	info.Disabled = disabled
	return true
}
