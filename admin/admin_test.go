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

