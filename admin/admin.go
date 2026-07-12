package admin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aigateway/auth"
	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
	"github.com/aigateway/proxy"
	"github.com/aigateway/tunnel"
)

// AdminHandler handles admin API endpoints.
type AdminHandler struct {
	mu          sync.RWMutex
	keyStore    *auth.KeyStore
	adminSecret string
	stats       *proxy.Stats
	reloadFunc  func() error
	configPath  string
	cfg         *config.Config
	registry    *provider.Registry
	tunnelMgr   *tunnel.TunnelManager
}

// NewAdminHandler creates a new admin handler.
func NewAdminHandler(keyStore *auth.KeyStore, adminSecret string, stats *proxy.Stats, reloadFunc func() error, configPath string, cfg *config.Config, registry *provider.Registry, tunnelMgr *tunnel.TunnelManager) *AdminHandler {
	return &AdminHandler{
		keyStore:    keyStore,
		adminSecret: adminSecret,
		stats:       stats,
		reloadFunc:  reloadFunc,
		configPath:  configPath,
		cfg:         cfg,
		registry:    registry,
		tunnelMgr:   tunnelMgr,
	}
}

// UpdateConfig updates the admin handler's keystore, secret, config, and registry after a hot reload.
func (a *AdminHandler) UpdateConfig(keyStore *auth.KeyStore, adminSecret string, cfg *config.Config, registry *provider.Registry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keyStore = keyStore
	a.adminSecret = adminSecret
	a.cfg = cfg
	a.registry = registry
}

// getKeyStore returns the current keyStore (thread-safe).
func (a *AdminHandler) getKeyStore() *auth.KeyStore {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.keyStore
}

// getAdminSecret returns the current admin secret (thread-safe).
func (a *AdminHandler) getAdminSecret() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.adminSecret
}

// checkAuth validates the admin secret from the X-Admin-Secret header.
// Uses constant-time comparison to prevent timing attacks.
func (a *AdminHandler) checkAuth(r *http.Request) bool {
	return a.CheckAuth(r)
}

// CheckAuth is the exported version of checkAuth for use by external packages.
// Uses constant-time comparison to prevent timing attacks.
func (a *AdminHandler) CheckAuth(r *http.Request) bool {
	secret := a.getAdminSecret()
	if secret == "" {
		return false
	}
	provided := r.Header.Get("X-Admin-Secret")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) == 1
}

// persistKeyStore saves the current key store state to the config file.
// Holds the read lock for the entire operation to prevent stale data races.
func (a *AdminHandler) persistKeyStore() {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.cfg == nil || a.configPath == "" {
		return
	}

	// Sync KeyStore keys back to config (ListKeys has its own lock)
	allKeys := a.keyStore.ListKeys()
	a.cfg.APIKeys = make([]config.APIKeyConfig, 0, len(allKeys))
	for _, k := range allKeys {
		a.cfg.APIKeys = append(a.cfg.APIKeys, config.APIKeyConfig{
			Key:           k.Key,
			Name:          k.Name,
			AllowedModels: k.AllowedModels,
			RateLimit:     k.RateLimit,
			TokenSaver:    k.TokenSaver,
			Disabled:      k.Disabled,
		})
	}

	if err := a.cfg.SaveConfig(a.configPath); err != nil {
		slog.Error("failed to persist key store to config", "error", err)
	} else {
		slog.Info("key store persisted to config", "path", a.configPath, "keys", len(allKeys))
	}
}

// sendError sends an error response.
func (a *AdminHandler) sendError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: msg,
			Type:    "admin_error",
		},
	})
}

// HandleListKeys handles GET /admin/keys — list all API keys.
func (a *AdminHandler) HandleListKeys(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	keys := a.getKeyStore().ListKeys()

	type keyResponse struct {
		ID            string   `json:"id"`
		Key           string   `json:"key"`
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		RateLimit     int      `json:"rate_limit"`
		TokenSaver    *bool    `json:"token_saver,omitempty"`
		Disabled      bool     `json:"disabled"`
	}

	resp := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		masked := k.Key
		if len(masked) >= 12 {
			masked = masked[:9] + "..." + masked[len(masked)-4:]
		}
		resp = append(resp, keyResponse{
			ID:            auth.HashKey(k.Key),
			Key:           masked,
			Name:          k.Name,
			AllowedModels: k.AllowedModels,
			RateLimit:     k.RateLimit,
			TokenSaver:    k.TokenSaver,
			Disabled:      k.Disabled,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleCreateKey handles POST /admin/keys — generate a new API key.
func (a *AdminHandler) HandleCreateKey(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var req struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		RateLimit     int      `json:"rate_limit"`
		TokenSaver    *bool    `json:"token_saver"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		a.sendError(w, http.StatusBadRequest, "'name' is required")
		return
	}
	if len(req.AllowedModels) == 0 {
		req.AllowedModels = []string{"*"}
	}

	keyInfo := a.getKeyStore().AddKey(req.Name, req.AllowedModels, req.RateLimit, req.TokenSaver)

	slog.Info("API key created",
		"name", req.Name,
		"key_prefix", keyInfo.Key[:9]+"...",
		"allowed_models", req.AllowedModels,
		"rate_limit", req.RateLimit,
	)

	// Persist to config file
	a.persistKeyStore()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"key":            keyInfo.Key,
		"name":           keyInfo.Name,
		"allowed_models": keyInfo.AllowedModels,
		"rate_limit":     keyInfo.RateLimit,
	})
}

// HandleDeleteKey handles DELETE /admin/keys/{key} — delete an API key.
func (a *AdminHandler) HandleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	// Extract key hash from URL path: /admin/keys/{hash}
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		a.sendError(w, http.StatusBadRequest, "Key not specified in URL path")
		return
	}
	keyHash := parts[len(parts)-1]

	if a.getKeyStore().DeleteKey(keyHash) {
		slog.Info("API key deleted", "hash_prefix", keyHash[:min(8, len(keyHash))]+"...")
		// Persist to config file
		a.persistKeyStore()
		w.WriteHeader(http.StatusNoContent)
	} else {
		a.sendError(w, http.StatusNotFound, "Key not found")
	}
}

// HandleStats handles GET /admin/stats — return server statistics.
func (a *AdminHandler) HandleStats(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_requests":  a.stats.TotalRequests.Load(),
		"active_requests": a.stats.ActiveRequests.Load(),
	})
}

// HandleReloadConfig handles POST /admin/config/reload.
func (a *AdminHandler) HandleReloadConfig(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	if a.reloadFunc == nil {
		a.sendError(w, http.StatusInternalServerError, "Reload function not configured")
		return
	}

	if err := a.reloadFunc(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Failed to reload config: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "config reloaded successfully",
	})
}

// HandleProviders handles GET /admin/providers — list providers with health status.
func (a *AdminHandler) HandleProviders(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	cfg := a.cfg
	registry := a.getRegistry()

	type providerInfo struct {
		Name        string   `json:"name"`
		Type        string   `json:"type"`
		Tier        int      `json:"tier"`
		BaseURL     string   `json:"base_url"`
		Models      []string `json:"models"`
		Healthy     bool     `json:"healthy"`
		HasKey      bool     `json:"has_key"`
		KeyCount    int      `json:"key_count"`
	}

	result := make([]providerInfo, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		info := providerInfo{
			Name:    p.Name,
			Type:    p.Type,
			Tier:    p.Tier,
			BaseURL: p.BaseURL,
			Models:  p.Models,
			HasKey:  len(p.APIKeys) > 0,
			KeyCount: len(p.APIKeys),
		}
		// Check health from registry
		if registry != nil {
			if prov, ok := registry.Get(p.Name); ok {
				info.Healthy = prov.IsHealthy()
			} else {
				info.Healthy = false
			}
		} else {
			info.Healthy = len(p.APIKeys) > 0 // assume healthy if has keys but no registry
		}
		result = append(result, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleModels handles GET /admin/models — list all model routes.
func (a *AdminHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	cfg := a.cfg

	type backendInfo struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Tier     int    `json:"tier"`
	}
	type modelInfo struct {
		Name      string        `json:"name"`
		Type      string        `json:"type"` // "direct" or "combo"
		Provider  string        `json:"provider,omitempty"`
		Model     string        `json:"model,omitempty"`
		Strategy  string        `json:"strategy,omitempty"`
		Backends  []backendInfo `json:"backends,omitempty"`
		Reasoning bool          `json:"reasoning,omitempty"`
		Disabled  bool          `json:"disabled,omitempty"`
	}

	result := make([]modelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		info := modelInfo{Name: m.Name, Reasoning: m.Reasoning, Disabled: m.Disabled}
		if m.Strategy != "" {
			info.Type = "combo"
			info.Strategy = m.Strategy
			for _, b := range m.Backends {
				info.Backends = append(info.Backends, backendInfo{
					Provider: b.Provider,
					Model:    b.Model,
					Tier:     b.Tier,
				})
			}
		} else {
			info.Type = "direct"
			info.Provider = m.Provider
			info.Model = m.Model
		}
		result = append(result, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleConfig handles GET /admin/config — returns sanitized config (no secrets).
func (a *AdminHandler) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	cfg := a.cfg

	result := map[string]interface{}{
		"server": map[string]interface{}{
			"host":      cfg.Server.Host,
			"port":      cfg.Server.Port,
			"log_level": cfg.Server.LogLevel,
		},
		"concurrency": map[string]interface{}{
			"max_concurrent": cfg.Concurrency.MaxConcurrent,
			"per_provider":   cfg.Concurrency.PerProvider,
			"per_model":      cfg.Concurrency.PerModel,
			"queue_depth":    cfg.Concurrency.QueueDepth,
		},
		"cache": map[string]interface{}{
			"enabled":  cfg.Cache.Enabled,
			"max_size": cfg.Cache.MaxSize,
			"ttl":      cfg.Cache.TTL,
		},
		"retry": map[string]interface{}{
			"max_retries":     cfg.Retry.MaxRetries,
			"initial_backoff": cfg.Retry.InitialBackoff,
			"max_backoff":     cfg.Retry.MaxBackoff,
		},
		"provider_count": len(cfg.Providers),
		"model_count":     len(cfg.Models),
		"api_key_count":   len(cfg.APIKeys),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// UsageTrackerProvider is an interface that the proxy handler implements
// to provide access to the usage tracker.
type UsageTrackerProvider interface {
	GetTracker() interface {
		GetStats() interface{}
	}
}

// getRegistry returns the current provider registry (thread-safe).
func (a *AdminHandler) getRegistry() *provider.Registry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.registry
}

// saveAndReload validates, saves config to disk, and hot-reloads the server.
// Uses write lock to prevent concurrent mutations from interleaving.
func (a *AdminHandler) saveAndReload() error {
	a.mu.Lock()
	if a.cfg == nil || a.configPath == "" {
		a.mu.Unlock()
		return fmt.Errorf("config not available")
	}
	if err := a.cfg.SaveConfig(a.configPath); err != nil {
		a.mu.Unlock()
		return fmt.Errorf("save config: %w", err)
	}
	a.mu.Unlock()
	// Reload outside of lock to avoid deadlock (ReloadConfig calls UpdateConfig)
	if a.reloadFunc != nil {
		if err := a.reloadFunc(); err != nil {
			return fmt.Errorf("reload: %w", err)
		}
	}
	return nil
}

// extractName extracts a name from URL path like /admin/providers/{name}.
func extractName(path, prefix string) string {
	p := strings.TrimPrefix(path, prefix)
	p = strings.TrimSuffix(p, "/")
	return p
}

// ==================== Provider CRUD Endpoints ====================

// HandleCreateProvider handles POST /admin/providers.
func (a *AdminHandler) HandleCreateProvider(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var p config.ProviderConfig
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if p.Name == "" {
		a.sendError(w, http.StatusBadRequest, "'name' is required")
		return
	}
	if p.Type == "" {
		a.sendError(w, http.StatusBadRequest, "'type' is required")
		return
	}
	if p.BaseURL == "" {
		a.sendError(w, http.StatusBadRequest, "'base_url' is required")
		return
	}
	if p.Tier == 0 {
		p.Tier = 1
	}

	if err := a.cfg.AddProvider(p); err != nil {
		a.sendError(w, http.StatusConflict, err.Error())
		return
	}

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("provider created via admin", "name", p.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "created", "name": p.Name})
}

// HandleUpdateProvider handles PUT /admin/providers/{name}.
// Supports partial updates — only sent fields are changed.
func (a *AdminHandler) HandleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	name := extractName(r.URL.Path, "/admin/providers/")
	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Provider name required in URL")
		return
	}

	existing := a.cfg.GetProvider(name)
	if existing == nil {
		a.sendError(w, http.StatusNotFound, fmt.Sprintf("provider %q not found", name))
		return
	}

	var req struct {
		Type    string   `json:"type"`
		BaseURL string   `json:"base_url"`
		APIKeys []string `json:"api_keys"`
		Models  []string `json:"models"`
		Tier    *int     `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Partial update: only overwrite fields that were provided
	if req.Type != "" {
		existing.Type = req.Type
	}
	if req.BaseURL != "" {
		existing.BaseURL = req.BaseURL
	}
	if req.APIKeys != nil {
		// Clean and remove duplicates
		uniqueKeys := make([]string, 0, len(req.APIKeys))
		seen := make(map[string]bool)
		for _, k := range req.APIKeys {
			k = strings.TrimSpace(k)
			if k != "" && !seen[k] {
				seen[k] = true
				uniqueKeys = append(uniqueKeys, k)
			}
		}
		existing.APIKeys = uniqueKeys
	}
	if req.Models != nil {
		existing.Models = req.Models
	}
	if req.Tier != nil {
		existing.Tier = *req.Tier
	}

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("provider updated via admin", "name", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated", "name": name})
}

// HandleDeleteProvider handles DELETE /admin/providers/{name}.
func (a *AdminHandler) HandleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	name := extractName(r.URL.Path, "/admin/providers/")
	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Provider name required in URL")
		return
	}

	routes, combos, err := a.cfg.DeleteProvider(name)
	if err != nil {
		a.sendError(w, http.StatusNotFound, err.Error())
		return
	}

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("provider deleted via admin", "name", name, "routes_removed", routes, "combos_removed", combos)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "deleted",
		"name":           name,
		"routes_removed":  routes,
		"combos_removed":  combos,
	})
}

// ==================== Model CRUD Endpoints ====================

// HandleCreateModel handles POST /admin/models.
func (a *AdminHandler) HandleCreateModel(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var m config.ModelConfig
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if m.Name == "" {
		a.sendError(w, http.StatusBadRequest, "'name' is required")
		return
	}

	if err := a.cfg.AddModel(m); err != nil {
		a.sendError(w, http.StatusConflict, err.Error())
		return
	}

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("model created via admin", "name", m.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "created", "name": m.Name})
}

// HandleUpdateModel handles PUT /admin/models/{name} — update model route.
func (a *AdminHandler) HandleUpdateModel(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	name := extractName(r.URL.Path, "/admin/models/")
	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Model name required in URL")
		return
	}

	existing := a.cfg.GetModel(name)
	if existing == nil {
		a.sendError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", name))
		return
	}

	var req struct {
		Provider  string                 `json:"provider"`
		Model     string                 `json:"model"`
		Strategy  string                 `json:"strategy"`
		Backends  []config.BackendConfig `json:"backends"`
		Reasoning *bool                  `json:"reasoning"`
		Disabled  *bool                  `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Partial update
	if req.Provider != "" {
		existing.Provider = req.Provider
	}
	if req.Model != "" {
		existing.Model = req.Model
	}
	if req.Strategy != "" {
		existing.Strategy = req.Strategy
	}
	if req.Backends != nil {
		existing.Backends = req.Backends
	}
	if req.Reasoning != nil {
		existing.Reasoning = *req.Reasoning
	}
	if req.Disabled != nil {
		existing.Disabled = *req.Disabled
	}

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("model updated via admin", "name", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated", "name": name})
}

// HandleDeleteModel handles DELETE /admin/models/{name}.
func (a *AdminHandler) HandleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	name := extractName(r.URL.Path, "/admin/models/")
	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Model name required in URL")
		return
	}

	if err := a.cfg.DeleteModel(name); err != nil {
		a.sendError(w, http.StatusNotFound, err.Error())
		return
	}

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("model deleted via admin", "name", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "name": name})
}

// ==================== API Key Update Endpoint ====================

// HandleUpdateKey handles PUT /admin/keys/{key} — update an existing API key.
func (a *AdminHandler) HandleUpdateKey(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	// Extract key from URL path: /admin/keys/{key}
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		a.sendError(w, http.StatusBadRequest, "Key not specified in URL path")
		return
	}
	key := parts[len(parts)-1]

	var req struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		RateLimit     int      `json:"rate_limit"`
		TokenSaver    *bool    `json:"token_saver"`
		Disabled      *bool    `json:"disabled"`
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, "Read body error: "+err.Error())
		return
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	var rawMap map[string]interface{}
	json.Unmarshal(bodyBytes, &rawMap)
	_, hasTokenSaver := rawMap["token_saver"]

	// Find the key in config by its hash
	var existing *config.APIKeyConfig
	for i := range a.cfg.APIKeys {
		if auth.HashKey(a.cfg.APIKeys[i].Key) == key {
			existing = &a.cfg.APIKeys[i]
			break
		}
	}
	if existing == nil {
		a.sendError(w, http.StatusNotFound, "Key not found")
		return
	}

	disabledVal := existing.Disabled
	if req.Disabled != nil {
		disabledVal = *req.Disabled
	}

	tokenSaverVal := existing.TokenSaver
	if hasTokenSaver {
		tokenSaverVal = req.TokenSaver
	}

	if !a.getKeyStore().UpdateKey(key, req.Name, req.AllowedModels, req.RateLimit, tokenSaverVal, disabledVal) {
		a.sendError(w, http.StatusNotFound, "Key not found in keystore")
		return
	}

	a.persistKeyStore()

	slog.Info("API key updated via admin", "hash_prefix", key[:min(8, len(key))]+"...")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// ==================== Config Update Endpoints ====================

// HandleUpdateConfigServer handles PUT /admin/config/server.
func (a *AdminHandler) HandleUpdateConfigServer(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		LogLevel string `json:"log_level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if req.Host != "" {
		a.cfg.Server.Host = req.Host
	}
	if req.Port > 0 {
		a.cfg.Server.Port = req.Port
	}
	if req.LogLevel != "" {
		a.cfg.Server.LogLevel = req.LogLevel
	}
	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// HandleUpdateConfigConcurrency handles PUT /admin/config/concurrency.
func (a *AdminHandler) HandleUpdateConfigConcurrency(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		MaxConcurrent int `json:"max_concurrent"`
		PerProvider   int `json:"per_provider"`
		PerModel      int `json:"per_model"`
		QueueDepth    int `json:"queue_depth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if req.MaxConcurrent > 0 {
		a.cfg.Concurrency.MaxConcurrent = req.MaxConcurrent
	}
	if req.PerProvider > 0 {
		a.cfg.Concurrency.PerProvider = req.PerProvider
	}
	if req.PerModel > 0 {
		a.cfg.Concurrency.PerModel = req.PerModel
	}
	if req.QueueDepth > 0 {
		a.cfg.Concurrency.QueueDepth = req.QueueDepth
	}
	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// HandleUpdateConfigCache handles PUT /admin/config/cache.
func (a *AdminHandler) HandleUpdateConfigCache(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
		MaxSize int  `json:"max_size"`
		TTL     int  `json:"ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	a.cfg.Cache.Enabled = req.Enabled
	if req.MaxSize > 0 {
		a.cfg.Cache.MaxSize = req.MaxSize
	}
	if req.TTL > 0 {
		a.cfg.Cache.TTL = req.TTL
	}
	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// HandleChangePassword handles PUT /admin/change-password — change the admin secret.
// Supports two auth modes:
//   1. X-Admin-Secret header (when already logged in)
//   2. current_secret in request body (when changing from login page)
func (a *AdminHandler) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentSecret string `json:"current_secret"`
		NewSecret     string `json:"new_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	// Auth: accept either header or body current_secret
	if !a.checkAuth(r) {
		// Header auth failed — try body-based auth
		if req.CurrentSecret == "" {
			a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
			return
		}
		secret := a.getAdminSecret()
		if subtle.ConstantTimeCompare([]byte(req.CurrentSecret), []byte(secret)) != 1 {
			a.sendError(w, http.StatusUnauthorized, "Current password is incorrect")
			return
		}
	}

	if len(req.NewSecret) < 6 {
		a.sendError(w, http.StatusBadRequest, "New secret must be at least 6 characters")
		return
	}

	// Update config and save
	a.mu.Lock()
	a.adminSecret = req.NewSecret
	a.cfg.Server.AdminSecret = req.NewSecret
	if err := a.cfg.SaveConfig(a.configPath); err != nil {
		a.mu.Unlock()
		a.sendError(w, http.StatusInternalServerError, "Failed to save config: "+err.Error())
		return
	}
	a.mu.Unlock()

	slog.Info("admin secret changed via dashboard")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "password changed successfully"})
}

// HandleUpdateConfigRetry handles PUT /admin/config/retry.
func (a *AdminHandler) HandleUpdateConfigRetry(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		MaxRetries     int `json:"max_retries"`
		InitialBackoff int `json:"initial_backoff"`
		MaxBackoff     int `json:"max_backoff"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if req.MaxRetries > 0 {
		a.cfg.Retry.MaxRetries = req.MaxRetries
	}
	if req.InitialBackoff > 0 {
		a.cfg.Retry.InitialBackoff = req.InitialBackoff
	}
	if req.MaxBackoff > 0 {
		a.cfg.Retry.MaxBackoff = req.MaxBackoff
	}
	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// ==================== Diagnostic Endpoints ====================

// providerTemplate defines a quick setup template for a provider.
type providerTemplate struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	BaseURL        string   `json:"base_url"`
	Desc           string   `json:"desc"`
	HelpURL        string   `json:"help_url"`
	FallbackModels []string `json:"fallback_models,omitempty"`
}

var providerTemplates = []providerTemplate{
	{Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1", Desc: "OpenAI \u2014 GPT-4o, GPT-4.1", HelpURL: "https://platform.openai.com/api-keys"},
	{Name: "anthropic", Type: "anthropic", BaseURL: "https://api.anthropic.com", Desc: "Anthropic \u2014 Claude Sonnet, Haiku", HelpURL: "https://console.anthropic.com/settings/keys", FallbackModels: []string{"claude-sonnet-4-20250514", "claude-haiku-4-20250514"}},
	{Name: "groq", Type: "groq", BaseURL: "https://api.groq.com/openai/v1", Desc: "Groq \u2014 Llama, Mixtral (super cepat)", HelpURL: "https://console.groq.com/keys"},
	{Name: "mistral", Type: "mistral", BaseURL: "https://api.mistral.ai/v1", Desc: "Mistral AI \u2014 Mistral Large, Small", HelpURL: "https://console.mistral.ai/api-keys/"},
	{Name: "deepseek", Type: "openai", BaseURL: "https://api.deepseek.com/v1", Desc: "DeepSeek \u2014 Chat, Reasoner", HelpURL: "https://platform.deepseek.com/api_keys"},
	{Name: "openrouter", Type: "openai", BaseURL: "https://openrouter.ai/api/v1", Desc: "OpenRouter \u2014 akses 300+ model", HelpURL: "https://openrouter.ai/keys"},
	{Name: "gemini", Type: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Desc: "Google Gemini \u2014 Gemini 1.5, 2.0, 2.5 Pro/Flash", HelpURL: "https://aistudio.google.com/app/apikey"},
}

// HandleTemplates handles GET /admin/templates \u2014 returns provider templates.
func (a *AdminHandler) HandleTemplates(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providerTemplates)
}

// diagFetchModels calls a provider's /models endpoint to get available models.
func diagFetchModels(baseURL, apiKey, providerType string) ([]string, error) {
	if providerType == "anthropic" {
		return nil, fmt.Errorf("Anthropic does not have a /models endpoint")
	}
	url := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(url, "/models") {
		url += "/models"
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b)[:min(len(b), 200)])
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// Try OpenAI format: {"data": [...]}
	var listResp struct {
		Data []struct{ ID string `json:"id"` } `json:"data"`
	}
	if err := json.Unmarshal(body, &listResp); err == nil && len(listResp.Data) > 0 {
		var models []string
		for _, m := range listResp.Data {
			models = append(models, m.ID)
		}
		return models, nil
	}
	// Try direct array format
	var arrResp []struct{ ID string `json:"id"` }
	if err := json.Unmarshal(body, &arrResp); err == nil && len(arrResp) > 0 {
		var models []string
		for _, m := range arrResp {
			models = append(models, m.ID)
		}
		return models, nil
	}
	return nil, fmt.Errorf("unrecognized response format")
}

// diagTestModel sends a minimal chat completion request to verify the model works.
func diagTestModel(baseURL, apiKey, modelID, providerType string) (string, int64, error) {
	var url string
	var reqBody []byte
	var headers map[string]string
	if providerType == "anthropic" {
		url = strings.TrimRight(baseURL, "/") + "/v1/messages"
		body := map[string]interface{}{"model": modelID, "max_tokens": 10, "messages": []map[string]string{{"role": "user", "content": "Say OK"}}}
		reqBody, _ = json.Marshal(body)
		headers = map[string]string{"Content-Type": "application/json", "x-api-key": apiKey, "anthropic-version": "2023-06-01"}
	} else {
		url = strings.TrimRight(baseURL, "/") + "/chat/completions"
		body := map[string]interface{}{"model": modelID, "messages": []map[string]string{{"role": "user", "content": "Say OK"}}, "max_tokens": 10}
		reqBody, _ = json.Marshal(body)
		headers = map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + apiKey}
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return "", 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", latency, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody)[:min(len(respBody), 300)])
	}
	if providerType == "anthropic" {
		var r struct{ Content []struct{ Type, Text string } }
		json.Unmarshal(respBody, &r)
		for _, c := range r.Content {
			if c.Type == "text" {
				return c.Text, latency, nil
			}
		}
	} else {
		var r struct{ Choices []struct{ Message struct{ Content string } } }
		json.Unmarshal(respBody, &r)
		if len(r.Choices) > 0 {
			return r.Choices[0].Message.Content, latency, nil
		}
	}
	return "(empty)", latency, nil
}

// resolveDiagParams resolves baseURL, apiKey, and providerType from provider name and optional keyIndex,
// or falls back to using the raw baseURL/apiKey passed directly.
func (a *AdminHandler) resolveDiagParams(providerName string, keyIndex *int, baseURL, apiKey, providerType *string) error {
	if providerName != "" {
		prov := a.cfg.GetProvider(providerName)
		if prov == nil {
			return fmt.Errorf("provider '%s' not found", providerName)
		}
		*baseURL = prov.BaseURL
		*providerType = prov.Type
		if len(prov.APIKeys) == 0 {
			return fmt.Errorf("provider '%s' has no API keys", providerName)
		}
		idx := 0
		if keyIndex != nil && *keyIndex >= 0 && *keyIndex < len(prov.APIKeys) {
			idx = *keyIndex
		}
		*apiKey = prov.APIKeys[idx]
		return nil
	}
	if *baseURL == "" || *apiKey == "" {
		return fmt.Errorf("base_url and api_key (or provider name) required")
	}
	return nil
}

// HandleDiagTestKey handles POST /admin/diag/test-key.
func (a *AdminHandler) HandleDiagTestKey(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Type     string `json:"type"`
		Provider string `json:"provider"`
		KeyIndex *int   `json:"key_index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if err := a.resolveDiagParams(req.Provider, req.KeyIndex, &req.BaseURL, &req.APIKey, &req.Type); err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	models, err := diagFetchModels(req.BaseURL, req.APIKey, req.Type)
	if err != nil {
		a.sendError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "model_count": len(models), "models": models})
}

// HandleDiagTestModel handles POST /admin/diag/test-model.
func (a *AdminHandler) HandleDiagTestModel(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Type     string `json:"type"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
		KeyIndex *int   `json:"key_index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		a.sendError(w, http.StatusBadRequest, "model required")
		return
	}
	if err := a.resolveDiagParams(req.Provider, req.KeyIndex, &req.BaseURL, &req.APIKey, &req.Type); err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	response, latency, err := diagTestModel(req.BaseURL, req.APIKey, req.Model, req.Type)
	if err != nil {
		a.sendError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "response": response, "latency_ms": latency})
}

// HandleDiagFetchModels handles POST /admin/diag/fetch-models.
func (a *AdminHandler) HandleDiagFetchModels(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Type     string `json:"type"`
		Provider string `json:"provider"`
		KeyIndex *int   `json:"key_index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if err := a.resolveDiagParams(req.Provider, req.KeyIndex, &req.BaseURL, &req.APIKey, &req.Type); err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	models, err := diagFetchModels(req.BaseURL, req.APIKey, req.Type)
	if err != nil {
		a.sendError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"models": models, "count": len(models)})
}

// HandleQuickSetup handles POST /admin/templates/setup \u2014 create provider from template + fetch models.
func (a *AdminHandler) HandleQuickSetup(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	var req struct {
		TemplateName string   `json:"template_name"`
		APIKey       string   `json:"api_key"`
		Models       []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if req.APIKey == "" {
		a.sendError(w, http.StatusBadRequest, "api_key required")
		return
	}
	// Find template
	var tmpl *providerTemplate
	for _, t := range providerTemplates {
		if t.Name == req.TemplateName {
			tmpl = &t
			break
		}
	}
	if tmpl == nil {
		a.sendError(w, http.StatusBadRequest, "Unknown template: "+req.TemplateName)
		return
	}
	if a.cfg.GetProvider(tmpl.Name) != nil {
		a.sendError(w, http.StatusConflict, "Provider '"+tmpl.Name+"' already exists")
		return
	}
	// Fetch models if not provided
	models := req.Models
	if len(models) == 0 {
		fetched, err := diagFetchModels(tmpl.BaseURL, req.APIKey, tmpl.Type)
		if err != nil {
			models = tmpl.FallbackModels
			if len(models) == 0 {
				a.sendError(w, http.StatusBadGateway, "Failed to fetch models and no fallback: "+err.Error())
				return
			}
		} else {
			models = fetched
		}
	}
	// Create provider
	p := config.ProviderConfig{
		Name:    tmpl.Name,
		Type:    tmpl.Type,
		BaseURL: tmpl.BaseURL,
		APIKeys: []string{req.APIKey},
		Models:  models,
		Tier:    1,
	}
	if err := a.cfg.AddProvider(p); err != nil {
		a.sendError(w, http.StatusConflict, err.Error())
		return
	}
	// Create model routes
	for _, m := range models {
		a.cfg.AddModel(config.ModelConfig{Name: m, Provider: tmpl.Name, Model: m})
	}
	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}
	slog.Info("quick setup via admin", "provider", tmpl.Name, "models", len(models))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "created", "provider": tmpl.Name, "models_count": len(models), "models": models})
}

// HandleGetTokenSaverConfig handles GET /admin/config/token-saver.
func (a *AdminHandler) HandleGetTokenSaverConfig(w http.ResponseWriter, r *http.Request) {
	if !a.CheckAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	a.mu.RLock()
	ts := a.cfg.TokenSaver
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ts)
}

// HandleUpdateTokenSaverConfig handles PUT /admin/config/token-saver.
func (a *AdminHandler) HandleUpdateTokenSaverConfig(w http.ResponseWriter, r *http.Request) {
	if !a.CheckAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var update config.TokenSaverConfig
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	if update.MaxInputBytes <= 0 {
		update.MaxInputBytes = 4096
	}

	a.mu.Lock()
	a.cfg.TokenSaver = update
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("token saver config updated",
		"enabled", update.Enabled,
		"max_input_bytes", update.MaxInputBytes,
		"caveman_mode", update.CavemanMode,
		"compress_user", update.CompressUser,
		"minify_json", update.MinifyJSON,
	)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "updated", "token_saver": update})
}

// HandleGetTunnelStatus serves GET /admin/tunnel.
func (a *AdminHandler) HandleGetTunnelStatus(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"is_running": a.tunnelMgr.IsRunning(),
		"url":        a.tunnelMgr.GetURL(),
	})
}

// HandleToggleTunnel serves POST /admin/tunnel/toggle.
func (a *AdminHandler) HandleToggleTunnel(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}

	a.mu.Lock()
	a.cfg.Server.QuickTunnel = req.Enabled
	a.mu.Unlock()

	// Persist the new configuration
	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Failed to save configuration: "+err.Error())
		return
	}

	// Dynamically start/stop the tunnel
	if req.Enabled {
		if !a.tunnelMgr.IsRunning() {
			go func() {
				slog.Info("starting Cloudflare Quick Tunnel via Admin dashboard toggle...")
				ctx := context.Background()
				_, err := a.tunnelMgr.Start(ctx, a.cfg.Server.Port)
				if err != nil {
					slog.Error("failed to start Cloudflare Quick Tunnel via toggle", "error", err)
				}
			}()
			// Give it a brief moment to start generating URL (e.g. 500ms) or let UI poll it
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		if a.tunnelMgr.IsRunning() {
			a.tunnelMgr.Stop()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"is_running": a.tunnelMgr.IsRunning(),
		"url":        a.tunnelMgr.GetURL(),
	})
}
