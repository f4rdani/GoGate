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

func setupFullAdmin(t *testing.T) (*AdminHandler, string) {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	cfg := &config.Config{
		Server: config.ServerConfig{AdminSecret: "test-secret-123"},
		Providers: []config.ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: "http://127.0.0.1:9", APIKeys: []string{"k1"}, Models: []string{"m1"}},
		},
		Models: []config.ModelConfig{
			{Name: "m1", Provider: "p1", Model: "m1"},
		},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-test-key", Name: "Key 1", AllowedModels: []string{"*"}},
		},
	}
	ks := auth.NewKeyStore(cfg.APIKeys)
	adm := NewAdminHandler(ks, cfg.Server.AdminSecret, &proxy.Stats{}, func() error { return nil }, path, cfg, nil, tunnel.NewTunnelManager(), relay.NewProxyPool(cfg.ProxyPool))
	return adm, path
}

func doAdmin(t *testing.T, adm *AdminHandler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(target, "/admin/providers/") && method == "PUT":
		adm.HandleUpdateProvider(w, req)
	case strings.HasPrefix(target, "/admin/providers/") && method == "DELETE":
		adm.HandleDeleteProvider(w, req)
	case strings.HasPrefix(target, "/admin/models/") && method == "PUT":
		adm.HandleUpdateModel(w, req)
	case strings.HasPrefix(target, "/admin/models/") && method == "DELETE":
		adm.HandleDeleteModel(w, req)
	case target == "/admin/providers" && method == "POST":
		adm.HandleCreateProvider(w, req)
	case target == "/admin/providers" && method == "GET":
		adm.HandleProviders(w, req)
	case target == "/admin/models" && method == "POST":
		adm.HandleCreateModel(w, req)
	case target == "/admin/models" && method == "GET":
		adm.HandleModels(w, req)
	case target == "/admin/config" && method == "GET":
		adm.HandleConfig(w, req)
	case target == "/admin/stats":
		adm.HandleStats(w, req)
	case target == "/admin/config/server":
		adm.HandleUpdateConfigServer(w, req)
	case target == "/admin/config/concurrency":
		adm.HandleUpdateConfigConcurrency(w, req)
	case target == "/admin/config/cache":
		adm.HandleUpdateConfigCache(w, req)
	case target == "/admin/config/retry":
		adm.HandleUpdateConfigRetry(w, req)
	case target == "/admin/config/token-saver" && method == "GET":
		adm.HandleGetTokenSaverConfig(w, req)
	case target == "/admin/config/token-saver" && method == "PUT":
		adm.HandleUpdateTokenSaverConfig(w, req)
	case target == "/admin/change-password":
		adm.HandleChangePassword(w, req)
	case target == "/admin/templates":
		adm.HandleTemplates(w, req)
	case target == "/admin/keys" && method == "POST":
		adm.HandleCreateKey(w, req)
	case target == "/admin/proxy-pool" && method == "GET":
		adm.HandleGetProxyPool(w, req)
	case target == "/admin/proxy-pool/toggle":
		adm.HandleToggleProxyPool(w, req)
	case target == "/admin/logs":
		adm.HandleGetLogs(w, req)
	case target == "/admin/config/reload":
		adm.HandleReloadConfig(w, req)
	default:
		t.Fatalf("no route for %s %s", method, target)
	}
	return w
}

func TestAdminProvidersAndModelsCRUD(t *testing.T) {
	adm, _ := setupFullAdmin(t)

	// Providers list contains seeded provider.
	w := doAdmin(t, adm, "GET", "/admin/providers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("providers: %d %s", w.Code, w.Body.String())
	}

	// Create provider.
	w = doAdmin(t, adm, "POST", "/admin/providers", `{"name":"p2","type":"openai","base_url":"http://x","api_keys":["k"],"models":["m2"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create provider: %d %s", w.Code, w.Body.String())
	}
	// Duplicate → 409.
	w = doAdmin(t, adm, "POST", "/admin/providers", `{"name":"p2","type":"openai","base_url":"http://x"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate provider must conflict: %d", w.Code)
	}
	// Invalid body.
	w = doAdmin(t, adm, "POST", "/admin/providers", `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json must 400: %d", w.Code)
	}

	// Update provider tier.
	w = doAdmin(t, adm, "PUT", "/admin/providers/p2", `{"tier":3}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update provider: %d %s", w.Code, w.Body.String())
	}
	if got := adm.cfg.GetProvider("p2"); got == nil || got.Tier != 3 {
		t.Fatalf("tier must update: %+v", got)
	}
	// Update missing → 404.
	w = doAdmin(t, adm, "PUT", "/admin/providers/nope", `{"tier":2}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing provider must 404: %d", w.Code)
	}

	// Models CRUD.
	w = doAdmin(t, adm, "GET", "/admin/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("models: %d", w.Code)
	}
	w = doAdmin(t, adm, "POST", "/admin/models", `{"name":"m2","provider":"p2","model":"m2"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create model: %d %s", w.Code, w.Body.String())
	}
	w = doAdmin(t, adm, "PUT", "/admin/models/m2", `{"reasoning":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update model: %d", w.Code)
	}
	if got := adm.cfg.GetModel("m2"); got == nil || !got.Reasoning {
		t.Fatalf("reasoning must update: %+v", got)
	}
	w = doAdmin(t, adm, "DELETE", "/admin/models/m2", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete model: %d", w.Code)
	}
	w = doAdmin(t, adm, "DELETE", "/admin/models/m2", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("double delete must 404: %d", w.Code)
	}

	// Delete provider cascades.
	w = doAdmin(t, adm, "DELETE", "/admin/providers/p2", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete provider: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminConfigEndpoints(t *testing.T) {
	adm, _ := setupFullAdmin(t)

	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/admin/stats", ""},
		{"GET", "/admin/config", ""},
		{"PUT", "/admin/config/server", `{"log_level":"debug"}`},
		{"PUT", "/admin/config/concurrency", `{"max_concurrent":10}`},
		{"PUT", "/admin/config/cache", `{"enabled":true,"max_size":10,"ttl":60}`},
		{"PUT", "/admin/config/retry", `{"max_retries":3}`},
		{"GET", "/admin/config/token-saver", ""},
		{"PUT", "/admin/config/token-saver", `{"enabled":true,"max_input_bytes":1024}`},
		{"GET", "/admin/templates", ""},
		{"GET", "/admin/proxy-pool", ""},
		{"POST", "/admin/proxy-pool/toggle", `{"enabled":true}`},
		{"GET", "/admin/logs", ""},
		{"POST", "/admin/config/reload", ""},
	} {
		w := doAdmin(t, adm, tc.method, tc.target, tc.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", tc.method, tc.target, w.Code, w.Body.String())
		}
	}

	if adm.cfg.Server.LogLevel != "debug" {
		t.Fatalf("log level must update: %+v", adm.cfg.Server)
	}
	if adm.cfg.Retry.MaxRetries != 3 {
		t.Fatalf("retry must update: %+v", adm.cfg.Retry)
	}

	// Create key returns the secret once.
	w := doAdmin(t, adm, "POST", "/admin/keys", `{"name":"n2"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil || created["key"] == nil {
		t.Fatalf("created key must be returned: %v %s", err, w.Body.String())
	}

	// Change password flow.
	w = doAdmin(t, adm, "PUT", "/admin/change-password", `{"current_secret":"test-secret-123","new_secret":"brand-new-secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	if adm.getAdminSecret() != "brand-new-secret" {
		t.Fatal("secret must rotate")
	}
	// Old secret rejected now.
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	req.Header.Set("X-Admin-Secret", "test-secret-123")
	w2 := httptest.NewRecorder()
	adm.HandleStats(w2, req)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("old secret must fail: %d", w2.Code)
	}
}

func TestAdminDiagHelpers(t *testing.T) {
	if got := maskDiagKey("abcdefgh12345678"); got != "abcdefgh...5678" {
		t.Fatalf("bad mask: %q", got)
	}
	if maskDiagKey("") != "-" {
		t.Fatal("empty mask must be dash")
	}
	if !isDiagFallbackRetryable(errWith("HTTP 429 too many")) {
		t.Fatal("429 must fallback")
	}
	if isDiagFallbackRetryable(errWith("HTTP 404 model_not_found")) {
		t.Fatal("404 must not fallback")
	}
	if isDiagFallbackRetryable(nil) {
		t.Fatal("nil must not fallback")
	}

	adm, _ := setupFullAdmin(t)
	// Non-kiro provider → not kiro.
	if _, isKiro, err := adm.kiroDiagCredentials("p1", "", nil); err != nil || isKiro {
		t.Fatalf("p1 must not be kiro: %v %v", isKiro, err)
	}
	// Missing provider → not kiro, no error.
	if _, isKiro, err := adm.kiroDiagCredentials("nope", "", nil); err != nil || isKiro {
		t.Fatalf("missing must not be kiro: %v %v", isKiro, err)
	}
	// Explicit refresh-looking key routes to refresh mode.
	adm.cfg.Providers = append(adm.cfg.Providers, config.ProviderConfig{Name: "kr", Type: "kiro", BaseURL: "http://x"})
	cred, isKiro, err := adm.kiroDiagCredentials("kr", "aorAAAAAGxyz", nil)
	if err != nil || !isKiro || cred.RefreshToken == "" {
		t.Fatalf("refresh-looking key must map: %+v %v %v", cred, isKiro, err)
	}
	cred, isKiro, err = adm.kiroDiagCredentials("kr", "plain-key", nil)
	if err != nil || !isKiro || cred.APIKey == "" {
		t.Fatalf("plain key must map: %+v %v %v", cred, isKiro, err)
	}
	if _, _, err := adm.kiroDiagCredentials("kr", "", nil); err == nil {
		t.Fatal("credential-less kiro must error")
	}
}

type errWith string

func (e errWith) Error() string { return string(e) }

func TestAdminDiagTestKeyAndFetchModels(t *testing.T) {
	var keyHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			keyHits++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	adm, _ := setupFullAdmin(t)
	adm.cfg.Providers = append(adm.cfg.Providers, config.ProviderConfig{
		Name: "live", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"k"},
	})

	post := func(target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, strings.NewReader(body))
		req.Header.Set("X-Admin-Secret", "test-secret-123")
		w := httptest.NewRecorder()
		switch target {
		case "/admin/diag/test-key":
			adm.HandleDiagTestKey(w, req)
		case "/admin/diag/fetch-models":
			adm.HandleDiagFetchModels(w, req)
		}
		return w
	}

	w := post("/admin/diag/test-key", `{"provider":"live"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("test-key: %d %s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil || out["model_count"] == nil {
		t.Fatalf("test-key shape: %v %s", err, w.Body.String())
	}

	w = post("/admin/diag/fetch-models", `{"provider":"live"}`)
	if w.Code != http.StatusOK || keyHits != 2 {
		t.Fatalf("fetch-models: %d hits=%d %s", w.Code, keyHits, w.Body.String())
	}

	// Unknown provider → 400.
	w = post("/admin/diag/test-key", `{"provider":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider must 400: %d", w.Code)
	}
}

func TestAdminDiagTestModelFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "key-1") {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	adm, _ := setupFullAdmin(t)
	adm.cfg.Providers = append(adm.cfg.Providers, config.ProviderConfig{
		Name: "live", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"key-1", "key-2"},
	})

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/admin/diag/test-model", strings.NewReader(body))
		req.Header.Set("X-Admin-Secret", "test-secret-123")
		w := httptest.NewRecorder()
		adm.HandleDiagTestModel(w, req)
		return w
	}

	// Auto-fallback: key-1 rate limited → success with key #2.
	w := post(`{"provider":"live","model":"m1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("fallback: %d %s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&out)
	if out["key_number"] != float64(2) || out["fallback_used"] != true {
		t.Fatalf("must report key #2 + fallback: %v", out)
	}

	// Explicit single key keeps failing (no fallback by design).
	w = post(`{"provider":"live","model":"m1","key_index":0}`)
	if w.Code == http.StatusOK {
		t.Fatalf("explicit key-1 must fail: %s", w.Body.String())
	}

	// Missing model → 400.
	w = post(`{"provider":"live"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing model must 400: %d", w.Code)
	}
}

func TestAdminUpdateConfigAndReload(t *testing.T) {
	adm, path := setupFullAdmin(t)
	_ = path
	ks := auth.NewKeyStore(nil)
	adm.UpdateConfig(ks, "new-secret", adm.cfg, nil)
	if adm.getAdminSecret() != "new-secret" {
		t.Fatal("UpdateConfig must swap secret")
	}
	if adm.getKeyStore() != ks {
		t.Fatal("UpdateConfig must swap keystore")
	}
	if adm.getRegistry() != nil {
		t.Fatal("registry must be swappable to nil")
	}
}
