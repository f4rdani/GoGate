package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aigateway/auth"
	"github.com/aigateway/config"
	"github.com/aigateway/proxy"
	"github.com/aigateway/relay"
	"github.com/aigateway/tunnel"
)

func setupTestAdmin() *AdminHandler {
	cfg := &config.Config{
		Server: config.ServerConfig{
			AdminSecret: "test-secret-123",
		},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-test-key", Name: "Key 1", AllowedModels: []string{"*"}},
		},
		ProxyPool: config.ProxyPoolConfig{
			Enabled: true,
		},
	}
	ks := auth.NewKeyStore(cfg.APIKeys)
	stats := &proxy.Stats{}
	tunnelMgr := tunnel.NewTunnelManager()
	proxyPool := relay.NewProxyPool(cfg.ProxyPool)

	return NewAdminHandler(ks, cfg.Server.AdminSecret, stats, nil, "", cfg, nil, tunnelMgr, proxyPool)
}

func TestDetectReasoning(t *testing.T) {
	// Response evidence wins.
	if !detectReasoning("plain-model", `{"choices":[{"message":{"content":"ok","reasoning_content":"because"}}]}`) {
		t.Error("reasoning_content in body must detect")
	}
	if !detectReasoning("plain-model", `<think>hmm</thinking>ok`) {
		t.Error("<think> tag must detect")
	}
	if !detectReasoning("plain-model", `{"content":[{"type":"thinking","thinking":"..."}]}`) {
		t.Error("thinking block must detect")
	}
	// Model-ID keywords (mirror cli.isReasoningModelID).
	for _, id := range []string{"deepseek-r1", "o1-mini", "gpt-oss-120b", "qwen3-32b", "claude-thinking", "qwq-32b"} {
		if !detectReasoning(id, `{"choices":[{"message":{"content":"ok"}}]}`) {
			t.Errorf("model id %q must detect", id)
		}
	}
	// Gemini thinks by default — except Gemma and embedding variants.
	for _, id := range []string{"google/gemini-2.5-flash", "models/gemini-3-flash-preview", "gemini-3.5-flash"} {
		if !detectReasoning(id, `{"choices":[{"message":{"content":"ok"}}]}`) {
			t.Errorf("gemini model %q must detect", id)
		}
	}
	for _, id := range []string{"google/gemma-4-26b-a4b-it", "gemini-embedding-001", "text-embedding-3"} {
		if detectReasoning(id, `{"choices":[{"message":{"content":"ok"}}]}`) {
			t.Errorf("non-reasoning model %q must not detect", id)
		}
	}
	// Plain model, plain response → false.
	if detectReasoning("mistral-small-latest", `{"choices":[{"message":{"content":"OK"}}]}`) {
		t.Error("plain model must not detect")
	}
}

func TestDiagTestModelReasoningFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok","reasoning_content":"let me think"}}]}`))
	}))
	defer srv.Close()

	text, latency, reasoning, err := diagTestModel(nil, srv.URL, "k", "my-reasoner", "openai", "")
	if err != nil {
		t.Fatalf("diag test failed: %v", err)
	}
	if text != "ok" || latency < 0 {
		t.Fatalf("unexpected result: %q %d", text, latency)
	}
	if !reasoning {
		t.Error("reasoning flag must be true")
	}

	text2, _, reasoning2, err := diagTestModel(nil, srv.URL, "k", "plain", "openai", "")
	if err != nil || text2 != "ok" || !reasoning2 {
		// NOTE: body still carries reasoning_content, so detection stays true
		// regardless of model id — this asserts body-evidence wins.
		t.Fatalf("body evidence must win: %q %v %v", text2, reasoning2, err)
	}
}

func TestDiagTestModelOpenCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-opencode-client") != "cli" {
			t.Errorf("missing x-opencode-client")
		}
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("missing User-Agent")
		}
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("invalid body: %v", err)
		}
		if req["stream"] != true {
			t.Errorf("opencode request must have stream=true")
		}
		tools, ok := req["tools"].([]interface{})
		if !ok || len(tools) == 0 {
			t.Errorf("opencode request must have injected tools")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello from OpenCode\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	text, latency, reasoning, err := diagTestModel(nil, srv.URL, "sk-test", "mimo-v2.5-free", "opencode", "")
	if err != nil {
		t.Fatalf("diag opencode test failed: %v", err)
	}
	if text != "Hello from OpenCode" || latency < 0 {
		t.Fatalf("unexpected result: %q %d", text, latency)
	}
	if reasoning {
		t.Error("expected reasoning=false for non-reasoning response")
	}
}


func TestAdminHandler_CheckAuth(t *testing.T) {
	adm := setupTestAdmin()

	// Valid auth header
	reqGood := httptest.NewRequest("GET", "/admin/keys", nil)
	reqGood.Header.Set("X-Admin-Secret", "test-secret-123")
	if !adm.CheckAuth(reqGood) {
		t.Error("expected CheckAuth to return true for correct secret")
	}

	// Missing auth header
	reqMissing := httptest.NewRequest("GET", "/admin/keys", nil)
	if adm.CheckAuth(reqMissing) {
		t.Error("expected CheckAuth to return false for missing header")
	}
}

func TestAdminHandler_HandleListKeys(t *testing.T) {
	adm := setupTestAdmin()

	// Unauthorized
	reqUnauthorized := httptest.NewRequest("GET", "/admin/keys", nil)
	wUnauthorized := httptest.NewRecorder()
	adm.HandleListKeys(wUnauthorized, reqUnauthorized)
	if wUnauthorized.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized, got %d", wUnauthorized.Code)
	}

	// Authorized
	reqAuthorized := httptest.NewRequest("GET", "/admin/keys", nil)
	reqAuthorized.Header.Set("X-Admin-Secret", "test-secret-123")
	wAuthorized := httptest.NewRecorder()
	adm.HandleListKeys(wAuthorized, reqAuthorized)
	if wAuthorized.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", wAuthorized.Code)
	}

	var keys []map[string]interface{}
	if err := json.NewDecoder(wAuthorized.Body).Decode(&keys); err != nil {
		t.Fatalf("decode keys failed: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 key, got %d", len(keys))
	}
}

func TestAdminHandler_HandleGetProxyPool(t *testing.T) {
	adm := setupTestAdmin()

	req := httptest.NewRequest("GET", "/admin/proxy-pool", nil)
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w := httptest.NewRecorder()

	adm.HandleGetProxyPool(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var stats relay.ProxyPoolStats
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("decode proxy pool stats failed: %v", err)
	}
	if !stats.Enabled {
		t.Errorf("expected proxy pool to be enabled in test config")
	}
}

func TestAdminHandler_ServeDashboard(t *testing.T) {
	// 1. Test with default secret (warning banner should be present)
	cfgDefault := &config.Config{
		Server: config.ServerConfig{
			AdminSecret: "123456",
		},
	}
	admDefault := NewAdminHandler(nil, cfgDefault.Server.AdminSecret, nil, nil, "", cfgDefault, nil, nil, nil)
	req := httptest.NewRequest("GET", "/admin", nil)
	wDefault := httptest.NewRecorder()
	admDefault.ServeDashboard(wDefault, req)

	if wDefault.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", wDefault.Code)
	}
	bodyDefault := wDefault.Body.String()
	if !strings.Contains(bodyDefault, "Security Notice") {
		t.Error("expected default password warning banner to be injected for default secret 123456")
	}
	if !strings.Contains(bodyDefault, "app-sidebar") {
		t.Error("expected dashboard HTML to contain app-sidebar layout")
	}
	if !strings.Contains(bodyDefault, "data-theme") {
		t.Error("expected dashboard HTML to support dual theme")
	}

	// 2. Test with custom secure secret (warning banner should NOT be present)
	cfgSecure := &config.Config{
		Server: config.ServerConfig{
			AdminSecret: "super-secure-random-pass-xyz-987",
		},
	}
	admSecure := NewAdminHandler(nil, cfgSecure.Server.AdminSecret, nil, nil, "", cfgSecure, nil, nil, nil)
	wSecure := httptest.NewRecorder()
	admSecure.ServeDashboard(wSecure, req)

	if wSecure.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", wSecure.Code)
	}
	bodySecure := wSecure.Body.String()
	if strings.Contains(bodySecure, "Security Notice") {
		t.Error("expected warning banner to be omitted for custom secure secret")
	}
}

