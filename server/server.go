package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aigateway/admin"
	"github.com/aigateway/auth"
	"github.com/aigateway/cache"
	"github.com/aigateway/config"
	midware "github.com/aigateway/middleware"
	"github.com/aigateway/playground"
	"github.com/aigateway/provider"
	"github.com/aigateway/proxy"
	"github.com/aigateway/relay"
	"github.com/aigateway/router"
	"github.com/aigateway/tunnel"
	"github.com/aigateway/usage"
)

// DisableStdoutTunnelPrint prevents printing the tunnel status message to stderr (used to keep CLI/TUI output clean).
var DisableStdoutTunnelPrint bool

// healthCheckClient is a shared HTTP client for all provider health checks.
var healthCheckClient = &http.Client{Timeout: 10 * time.Second}

// Server is the main AI Gateway HTTP server.
type Server struct {
	configPath string
	cfgMu      sync.RWMutex // guards cfg (read by health checks/wrappers, written by ReloadConfig)
	cfg        *config.Config
	handler    *proxy.Handler
	admin      *admin.AdminHandler
	mux        *http.ServeMux
	registry   *provider.Registry
	cancelFunc context.CancelFunc // for stopping background goroutines
	tunnelMgr  *tunnel.TunnelManager
	proxyPool  *relay.ProxyPool
}

// getConfig returns the active config (thread-safe).
func (s *Server) getConfig() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// wireEgressPool routes a provider through the rotating egress proxy pool and
// attaches the pool for per-attempt checkout + failure reporting, so dead
// proxies are evicted after 3 consecutive transport failures instead of
// lingering until the next periodic refresh.
func wireEgressPool(p provider.Provider, pCfg config.ProviderConfig, proxyPool *relay.ProxyPool) {
	if proxyPool == nil {
		return
	}
	if pCfg.ProxyURL == "direct" || pCfg.ProxyURL == "none" {
		return
	}
	if pCfg.ProxyURL != "" && pCfg.ProxyURL != "auto" && pCfg.ProxyURL != "pool" {
		return
	}
	isKeyless := len(pCfg.APIKeys) == 0 && (pCfg.Type == "opencode" || pCfg.Type == "mimo")
	shouldPool := pCfg.ProxyURL == "auto" || pCfg.ProxyURL == "pool" || isKeyless
	if !shouldPool {
		return
	}

	if up, ok := p.(provider.UpstreamConfigProvider); ok && up.Client() != nil {
		if tr, ok := up.Client().Transport.(*http.Transport); ok {
			tr.Proxy = proxyPool.ContextProxyFunc()
			slog.Info("provider routed through dynamic proxy pool", "name", pCfg.Name)
		}
	}
	if ep, ok := p.(interface{ SetEgressPool(provider.EgressPool) }); ok {
		ep.SetEgressPool(proxyPool)
	}
}

// prefetchDynamicCatalogs warms the in-memory model catalog used by virtual
// auto routes (oc/auto, mimo/auto) so the first request never cold-fetches.
// Catalogs always come from the live upstream /models endpoint — never hardcoded.
func prefetchDynamicCatalogs(ctx context.Context, registry *provider.Registry) {
	for _, p := range registry.All() {
		up, ok := p.(provider.UpstreamConfigProvider)
		if !ok {
			continue
		}
		if up.ProviderType() != "opencode" && up.ProviderType() != "mimo" {
			continue
		}
		if len(provider.GetCachedDynamicModels(p.Name())) > 0 {
			continue
		}
		go func(prov provider.Provider, up provider.UpstreamConfigProvider) {
			fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			var keys []string
			for _, k := range up.APIKeys() {
				if strings.TrimSpace(k.Key) != "" {
					keys = append(keys, k.Key)
				}
			}
			keys = append(keys, "") // keyless public catalog attempt
			for _, k := range keys {
				list, err := provider.FetchUpstreamModels(fetchCtx, up.Client(), up.BaseURL(), k, up.ProviderType())
				if err == nil {
					provider.SetCachedDynamicModels(prov.Name(), list)
					slog.Info("prefetched dynamic model catalog", "provider", prov.Name(), "models", len(list))
					return
				}
			}
			slog.Warn("could not prefetch dynamic model catalog (will lazy-fetch on first request)", "provider", prov.Name())
		}(p, up)
	}
}

// New creates and initializes a new Server from configuration.
func New(cfg *config.Config, configPath string) (*Server, error) {
	// 0. Initialize proxy pool
	proxyPool := relay.NewProxyPool(cfg.ProxyPool)

	// 1. Initialize provider registry
	registry := provider.NewRegistry()
	for _, pCfg := range cfg.Providers {
		// Skip providers with no API keys — unless they are keyless type opencode or mimo
		if !pCfg.HasCredentials() {
			slog.Warn("skipping provider with no API keys", "name", pCfg.Name)
			continue
		}
		if pCfg.RelayURL != "" {
			if cr := cfg.FindCFRelay(pCfg.RelayURL); cr != nil {
				pCfg.RelayURL = cr.GetEffectiveBaseURL()
				if pCfg.RelaySecret == "" {
					pCfg.RelaySecret = cr.Token
				}
			}
		}
		p, err := provider.NewProviderFromConfig(pCfg)
		if err != nil {
			return nil, fmt.Errorf("init provider %s: %w", pCfg.Name, err)
		}

		// Wire dynamic proxy pool if configured or specifically for opencode/mimo
		wireEgressPool(p, pCfg, proxyPool)

		if pCfg.Disabled {
			p.SetHealthy(false)
		}
		registry.Register(pCfg.Name, p)
		slog.Info("provider registered",
			"name", pCfg.Name,
			"type", pCfg.Type,
			"tier", pCfg.Tier,
			"models", len(pCfg.Models),
			"api_keys", len(pCfg.APIKeys),
			"key_rotation", pCfg.KeyRotationMode(),
		)
	}

	// 2. Initialize model router
	r, err := router.NewRouter(cfg, registry)
	if err != nil {
		return nil, fmt.Errorf("init router: %w", err)
	}

	// 3. Initialize auth key store
	keyStore := auth.NewKeyStore(cfg.APIKeys)

	// 4. Initialize concurrency limiter with queue support
	limiter := midware.NewConcurrencyLimiterWithQueue(
		cfg.Concurrency.MaxConcurrent,
		cfg.Concurrency.QueueDepth,
		cfg.Concurrency.PerProvider,
		cfg.Concurrency.PerModel,
		cfg.Concurrency.QueueTimeout,
	)

	// 5. Create handlers
	proxyHandler := proxy.NewHandler(r, keyStore, limiter, cfg.TokenSaver, cfg.Privacy)

	// Wire the limiter into the router for per-provider slots
	r.SetLimiter(limiter)

	// 6. Initialize response cache
	responseCache := cache.New(cfg.Cache.MaxSize, time.Duration(cfg.Cache.TTL)*time.Second)
	if cfg.Cache.Enabled {
		proxyHandler.SetCache(responseCache)
		slog.Info("response cache enabled", "max_size", cfg.Cache.MaxSize, "ttl_seconds", cfg.Cache.TTL)
	}

	// 7. Initialize usage tracker
	tracker := usage.NewTracker()
	tracker.SetPrices(cfg.Prices)
	tracker.SetBudgets(cfg.Budgets)
	if cfg.Server.UsageFile != "" && cfg.Server.UsageFile != "-" {
		if err := tracker.LoadUsage(cfg.Server.UsageFile); err != nil {
			slog.Warn("could not restore usage stats", "file", cfg.Server.UsageFile, "error", err)
		} else {
			slog.Info("usage stats restored", "file", cfg.Server.UsageFile)
		}
	}
	proxyHandler.SetTracker(tracker)
	slog.Info("usage tracker initialized")

	var srv *Server
	reloadFunc := func() error {
		if srv == nil {
			return fmt.Errorf("server not initialized yet")
		}
		return srv.ReloadConfig()
	}

	tunnelMgr := tunnel.NewTunnelManager()

	adminHandler := admin.NewAdminHandler(keyStore, cfg.Server.AdminSecret, proxyHandler.Stats, reloadFunc, configPath, cfg, registry, tunnelMgr, proxyPool)

	// 8. Setup routes (Go 1.22 pattern matching)
	mux := http.NewServeMux()

	// OpenAI-compatible API endpoints
	mux.HandleFunc("POST /v1/chat/completions", proxyHandler.HandleChatCompletion)
	mux.HandleFunc("POST /v1/responses", proxyHandler.HandleResponses)
	mux.HandleFunc("POST /v1/audio/transcriptions", proxyHandler.HandleAudioTranscriptions)
	mux.HandleFunc("POST /v1/audio/speech", proxyHandler.HandleAudioSpeech)
	mux.HandleFunc("POST /v1/embeddings", proxyHandler.HandleEmbeddings)
	mux.HandleFunc("GET /v1/models", proxyHandler.HandleListModels)
	mux.HandleFunc("GET /health", proxyHandler.HandleHealth)

	// Helper to wrap admin endpoints and check if dashboard is enabled
	wrapAdmin := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			targetCfg := cfg
			if srv != nil {
				targetCfg = srv.getConfig()
			}
			if !targetCfg.Server.IsDashboardEnabled() && pattern != "POST /admin/config/reload" {
				http.Error(w, "Web dashboard & admin API are disabled in configuration", http.StatusForbidden)
				return
			}
			handler(w, r)
		})
	}

	// Admin API endpoints
	wrapAdmin("GET /admin/keys", adminHandler.HandleListKeys)
	wrapAdmin("POST /admin/keys", adminHandler.HandleCreateKey)
	wrapAdmin("DELETE /admin/keys/", adminHandler.HandleDeleteKey)
	wrapAdmin("GET /admin/stats", adminHandler.HandleStats)
	wrapAdmin("GET /admin/usage", func(w http.ResponseWriter, r *http.Request) {
		handleUsageStats(w, r, adminHandler, proxyHandler)
	})
	wrapAdmin("POST /admin/config/reload", adminHandler.HandleReloadConfig)
	wrapAdmin("GET /admin/tunnel", adminHandler.HandleGetTunnelStatus)
	wrapAdmin("POST /admin/tunnel/toggle", adminHandler.HandleToggleTunnel)
	wrapAdmin("GET /admin/proxy-pool", adminHandler.HandleGetProxyPool)
	wrapAdmin("POST /admin/proxy-pool/refresh", adminHandler.HandleRefreshProxyPool)
	wrapAdmin("POST /admin/proxy-pool/toggle", adminHandler.HandleToggleProxyPool)
	wrapAdmin("POST /admin/proxy-pool/manual", adminHandler.HandleAddManualProxy)
	wrapAdmin("DELETE /admin/proxy-pool/manual", adminHandler.HandleRemoveManualProxy)
	wrapAdmin("POST /admin/proxy-pool/test", adminHandler.HandleTestProxy)
	wrapAdmin("GET /admin/proxy-pool/webshare", adminHandler.HandleGetWebshareAccounts)
	wrapAdmin("POST /admin/proxy-pool/webshare", adminHandler.HandleAddWebshareAccount)
	wrapAdmin("DELETE /admin/proxy-pool/webshare", adminHandler.HandleDeleteWebshareAccount)
	wrapAdmin("POST /admin/proxy-pool/webshare/reset", adminHandler.HandleResetWebshareAccount)

	// Cloudflare Relay CRUD
	wrapAdmin("GET /admin/cf-relays", adminHandler.HandleGetCFRelays)
	wrapAdmin("POST /admin/cf-relays", adminHandler.HandleAddCFRelay)
	wrapAdmin("DELETE /admin/cf-relays/", adminHandler.HandleDeleteCFRelay)
	wrapAdmin("POST /admin/cf-relays/test", adminHandler.HandleTestCFRelay)

	// Provider CRUD
	wrapAdmin("GET /admin/providers", adminHandler.HandleProviders)
	wrapAdmin("POST /admin/providers", adminHandler.HandleCreateProvider)
	wrapAdmin("PUT /admin/providers/", adminHandler.HandleUpdateProvider)
	wrapAdmin("DELETE /admin/providers/", adminHandler.HandleDeleteProvider)

	// Model CRUD
	wrapAdmin("GET /admin/models", adminHandler.HandleModels)
	wrapAdmin("POST /admin/models", adminHandler.HandleCreateModel)
	wrapAdmin("PUT /admin/models/", adminHandler.HandleUpdateModel)
	wrapAdmin("DELETE /admin/models/", adminHandler.HandleDeleteModel)

	// API Key update
	wrapAdmin("PUT /admin/keys/", adminHandler.HandleUpdateKey)

	// Config sections
	wrapAdmin("GET /admin/config", adminHandler.HandleConfig)
	wrapAdmin("PUT /admin/config/server", adminHandler.HandleUpdateConfigServer)
	wrapAdmin("PUT /admin/config/concurrency", adminHandler.HandleUpdateConfigConcurrency)
	wrapAdmin("PUT /admin/config/cache", adminHandler.HandleUpdateConfigCache)
	wrapAdmin("PUT /admin/config/retry", adminHandler.HandleUpdateConfigRetry)
	wrapAdmin("GET /admin/config/token-saver", adminHandler.HandleGetTokenSaverConfig)
	wrapAdmin("PUT /admin/config/token-saver", adminHandler.HandleUpdateTokenSaverConfig)
	wrapAdmin("GET /admin/config/privacy", adminHandler.HandleGetPrivacyConfig)
	wrapAdmin("PUT /admin/config/privacy", adminHandler.HandleUpdatePrivacyConfig)

	// Diagnostic endpoints
	wrapAdmin("GET /admin/templates", adminHandler.HandleTemplates)
	wrapAdmin("POST /admin/templates/setup", adminHandler.HandleQuickSetup)
	wrapAdmin("POST /admin/diag/test-key", adminHandler.HandleDiagTestKey)
	wrapAdmin("POST /admin/diag/test-model", adminHandler.HandleDiagTestModel)
	wrapAdmin("POST /admin/diag/fetch-models", adminHandler.HandleDiagFetchModels)

	// Admin password change
	wrapAdmin("PUT /admin/change-password", adminHandler.HandleChangePassword)

	// Console log endpoints
	wrapAdmin("GET /admin/logs", adminHandler.HandleGetLogs)
	wrapAdmin("GET /admin/logs/stream", adminHandler.HandleStreamLogs)

	// Login page — standalone authentication page
	wrapAdmin("GET /login", adminHandler.ServeLogin)
	wrapAdmin("GET /admin/login", adminHandler.ServeLogin)

	// Dashboard — admin web UI (with dynamic secret injection)
	wrapAdmin("GET /admin", adminHandler.ServeDashboard)
	wrapAdmin("GET /dashboard", adminHandler.ServeDashboard)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusTemporaryRedirect)
	})

	// Playground — interactive chat UI
	wrapAdmin("GET /playground", playground.Handler)

	srv = &Server{
		configPath: configPath,
		cfg:        cfg,
		handler:    proxyHandler,
		admin:      adminHandler,
		mux:        mux,
		registry:   registry,
		tunnelMgr:  tunnelMgr,
		proxyPool:  proxyPool,
	}
	return srv, nil
}

// handleUsageStats serves GET /admin/usage with detailed usage statistics.
func handleUsageStats(w http.ResponseWriter, r *http.Request, adminHandler *admin.AdminHandler, handler *proxy.Handler) {
	if !adminHandler.CheckAuth(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"Invalid admin secret","type":"admin_error"}}`))
		return
	}

	tracker := handler.GetTracker()
	if tracker == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":{"message":"Usage tracker not initialized","type":"admin_error"}}`))
		return
	}

	stats := tracker.GetStats()

	// Add cache stats
	cacheStats := map[string]interface{}{}
	if c := handler.GetCache(); c != nil {
		hits, misses, size := c.Stats()
		cacheStats["hits"] = hits
		cacheStats["misses"] = misses
		cacheStats["size"] = size
		if hits+misses > 0 {
			cacheStats["hit_rate"] = fmt.Sprintf("%.1f%%", float64(hits)/float64(hits+misses)*100)
		}
	}

	result := map[string]interface{}{
		"uptime":             stats.Uptime,
		"by_model":           stats.ByModel,
		"by_api_key":         stats.ByAPIKey,
		"cache":              cacheStats,
		"estimated_cost_usd": stats.EstimatedCostUSD,
		"cost_by_provider":   stats.CostByProvider,
		"monthly_tokens":     stats.MonthlyTokens,
		"budgets":            stats.Budgets,
		"token_saver_bytes_saved": stats.TokenSaverSaved,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// Start starts the HTTP server and blocks.
func (s *Server) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelFunc = cancel
	defer s.tunnelMgr.Stop()

	// Snapshot config (ReloadConfig may swap it concurrently)
	cfg := s.getConfig()

	// Start background health checks
	s.startHealthChecks(ctx)

	// Warm dynamic model catalogs for virtual auto routes (non-blocking)
	prefetchDynamicCatalogs(ctx, s.registry)

	// Start background proxy pool rotator if enabled
	if s.proxyPool != nil && s.proxyPool.IsEnabled() {
		s.proxyPool.Start(ctx)
	}

	// Start Cloudflare Quick Tunnel if enabled in configuration
	if cfg.Server.QuickTunnel {
		go func() {
			slog.Info("starting Cloudflare Quick Tunnel as requested by config...")
			url, err := s.tunnelMgr.Start(ctx, cfg.Server.Port)
			if err != nil {
				slog.Error("failed to start Cloudflare Quick Tunnel", "error", err)
				if !DisableStdoutTunnelPrint {
					fmt.Fprintf(os.Stderr, "  ❌ Quick Tunnel Error: %v\n", err)
				}
			} else {
				if !DisableStdoutTunnelPrint {
					fmt.Fprintf(os.Stderr, "  🌐 [Quick Tunnel Active] Public URL: %s\n\n", url)
				}
			}
		}()
	}

	// Start background cache cleanup
	if cfg.Cache.Enabled {
		if c := s.handler.GetCache(); c != nil {
			c.StartCleanup(ctx, 60*time.Second)
			slog.Info("cache cleanup goroutine started", "interval", "60s")
		}
	}

	// Periodically persist usage stats so they survive restarts/crashes
	if usageFile := cfg.Server.UsageFile; usageFile != "" && usageFile != "-" {
		if tr := s.handler.GetTracker(); tr != nil {
			go func() {
				ticker := time.NewTicker(60 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						// Best-effort final save on shutdown
						_ = tr.PersistUsage(usageFile)
						return
					case <-ticker.C:
						if err := tr.PersistUsage(usageFile); err != nil {
							slog.Warn("could not persist usage stats", "file", usageFile, "error", err)
						}
					}
				}
			}()
			slog.Info("usage persistence enabled", "file", usageFile, "interval", "60s")
		}
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)

	// Wrap with CORS + logging + body limit middleware
	handler := midware.CORSMiddleware(midware.LoggingMiddlewareWithBodies(midware.BodyLimitMiddleware(s.mux), cfg.Server.LogBodies))

	// Print startup info
	slog.Info("========================================")
	slog.Info("  AI Gateway starting", "address", addr)
	slog.Info("========================================")

	slog.Info("providers loaded", "count", len(cfg.Providers))
	for _, p := range cfg.Providers {
		slog.Info("  provider",
			"name", p.Name,
			"type", p.Type,
			"tier", p.Tier,
			"health_check", p.HealthCheckInterval.String(),
		)
	}
	slog.Info("models configured", "count", len(cfg.Models))
	for _, m := range cfg.Models {
		if m.Strategy != "" {
			slog.Info("  model (combo)",
				"name", m.Name,
				"strategy", m.Strategy,
				"backends", len(m.Backends),
			)
		} else {
			slog.Info("  model (direct)",
				"name", m.Name,
				"provider", m.Provider,
				"model", m.Model,
			)
		}
	}
	slog.Info("API keys loaded", "count", len(cfg.APIKeys))

	slog.Info("features enabled",
		"cache", cfg.Cache.Enabled,
		"token_saver", cfg.TokenSaver.Enabled,
		"caveman_mode", cfg.TokenSaver.CavemanMode,
		"usage_tracking", true,
		"request_queuing", true,
		"retry_with_backoff", true,
		"tiered_fallback", true,
	)

	slog.Info("endpoints available",
		"chat", "POST /v1/chat/completions",
		"embeddings", "POST /v1/embeddings",
		"models", "GET /v1/models",
		"health", "GET /health",
		"playground", "GET /playground",
		"admin_keys", "GET/POST /admin/keys",
		"admin_stats", "GET /admin/stats",
		"admin_usage", "GET /admin/usage",
	)

	// Create explicit server with timeouts to prevent Slowloris attacks.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // no write timeout — streaming can be long
		IdleTimeout:       5 * time.Minute,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	slog.Info("server ready", "address", addr)
	return srv.ListenAndServe()
}

// startHealthChecks starts background goroutines to periodically check provider health.
func (s *Server) startHealthChecks(ctx context.Context) {
	hasHealthChecks := false
	for _, pCfg := range s.getConfig().Providers {
		if pCfg.HealthCheckInterval > 0 {
			hasHealthChecks = true
			slog.Info("health check enabled",
				"provider", pCfg.Name,
				"interval", pCfg.HealthCheckInterval.String(),
			)
		}
	}

	if !hasHealthChecks {
		return
	}

	// Start goroutines per provider with configured intervals
	for _, pCfg := range s.getConfig().Providers {
		if pCfg.HealthCheckInterval <= 0 {
			continue
		}
		interval := pCfg.HealthCheckInterval
		name := pCfg.Name
		go func(providerName string, checkInterval time.Duration) {
			ticker := time.NewTicker(checkInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.checkProvider(providerName)
				}
			}
		}(name, interval)
	}
}

// checkProvider checks the health of a single provider by name.
// Tries each API key in turn: the provider is healthy when ANY key answers
// the catalog endpoint, so one dead key no longer takes the whole provider
// offline while sibling keys still serve traffic.
func (s *Server) checkProvider(name string) {
	var pCfg config.ProviderConfig
	for _, p := range s.getConfig().Providers {
		if p.Name == name {
			pCfg = p
			break
		}
	}
	if pCfg.Name == "" {
		return
	}

	// Kiro has no GET /models endpoint — validate via its live catalog instead.
	if pCfg.Type == "kiro" && pCfg.HealthCheckURL == "" {
		s.checkKiroProvider(pCfg)
		return
	}

	checkURL := pCfg.HealthCheckURL
	if checkURL == "" {
		checkURL = pCfg.BaseURL + "/models"
	}

	keys := pCfg.APIKeys
	if len(keys) == 0 {
		keys = []string{""} // keyless providers are probed without auth
	}
	lastStatus := 0
	for _, k := range keys {
		req, err := http.NewRequest("GET", checkURL, nil)
		if err != nil {
			return
		}
		if k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}

		resp, err := healthCheckClient.Do(req)
		if err != nil {
			continue // try next key
		}
		lastStatus = resp.StatusCode
		resp.Body.Close()

		if lastStatus >= 200 && lastStatus < 400 {
			s.setProviderHealth(name, true)
			return
		}
		// Otherwise try the next key — a single bad key must not
		// take the whole provider offline.
	}

	slog.Warn("health check failed", "provider", name, "reason", "no key answered the catalog endpoint", "last_status", lastStatus)
	s.setProviderHealth(name, false)
}

// checkKiroProvider validates Kiro credentials against the live model catalog:
// any working API key or the refreshed OAuth token marks the provider healthy.
func (s *Server) checkKiroProvider(pCfg config.ProviderConfig) {
	name := pCfg.Name
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, k := range pCfg.APIKeys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if _, err := provider.KiroListModels(ctx, healthCheckClient, provider.KiroCredentials{APIKey: k}); err == nil {
			s.setProviderHealth(name, true)
			return
		}
	}
	if strings.TrimSpace(pCfg.RefreshToken) != "" {
		if _, err := provider.KiroListModels(ctx, healthCheckClient, provider.KiroCredentials{
			RefreshToken: pCfg.RefreshToken,
			ProfileARN:   pCfg.ProfileARN,
		}); err == nil {
			s.setProviderHealth(name, true)
			return
		}
	}
	slog.Warn("health check failed", "provider", name, "reason", "kiro catalog unreachable")
	s.setProviderHealth(name, false)
}

// setProviderHealth sets the health status of a provider by name.
func (s *Server) setProviderHealth(name string, healthy bool) {
	s.cfgMu.RLock()
	reg := s.registry
	s.cfgMu.RUnlock()
	if reg == nil {
		return
	}
	if p, ok := reg.Get(name); ok {
		wasHealthy := p.IsHealthy()
		p.SetHealthy(healthy)
		if wasHealthy != healthy {
			if healthy {
				slog.Info("provider recovered", "provider", name)
			} else {
				slog.Warn("provider marked unhealthy", "provider", name)
			}
		}
	}
}

// ReloadConfig reloads the config file and updates active components dynamically.
func (s *Server) ReloadConfig() error {
	slog.Info("reloading configuration...", "path", s.configPath)

	newCfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	registry := provider.NewRegistry()
	for _, pCfg := range newCfg.Providers {
		if !pCfg.HasCredentials() {
			slog.Warn("skipping provider with no API keys", "name", pCfg.Name)
			continue
		}
		if pCfg.RelayURL != "" {
			if cr := newCfg.FindCFRelay(pCfg.RelayURL); cr != nil {
				pCfg.RelayURL = cr.GetEffectiveBaseURL()
				if pCfg.RelaySecret == "" {
					pCfg.RelaySecret = cr.Token
				}
			}
		}
		p, err := provider.NewProviderFromConfig(pCfg)
		if err != nil {
			return fmt.Errorf("init provider %s: %w", pCfg.Name, err)
		}
		wireEgressPool(p, pCfg, s.proxyPool)
		if pCfg.Disabled {
			p.SetHealthy(false)
		}
		registry.Register(pCfg.Name, p)
	}

	r, err := router.NewRouter(newCfg, registry)
	if err != nil {
		return fmt.Errorf("init router: %w", err)
	}

	keyStore := auth.NewKeyStore(newCfg.APIKeys)

	// Create new concurrency limiter if limits changed
	oldCfg := s.getConfig()
	var newLimiter *midware.ConcurrencyLimiter
	if newCfg.Concurrency.MaxConcurrent != oldCfg.Concurrency.MaxConcurrent ||
		newCfg.Concurrency.QueueDepth != oldCfg.Concurrency.QueueDepth ||
		newCfg.Concurrency.PerProvider != oldCfg.Concurrency.PerProvider ||
		newCfg.Concurrency.PerModel != oldCfg.Concurrency.PerModel ||
		newCfg.Concurrency.QueueTimeout != oldCfg.Concurrency.QueueTimeout {
		newLimiter = midware.NewConcurrencyLimiterWithQueue(
			newCfg.Concurrency.MaxConcurrent,
			newCfg.Concurrency.QueueDepth,
			newCfg.Concurrency.PerProvider,
			newCfg.Concurrency.PerModel,
			newCfg.Concurrency.QueueTimeout,
		)
		slog.Info("concurrency limiter updated",
			"max_concurrent", newCfg.Concurrency.MaxConcurrent,
			"queue_depth", newCfg.Concurrency.QueueDepth,
			"per_provider", newCfg.Concurrency.PerProvider,
			"per_model", newCfg.Concurrency.PerModel,
			"queue_timeout", newCfg.Concurrency.QueueTimeout,
		)
	}

	// Thread-safe update of proxy handler configs
	s.handler.UpdateConfig(r, keyStore, newLimiter, &newCfg.TokenSaver, &newCfg.Privacy)

	// Swap the registry so health checks and lookups track the new providers.
	// (Without this, post-reload health flips landed on orphaned objects.)
	s.cfgMu.Lock()
	s.registry = registry
	s.cfgMu.Unlock()

	// Re-wire the (possibly new) limiter into the fresh router
	r.SetLimiter(s.handler.GetLimiter())

	// Refresh cost/budget tables (usage history itself is preserved)
	if tr := s.handler.GetTracker(); tr != nil {
		tr.SetPrices(newCfg.Prices)
		tr.SetBudgets(newCfg.Budgets)
	}

	// Thread-safe update of admin handler configs
	s.admin.UpdateConfig(keyStore, newCfg.Server.AdminSecret, newCfg, registry)

	// Sync proxy pool state
	if s.proxyPool != nil {
		wasEnabled := s.proxyPool.IsEnabled()
		s.proxyPool.SetEnabled(newCfg.ProxyPool.Enabled)
		if !wasEnabled && newCfg.ProxyPool.Enabled {
			go func() {
				_ = s.proxyPool.Refresh(context.Background())
			}()
		}
	}

	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()
	slog.Info("configuration reloaded successfully")
	return nil
}
