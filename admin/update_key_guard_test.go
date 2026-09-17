package admin

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aigateway/auth"
	"github.com/aigateway/config"
	"github.com/aigateway/provider"
	"github.com/aigateway/proxy"
	"github.com/aigateway/relay"
	"github.com/aigateway/tunnel"
)

// kiroTestFrame builds one AWS EventStream frame for tests.
func kiroTestFrame(eventType, payload string) []byte {
	var hdr bytes.Buffer
	writeHdr := func(name, value string) {
		hdr.WriteByte(byte(len(name)))
		hdr.WriteString(name)
		hdr.WriteByte(6)
		_ = binary.Write(&hdr, binary.BigEndian, uint16(len(value)))
		hdr.WriteString(value)
	}
	writeHdr(":message-type", "event")
	writeHdr(":event-type", eventType)
	total := 12 + hdr.Len() + len(payload) + 4
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(total))
	_ = binary.Write(&out, binary.BigEndian, uint32(hdr.Len()))
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(out.Bytes()[:8]))
	out.Write(hdr.Bytes())
	out.WriteString(payload)
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(out.Bytes()))
	return out.Bytes()
}

func setupUpdateKeyAdmin() (*AdminHandler, string) {
	cfg := &config.Config{
		Server: config.ServerConfig{AdminSecret: "test-secret-123"},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-gw-keep-limit", Name: "limited", AllowedModels: []string{"*"}, RateLimit: 60},
		},
		ProxyPool: config.ProxyPoolConfig{Enabled: true},
	}
	ks := auth.NewKeyStore(cfg.APIKeys)
	adm := NewAdminHandler(ks, cfg.Server.AdminSecret, &proxy.Stats{}, nil, "", cfg, nil, tunnel.NewTunnelManager(), relay.NewProxyPool(cfg.ProxyPool))
	return adm, auth.HashKey("sk-gw-keep-limit")
}

// Updating only the name must NOT reset the existing rate limit to 0.
func TestUpdateKeyPreservesRateLimit(t *testing.T) {
	adm, hash := setupUpdateKeyAdmin()
	req := httptest.NewRequest("PUT", "/admin/keys/"+hash, strings.NewReader(`{"name":"renamed"}`))
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w := httptest.NewRecorder()
	adm.HandleUpdateKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	keys := adm.getKeyStore().ListKeys()
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].RateLimit != 60 {
		t.Fatalf("rate limit must be preserved, got %d", keys[0].RateLimit)
	}
	if keys[0].Name != "renamed" {
		t.Fatalf("name must update, got %q", keys[0].Name)
	}
}

// Explicit rate_limit is still honored.
func TestUpdateKeyExplicitRateLimit(t *testing.T) {
	adm, hash := setupUpdateKeyAdmin()
	req := httptest.NewRequest("PUT", "/admin/keys/"+hash, strings.NewReader(`{"rate_limit":5}`))
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w := httptest.NewRecorder()
	adm.HandleUpdateKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := adm.getKeyStore().ListKeys()[0].RateLimit; got != 5 {
		t.Fatalf("expected rate limit 5, got %d", got)
	}
}

// Quick setup without models for a catalog-less provider must fail loudly
// instead of inventing hardcoded model names.
func TestQuickSetupRequiresModelsWhenFetchFails(t *testing.T) {
	adm, _ := setupUpdateKeyAdmin()
	req := httptest.NewRequest("POST", "/admin/templates/setup", strings.NewReader(`{"template_name":"anthropic","api_key":"sk-x"}`))
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w := httptest.NewRecorder()
	adm.HandleQuickSetup(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "models") {
		t.Fatalf("error must mention supplying models, got %s", w.Body.String())
	}
}

// Kiro diag runs through the registered provider (EventStream translation).
func TestDiagTestModelKiro(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("TokenType"); got != "API_KEY" {
			t.Errorf("need TokenType API_KEY, got %q", got)
		}
		w.Write(append(
			kiroTestFrame("assistantResponseEvent", `{"content":"ok"}`),
			kiroTestFrame("messageStopEvent", `{"stopReason":"end_turn"}`)...,
		))
	}))
	defer upSrv.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{AdminSecret: "test-secret-123"},
		Providers: []config.ProviderConfig{
			{Name: "kr", Type: "kiro", BaseURL: upSrv.URL, APIKeys: []string{"k-key"}},
		},
	}
	reg := provider.NewRegistry()
	p, err := provider.NewProviderFromConfig(cfg.Providers[0])
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("kr", p)
	adm := NewAdminHandler(auth.NewKeyStore(nil), cfg.Server.AdminSecret, &proxy.Stats{}, nil, "", cfg, reg, tunnel.NewTunnelManager(), relay.NewProxyPool(cfg.ProxyPool))

	req := httptest.NewRequest("POST", "/admin/diag/test-model", strings.NewReader(`{"provider":"kr","model":"claude-sonnet-4.5"}`))
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w := httptest.NewRecorder()
	adm.HandleDiagTestModel(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("expected translated text: %s", w.Body.String())
	}
}

// OAuth providers authenticate diagnostics with a live refreshed token.
func TestDiagTestModelOAuthLiveToken(t *testing.T) {
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"diag-tok","expires_in":3600}`))
	}))
	defer tokSrv.Close()

	var gotAuth string
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer upSrv.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{AdminSecret: "test-secret-123"},
		Providers: []config.ProviderConfig{
			{Name: "oa", Type: "oauth", BaseURL: upSrv.URL, TokenURL: tokSrv.URL, ClientID: "c", RefreshToken: "rt"},
		},
	}
	reg := provider.NewRegistry()
	p, err := provider.NewProviderFromConfig(cfg.Providers[0])
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("oa", p)
	adm := NewAdminHandler(auth.NewKeyStore(nil), cfg.Server.AdminSecret, &proxy.Stats{}, nil, "", cfg, reg, tunnel.NewTunnelManager(), relay.NewProxyPool(cfg.ProxyPool))

	req := httptest.NewRequest("POST", "/admin/diag/test-model", strings.NewReader(`{"provider":"oa","model":"m"}`))
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w := httptest.NewRecorder()
	adm.HandleDiagTestModel(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer diag-tok" {
		t.Fatalf("diag must use the live oauth token, got %q", gotAuth)
	}
}
