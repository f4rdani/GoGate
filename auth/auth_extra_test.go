package auth

import (
	"testing"

	"github.com/aigateway/config"
)

func TestKeyStoreListKeys(t *testing.T) {
	ks := NewKeyStore([]config.APIKeyConfig{
		{Key: "sk-1", Name: "a", AllowedModels: []string{"*"}},
		{Key: "sk-2", Name: "b", AllowedModels: []string{"m"}},
	})
	keys := ks.ListKeys()
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}
