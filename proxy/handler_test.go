package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aigateway/auth"
	"github.com/aigateway/cache"
	"github.com/aigateway/config"
	"github.com/aigateway/middleware"
	"github.com/aigateway/provider"
	"github.com/aigateway/router"
)

func setupTestHandler() (*Handler, *auth.KeyStore) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1", APIKeys: []string{"sk-dummy"}},
		},
		Models: []config.ModelConfig{
			{Name: "gpt-4o", Provider: "openai", Model: "gpt-4o"},
		},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-valid-key", Name: "User Key", AllowedModels: []string{"gpt-4o"}, RateLimit: 100},
		},
	}

	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("openai", p)

	r, _ := router.NewRouter(cfg, reg)
	ks := auth.NewKeyStore(cfg.APIKeys)
	limiter := middleware.NewConcurrencyLimiter(10)
	c := cache.New(100, 5*time.Minute)

	h := NewHandler(r, ks, limiter, config.TokenSaverConfig{})
	h.SetCache(c)
	return h, ks
}

func TestHandler_HandleHealth(t *testing.T) {
	h, _ := setupTestHandler()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	h.HandleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode health response failed: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %v", resp["status"])
	}
}

func TestHandler_HandleListModels(t *testing.T) {
	h, _ := setupTestHandler()

	// 1. Missing auth
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	h.HandleListModels(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing auth, got %d", w.Code)
	}

	// 2. Valid auth
	reqWithAuth := httptest.NewRequest("GET", "/v1/models", nil)
	reqWithAuth.Header.Set("Authorization", "Bearer sk-valid-key")
	wWithAuth := httptest.NewRecorder()
	h.HandleListModels(wWithAuth, reqWithAuth)
	if wWithAuth.Code != http.StatusOK {
		t.Errorf("expected 200 with valid key, got %d", wWithAuth.Code)
	}

	var listResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(wWithAuth.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode models failed: %v", err)
	}

	if len(listResp.Data) != 1 || listResp.Data[0].ID != "gpt-4o" {
		t.Errorf("expected gpt-4o in models list, got %+v", listResp.Data)
	}
}

func TestHandler_HandleChatCompletion_AuthAndValidation(t *testing.T) {
	h, _ := setupTestHandler()

	// 1. Missing Authorization header
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.HandleChatCompletion(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing auth, got %d", w.Code)
	}

	// 2. Invalid API key
	reqBadKey := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	reqBadKey.Header.Set("Authorization", "Bearer sk-invalid")
	wBadKey := httptest.NewRecorder()
	h.HandleChatCompletion(wBadKey, reqBadKey)
	if wBadKey.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid key, got %d", wBadKey.Code)
	}

	// 3. Valid key, but empty model
	reqNoModel := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	reqNoModel.Header.Set("Authorization", "Bearer sk-valid-key")
	wNoModel := httptest.NewRecorder()
	h.HandleChatCompletion(wNoModel, reqNoModel)
	if wNoModel.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing model, got %d", wNoModel.Code)
	}

	// 4. Disallowed model
	reqDisallowed := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`))
	reqDisallowed.Header.Set("Authorization", "Bearer sk-valid-key")
	wDisallowed := httptest.NewRecorder()
	h.HandleChatCompletion(wDisallowed, reqDisallowed)
	if wDisallowed.Code != http.StatusForbidden {
		t.Errorf("expected 403 for disallowed model, got %d", wDisallowed.Code)
	}
}
