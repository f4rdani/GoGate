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

func TestHandler_PrivacyFilter_BlockMode(t *testing.T) {
	h, _ := setupTestHandler()
	h.SetPrivacyConfig(config.PrivacyConfig{
		Enabled:     true,
		Mode:        "block",
		Scope:       "all",
		MaskSecrets: true,
		MaskPII:     true,
	})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"My key is sk-proj-1234567890abcdef1234567890"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()

	h.HandleChatCompletion(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for blocked privacy violation, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "blocked by privacy policy") {
		t.Fatalf("expected error message to mention privacy policy, got: %s", w.Body.String())
	}
}

func TestHandler_PrivacyFilter_BypassHeader(t *testing.T) {
	h, _ := setupTestHandler()
	h.SetPrivacyConfig(config.PrivacyConfig{
		Enabled:     true,
		Mode:        "block",
		Scope:       "all",
		MaskSecrets: true,
		MaskPII:     true,
	})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"My key is sk-proj-1234567890abcdef1234567890"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	req.Header.Set("X-Privacy-Filter", "off")
	w := httptest.NewRecorder()

	h.HandleChatCompletion(w, req)
	// Because dummy key fails at upstream, status will not be 400 (it bypassed block validation)
	if w.Code == http.StatusBadRequest {
		t.Fatalf("expected request with X-Privacy-Filter: off to bypass block mode, but got 400: %s", w.Body.String())
	}
}

func TestHandler_PrivacyFilter_VaultMode_RoundTrip(t *testing.T) {
	var upstreamReceivedPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			upstreamReceivedPrompt = req.Messages[0].Content
		}

		resp := map[string]interface{}{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": 1234567890,
			"model":   "gpt-4o",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Confirmed key is " + upstreamReceivedPrompt,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "mock-openai", Type: "openai", BaseURL: upstream.URL, APIKeys: []string{"upstream-dummy-key"}},
		},
		Models: []config.ModelConfig{
			{Name: "gpt-4o", Provider: "mock-openai", Model: "gpt-4o"},
		},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-valid-key", Name: "User Key", AllowedModels: []string{"gpt-4o"}, RateLimit: 100},
		},
	}

	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("mock-openai", p)

	r, _ := router.NewRouter(cfg, reg)
	ks := auth.NewKeyStore(cfg.APIKeys)
	limiter := middleware.NewConcurrencyLimiter(10)

	h := NewHandler(r, ks, limiter, config.TokenSaverConfig{})
	h.SetPrivacyConfig(config.PrivacyConfig{
		Enabled:     true,
		Mode:        "vault",
		Scope:       "all",
		MaskSecrets: true,
		MaskPII:     true,
	})

	originalSecretKey := "sk-proj-1234567890abcdef1234567890"
	clientBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"My key is ` + originalSecretKey + `"}]}`
	clientReq := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(clientBody))
	clientReq.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()

	h.HandleChatCompletion(w, clientReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from proxy, got %d: %s", w.Code, w.Body.String())
	}

	// 1. Verify UPSTREAM never saw the real secret key!
	if strings.Contains(upstreamReceivedPrompt, originalSecretKey) {
		t.Fatalf("LEAK: upstream received the original secret key! Got: %s", upstreamReceivedPrompt)
	}
	if !strings.Contains(upstreamReceivedPrompt, "sk-proj-mocksec") {
		t.Fatalf("upstream did not receive synthetic dummy key! Got: %s", upstreamReceivedPrompt)
	}

	// 2. Verify CLIENT received the response with original secret key fully restored!
	var clientResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(w.Body).Decode(&clientResp); err != nil {
		t.Fatalf("failed to decode client response: %v", err)
	}

	clientGotContent := clientResp.Choices[0].Message.Content
	if !strings.Contains(clientGotContent, originalSecretKey) {
		t.Fatalf("CLIENT did not receive restored secret key! Got: %s", clientGotContent)
	}
	if strings.Contains(clientGotContent, "sk-proj-mocksec") {
		t.Fatalf("CLIENT response still contains leaked synthetic token! Got: %s", clientGotContent)
	}
}

func TestHandler_PrivacyFilter_VaultMode_StreamingRoundTrip(t *testing.T) {
	var upstreamReceivedPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			upstreamReceivedPrompt = req.Messages[0].Content
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, _ := w.(http.Flusher)

		// Upstream streams tokens in chunks:
		// Chunk 1: "Confirmed: "
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Confirmed: \"}}]}\n\n"))
		flusher.Flush()

		// Chunk 2 & 3: Upstream emits the synthetic key split across chunks!
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"sk-proj-mocksec000000000000000000\"}}]}\n\n"))
		flusher.Flush()

		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"01 done\"}}]}\n\n"))
		flusher.Flush()

		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "mock-openai", Type: "openai", BaseURL: upstream.URL, APIKeys: []string{"upstream-dummy-key"}},
		},
		Models: []config.ModelConfig{
			{Name: "gpt-4o", Provider: "mock-openai", Model: "gpt-4o"},
		},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-valid-key", Name: "User Key", AllowedModels: []string{"gpt-4o"}, RateLimit: 100},
		},
	}

	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("mock-openai", p)

	r, _ := router.NewRouter(cfg, reg)
	ks := auth.NewKeyStore(cfg.APIKeys)
	limiter := middleware.NewConcurrencyLimiter(10)

	h := NewHandler(r, ks, limiter, config.TokenSaverConfig{})
	h.SetPrivacyConfig(config.PrivacyConfig{
		Enabled:     true,
		Mode:        "vault",
		Scope:       "all",
		MaskSecrets: true,
	})

	originalSecretKey := "sk-proj-1234567890abcdef1234567890"
	clientBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"My key is ` + originalSecretKey + `"}]}`
	clientReq := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(clientBody))
	clientReq.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()

	h.HandleChatCompletion(w, clientReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from proxy stream, got %d: %s", w.Code, w.Body.String())
	}

	// 1. Verify UPSTREAM never saw the real secret key!
	if strings.Contains(upstreamReceivedPrompt, originalSecretKey) {
		t.Fatalf("LEAK: upstream received the original secret key! Got: %s", upstreamReceivedPrompt)
	}

	// 2. Verify CLIENT received stream with the real secret key restored across chunk splits!
	clientStreamOutput := w.Body.String()
	if !strings.Contains(clientStreamOutput, originalSecretKey) {
		t.Fatalf("CLIENT stream did not receive restored secret key! Got:\n%s", clientStreamOutput)
	}
	if strings.Contains(clientStreamOutput, "sk-proj-mocksec") {
		t.Fatalf("CLIENT stream still contains synthetic dummy token! Got:\n%s", clientStreamOutput)
	}
}

