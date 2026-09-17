package cache

import (
	"testing"
	"time"

	"github.com/aigateway/models"
)

func testCacheResp() *models.ChatCompletionResponse {
	return &models.ChatCompletionResponse{ID: "r1", Model: "m"}
}

func TestCleanExpired(t *testing.T) {
	c := New(10, -time.Second) // already-expired TTL
	c.Set("gone", testCacheResp())
	c.Set("gone2", testCacheResp())
	c.cleanExpired()
	if _, _, size := c.Stats(); size != 0 {
		t.Fatalf("expired entries must be cleaned, size=%d", size)
	}

	c2 := New(10, time.Minute)
	c2.Set("live", testCacheResp())
	c2.cleanExpired()
	if _, _, size := c2.Stats(); size != 1 {
		t.Fatalf("live entries must survive, size=%d", size)
	}
}
