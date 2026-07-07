package cache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aigateway/models"
)

// entry holds a cached item.
type entry struct {
	key       string
	response  *models.ChatCompletionResponse
	expiresAt time.Time
}

// LRUCache is a thread-safe LRU cache with TTL for chat completion responses.
type LRUCache struct {
	mu        sync.RWMutex
	capacity  int
	ttl       time.Duration
	items     map[string]*list.Element
	evictList *list.List
	hits      atomic.Int64
	misses    atomic.Int64
}

// New creates a new LRU cache with the given capacity and TTL.
func New(capacity int, ttl time.Duration) *LRUCache {
	return &LRUCache{
		capacity:  capacity,
		ttl:       ttl,
		items:     make(map[string]*list.Element),
		evictList: list.New(),
	}
}

// HashRequest creates a deterministic cache key from a chat completion request.
func HashRequest(req *models.ChatCompletionRequest) string {
	// Only cache if not streaming
	if req.Stream {
		return ""
	}

	// Create a canonical representation
	key := struct {
		Model    string                `json:"model"`
		Messages []models.Message      `json:"messages"`
		Temp     *float64              `json:"temperature,omitempty"`
		MaxTok   *int                  `json:"max_tokens,omitempty"`
		TopP     *float64              `json:"top_p,omitempty"`
	}{
		Model:    req.Model,
		Messages: req.Messages,
		Temp:     req.Temperature,
		MaxTok:   req.MaxTokens,
		TopP:     req.TopP,
	}

	data, err := json.Marshal(key)
	if err != nil {
		return ""
	}

	hash := sha256.Sum256(data)
	return string(hash[:])
}

// Get retrieves a cached response. Returns nil if not found or expired.
func (c *LRUCache) Get(key string) *models.ChatCompletionResponse {
	if key == "" {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		e := elem.Value.(*entry)
		if time.Now().Before(e.expiresAt) {
			// Move to front (most recently used)
			c.evictList.MoveToFront(elem)
			c.hits.Add(1)
			return e.response
		}
		// Expired — remove it
		c.removeElement(elem)
	}

	c.misses.Add(1)
	return nil
}

// Set stores a response in the cache.
func (c *LRUCache) Set(key string, resp *models.ChatCompletionResponse) {
	if key == "" || resp == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// If already exists, update and move to front
	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		elem.Value.(*entry).response = resp
		elem.Value.(*entry).expiresAt = time.Now().Add(c.ttl)
		return
	}

	// Evict oldest if at capacity
	for c.evictList.Len() >= c.capacity {
		oldest := c.evictList.Back()
		if oldest != nil {
			c.removeElement(oldest)
		}
	}

	// Add new entry
	e := &entry{
		key:       key,
		response:  resp,
		expiresAt: time.Now().Add(c.ttl),
	}
	elem := c.evictList.PushFront(e)
	c.items[key] = elem
}

// removeElement removes an element from the cache.
func (c *LRUCache) removeElement(elem *list.Element) {
	c.evictList.Remove(elem)
	delete(c.items, elem.Value.(*entry).key)
}

// Stats returns cache hit/miss statistics.
func (c *LRUCache) Stats() (hits, misses int64, size int) {
	c.mu.RLock()
	size = c.evictList.Len()
	c.mu.RUnlock()
	return c.hits.Load(), c.misses.Load(), size
}

// StartCleanup starts a background goroutine that periodically removes expired entries.
func (c *LRUCache) StartCleanup(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.cleanExpired()
			}
		}
	}()
}

func (c *LRUCache) cleanExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for c.evictList.Len() > 0 {
		elem := c.evictList.Back()
		if elem == nil {
			break
		}
		e := elem.Value.(*entry)
		if now.Before(e.expiresAt) {
			break // entries are ordered, no more expired
		}
		c.removeElement(elem)
	}
}

// Clear removes all entries from the cache.
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element)
	c.evictList.Init()
}
