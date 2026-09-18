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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aigateway/auth"
	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
	"github.com/aigateway/proxy"
	"github.com/aigateway/relay"
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
	proxyPool   *relay.ProxyPool

	quotaMu    sync.Mutex
	quotaCache map[string]kiroQuotaEntry
}

// kiroQuotaEntry is one cached Kiro quota lookup.
type kiroQuotaEntry struct {
	at    time.Time
	usage *provider.KiroUsage
}

// NewAdminHandler creates a new admin handler.
func NewAdminHandler(keyStore *auth.KeyStore, adminSecret string, stats *proxy.Stats, reloadFunc func() error, configPath string, cfg *config.Config, registry *provider.Registry, tunnelMgr *tunnel.TunnelManager, proxyPool *relay.ProxyPool) *AdminHandler {
	return &AdminHandler{
		keyStore:    keyStore,
		adminSecret: adminSecret,
		stats:       stats,
		reloadFunc:  reloadFunc,
		configPath:  configPath,
		cfg:         cfg,
		registry:    registry,
		tunnelMgr:   tunnelMgr,
		proxyPool:   proxyPool,
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
	if provided == "" {
		authHdr := r.Header.Get("Authorization")
		if strings.HasPrefix(authHdr, "Bearer ") {
			provided = strings.TrimPrefix(authHdr, "Bearer ")
		}
	}
	ok := subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) == 1
	if !ok {
		// Brute-force protection: delay failed attempts
		time.Sleep(1 * time.Second)
	}
	return ok
}

// persistKeyStore saves the current key store state to the config file.
// Holds the write lock for the entire operation to prevent stale data races.
func (a *AdminHandler) persistKeyStore() {
	a.mu.Lock()
	defer a.mu.Unlock()

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

// sendDiagError sends a detailed diagnostic error response with rich metadata.
func (a *AdminHandler) sendDiagError(w http.ResponseWriter, status int, msg string, details map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]interface{}{
		"error": models.ErrorDetail{
			Message: msg,
			Type:    "diag_error",
		},
		"message": msg,
		"ok":      false,
	}
	for k, v := range details {
		resp[k] = v
	}
	json.NewEncoder(w).Encode(resp)
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

	uptimeStr := ""
	if a.stats != nil && !a.stats.StartTime.IsZero() {
		d := time.Since(a.stats.StartTime).Truncate(time.Second)
		uptimeStr = d.String()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_requests":  a.stats.TotalRequests.Load(),
		"active_requests": a.stats.ActiveRequests.Load(),
		"uptime":          uptimeStr,
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
		AccountID   string   `json:"account_id,omitempty"`
		RelayURL    string   `json:"relay_url,omitempty"`
		RelaySecret string   `json:"relay_secret,omitempty"`
		ProxyURL    string   `json:"proxy_url,omitempty"`
		KeyRotation string   `json:"key_rotation,omitempty"`
		Models      []string `json:"models"`
		Healthy     bool     `json:"healthy"`
		HasKey      bool     `json:"has_key"`
		KeyCount     int      `json:"key_count"`
		Keys         []string `json:"keys"`
		APIKeys      []string `json:"api_keys"`
		DisabledKeys []string `json:"disabled_keys"`
		Disabled     bool     `json:"disabled"`
		Quota       *provider.KiroUsage `json:"quota,omitempty"`
	}

	result := make([]providerInfo, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		maskedKeys := make([]string, 0, len(p.APIKeys))
		for _, k := range p.APIKeys {
			k = strings.TrimSpace(k)
			if len(k) >= 12 {
				maskedKeys = append(maskedKeys, k[:8]+"..."+k[len(k)-4:])
			} else if len(k) > 4 {
				maskedKeys = append(maskedKeys, k[:2]+"..."+k[len(k)-2:])
			} else if len(k) > 0 {
				maskedKeys = append(maskedKeys, "***")
			}
		}

		modelsList := p.Models
		if modelsList == nil {
			modelsList = []string{}
		}

		rawKeys := p.APIKeys
		if rawKeys == nil {
			rawKeys = []string{}
		}

		rawDisabledKeys := p.DisabledKeys
		if rawDisabledKeys == nil {
			rawDisabledKeys = []string{}
		}

		info := providerInfo{
			Name:         p.Name,
			Type:         p.Type,
			Tier:         p.Tier,
			BaseURL:      p.BaseURL,
			AccountID:    p.AccountID,
			RelayURL:     p.RelayURL,
			RelaySecret:  p.RelaySecret,
			ProxyURL:     p.ProxyURL,
			KeyRotation:  p.KeyRotationMode(),
			Models:       modelsList,
			HasKey:       len(p.APIKeys) > 0,
			KeyCount:     len(p.APIKeys),
			Keys:         maskedKeys,
			APIKeys:      rawKeys,
			DisabledKeys: rawDisabledKeys,
			Disabled:     p.Disabled,
		}
		// Check health from registry
		if p.Disabled {
			info.Healthy = false
		} else if registry != nil {
			if prov, ok := registry.Get(p.Name); ok {
				info.Healthy = prov.IsHealthy()
			} else {
				info.Healthy = false
			}
		} else {
			info.Healthy = len(p.APIKeys) > 0 // assume healthy if has keys but no registry
		}
		// Kiro free-tier quota (best-effort, 5-minute cache).
		if p.Type == "kiro" && !p.Disabled {
			info.Quota = a.getKiroQuota(p.Name)
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

// getKiroQuota returns cached remaining quota for a Kiro provider (5-minute
// TTL, best-effort — nil when unreachable so listings never block).
func (a *AdminHandler) getKiroQuota(providerName string) *provider.KiroUsage {
	a.quotaMu.Lock()
	if a.quotaCache != nil {
		if e, ok := a.quotaCache[providerName]; ok && time.Since(e.at) < 5*time.Minute {
			a.quotaMu.Unlock()
			return e.usage
		}
	}
	a.quotaMu.Unlock()

	var usage *provider.KiroUsage
	if reg := a.getRegistry(); reg != nil {
		if p, ok := reg.Get(providerName); ok {
			if kp, ok := p.(interface {
				UsageLimits(context.Context) (*provider.KiroUsage, error)
			}); ok {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				u, err := kp.UsageLimits(ctx)
				cancel()
				if err == nil {
					usage = u
				}
			}
		}
	}

	a.quotaMu.Lock()
	if a.quotaCache == nil {
		a.quotaCache = make(map[string]kiroQuotaEntry)
	}
	a.quotaCache[providerName] = kiroQuotaEntry{at: time.Now(), usage: usage}
	a.quotaMu.Unlock()
	return usage
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
	if p.BaseURL == "" && p.Type != "cloudflare" {
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
		Type         string   `json:"type"`
		BaseURL      string   `json:"base_url"`
		AccountID    string   `json:"account_id"`
		APIKeys      []string `json:"api_keys"`
		DisabledKeys []string `json:"disabled_keys"`
		Models       []string `json:"models"`
		Tier         *int     `json:"tier"`
		RelayURL     *string  `json:"relay_url"`
		RelaySecret  *string  `json:"relay_secret"`
		ProxyURL     *string  `json:"proxy_url"`
		KeyRotation  *string  `json:"key_rotation"`
		TokenURL     *string  `json:"token_url"`
		ClientID     *string  `json:"client_id"`
		ClientSecret *string  `json:"client_secret"`
		RefreshToken *string  `json:"refresh_token"`
		ProfileARN   *string  `json:"profile_arn"`
		Region       *string  `json:"region"`
		Disabled     *bool    `json:"disabled"`
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
	if req.AccountID != "" {
		existing.AccountID = req.AccountID
		if existing.Type == "cloudflare" {
			existing.BaseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", existing.AccountID)
		}
	}
	if req.RelayURL != nil {
		existing.RelayURL = *req.RelayURL
	}
	if req.RelaySecret != nil {
		existing.RelaySecret = *req.RelaySecret
	}
	if req.ProxyURL != nil {
		existing.ProxyURL = *req.ProxyURL
	}
	if req.KeyRotation != nil {
		kr := strings.ToLower(strings.TrimSpace(*req.KeyRotation))
		if kr != "" && kr != "round-robin" && kr != "sticky" {
			a.sendError(w, http.StatusBadRequest, "invalid key_rotation (valid: round-robin, sticky)")
			return
		}
		if kr == "round-robin" {
			kr = "" // default — keep config file clean
		}
		existing.KeyRotation = kr
	}
	if req.TokenURL != nil {
		existing.TokenURL = *req.TokenURL
	}
	if req.ClientID != nil {
		existing.ClientID = *req.ClientID
	}
	if req.ClientSecret != nil {
		existing.ClientSecret = *req.ClientSecret
	}
	if req.RefreshToken != nil {
		existing.RefreshToken = *req.RefreshToken
	}
	if req.ProfileARN != nil {
		existing.ProfileARN = *req.ProfileARN
	}
	if req.Region != nil {
		existing.Region = *req.Region
	}
	if req.Disabled != nil {
		existing.Disabled = *req.Disabled
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
	if req.DisabledKeys != nil {
		uniqueDisabled := make([]string, 0, len(req.DisabledKeys))
		seen := make(map[string]bool)
		for _, k := range req.DisabledKeys {
			k = strings.TrimSpace(k)
			if k != "" && !seen[k] {
				seen[k] = true
				uniqueDisabled = append(uniqueDisabled, k)
			}
		}
		existing.DisabledKeys = uniqueDisabled
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
	_, hasRateLimit := rawMap["rate_limit"]

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

	// Absent rate_limit must preserve the existing value (not reset to 0/unlimited).
	rateLimitVal := existing.RateLimit
	if hasRateLimit {
		rateLimitVal = req.RateLimit
	}

	if !a.getKeyStore().UpdateKey(key, req.Name, req.AllowedModels, rateLimitVal, tokenSaverVal, disabledVal) {
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
// Model lists are never hardcoded here — they are always fetched live from
// the provider catalog (or supplied manually by the caller).
type providerTemplate struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	BaseURL        string   `json:"base_url"`
	Desc           string   `json:"desc"`
	HelpURL        string   `json:"help_url"`
}

var providerTemplates = []providerTemplate{
	{Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1", Desc: "OpenAI \u2014 GPT-4o, GPT-4.1", HelpURL: "https://platform.openai.com/api-keys"},
	{Name: "anthropic", Type: "anthropic", BaseURL: "https://api.anthropic.com", Desc: "Anthropic \u2014 Claude Sonnet, Haiku", HelpURL: "https://console.anthropic.com/settings/keys"},
	{Name: "groq", Type: "groq", BaseURL: "https://api.groq.com/openai/v1", Desc: "Groq \u2014 Llama, Mixtral (super cepat)", HelpURL: "https://console.groq.com/keys"},
	{Name: "mistral", Type: "mistral", BaseURL: "https://api.mistral.ai/v1", Desc: "Mistral AI \u2014 Mistral Large, Small", HelpURL: "https://console.mistral.ai/api-keys/"},
	{Name: "deepseek", Type: "openai", BaseURL: "https://api.deepseek.com/v1", Desc: "DeepSeek \u2014 Chat, Reasoner", HelpURL: "https://platform.deepseek.com/api_keys"},
	{Name: "openrouter", Type: "openai", BaseURL: "https://openrouter.ai/api/v1", Desc: "OpenRouter \u2014 akses 300+ model", HelpURL: "https://openrouter.ai/keys"},
	{Name: "oauth", Type: "oauth", BaseURL: "", Desc: "OAuth2 generic - token refresh otomatis (isi base_url + token_url + refresh_token)", HelpURL: ""},
	{Name: "kiro", Type: "kiro", BaseURL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse", Desc: "Kiro AI - free tier via API key atau refresh token", HelpURL: "https://kiro.dev"},
	{Name: "gemini", Type: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Desc: "Google Gemini — Gemini 1.5, 2.0, 2.5 Pro/Flash", HelpURL: "https://aistudio.google.com/app/apikey"},
	{Name: "opencode", Type: "opencode", BaseURL: "https://opencode.ai/zen/v1", Desc: "OpenCode Zen — Keyless Free Models (MiMo, DeepSeek, Nemotron)", HelpURL: "https://opencode.ai"},
	{Name: "mimo", Type: "mimo", BaseURL: "https://opencode.ai/zen/v1", Desc: "Xiaomi MiMo Free — Keyless AI via MiMo Code CLI", HelpURL: "https://xiaomimimo.com"},
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
// proxyURL pins this attempt to a checked-out egress proxy ("" = direct);
// the caller's client transport resolves it via ContextProxyFunc.
func diagFetchModels(client *http.Client, baseURL, apiKey, providerType, proxyURL string) ([]string, error) {
	if providerType == "anthropic" {
		return nil, fmt.Errorf("Anthropic does not have a /models endpoint")
	}

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	reqCtx := context.Background()
	if strings.TrimSpace(proxyURL) != "" {
		reqCtx = provider.WithEgressProxy(reqCtx, proxyURL)
	}

	if providerType == "cloudflare" {
		url := strings.TrimRight(baseURL, "/")
		url = strings.Replace(url, "/v1", "/models/search", 1)
		req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b)[:min(len(b), 200)])
		}
		var cfResp struct {
			Result []struct {
				Name string `json:"name"`
			} `json:"result"`
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(body, &cfResp); err != nil {
			return nil, err
		}
		var models []string
		for _, m := range cfResp.Result {
			if m.Name != "" {
				models = append(models, m.Name)
			}
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("no models found in Cloudflare catalog")
		}
		return models, nil
	}

	url := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(url, "/models") {
		url += "/models"
	}
	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else if providerType == "opencode" {
		req.Header.Set("Authorization", "Bearer public")
	}
	if providerType == "opencode" {
		sess := provider.GenerateOpenCodeSessionID()
		req.Header.Set("x-opencode-client", "cli")
		req.Header.Set("x-opencode-project", provider.OpenCodeProjectID())
		req.Header.Set("x-opencode-session", sess)
		req.Header.Set("x-opencode-request", provider.GenerateOpenCodeRequestID())
		req.Header.Set("X-Session-ID", sess)
		req.Header.Set("User-Agent", provider.OpenCodeDefaultUA)
	} else if providerType == "mimo" {
		req.Header.Set("X-Mimo-Source", "mimocode-cli")
	}
	req.Header.Set("Accept", "application/json")
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

// detectReasoning reports whether a model looks like a reasoning/thinking model,
// from response evidence (reasoning blocks in the raw payload) or well-known
// model-ID keywords (mirrors cli.isReasoningModelID).
func detectReasoning(modelID, rawBody string) bool {
	lowerBody := strings.ToLower(rawBody)
	if strings.Contains(lowerBody, `"reasoning_content"`) ||
		strings.Contains(lowerBody, `"reasoning":`) ||
		strings.Contains(lowerBody, "<think") ||
		strings.Contains(lowerBody, `"type":"thinking"`) ||
		strings.Contains(lowerBody, `"thinking":`) {
		return true
	}
	m := strings.ToLower(modelID)
	// Gemini (2.5/3.x) thinks by default — except Gemma and embedding models.
	if strings.Contains(m, "gemini") && !strings.Contains(m, "gemma") && !strings.Contains(m, "embed") {
		return true
	}
	return strings.Contains(m, "r1") ||
		strings.Contains(m, "reasoning") ||
		strings.Contains(m, "o1") ||
		strings.Contains(m, "o3") ||
		strings.Contains(m, "thinking") ||
		strings.Contains(m, "qwen3") ||
		strings.Contains(m, "qwq") ||
		strings.Contains(m, "gpt-oss") ||
		strings.Contains(m, "command-a") ||
		strings.Contains(m, "north-mini")
}

// diagTestModel sends a minimal chat completion request to verify the model works.
// Returns the response text, latency, whether reasoning was detected, and error.
// proxyURL pins this attempt to a checked-out egress proxy ("" = direct);
// the caller's client transport resolves it via ContextProxyFunc.
func diagTestModel(client *http.Client, baseURL, apiKey, modelID, providerType, proxyURL string) (string, int64, bool, error) {
	var url string
	var reqBody []byte
	headers := make(map[string]string)
	if providerType == "anthropic" {
		url = strings.TrimRight(baseURL, "/") + "/v1/messages"
		body := map[string]interface{}{"model": modelID, "max_tokens": 10, "messages": []map[string]string{{"role": "user", "content": "Say OK"}}}
		reqBody, _ = json.Marshal(body)
		headers["Content-Type"] = "application/json"
		headers["anthropic-version"] = "2023-06-01"
		if apiKey != "" {
			headers["x-api-key"] = apiKey
		}
	} else if providerType == "opencode" {
		url = strings.TrimRight(baseURL, "/") + "/chat/completions"
		chatReq := &models.ChatCompletionRequest{
			Model: modelID,
			Messages: []models.Message{
				{Role: "user", Content: json.RawMessage(`"Say OK"`)},
			},
			Stream: true,
		}
		prepared := provider.PrepareOpenCodeRequest(chatReq)
		var err error
		reqBody, err = json.Marshal(prepared)
		if err != nil {
			return "", 0, false, err
		}
		headers["Content-Type"] = "application/json"
		sess := provider.GenerateOpenCodeSessionID()
		headers["x-opencode-client"] = "cli"
		headers["x-opencode-project"] = provider.OpenCodeProjectID()
		headers["x-opencode-session"] = sess
		headers["x-opencode-request"] = provider.GenerateOpenCodeRequestID()
		headers["X-Session-ID"] = sess
		headers["User-Agent"] = provider.OpenCodeDefaultUA
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		} else {
			headers["Authorization"] = "Bearer public"
		}
	} else {
		url = strings.TrimRight(baseURL, "/") + "/chat/completions"
		body := map[string]interface{}{"model": modelID, "messages": []map[string]string{{"role": "user", "content": "Say OK"}}, "max_tokens": 10}
		reqBody, _ = json.Marshal(body)
		headers["Content-Type"] = "application/json"
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
		if providerType == "mimo" {
			headers["X-Mimo-Source"] = "mimocode-cli"
		}
	}
	reqCtx := context.Background()
	if strings.TrimSpace(proxyURL) != "" {
		reqCtx = provider.WithEgressProxy(reqCtx, proxyURL)
	}
	req, err := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return "", 0, false, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", latency, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody)[:min(len(respBody), 300)])
	}
	if providerType == "opencode" {
		chatResp, err := provider.ParseOpenCodeSSEStream(resp.Body, modelID)
		if err != nil {
			return "", latency, false, fmt.Errorf("parse opencode sse stream: %w", err)
		}
		content := ""
		reasoning := false
		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message != nil {
			content = chatResp.Choices[0].Message.ContentString()
			if chatResp.Choices[0].Message.ReasoningContent != "" {
				reasoning = true
			}
		}
		if content == "" {
			content = "OK"
		}
		if !reasoning {
			reasoning = detectReasoning(modelID, content)
		}
		return content, latency, reasoning, nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	reasoning := detectReasoning(modelID, string(respBody))
	if providerType == "anthropic" {
		var r struct{ Content []struct{ Type, Text string } }
		json.Unmarshal(respBody, &r)
		for _, c := range r.Content {
			if c.Type == "text" {
				return c.Text, latency, reasoning, nil
			}
		}
	} else {
		var r struct{ Choices []struct{ Message struct{ Content string } } }
		json.Unmarshal(respBody, &r)
		if len(r.Choices) > 0 {
			return r.Choices[0].Message.Content, latency, reasoning, nil
		}
	}
	return "(empty)", latency, reasoning, nil
}

// maskDiagKey returns a masked representation of an API key for display (e.g. "abcd1234...wxyz").
func maskDiagKey(k string) string {
	k = strings.TrimSpace(k)
	if len(k) >= 12 {
		return k[:8] + "..." + k[len(k)-4:]
	}
	if len(k) > 4 {
		return k[:2] + "..." + k[len(k)-2:]
	}
	if len(k) > 0 {
		return "***"
	}
	return "-"
}

// maskProxyURL returns a credential-stripped display form of a proxy URL
// (e.g. "http://user:pass@1.2.3.4:8080" → "http://1.2.3.4:8080").
// Returns "none" when empty/direct so logs explicitly show proxy absence.
func maskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "none" || raw == "direct" {
		return "none"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "***"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host)
}

// diagProxyInfo describes which egress proxy a diag attempt will use.
type diagProxyInfo struct {
	URL     string // raw proxy URL ("" = direct)
	Mode    string // "pool" | "custom" | "none" | "pool (empty)" | "none (pool disabled)"
	Display string // masked URL or "none" — safe for logs/JSON
}

// diagCheckoutProxy resolves the egress proxy for ONE diag attempt on providerName.
// Pool mode checks out a fresh proxy per call so each key attempt can rotate.
// Custom proxy_url returns the configured URL; anything else is direct ("none").
func (a *AdminHandler) diagCheckoutProxy(providerName string) diagProxyInfo {
	prov := a.cfg.GetProvider(providerName)
	if prov == nil {
		return diagProxyInfo{Mode: "none", Display: "none"}
	}
	raw := strings.TrimSpace(prov.ProxyURL)
	poolEnabled := a.proxyPool != nil && a.proxyPool.IsEnabled()
	// Explicit custom proxy — transport already pins it, just display masked.
	if raw != "" && raw != "auto" && raw != "pool" && raw != "direct" && raw != "none" {
		return diagProxyInfo{URL: raw, Mode: "custom", Display: maskProxyURL(raw)}
	}
	if raw == "direct" || raw == "none" {
		return diagProxyInfo{Mode: "none", Display: "none"}
	}
	isPoolMode := raw == "pool" || raw == "auto"
	isKeylessAuto := raw == "" && (prov.Type == "opencode" || prov.Type == "mimo")
	if isPoolMode || isKeylessAuto {
		if !poolEnabled {
			return diagProxyInfo{Mode: "none (pool disabled)", Display: "none"}
		}
		checked := a.proxyPool.Checkout()
		if checked == "" {
			return diagProxyInfo{Mode: "pool (empty)", Display: "none"}
		}
		return diagProxyInfo{URL: checked, Mode: "pool", Display: maskProxyURL(checked)}
	}
	return diagProxyInfo{Mode: "none", Display: "none"}
}

// diagProxyModeLabel returns the configured proxy mode for a provider without
// checking out a URL (for start-lines and Kiro rotation where the exact egress
// URL is chosen inside the provider).
func (a *AdminHandler) diagProxyModeLabel(providerName string) string {
	prov := a.cfg.GetProvider(providerName)
	if prov == nil {
		return "none"
	}
	raw := strings.TrimSpace(prov.ProxyURL)
	if raw != "" && raw != "auto" && raw != "pool" && raw != "direct" && raw != "none" {
		return "custom:" + maskProxyURL(raw)
	}
	if raw == "pool" || raw == "auto" {
		if a.proxyPool != nil && a.proxyPool.IsEnabled() {
			return "pool"
		}
		return "none (pool disabled)"
	}
	if raw == "" && (prov.Type == "opencode" || prov.Type == "mimo") {
		if a.proxyPool != nil && a.proxyPool.IsEnabled() {
			return "pool"
		}
		return "none"
	}
	return "none"
}

// diagReportProxy feeds a per-attempt outcome back to the pool.
// Only transport-level failures (proxy never reached upstream) count as proxy
// failures; upstream HTTP errors prove the proxy works.
func (a *AdminHandler) diagReportProxy(proxyURL string, err error) {
	if a.proxyPool == nil || strings.TrimSpace(proxyURL) == "" {
		return
	}
	if err == nil {
		a.proxyPool.Report(proxyURL, false)
		return
	}
	msg := err.Error()
	// Upstream answered with HTTP status / parse error → proxy itself is fine.
	if strings.Contains(msg, "HTTP ") || strings.Contains(msg, "parse ") {
		a.proxyPool.Report(proxyURL, false)
		return
	}
	a.proxyPool.Report(proxyURL, true)
}

// diagKeyTag returns a human-readable key label for logs (e.g. "key #1 (sk-XXXX...YYYY)").
func diagKeyTag(index int, key string) string {
	if strings.TrimSpace(key) == "" {
		return "keyless"
	}
	if index >= 0 {
		return fmt.Sprintf("key #%d (%s)", index+1, maskDiagKey(key))
	}
	return fmt.Sprintf("custom key (%s)", maskDiagKey(key))
}

// diagProxySuffix formats "proxy ..." for one-line diag logs:
// "proxy none", "proxy none (pool empty)", or "proxy http://1.2.3.4:8080 (pool)".
func diagProxySuffix(px diagProxyInfo) string {
	if px.Display == "" || px.Display == "none" {
		if px.Mode == "" || px.Mode == "none" {
			return "proxy none"
		}
		return fmt.Sprintf("proxy none (%s)", px.Mode)
	}
	if px.Mode == "" {
		return fmt.Sprintf("proxy %s", px.Display)
	}
	return fmt.Sprintf("proxy %s (%s)", px.Display, px.Mode)
}

// diagKeyCandidate is one upstream key to try during a diag request.
type diagKeyCandidate struct {
	Key   string
	Index int // 0-based index into provider APIKeys, -1 for explicit/raw keys
}

// isDiagFallbackRetryable reports whether a diag failure is worth retrying with the next key.
// Model/payload errors (400, 404, model not found) fail identically for every key,
// so they must NOT trigger fallback. Everything else (429 rate limit, 401/403 key
// problems, 5xx, network errors) is key-specific or transient and should try the next key.
func isDiagFallbackRetryable(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	// Never fallback on these: same result for every key.
	nonRetryableMarkers := []string{
		"http 400", "http 404",
		"model_not_found", "does not exist", "model not found",
		"invalid_request", "invalid model",
	}
	for _, m := range nonRetryableMarkers {
		if strings.Contains(lower, m) {
			return false
		}
	}
	return true
}

// buildDiagKeyCandidates resolves baseURL/providerType from the provider name and builds
// the ordered list of upstream keys to try.
//   - explicit api_key in request  -> single candidate, no fallback (Index=-1)
//   - provider + valid key_index   -> single candidate, no fallback (tests one specific key)
//   - provider without key_index   -> ALL active provider keys in order (auto-fallback mode)
//   - raw baseURL/api_key          -> single candidate, no fallback
func (a *AdminHandler) buildDiagKeyCandidates(providerName string, keyIndex *int, baseURL, providerType *string, explicitAPIKey string) ([]diagKeyCandidate, error) {
	if providerName != "" {
		prov := a.cfg.GetProvider(providerName)
		if prov == nil {
			return nil, fmt.Errorf("provider '%s' not found", providerName)
		}
		*baseURL = prov.BaseURL
		if *providerType == "" {
			*providerType = prov.Type
		}
		if strings.TrimSpace(explicitAPIKey) != "" {
			return []diagKeyCandidate{{Key: strings.TrimSpace(explicitAPIKey), Index: -1}}, nil
		}
		// OAuth providers authenticate with a live refreshed token, not stored keys.
		if prov.Type == "oauth" {
			tok, err := a.oauthDiagToken(providerName)
			if err != nil {
				return nil, err
			}
			return []diagKeyCandidate{{Key: tok, Index: -1}}, nil
		}
		if keyIndex != nil && *keyIndex >= 0 && *keyIndex < len(prov.APIKeys) {
			return []diagKeyCandidate{{Key: prov.APIKeys[*keyIndex], Index: *keyIndex}}, nil
		}
		if len(prov.APIKeys) == 0 {
			if *providerType == "opencode" || *providerType == "mimo" {
				return []diagKeyCandidate{{Key: "", Index: -1}}, nil
			}
			if len(prov.DisabledKeys) > 0 {
				return nil, fmt.Errorf("provider '%s' has no active API keys (%d disabled)", providerName, len(prov.DisabledKeys))
			}
			return nil, fmt.Errorf("provider '%s' has no API keys", providerName)
		}
		candidates := make([]diagKeyCandidate, 0, len(prov.APIKeys))
		for i, k := range prov.APIKeys {
			if strings.TrimSpace(k) == "" {
				continue
			}
			candidates = append(candidates, diagKeyCandidate{Key: k, Index: i})
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("provider '%s' has no API keys", providerName)
		}
		return candidates, nil
	}
	if strings.TrimSpace(explicitAPIKey) == "" && *providerType != "opencode" && *providerType != "mimo" {
		return nil, fmt.Errorf("base_url and api_key (or provider name) required")
	}
	if *baseURL == "" {
		return nil, fmt.Errorf("base_url and api_key (or provider name) required")
	}
	return []diagKeyCandidate{{Key: strings.TrimSpace(explicitAPIKey), Index: -1}}, nil
}

// oauthDiagToken mints a live bearer token for an OAuth provider so the
// diagnostics endpoints can test it without a stored key.
func (a *AdminHandler) oauthDiagToken(providerName string) (string, error) {
	reg := a.getRegistry()
	if reg == nil {
		return "", fmt.Errorf("provider registry not initialized")
	}
	p, ok := reg.Get(providerName)
	if !ok {
		return "", fmt.Errorf("provider '%s' not found", providerName)
	}
	src, ok := p.(interface {
		OAuthToken(context.Context) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("provider '%s' does not support OAuth tokens", providerName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tok, err := src.OAuthToken(ctx)
	if err != nil {
		return "", fmt.Errorf("oauth token: %w", err)
	}
	if tok == "" {
		return "", fmt.Errorf("oauth provider '%s' returned an empty token", providerName)
	}
	return tok, nil
}

// kiroDiagCredentials resolves Kiro credentials for diagnostics: an explicit
// key (API key, or a refresh token starting with "aorAAAAAG"), a specific
// key index, or the provider's configured credentials.
func (a *AdminHandler) kiroDiagCredentials(providerName, explicitKey string, keyIndex *int) (provider.KiroCredentials, bool, error) {
	var cred provider.KiroCredentials
	if providerName == "" {
		return cred, false, nil
	}
	prov := a.cfg.GetProvider(providerName)
	if prov == nil || prov.Type != "kiro" {
		return cred, false, nil
	}
	if k := strings.TrimSpace(explicitKey); k != "" {
		if strings.HasPrefix(k, "aorAAAAAG") {
			return provider.KiroCredentials{RefreshToken: k}, true, nil
		}
		return provider.KiroCredentials{APIKey: k}, true, nil
	}
	if keyIndex != nil && *keyIndex >= 0 && *keyIndex < len(prov.APIKeys) {
		return provider.KiroCredentials{APIKey: prov.APIKeys[*keyIndex]}, true, nil
	}
	if len(prov.APIKeys) > 0 {
		return provider.KiroCredentials{APIKey: prov.APIKeys[0]}, true, nil
	}
	if strings.TrimSpace(prov.RefreshToken) != "" {
		return provider.KiroCredentials{RefreshToken: prov.RefreshToken, ProfileARN: prov.ProfileARN}, true, nil
	}
	return cred, true, fmt.Errorf("provider '%s' has no kiro credentials (api key or refresh token)", providerName)
}

// diagTestKiroModel runs a minimal completion through the registered Kiro
// provider (which handles key rotation and surface failover internally).
func (a *AdminHandler) diagTestKiroModel(w http.ResponseWriter, r *http.Request, providerName, model string) {
	reg := a.getRegistry()
	if reg == nil {
		a.sendError(w, http.StatusBadRequest, "provider registry not initialized")
		return
	}
	p, ok := reg.Get(providerName)
	if !ok {
		a.sendError(w, http.StatusBadRequest, fmt.Sprintf("provider '%s' not found", providerName))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	start := time.Now()
	proxyLabel := a.diagProxyModeLabel(providerName)
	slog.Info(fmt.Sprintf("🧪 [DIAG] test-model %s/%s → kiro rotation · proxy %s", providerName, model, proxyLabel))
	resp, err := p.ChatCompletion(ctx, &models.ChatCompletionRequest{
		Model:    model,
		Messages: []models.Message{{Role: "user", Content: json.RawMessage(`"Say OK"`)}},
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		slog.Warn(fmt.Sprintf("❌ [DIAG] test-model %s/%s fail via kiro rotation · proxy %s · %dms · %s", providerName, model, proxyLabel, latency, err.Error()))
		a.sendDiagError(w, http.StatusBadGateway, err.Error(), map[string]interface{}{
			"provider": providerName, "model": model, "type": "kiro",
			"target": "kiro-generateAssistantResponse", "latency_ms": latency,
			"proxy": proxyLabel, "proxy_mode": proxyLabel,
		})
		return
	}
	text := "(empty)"
	if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
		text = resp.Choices[0].Message.ContentString()
	}
	slog.Info(fmt.Sprintf("✅ [DIAG] test-model %s/%s ok via kiro rotation · proxy %s · %dms", providerName, model, proxyLabel, latency))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true, "response": text, "latency_ms": latency, "status": "OK",
		"provider": providerName, "model": model, "type": "kiro",
		"target":              "kiro-generateAssistantResponse",
		"key_label":           "auto (kiro rotation)",
		"proxy":               proxyLabel,
		"proxy_mode":          proxyLabel,
		"reasoning_detected":  detectReasoning(model, text),
	})
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
		if *providerType == "" {
			*providerType = prov.Type
		}
		if *apiKey == "" {
			if len(prov.APIKeys) == 0 && len(prov.DisabledKeys) == 0 {
				if prov.Type != "opencode" && prov.Type != "mimo" {
					return fmt.Errorf("provider '%s' has no API keys", providerName)
				}
				*apiKey = ""
			} else {
				idx := 0
				if keyIndex != nil && *keyIndex >= 0 && *keyIndex < len(prov.APIKeys) {
					idx = *keyIndex
				}
				if idx < len(prov.APIKeys) {
					*apiKey = prov.APIKeys[idx]
				} else if len(prov.DisabledKeys) > 0 {
					*apiKey = prov.DisabledKeys[0]
				}
			}
		}
		return nil
	}
	if *baseURL == "" || (*apiKey == "" && *providerType != "opencode" && *providerType != "mimo") {
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
	if cred, isKiro, err := a.kiroDiagCredentials(req.Provider, req.APIKey, req.KeyIndex); err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	} else if isKiro {
		proxyLabel := a.diagProxyModeLabel(req.Provider)
		slog.Info(fmt.Sprintf("🧪 [DIAG] test-key %s → kiro-catalog · proxy %s", req.Provider, proxyLabel))
		start := time.Now()
		models, ferr := provider.KiroListModels(r.Context(), nil, cred)
		latency := time.Since(start).Milliseconds()
		targetURL := "kiro-catalog"
		if ferr != nil {
			slog.Warn(fmt.Sprintf("❌ [DIAG] test-key %s fail · proxy %s · %dms · %s", req.Provider, proxyLabel, latency, ferr.Error()))
			a.sendDiagError(w, http.StatusBadGateway, ferr.Error(), map[string]interface{}{
				"provider":   req.Provider,
				"target":     targetURL,
				"key_index":  req.KeyIndex,
				"type":       "kiro",
				"latency_ms": latency,
				"proxy":      proxyLabel,
				"proxy_mode": proxyLabel,
			})
			return
		}
		slog.Info(fmt.Sprintf("✅ [DIAG] test-key %s ok (%d models) · proxy %s · %dms", req.Provider, len(models), proxyLabel, latency))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":          true,
			"model_count": len(models),
			"models":      models,
			"latency_ms":  latency,
			"status":      "OK",
			"provider":    req.Provider,
			"target":      targetURL,
			"proxy":       proxyLabel,
			"proxy_mode":  proxyLabel,
		})
		return
	}
	var client *http.Client
	if req.Provider != "" && a.registry != nil {
		if p, ok := a.registry.Get(req.Provider); ok {
			if up, ok := p.(provider.UpstreamConfigProvider); ok && up.Client() != nil {
				client = up.Client()
			}
		}
	}
	px := a.diagCheckoutProxy(req.Provider)
	keyLog := maskDiagKey(req.APIKey)
	if strings.TrimSpace(req.APIKey) == "" {
		keyLog = "keyless"
	}
	slog.Info(fmt.Sprintf("🧪 [DIAG] test-key %s → %s/models · key %s · %s", req.Provider, strings.TrimRight(req.BaseURL, "/"), keyLog, diagProxySuffix(px)))
	start := time.Now()
	models, err := diagFetchModels(client, req.BaseURL, req.APIKey, req.Type, px.URL)
	latency := time.Since(start).Milliseconds()
	a.diagReportProxy(px.URL, err)
	targetURL := strings.TrimRight(req.BaseURL, "/") + "/models"
	if err != nil {
		slog.Warn(fmt.Sprintf("❌ [DIAG] test-key %s fail · key %s · %s · %dms · %s", req.Provider, keyLog, diagProxySuffix(px), latency, err.Error()))
		a.sendDiagError(w, http.StatusBadGateway, err.Error(), map[string]interface{}{
			"provider":   req.Provider,
			"base_url":   req.BaseURL,
			"target":     targetURL,
			"key_index":  req.KeyIndex,
			"key_masked": keyLog,
			"type":       req.Type,
			"latency_ms": latency,
			"proxy":      px.Display,
			"proxy_mode": px.Mode,
		})
		return
	}
	slog.Info(fmt.Sprintf("✅ [DIAG] test-key %s ok (%d models) · key %s · %s · %dms", req.Provider, len(models), keyLog, diagProxySuffix(px), latency))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":          true,
		"model_count": len(models),
		"models":      models,
		"latency_ms":  latency,
		"status":      "OK",
		"provider":    req.Provider,
		"base_url":    req.BaseURL,
		"target":      targetURL,
		"key_masked":  keyLog,
		"proxy":       px.Display,
		"proxy_mode":  px.Mode,
	})
}

// HandleDiagTestModel handles POST /admin/diag/test-model.
// Auto-fallback: when provider is given WITHOUT explicit api_key/key_index,
// tries each active key in order until one succeeds (e.g. on 429 rate limit).
// Success response includes key_index (0-based), key_number (1-based), key_masked,
// attempts, fallback_used, and key_count so the dashboard can show "success with key N".
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
	// Kiro speaks CodeWhisperer EventStream, not OpenAI HTTP — run the test
	// through the registered provider (which rotates keys/surfaces itself).
	if prov := a.cfg.GetProvider(req.Provider); prov != nil && prov.Type == "kiro" {
		a.diagTestKiroModel(w, r, req.Provider, req.Model)
		return
	}
	candidates, err := a.buildDiagKeyCandidates(req.Provider, req.KeyIndex, &req.BaseURL, &req.Type, req.APIKey)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	var client *http.Client
	if req.Provider != "" && a.registry != nil {
		if p, ok := a.registry.Get(req.Provider); ok {
			if up, ok := p.(provider.UpstreamConfigProvider); ok && up.Client() != nil {
				client = up.Client()
			}
		}
	}
	targetURL := strings.TrimRight(req.BaseURL, "/") + "/chat/completions"
	if req.Type == "anthropic" {
		targetURL = strings.TrimRight(req.BaseURL, "/") + "/v1/messages"
	}
	type keyFailure struct {
		KeyIndex  interface{} `json:"key_index"`
		KeyMasked string      `json:"key_masked"`
		Proxy     string      `json:"proxy,omitempty"`
		ProxyMode string      `json:"proxy_mode,omitempty"`
		Error     string      `json:"error"`
	}
	failures := make([]keyFailure, 0, len(candidates))
	var lastLatency int64
	var lastProxy diagProxyInfo
	proxyModeLabel := a.diagProxyModeLabel(req.Provider)
	slog.Info(fmt.Sprintf("🧪 [DIAG] test-model %s/%s → trying %d key(s) · proxy %s", req.Provider, req.Model, len(candidates), proxyModeLabel))
	for i, cand := range candidates {
		keyTag := diagKeyTag(cand.Index, cand.Key)
		px := a.diagCheckoutProxy(req.Provider)
		proxySuffix := diagProxySuffix(px)
		response, latency, reasoning, err := diagTestModel(client, req.BaseURL, cand.Key, req.Model, req.Type, px.URL)
		a.diagReportProxy(px.URL, err)
		if err == nil {
			resp := map[string]interface{}{
				"ok":                 true,
				"response":           response,
				"latency_ms":         latency,
				"status":             "OK",
				"provider":           req.Provider,
				"model":              req.Model,
				"target":             targetURL,
				"attempts":           i + 1,
				"key_count":          len(candidates),
				"reasoning_detected": reasoning,
				"proxy":              px.Display,
				"proxy_mode":         px.Mode,
			}
			if cand.Index >= 0 {
				resp["key_index"] = cand.Index
				resp["key_number"] = cand.Index + 1
				resp["key_label"] = fmt.Sprintf("Key #%d", cand.Index+1)
				resp["key_masked"] = maskDiagKey(cand.Key)
				resp["fallback_used"] = i > 0
			} else {
				resp["key_masked"] = maskDiagKey(cand.Key)
			}
			if len(failures) > 0 {
				resp["failed_attempts"] = failures
			}
			slog.Info(fmt.Sprintf("✅ [DIAG] test-model %s/%s ok via %s · %s · %dms", req.Provider, req.Model, keyTag, proxySuffix, latency))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		lastLatency = latency
		lastProxy = px
		var idx interface{}
		if cand.Index >= 0 {
			idx = cand.Index
		}
		failures = append(failures, keyFailure{KeyIndex: idx, KeyMasked: maskDiagKey(cand.Key), Proxy: px.Display, ProxyMode: px.Mode, Error: err.Error()})
		slog.Warn(fmt.Sprintf("❌ [DIAG] test-model %s/%s fail via %s · %s · %dms · %s", req.Provider, req.Model, keyTag, proxySuffix, latency, err.Error()))
		if i < len(candidates)-1 {
			if !isDiagFallbackRetryable(err) {
				break
			}
			// Small pacing so rapid fallback doesn't trip per-IP limits.
			time.Sleep(150 * time.Millisecond)
			continue
		}
	}
	// All candidates exhausted (or non-retryable error on first key).
	lastErrMsg := ""
	if len(failures) > 0 {
		lastErrMsg = failures[len(failures)-1].Error
	}
	if strings.TrimSpace(lastErrMsg) == "" {
		lastErrMsg = fmt.Sprintf("upstream %s/%s failed without detail (target %s, %d attempt(s))", req.Provider, req.Model, targetURL, len(failures))
	}
	details := map[string]interface{}{
		"provider":   req.Provider,
		"base_url":   req.BaseURL,
		"model":      req.Model,
		"type":       req.Type,
		"target":     targetURL,
		"latency_ms": lastLatency,
		"attempts":   len(failures),
		"key_count":  len(candidates),
		"failures":   failures,
		"proxy":      lastProxy.Display,
		"proxy_mode": lastProxy.Mode,
	}
	if lastProxy.Display == "" {
		details["proxy"] = proxyModeLabel
		details["proxy_mode"] = proxyModeLabel
	}
	if len(candidates) == 1 && candidates[0].Index >= 0 {
		details["key_index"] = candidates[0].Index
		details["key_masked"] = maskDiagKey(candidates[0].Key)
	}
	a.sendDiagError(w, http.StatusBadGateway, lastErrMsg, details)
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
	if cred, isKiro, err := a.kiroDiagCredentials(req.Provider, req.APIKey, req.KeyIndex); err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	} else if isKiro {
		proxyLabel := a.diagProxyModeLabel(req.Provider)
		slog.Info(fmt.Sprintf("🧪 [DIAG] fetch-models %s → kiro-catalog · proxy %s", req.Provider, proxyLabel))
		start := time.Now()
		models, ferr := provider.KiroListModels(r.Context(), nil, cred)
		latency := time.Since(start).Milliseconds()
		if ferr != nil {
			slog.Warn(fmt.Sprintf("❌ [DIAG] fetch-models %s fail · proxy %s · %dms · %s", req.Provider, proxyLabel, latency, ferr.Error()))
			a.sendDiagError(w, http.StatusBadGateway, ferr.Error(), map[string]interface{}{
				"provider": req.Provider, "target": "kiro-catalog", "type": "kiro",
				"latency_ms": latency, "proxy": proxyLabel, "proxy_mode": proxyLabel,
			})
			return
		}
		slog.Info(fmt.Sprintf("✅ [DIAG] fetch-models %s ok (%d models) · proxy %s · %dms", req.Provider, len(models), proxyLabel, latency))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true, "models": models, "count": len(models),
			"provider": req.Provider, "target": "kiro-catalog", "latency_ms": latency,
			"proxy": proxyLabel, "proxy_mode": proxyLabel,
		})
		return
	}
	candidates, err := a.buildDiagKeyCandidates(req.Provider, req.KeyIndex, &req.BaseURL, &req.Type, req.APIKey)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	var client *http.Client
	if req.Provider != "" && a.registry != nil {
		if p, ok := a.registry.Get(req.Provider); ok {
			if up, ok := p.(provider.UpstreamConfigProvider); ok && up.Client() != nil {
				client = up.Client()
			}
		}
	}
	targetURL := strings.TrimRight(req.BaseURL, "/") + "/models"
	var lastErr error
	var lastProxy diagProxyInfo
	proxyModeLabel := a.diagProxyModeLabel(req.Provider)
	slog.Info(fmt.Sprintf("🧪 [DIAG] fetch-models %s → trying %d key(s) · proxy %s", req.Provider, len(candidates), proxyModeLabel))
	for i, cand := range candidates {
		keyTag := diagKeyTag(cand.Index, cand.Key)
		px := a.diagCheckoutProxy(req.Provider)
		proxySuffix := diagProxySuffix(px)
		models, err := diagFetchModels(client, req.BaseURL, cand.Key, req.Type, px.URL)
		a.diagReportProxy(px.URL, err)
		if err == nil {
			resp := map[string]interface{}{
				"ok":        true,
				"models":    models,
				"count":     len(models),
				"provider":  req.Provider,
				"base_url":  req.BaseURL,
				"target":    targetURL,
				"attempts":  i + 1,
				"key_count": len(candidates),
				"proxy":     px.Display,
				"proxy_mode": px.Mode,
			}
			if cand.Index >= 0 {
				resp["key_index"] = cand.Index
				resp["key_number"] = cand.Index + 1
				resp["key_label"] = fmt.Sprintf("Key #%d", cand.Index+1)
				resp["key_masked"] = maskDiagKey(cand.Key)
				resp["fallback_used"] = i > 0
			} else {
				resp["key_masked"] = maskDiagKey(cand.Key)
			}
			slog.Info(fmt.Sprintf("✅ [DIAG] fetch-models %s ok (%d models) via %s · %s", req.Provider, len(models), keyTag, proxySuffix))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		lastErr = err
		lastProxy = px
		slog.Warn(fmt.Sprintf("❌ [DIAG] fetch-models %s fail via %s · %s · %s", req.Provider, keyTag, proxySuffix, err.Error()))
		if i < len(candidates)-1 {
			if !isDiagFallbackRetryable(err) {
				break
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
	}
	proxyDisplay := lastProxy.Display
	proxyMode := lastProxy.Mode
	if proxyDisplay == "" {
		proxyDisplay = proxyModeLabel
		proxyMode = proxyModeLabel
	}
	details := map[string]interface{}{
		"provider":  req.Provider,
		"base_url":  req.BaseURL,
		"target":    targetURL,
		"type":      req.Type,
		"attempts":  1,
		"key_count": len(candidates),
		"proxy":     proxyDisplay,
		"proxy_mode": proxyMode,
	}
	if len(candidates) == 1 && candidates[0].Index >= 0 {
		details["key_index"] = candidates[0].Index
	}
	msg := ""
	if lastErr != nil {
		msg = lastErr.Error()
	}
	if strings.TrimSpace(msg) == "" {
		msg = fmt.Sprintf("upstream %s catalog failed without detail (target %s)", req.Provider, targetURL)
	}
	a.sendDiagError(w, http.StatusBadGateway, msg, details)
}

// HandleQuickSetup handles POST /admin/templates/setup — create provider from template + fetch models.
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
	isKeyless := tmpl.Type == "opencode" || tmpl.Type == "mimo"
	if req.APIKey == "" && !isKeyless {
		a.sendError(w, http.StatusBadRequest, "api_key required")
		return
	}
	if a.cfg.GetProvider(tmpl.Name) != nil {
		a.sendError(w, http.StatusConflict, "Provider '"+tmpl.Name+"' already exists")
		return
	}
	// Fetch models live if not provided — never fall back to hardcoded
	// model names. Providers without a catalog endpoint (e.g. anthropic)
	// must supply "models" explicitly.
	models := req.Models
	if len(models) == 0 {
		fetched, err := diagFetchModels(nil, tmpl.BaseURL, req.APIKey, tmpl.Type, "")
		if err != nil {
			if isKeyless {
				if tmpl.Type == "mimo" {
					models = []string{"mimo/auto", "mimo-v2.5-free"}
				} else {
					models = []string{"oc/auto", "mimo-v2.5-free", "deepseek-v4-flash-free", "nemotron-3-ultra-free"}
				}
			} else {
				a.sendError(w, http.StatusBadGateway, "Failed to fetch models from provider catalog ("+err.Error()+"). Supply \"models\" explicitly.")
				return
			}
		} else {
			models = fetched
		}
	}
	// Create provider
	tier := 1
	var keys []string
	if req.APIKey != "" {
		keys = []string{req.APIKey}
	} else if isKeyless {
		tier = 3
		keys = []string{}
	}
	p := config.ProviderConfig{
		Name:    tmpl.Name,
		Type:    tmpl.Type,
		BaseURL: tmpl.BaseURL,
		APIKeys: keys,
		Models:  models,
		Tier:    tier,
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

// HandleGetPrivacyConfig handles GET /admin/config/privacy.
func (a *AdminHandler) HandleGetPrivacyConfig(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	a.mu.RLock()
	priv := a.cfg.Privacy
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(priv)
}

// HandleUpdatePrivacyConfig handles PUT /admin/config/privacy.
func (a *AdminHandler) HandleUpdatePrivacyConfig(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var update config.PrivacyConfig
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	if update.Mode == "" {
		update.Mode = "vault"
	}
	if update.Scope == "" {
		update.Scope = "free-only"
	}

	a.mu.Lock()
	a.cfg.Privacy = update
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Save failed: "+err.Error())
		return
	}

	slog.Info("privacy config updated",
		"enabled", update.Enabled,
		"mode", update.Mode,
		"scope", update.Scope,
		"mask_secrets", update.MaskSecrets,
		"mask_pii", update.MaskPII,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "updated",
		"privacy": update,
	})
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

// HandleGetProxyPool returns proxy pool statistics (GET /admin/proxy-pool).
func (a *AdminHandler) HandleGetProxyPool(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if a.proxyPool == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"enabled": false, "active_count": 0})
		return
	}
	json.NewEncoder(w).Encode(a.proxyPool.Stats())
}

// HandleRefreshProxyPool initiates a manual background refresh of the proxy pool (POST /admin/proxy-pool/refresh).
func (a *AdminHandler) HandleRefreshProxyPool(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	go func() {
		if err := a.proxyPool.Refresh(context.Background()); err != nil {
			slog.Warn("manual proxy pool refresh failed", "error", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": "Proxy pool refresh started in background",
	})
}

// HandleToggleProxyPool enables or disables the rotating proxy pool (POST /admin/proxy-pool/toggle).
func (a *AdminHandler) HandleToggleProxyPool(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	a.mu.Lock()
	if a.cfg != nil {
		a.cfg.ProxyPool.Enabled = req.Enabled
	}
	if a.proxyPool != nil {
		a.proxyPool.SetEnabled(req.Enabled)
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Failed to save configuration: "+err.Error())
		return
	}

	if req.Enabled && a.proxyPool != nil {
		go func() {
			_ = a.proxyPool.Refresh(context.Background())
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	if a.proxyPool != nil {
		json.NewEncoder(w).Encode(a.proxyPool.Stats())
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{"enabled": req.Enabled})
	}
}

// ==================== Proxy Pool Manual & Testing Endpoints ====================

// HandleAddManualProxy adds a manual proxy to the pool (POST /admin/proxy-pool/manual).
func (a *AdminHandler) HandleAddManualProxy(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	req.Proxy = strings.TrimSpace(req.Proxy)
	if req.Proxy == "" {
		a.sendError(w, http.StatusBadRequest, "Proxy address cannot be empty")
		return
	}

	entry, err := a.proxyPool.AddManualProxy(r.Context(), req.Proxy)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, "Failed to connect to proxy: "+err.Error())
		return
	}

	a.mu.Lock()
	if a.cfg != nil {
		exists := false
		for _, mp := range a.cfg.ProxyPool.ManualProxies {
			if mp == entry.URL {
				exists = true
				break
			}
		}
		if !exists {
			a.cfg.ProxyPool.ManualProxies = append(a.cfg.ProxyPool.ManualProxies, entry.URL)
		}
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		slog.Warn("failed to persist manual proxy to config file", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": "Manual proxy verified and added successfully",
		"proxy":   entry,
		"stats":   a.proxyPool.Stats(),
	})
}

// HandleRemoveManualProxy removes a manual proxy (DELETE /admin/proxy-pool/manual).
func (a *AdminHandler) HandleRemoveManualProxy(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	proxyURL := r.URL.Query().Get("proxy")
	if proxyURL == "" {
		var req struct {
			Proxy string `json:"proxy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		proxyURL = req.Proxy
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		a.sendError(w, http.StatusBadRequest, "Proxy address cannot be empty")
		return
	}

	a.proxyPool.RemoveManualProxy(proxyURL)

	a.mu.Lock()
	if a.cfg != nil {
		var newManuals []string
		for _, mp := range a.cfg.ProxyPool.ManualProxies {
			if mp != proxyURL && mp != "http://"+proxyURL {
				newManuals = append(newManuals, mp)
			}
		}
		a.cfg.ProxyPool.ManualProxies = newManuals
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		slog.Warn("failed to persist proxy removal to config file", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": "Manual proxy removed",
		"stats":   a.proxyPool.Stats(),
	})
}

// HandleTestProxy tests connectivity to a proxy URL without adding it (POST /admin/proxy-pool/test).
func (a *AdminHandler) HandleTestProxy(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	entry, err := a.proxyPool.TestProxyDirect(r.Context(), req.Proxy)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, "Proxy test failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": "Proxy is healthy",
		"proxy":   entry,
	})
}

// HandleGetWebshareAccounts lists all managed Webshare accounts (GET /admin/proxy-pool/webshare).
func (a *AdminHandler) HandleGetWebshareAccounts(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	accounts := a.proxyPool.GetWebshareAccounts()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"accounts": accounts,
		"count":    len(accounts),
	})
}

// HandleAddWebshareAccount adds or updates a Webshare account (POST /admin/proxy-pool/webshare).
func (a *AdminHandler) HandleAddWebshareAccount(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	var req struct {
		Name           string `json:"name"`
		IsFreeTier     bool   `json:"is_free_tier"`
		BandwidthLimit int64  `json:"bandwidth_limit"`
		ProxyContent   string `json:"proxy_content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		a.sendError(w, http.StatusBadRequest, "Account name cannot be empty")
		return
	}
	if strings.TrimSpace(req.ProxyContent) == "" {
		a.sendError(w, http.StatusBadRequest, "Proxy content cannot be empty")
		return
	}

	accountView, err := a.proxyPool.AddWebshareAccount(req.Name, req.IsFreeTier, req.BandwidthLimit, req.ProxyContent)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persist to config
	a.mu.Lock()
	if a.cfg != nil {
		a.cfg.ProxyPool.WebshareAccounts = a.proxyPool.GetConfigAccounts()
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		slog.Warn("failed to persist webshare account to config file", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": fmt.Sprintf("Webshare account %q added with %d proxies", accountView.Name, accountView.ProxiesCount),
		"account": accountView,
		"stats":   a.proxyPool.Stats(),
	})
}

// HandleDeleteWebshareAccount removes a Webshare account (DELETE /admin/proxy-pool/webshare).
func (a *AdminHandler) HandleDeleteWebshareAccount(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		name = req.Name
	}
	name = strings.TrimSpace(name)
	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Account name cannot be empty")
		return
	}

	if ok := a.proxyPool.DeleteWebshareAccount(name); !ok {
		a.sendError(w, http.StatusNotFound, fmt.Sprintf("Account %q not found", name))
		return
	}

	// Persist to config
	a.mu.Lock()
	if a.cfg != nil {
		a.cfg.ProxyPool.WebshareAccounts = a.proxyPool.GetConfigAccounts()
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		slog.Warn("failed to persist webshare account deletion to config file", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": fmt.Sprintf("Webshare account %q removed", name),
		"stats":   a.proxyPool.Stats(),
	})
}

// HandleResetWebshareAccount resets monthly bandwidth on an account (POST /admin/proxy-pool/webshare/reset).
func (a *AdminHandler) HandleResetWebshareAccount(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	if a.proxyPool == nil {
		a.sendError(w, http.StatusBadRequest, "Proxy pool is not initialized")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Account name cannot be empty")
		return
	}

	if ok := a.proxyPool.ResetWebshareAccount(name); !ok {
		a.sendError(w, http.StatusNotFound, fmt.Sprintf("Account %q not found", name))
		return
	}

	// Persist to config
	a.mu.Lock()
	if a.cfg != nil {
		a.cfg.ProxyPool.WebshareAccounts = a.proxyPool.GetConfigAccounts()
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		slog.Warn("failed to persist webshare account reset to config file", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": fmt.Sprintf("Webshare account %q bandwidth reset to 0", name),
		"stats":   a.proxyPool.Stats(),
	})
}

// ==================== Cloudflare Relay (CF Relay) Endpoints ====================

// HandleGetCFRelays lists all configured Cloudflare Relays (GET /admin/cf-relays).
func (a *AdminHandler) HandleGetCFRelays(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	a.mu.RLock()
	relays := make([]map[string]interface{}, 0)
	if a.cfg != nil {
		for _, cr := range a.cfg.CFRelays {
			maskedToken := "••••••••"
			if len(cr.Token) > 6 {
				maskedToken = cr.Token[:3] + "..." + cr.Token[len(cr.Token)-3:]
			}
			relays = append(relays, map[string]interface{}{
				"name":       cr.Name,
				"account_id": cr.AccountID,
				"token":      maskedToken,
				"has_token":  cr.Token != "",
				"type":       cr.Type,
				"base_url":   cr.GetEffectiveBaseURL(),
			})
		}
	}
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(relays)
}

// HandleAddCFRelay creates or updates a Cloudflare Relay (POST /admin/cf-relays).
func (a *AdminHandler) HandleAddCFRelay(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var req struct {
		Name      string `json:"name"`
		AccountID string `json:"account_id"`
		Token     string `json:"token"`
		Type      string `json:"type"`
		BaseURL   string `json:"base_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	name := strings.TrimSpace(req.Name)
	accountID := strings.TrimSpace(req.AccountID)
	token := strings.TrimSpace(req.Token)
	rType := strings.TrimSpace(req.Type)
	if rType == "" {
		rType = "ai-gateway"
	}

	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Nama relay wajib diisi")
		return
	}
	if accountID == "" {
		a.sendError(w, http.StatusBadRequest, "Account ID (id) Cloudflare wajib diisi")
		return
	}
	if token == "" {
		a.sendError(w, http.StatusBadRequest, "Token Cloudflare wajib diisi")
		return
	}

	relay := config.CFRelay{
		Name:      name,
		AccountID: accountID,
		Token:     token,
		Type:      rType,
		BaseURL:   strings.TrimSpace(req.BaseURL),
	}

	a.mu.Lock()
	if a.cfg != nil {
		found := false
		for i, existing := range a.cfg.CFRelays {
			if strings.EqualFold(existing.Name, name) {
				a.cfg.CFRelays[i] = relay
				found = true
				break
			}
		}
		if !found {
			a.cfg.CFRelays = append(a.cfg.CFRelays, relay)
		}
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Failed to save configuration: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": fmt.Sprintf("Cloudflare Relay '%s' saved successfully", name),
		"relay": map[string]interface{}{
			"name":       relay.Name,
			"account_id": relay.AccountID,
			"type":       relay.Type,
			"base_url":   relay.GetEffectiveBaseURL(),
		},
	})
}

// HandleDeleteCFRelay deletes a Cloudflare Relay (DELETE /admin/cf-relays/{name}).
func (a *AdminHandler) HandleDeleteCFRelay(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	name := extractName(r.URL.Path, "/admin/cf-relays/")
	if name == "" {
		a.sendError(w, http.StatusBadRequest, "Relay name is required")
		return
	}

	a.mu.Lock()
	if a.cfg != nil {
		var filtered []config.CFRelay
		for _, cr := range a.cfg.CFRelays {
			if !strings.EqualFold(cr.Name, name) {
				filtered = append(filtered, cr)
			}
		}
		a.cfg.CFRelays = filtered
	}
	a.mu.Unlock()

	if err := a.saveAndReload(); err != nil {
		a.sendError(w, http.StatusInternalServerError, "Failed to save configuration: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": fmt.Sprintf("Cloudflare Relay '%s' deleted", name),
	})
}

// HandleTestCFRelay tests connectivity to a Cloudflare Relay (POST /admin/cf-relays/test).
func (a *AdminHandler) HandleTestCFRelay(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var req struct {
		Name      string `json:"name"`
		AccountID string `json:"account_id"`
		Token     string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	accountID := strings.TrimSpace(req.AccountID)
	token := strings.TrimSpace(req.Token)
	name := strings.TrimSpace(req.Name)

	if accountID == "" || token == "" {
		if name != "" && a.cfg != nil {
			a.mu.RLock()
			cr := a.cfg.FindCFRelay(name)
			if cr != nil {
				accountID = cr.AccountID
				token = cr.Token
			}
			a.mu.RUnlock()
		}
	}

	if accountID == "" || token == "" {
		a.sendError(w, http.StatusBadRequest, "Account ID and Token are required to test Cloudflare Relay")
		return
	}

	client := &http.Client{Timeout: 8 * time.Second}
	testURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/models/search", accountID)
	httpReq, err := http.NewRequestWithContext(r.Context(), "GET", testURL, nil)
	if err != nil {
		a.sendError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, "Cloudflare connection error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	latency := time.Since(start)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		a.sendError(w, http.StatusBadRequest, fmt.Sprintf("Cloudflare authentication failed (status %d): Check Account ID and Token", resp.StatusCode))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "ok",
		"message":     fmt.Sprintf("Cloudflare Relay connection verified (latency: %dms, status: %d)", latency.Milliseconds(), resp.StatusCode),
		"latency_ms":  latency.Milliseconds(),
		"status_code": resp.StatusCode,
	})
}


