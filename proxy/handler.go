package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aigateway/auth"
	"github.com/aigateway/cache"
	"github.com/aigateway/config"
	"github.com/aigateway/middleware"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
	"github.com/aigateway/router"
	"github.com/aigateway/usage"
)

// Stats tracks request statistics.
type Stats struct {
	TotalRequests  atomic.Int64
	ActiveRequests atomic.Int64
}

// Handler is the main proxy request handler.
type Handler struct {
	mu              sync.RWMutex
	router          *router.Router
	keyStore        *auth.KeyStore
	limiter         *middleware.ConcurrencyLimiter
	cache           *cache.LRUCache
	tracker         *usage.Tracker
	tokenSaverCfg   config.TokenSaverConfig
	Stats           *Stats
	dynamicModelsMu sync.Mutex
	dynamicModels   map[string][]string // providerName -> list of model IDs with prefix
	lastFetch       map[string]time.Time
}

// NewHandler creates a new proxy handler.
func NewHandler(r *router.Router, ks *auth.KeyStore, limiter *middleware.ConcurrencyLimiter, tsCfg config.TokenSaverConfig) *Handler {
	return &Handler{
		router:        r,
		keyStore:      ks,
		limiter:       limiter,
		tokenSaverCfg: tsCfg,
		Stats:         &Stats{},
		dynamicModels: make(map[string][]string),
		lastFetch:     make(map[string]time.Time),
	}
}

// SetCache sets the response cache for the handler.
func (h *Handler) SetCache(c *cache.LRUCache) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cache = c
}

// SetTracker sets the usage tracker for the handler.
func (h *Handler) SetTracker(t *usage.Tracker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tracker = t
}

// GetTracker returns the usage tracker.
func (h *Handler) GetTracker() *usage.Tracker {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tracker
}

// GetCache returns the cache.
func (h *Handler) GetCache() *cache.LRUCache {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cache
}

// UpdateConfig updates the router, keyStore, limiter, and tokenSaverCfg pointer thread-safely.
func (h *Handler) UpdateConfig(r *router.Router, ks *auth.KeyStore, limiter *middleware.ConcurrencyLimiter, tsCfg *config.TokenSaverConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.router = r
	h.keyStore = ks
	if limiter != nil {
		h.limiter = limiter
	}
	if tsCfg != nil {
		h.tokenSaverCfg = *tsCfg
	}
}

// extractAPIKey extracts the Bearer token from the Authorization header.
func (h *Handler) extractAPIKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	return ""
}

// sendError sends an OpenAI-compatible error response.
func (h *Handler) sendError(w http.ResponseWriter, status int, msg string, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: msg,
			Type:    errType,
		},
	})
}

// responseTracker wraps ResponseWriter to track if headers have been written.
// This is needed for streaming fallback — if headers are already sent,
// we can't send a different error status code.
type responseTracker struct {
	http.ResponseWriter
	headerWritten bool
}

func (rt *responseTracker) WriteHeader(code int) {
	rt.headerWritten = true
	rt.ResponseWriter.WriteHeader(code)
}

func (rt *responseTracker) Write(b []byte) (int, error) {
	rt.headerWritten = true
	return rt.ResponseWriter.Write(b)
}

func (rt *responseTracker) Flush() {
	if f, ok := rt.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// HeaderWritten returns true if response headers have already been sent.
func (rt *responseTracker) HeaderWritten() bool {
	return rt.headerWritten
}

// HandleChatCompletion handles POST /v1/chat/completions.
func (h *Handler) HandleChatCompletion(w http.ResponseWriter, r *http.Request) {
	h.Stats.TotalRequests.Add(1)
	h.Stats.ActiveRequests.Add(1)
	defer h.Stats.ActiveRequests.Add(-1)

	// === Get Config (Thread Safe) ===
	h.mu.RLock()
	routerInst := h.router
	keyStore := h.keyStore
	limiter := h.limiter
	responseCache := h.cache
	tracker := h.tracker
	tsCfg := h.tokenSaverCfg
	h.mu.RUnlock()

	// === Auth ===
	apiKey := h.extractAPIKey(r)
	if apiKey == "" {
		h.sendError(w, http.StatusUnauthorized, "Missing API key in Authorization header", "authentication_error")
		return
	}

	keyInfo, ok := keyStore.Validate(apiKey)
	if !ok {
		h.sendError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error")
		return
	}

	// === Rate limit ===
	if !keyInfo.CheckRateLimit() {
		middleware.TooManyRequestsResponse(w)
		return
	}

	// === Parse request (limit body to 10MB) ===
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendError(w, http.StatusBadRequest, "Failed to read request body (max 10MB)", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var req models.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request_error")
		return
	}

	if req.Model == "" {
		h.sendError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}

	slog.Info(fmt.Sprintf("📥 POST /v1/chat/completions | %s | %d msgs", req.Model, len(req.Messages)))

	// === Permission check ===
	if !keyInfo.IsModelAllowed(req.Model) {
		h.sendError(w, http.StatusForbidden,
			"Model '"+req.Model+"' is not allowed for this API key", "permission_error")
		return
	}

	// === Token Saver — compress verbose tool outputs (per-key toggle) ===
	if keyInfo.IsTokenSaverEnabled(tsCfg.Enabled) {
		stats := middleware.CompressMessages(&req, tsCfg)
		if stats != nil && stats.MessagesChanged > 0 {
			savedPct := float64(0)
			if stats.OriginalBytes > 0 {
				savedPct = float64(stats.OriginalBytes-stats.CompressedBytes) / float64(stats.OriginalBytes) * 100
			}
			slog.Debug("token saver compressed",
				"original_bytes", stats.OriginalBytes,
				"compressed_bytes", stats.CompressedBytes,
				"saved_pct", fmt.Sprintf("%.1f%%", savedPct),
				"messages_changed", stats.MessagesChanged,
			)
			if tracker != nil {
				tracker.RecordTokenSaving(int64(stats.OriginalBytes - stats.CompressedBytes))
			}
		}
		if tsCfg.CavemanMode {
			middleware.InjectCavemanMode(&req)
		}
	}

	// === Check Cache (non-streaming only) ===
	cacheKey := ""
	if !req.Stream && responseCache != nil {
		cacheKey = cache.HashRequest(&req)
		if cached := responseCache.Get(cacheKey); cached != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			json.NewEncoder(w).Encode(cached)
			if tracker != nil {
				tracker.RecordUsage(apiKey, "cache", req.Model, 0, 0, true)
			}
			return
		}
	}

	// === Concurrency limit with queuing ===
	if !limiter.AcquireGlobalWithQueue() {
		middleware.QueueFullResponse(w)
		return
	}
	defer limiter.ReleaseGlobal()

	// === Route request ===
	startTime := time.Now()

	if req.Stream {
		// Streaming response
		respTracker := &responseTracker{ResponseWriter: w}
		flusher, ok := w.(http.Flusher)
		if !ok {
			h.sendError(w, http.StatusInternalServerError, "Streaming not supported by server", "server_error")
			return
		}

		err := routerInst.ChatCompletionStream(r.Context(), req.Model, &req, respTracker, flusher)
		durationMs := time.Since(startTime).Milliseconds()
		if err != nil {
			slog.Error(fmt.Sprintf("🌊 [STREAM] %s | %dms | error: %v", req.Model, durationMs, err))
			if tracker != nil {
				tracker.RecordError("stream", req.Model)
			}
			if !respTracker.headerWritten {
				h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
			}
		} else {
			slog.Info(fmt.Sprintf("🌊 [STREAM] %s | %dms | complete", req.Model, durationMs))
		}
	} else {
		// Non-streaming response
		resp, err := routerInst.ChatCompletion(r.Context(), req.Model, &req)
		durationMs := time.Since(startTime).Milliseconds()
		if err != nil {
			slog.Error(fmt.Sprintf("❌ [ERROR] completion error: %v | model=%s | duration=%dms", err, req.Model, durationMs))
			if tracker != nil {
				tracker.RecordError("unknown", req.Model)
			}
			h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
			return
		}

		// Record usage — extract provider name from model if possible
		inTokens := 0
		outTokens := 0
		if resp.Usage != nil {
			inTokens = resp.Usage.PromptTokens
			outTokens = resp.Usage.CompletionTokens
			if tracker != nil {
				tracker.RecordUsage(apiKey, resp.Model, resp.Model,
					resp.Usage.PromptTokens, resp.Usage.CompletionTokens, false)
			}
		}
		slog.Info(fmt.Sprintf("📊 [USAGE] %s | in=%d | out=%d | latency=%dms", req.Model, inTokens, outTokens, durationMs))

		// Cache the response
		if responseCache != nil && cacheKey != "" && resp != nil {
			responseCache.Set(cacheKey, resp)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// HandleListModels handles GET /v1/models.
// Returns models filtered by the API key's permissions.
func (h *Handler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	// === Get Config (Thread Safe) ===
	h.mu.RLock()
	routerInst := h.router
	keyStore := h.keyStore
	h.mu.RUnlock()

	apiKey := h.extractAPIKey(r)
	if apiKey == "" {
		h.sendError(w, http.StatusUnauthorized, "Missing API key", "authentication_error")
		return
	}

	keyInfo, ok := keyStore.Validate(apiKey)
	if !ok {
		h.sendError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error")
		return
	}

	modelNames := routerInst.GetModelNames()
	var modelList []models.ModelObject
	for _, name := range modelNames {
		if keyInfo.IsModelAllowed(name) {
			modelList = append(modelList, models.ModelObject{
				ID:      name,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "aigateway",
			})
		}
	}

	// Dynamic models fetching
	if reg := routerInst.Registry(); reg != nil {
		for _, prov := range reg.All() {
			if up, ok := prov.(provider.UpstreamConfigProvider); ok {
				pType := up.ProviderType()
				if pType == "opencode" || pType == "mimo" {
					dynList := h.getDynamicModels(prov)
					for _, name := range dynList {
						if keyInfo.IsModelAllowed(name) {
							modelList = append(modelList, models.ModelObject{
								ID:      name,
								Object:  "model",
								Created: time.Now().Unix(),
								OwnedBy: prov.Name(),
							})
						}
					}
				}
			}
		}
	}

	if modelList == nil {
		modelList = []models.ModelObject{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models.ModelListResponse{
		Object: "list",
		Data:   modelList,
	})
}

func (h *Handler) getDynamicModels(prov provider.Provider) []string {
	h.dynamicModelsMu.Lock()
	defer h.dynamicModelsMu.Unlock()

	up, ok := prov.(provider.UpstreamConfigProvider)
	if !ok {
		return nil
	}

	pName := prov.Name()
	pType := up.ProviderType()
	baseURL := up.BaseURL()

	// 5 minutes cache to prevent rate limiting per IP
	if cached, ok := h.dynamicModels[pName]; ok && time.Since(h.lastFetch[pName]) < 5*time.Minute {
		return cached
	}

	// Fetch upstream models
	client := &http.Client{
		Transport: up.Client().Transport,
		Timeout:   3 * time.Second,
	}
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		slog.Warn("failed to create models request", "provider", pName, "error", err)
		return h.dynamicModels[pName]
	}

	if keys := up.APIKeys(); len(keys) > 0 && keys[0].Key != "" {
		req.Header.Set("Authorization", "Bearer "+keys[0].Key)
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("failed to fetch models from provider", "provider", pName, "error", err)
		return h.dynamicModels[pName]
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("models fetch returned bad status", "provider", pName, "status", resp.StatusCode)
		return h.dynamicModels[pName]
	}

	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		slog.Warn("failed to decode models JSON", "provider", pName, "error", err)
		return h.dynamicModels[pName]
	}

	var list []string
	prefix := ""
	if pType == "opencode" {
		prefix = "oc/"
	} else if pType == "mimo" {
		prefix = "mimo/"
	}

	// Prepend "auto" virtual model
	if prefix != "" {
		list = append(list, prefix+"auto")
	}

	var rawModels []string
	hasKeys := false
	if keys := up.APIKeys(); len(keys) > 0 && keys[0].Key != "" {
		hasKeys = true
	}

	for _, m := range data.Data {
		// If provider is opencode and has no custom api key, filter to keep only free models
		if pType == "opencode" && !hasKeys {
			if !strings.HasSuffix(m.ID, "-free") {
				continue // skip paid models
			}
		}
		rawModels = append(rawModels, m.ID)
		list = append(list, prefix+m.ID)
	}

	provider.SetCachedDynamicModels(pName, rawModels)

	h.dynamicModels[pName] = list
	h.lastFetch[pName] = time.Now()
	return list
}

// HandleHealth handles GET /health.
func (h *Handler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	limiter := h.limiter
	responseCache := h.cache
	h.mu.RUnlock()

	resp := map[string]interface{}{
		"status":           "ok",
		"active_requests":  h.Stats.ActiveRequests.Load(),
		"total_requests":   h.Stats.TotalRequests.Load(),
		"concurrency_used": limiter.ActiveCount(),
		"concurrency_max":  limiter.Capacity(),
	}

	if responseCache != nil {
		hits, misses, size := responseCache.Stats()
		resp["cache"] = map[string]interface{}{
			"hits":   hits,
			"misses": misses,
			"size":   size,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
