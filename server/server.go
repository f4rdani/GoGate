package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aigateway/admin"
	"github.com/aigateway/auth"
	"github.com/aigateway/cache"
	"github.com/aigateway/config"
	midware "github.com/aigateway/middleware"
	"github.com/aigateway/playground"
	"github.com/aigateway/provider"
	"github.com/aigateway/proxy"
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
	cfg        *config.Config
	handler    *proxy.Handler
	admin      *admin.AdminHandler
	mux        *http.ServeMux
	registry   *provider.Registry
	cancelFunc context.CancelFunc // for stopping background goroutines
	tunnelMgr  *tunnel.TunnelManager
}

// New creates and initializes a new Server from configuration.
func New(cfg *config.Config, configPath string) (*Server, error) {
	// 1. Initialize provider registry
	registry := provider.NewRegistry()
	for _, pCfg := range cfg.Providers {
		// Skip providers with no API keys — unless they are keyless type opencode or mimo
		if len(pCfg.APIKeys) == 0 && pCfg.Type != "opencode" && pCfg.Type != "mimo" {
			slog.Warn("skipping provider with no API keys", "name", pCfg.Name)
			continue
		}
		p, err := provider.NewProviderFromConfig(pCfg)
		if err != nil {
			return nil, fmt.Errorf("init provider %s: %w", pCfg.Name, err)
		}
		registry.Register(pCfg.Name, p)
		slog.Info("provider registered",
			"name", pCfg.Name,
			"type", pCfg.Type,
			"tier", pCfg.Tier,
			"models", len(pCfg.Models),
			"api_keys", len(pCfg.APIKeys),
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
		cfg.Concurrency.QueueTimeout,
	)

	// 5. Create handlers
	proxyHandler := proxy.NewHandler(r, keyStore, limiter, cfg.TokenSaver)

	// 6. Initialize response cache
	responseCache := cache.New(cfg.Cache.MaxSize, time.Duration(cfg.Cache.TTL)*time.Second)
	if cfg.Cache.Enabled {
		proxyHandler.SetCache(responseCache)
		slog.Info("response cache enabled", "max_size", cfg.Cache.MaxSize, "ttl_seconds", cfg.Cache.TTL)
	}

	// 7. Initialize usage tracker
	tracker := usage.NewTracker()
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

	adminHandler := admin.NewAdminHandler(keyStore, cfg.Server.AdminSecret, proxyHandler.Stats, reloadFunc, configPath, cfg, registry, tunnelMgr)

	// 8. Setup routes (Go 1.22 pattern matching)
	mux := http.NewServeMux()

	// OpenAI-compatible API endpoints
	mux.HandleFunc("POST /v1/chat/completions", proxyHandler.HandleChatCompletion)
	mux.HandleFunc("POST /v1/embeddings", proxyHandler.HandleEmbeddings)
	mux.HandleFunc("GET /v1/models", proxyHandler.HandleListModels)
	mux.HandleFunc("GET /health", proxyHandler.HandleHealth)

	// Helper to wrap admin endpoints and check if dashboard is enabled
	wrapAdmin := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			targetCfg := cfg
			if srv != nil {
				targetCfg = srv.cfg
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

	// Diagnostic endpoints
	wrapAdmin("GET /admin/templates", adminHandler.HandleTemplates)
	wrapAdmin("POST /admin/templates/setup", adminHandler.HandleQuickSetup)
	wrapAdmin("POST /admin/diag/test-key", adminHandler.HandleDiagTestKey)
	wrapAdmin("POST /admin/diag/test-model", adminHandler.HandleDiagTestModel)
	wrapAdmin("POST /admin/diag/fetch-models", adminHandler.HandleDiagFetchModels)

	// Admin password change
	wrapAdmin("PUT /admin/change-password", adminHandler.HandleChangePassword)

	// Dashboard — admin web UI (with dynamic secret injection)
	wrapAdmin("GET /admin", adminHandler.ServeDashboard)

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
		"uptime":    stats.Uptime,
		"by_model":  stats.ByModel,
		"by_api_key": stats.ByAPIKey,
		"cache":     cacheStats,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// Start starts the HTTP server and blocks.
func (s *Server) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelFunc = cancel
	defer s.tunnelMgr.Stop()

	// Start background health checks
	s.startHealthChecks(ctx)

	// Start Cloudflare Quick Tunnel if enabled in configuration
	if s.cfg.Server.QuickTunnel {
		go func() {
			slog.Info("starting Cloudflare Quick Tunnel as requested by config...")
			url, err := s.tunnelMgr.Start(ctx, s.cfg.Server.Port)
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
	if s.cfg.Cache.Enabled {
		if c := s.handler.GetCache(); c != nil {
			c.StartCleanup(ctx, 60*time.Second)
			slog.Info("cache cleanup goroutine started", "interval", "60s")
		}
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)

	// Wrap with CORS + logging + body limit middleware
	handler := midware.CORSMiddleware(midware.LoggingMiddleware(midware.BodyLimitMiddleware(s.mux)))

	// Print startup info
	slog.Info("========================================")
	slog.Info("  AI Gateway starting", "address", addr)
	slog.Info("========================================")

	slog.Info("providers loaded", "count", len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		slog.Info("  provider",
			"name", p.Name,
			"type", p.Type,
			"tier", p.Tier,
			"health_check", p.HealthCheckInterval.String(),
		)
	}
	slog.Info("models configured", "count", len(s.cfg.Models))
	for _, m := range s.cfg.Models {
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
	slog.Info("API keys loaded", "count", len(s.cfg.APIKeys))

	slog.Info("features enabled",
		"cache", s.cfg.Cache.Enabled,
		"token_saver", s.cfg.TokenSaver.Enabled,
		"caveman_mode", s.cfg.TokenSaver.CavemanMode,
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
	for _, pCfg := range s.cfg.Providers {
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
	for _, pCfg := range s.cfg.Providers {
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
func (s *Server) checkProvider(name string) {
	var pCfg config.ProviderConfig
	for _, p := range s.cfg.Providers {
		if p.Name == name {
			pCfg = p
			break
		}
	}
	if pCfg.Name == "" {
		return
	}

	checkURL := pCfg.HealthCheckURL
	if checkURL == "" {
		checkURL = pCfg.BaseURL + "/models"
	}

	client := healthCheckClient
	req, err := http.NewRequest("GET", checkURL, nil)
	if err != nil {
		return
	}
	if len(pCfg.APIKeys) > 0 {
		req.Header.Set("Authorization", "Bearer "+pCfg.APIKeys[0])
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("health check failed", "provider", name, "error", err)
		s.setProviderHealth(name, false)
		return
	}
	resp.Body.Close()

	healthy := resp.StatusCode >= 200 && resp.StatusCode < 400
	s.setProviderHealth(name, healthy)

	if !healthy {
		slog.Warn("provider unhealthy", "provider", name, "status", resp.StatusCode)
	}
}

// setProviderHealth sets the health status of a provider by name.
func (s *Server) setProviderHealth(name string, healthy bool) {
	if s.registry == nil {
		return
	}
	if p, ok := s.registry.Get(name); ok {
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
		if len(pCfg.APIKeys) == 0 && pCfg.Type != "opencode" && pCfg.Type != "mimo" {
			slog.Warn("skipping provider with no API keys", "name", pCfg.Name)
			continue
		}
		p, err := provider.NewProviderFromConfig(pCfg)
		if err != nil {
			return fmt.Errorf("init provider %s: %w", pCfg.Name, err)
		}
		registry.Register(pCfg.Name, p)
	}

	r, err := router.NewRouter(newCfg, registry)
	if err != nil {
		return fmt.Errorf("init router: %w", err)
	}

	keyStore := auth.NewKeyStore(newCfg.APIKeys)

	// Create new concurrency limiter if max changed
	var newLimiter *midware.ConcurrencyLimiter
	if newCfg.Concurrency.MaxConcurrent != s.cfg.Concurrency.MaxConcurrent ||
		newCfg.Concurrency.QueueDepth != s.cfg.Concurrency.QueueDepth ||
		newCfg.Concurrency.QueueTimeout != s.cfg.Concurrency.QueueTimeout {
		newLimiter = midware.NewConcurrencyLimiterWithQueue(
			newCfg.Concurrency.MaxConcurrent,
			newCfg.Concurrency.QueueDepth,
			newCfg.Concurrency.QueueTimeout,
		)
		slog.Info("concurrency limiter updated",
			"max_concurrent", newCfg.Concurrency.MaxConcurrent,
			"queue_depth", newCfg.Concurrency.QueueDepth,
			"queue_timeout", newCfg.Concurrency.QueueTimeout,
		)
	}

	// Thread-safe update of proxy handler configs
	s.handler.UpdateConfig(r, keyStore, newLimiter, &newCfg.TokenSaver)

	// Thread-safe update of admin handler configs
	s.admin.UpdateConfig(keyStore, newCfg.Server.AdminSecret, newCfg, registry)

	s.cfg = newCfg
	slog.Info("configuration reloaded successfully")
	return nil
}
