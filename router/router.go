package router

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aigateway/balancer"
	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

// circuitBreakerDuration is how long a backend stays disabled after an error.
const circuitBreakerDuration = 30 * time.Second

// Backend represents a single model deployment on a provider.
type Backend struct {
	Provider      provider.Provider
	Model         string
	Tier          int // 1=subscription, 2=cheap, 3=free
	DisabledUntil *atomic.Int64 // UnixNano timestamp; 0 = available
}

// ModelRoute holds the routing configuration for a model name.
type ModelRoute struct {
	Name     string
	Strategy string // "" (direct), "round-robin", "fallback", "tiered"
	Disabled bool

	// For direct model mapping
	Backend *Backend

	// For combo models (round-robin or fallback)
	Backends []Backend
	Balancer *balancer.RoundRobin[int] // Index-based round-robin
}

// RetryConfig holds retry with exponential backoff settings.
type RetryConfig struct {
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// Router resolves model names to provider backends.
type Router struct {
	routes   map[string]*ModelRoute
	registry *provider.Registry
	retryCfg RetryConfig
	cfg      *config.Config
}

// NewRouter creates a Router from configuration.
func NewRouter(cfg *config.Config, registry *provider.Registry) (*Router, error) {
	r := &Router{
		routes:   make(map[string]*ModelRoute),
		registry: registry,
		cfg:      cfg,
		retryCfg: RetryConfig{
			MaxRetries:     cfg.Retry.MaxRetries,
			InitialBackoff: time.Duration(cfg.Retry.InitialBackoff) * time.Millisecond,
			MaxBackoff:     time.Duration(cfg.Retry.MaxBackoff) * time.Millisecond,
		},
	}

	// Build map of disabled routes/models
	disabledRoutes := make(map[string]bool)
	for _, m := range cfg.Models {
		if m.Disabled {
			disabledRoutes[m.Name] = true
		}
	}

	for _, modelCfg := range cfg.Models {
		route := &ModelRoute{
			Name:     modelCfg.Name,
			Strategy: modelCfg.Strategy,
			Disabled: modelCfg.Disabled,
		}

		if modelCfg.Strategy == "" {
			// Direct model — single provider, single model
			p, ok := registry.Get(modelCfg.Provider)
			if !ok {
				slog.Warn("skipping model: provider not found (may have no API keys)",
					"model", modelCfg.Name, "provider", modelCfg.Provider)
				continue
			}
			route.Backend = &Backend{
				Provider:      p,
				Model:         modelCfg.Model,
				Tier:          1,
				DisabledUntil: &atomic.Int64{},
			}
		} else {
			// Combo model — multiple backends
			indices := make([]int, 0, len(modelCfg.Backends))
			for _, bCfg := range modelCfg.Backends {
				if disabledRoutes[bCfg.Model] {
					slog.Info("skipping disabled backend in combo model", "combo", modelCfg.Name, "backend", bCfg.Model)
					continue
				}
				p, ok := registry.Get(bCfg.Provider)
				if !ok {
					slog.Warn("skipping backend: provider not found",
						"model", modelCfg.Name, "provider", bCfg.Provider)
					continue
				}
				tier := bCfg.Tier
				if tier == 0 {
					tier = 1
				}
				route.Backends = append(route.Backends, Backend{
					Provider:      p,
					Model:         bCfg.Model,
					Tier:          tier,
					DisabledUntil: &atomic.Int64{},
				})
				indices = append(indices, len(route.Backends)-1)
			}
			if len(route.Backends) == 0 {
				slog.Warn("skipping combo model: all backends missing", "model", modelCfg.Name)
				continue
			}
			if modelCfg.Strategy == "round-robin" {
				route.Balancer = balancer.New(indices)
			}
			// Sort backends by tier for fallback and tiered strategies
			if modelCfg.Strategy == "fallback" || modelCfg.Strategy == "tiered" {
				sort.Slice(route.Backends, func(i, j int) bool {
					return route.Backends[i].Tier < route.Backends[j].Tier
				})
			}
		}

		r.routes[modelCfg.Name] = route
	}

	return r, nil
}

// GetModelNames returns all active (non-disabled) model names.
func (r *Router) GetModelNames() []string {
	names := make([]string, 0, len(r.routes))
	for name, route := range r.routes {
		if !route.Disabled {
			names = append(names, name)
		}
	}
	return names
}

// resolveSmartVisionRoute automatically redirects requests that contain an image
// but target a non-vision model, to the first active model route that supports vision.
func (r *Router) resolveSmartVisionRoute(modelName string, req *models.ChatCompletionRequest) string {
	if req == nil || r.cfg == nil {
		return modelName
	}
	hasImage := false
	for _, msg := range req.Messages {
		if msg.HasImage() {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return modelName
	}

	isVisionModel := false
	for _, m := range r.cfg.Models {
		if m.Name == modelName && m.Vision {
			isVisionModel = true
			break
		}
	}
	if isVisionModel {
		return modelName
	}

	// Smart routing: search for the first active vision model route
	var fallbackModel string
	for _, m := range r.cfg.Models {
		if m.Vision && !m.Disabled {
			if _, exists := r.routes[m.Name]; exists {
				fallbackModel = m.Name
				break
			}
		}
	}
	if fallbackModel != "" {
		slog.Warn("smart routing: requested model does not support vision but request contains an image, redirecting",
			"from", modelName, "to", fallbackModel)
		req.Model = fallbackModel
		return fallbackModel
	}

	slog.Warn("smart routing: request contains an image and requested model does not support vision, but no active vision model is configured")
	return modelName
}

func (r *Router) executeBackend(ctx context.Context, p provider.Provider, model string, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	if model == "oc/auto" {
		modelsList := provider.GetCachedDynamicModels(p.Name())
		if len(modelsList) == 0 {
			modelsList = []string{"deepseek-v4-flash-free", "mimo-v2.5-free", "nemotron-3-ultra-free", "north-mini-code-free"}
		}
		var lastErr error
		for _, mName := range modelsList {
			req.Model = mName
			slog.Info("routing oc/auto to model in combo", "model", mName)
			resp, err := p.ChatCompletion(ctx, req)
			if err == nil {
				return resp, nil
			}
			slog.Warn("oc/auto failed model fallback in combo", "model", mName, "error", err)
			lastErr = err
		}
		return nil, fmt.Errorf("all opencode free models failed in combo: %w", lastErr)
	}

	if model == "mimo/auto" {
		modelsList := provider.GetCachedDynamicModels(p.Name())
		if len(modelsList) == 0 {
			modelsList = []string{"mimo-v2.5-free"}
		}
		var lastErr error
		for _, mName := range modelsList {
			req.Model = mName
			slog.Info("routing mimo/auto to model in combo", "model", mName)
			resp, err := p.ChatCompletion(ctx, req)
			if err == nil {
				return resp, nil
			}
			slog.Warn("mimo/auto failed model fallback in combo", "model", mName, "error", err)
			lastErr = err
		}
		return nil, fmt.Errorf("all mimo free models failed in combo: %w", lastErr)
	}

	req.Model = model
	return p.ChatCompletion(ctx, req)
}

func (r *Router) executeBackendStream(ctx context.Context, p provider.Provider, model string, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	if model == "oc/auto" {
		modelsList := provider.GetCachedDynamicModels(p.Name())
		if len(modelsList) == 0 {
			modelsList = []string{"deepseek-v4-flash-free", "mimo-v2.5-free", "nemotron-3-ultra-free", "north-mini-code-free"}
		}
		var lastErr error
		for _, mName := range modelsList {
			req.Model = mName
			slog.Info("routing oc/auto stream to model in combo", "model", mName)
			err := p.ChatCompletionStream(ctx, req, w, flusher)
			if err == nil {
				return nil
			}
			slog.Warn("oc/auto stream failed model fallback in combo", "model", mName, "error", err)
			lastErr = err
		}
		return fmt.Errorf("all opencode free models failed in combo stream: %w", lastErr)
	}

	if model == "mimo/auto" {
		modelsList := provider.GetCachedDynamicModels(p.Name())
		if len(modelsList) == 0 {
			modelsList = []string{"mimo-v2.5-free"}
		}
		var lastErr error
		for _, mName := range modelsList {
			req.Model = mName
			slog.Info("routing mimo/auto stream to model in combo", "model", mName)
			err := p.ChatCompletionStream(ctx, req, w, flusher)
			if err == nil {
				return nil
			}
			slog.Warn("mimo/auto stream failed model fallback in combo", "model", mName, "error", err)
			lastErr = err
		}
		return fmt.Errorf("all mimo free models failed in combo stream: %w", lastErr)
	}

	req.Model = model
	return p.ChatCompletionStream(ctx, req, w, flusher)
}

// retryWithBackoff executes fn with exponential backoff on retryable errors.
func (r *Router) retryWithBackoff(ctx context.Context, fn func() (bool, error)) error {
	var lastErr error
	maxRetries := r.retryCfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 1
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		retryable, err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		if !retryable {
			return err
		}

		if attempt < maxRetries {
			backoff := time.Duration(float64(r.retryCfg.InitialBackoff) * math.Pow(2, float64(attempt)))
			if backoff > r.retryCfg.MaxBackoff {
				backoff = r.retryCfg.MaxBackoff
			}

			slog.Warn("retrying after backoff",
				"attempt", attempt+1,
				"backoff", backoff.String(),
				"error", err,
			)

			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
				continue
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}

	return lastErr
}

// ChatCompletion routes a non-streaming chat completion request.
func (r *Router) ChatCompletion(ctx context.Context, modelName string, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	modelName = r.resolveSmartVisionRoute(modelName, req)
	route, ok := r.routes[modelName]
	if !ok {
		// Dynamic prefix routing fallback
		if strings.HasPrefix(modelName, "oc/") {
			p, pok := r.registry.Get("opencode")
			if !pok {
				for _, prov := range r.registry.All() {
					if up, ok := prov.(provider.UpstreamConfigProvider); ok && up.ProviderType() == "opencode" {
						p = prov
						pok = true
						break
					}
				}
			}
			if pok {
				targetModel := strings.TrimPrefix(modelName, "oc/")
				if targetModel == "auto" {
					modelsList := provider.GetCachedDynamicModels(p.Name())
					if len(modelsList) == 0 {
						modelsList = []string{"deepseek-v4-flash-free", "mimo-v2.5-free", "nemotron-3-ultra-free", "north-mini-code-free"}
					}
					var lastErr error
					for _, mName := range modelsList {
						req.Model = mName
						slog.Info("routing oc/auto to model", "model", mName)
						resp, err := p.ChatCompletion(ctx, req)
						if err == nil {
							return resp, nil
						}
						slog.Warn("oc/auto failed model fallback", "model", mName, "error", err)
						lastErr = err
					}
					return nil, fmt.Errorf("all opencode free models failed: %w", lastErr)
				}
				req.Model = targetModel
				slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s", modelName, p.Name(), targetModel))
				return p.ChatCompletion(ctx, req)
			}
		}
		if strings.HasPrefix(modelName, "mimo/") {
			p, pok := r.registry.Get("mimo")
			if !pok {
				for _, prov := range r.registry.All() {
					if up, ok := prov.(provider.UpstreamConfigProvider); ok && up.ProviderType() == "mimo" {
						p = prov
						pok = true
						break
					}
				}
			}
			if pok {
				targetModel := strings.TrimPrefix(modelName, "mimo/")
				if targetModel == "auto" {
					modelsList := provider.GetCachedDynamicModels(p.Name())
					if len(modelsList) == 0 {
						modelsList = []string{"mimo-v2.5-free"}
					}
					var lastErr error
					for _, mName := range modelsList {
						req.Model = mName
						slog.Info("routing mimo/auto to model", "model", mName)
						resp, err := p.ChatCompletion(ctx, req)
						if err == nil {
							return resp, nil
						}
						slog.Warn("mimo/auto failed model fallback", "model", mName, "error", err)
						lastErr = err
					}
					return nil, fmt.Errorf("all mimo free models failed: %w", lastErr)
				}
				req.Model = targetModel
				slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s", modelName, p.Name(), targetModel))
				return p.ChatCompletion(ctx, req)
			}
		}
		return nil, fmt.Errorf("model not found: %s", modelName)
	}
	if route.Disabled {
		return nil, fmt.Errorf("model is disabled: %s", modelName)
	}

	switch route.Strategy {
	case "", "direct":
		// Direct: single backend
		req.Model = route.Backend.Model
		slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s", modelName, route.Backend.Provider.Name(), route.Backend.Model))
		return r.executeBackend(ctx, route.Backend.Provider, route.Backend.Model, req)

	case "round-robin":
		// Round-robin with circuit breaker: try available backends
		total := len(route.Backends)
		firstIdx := route.Balancer.Next()
		idx := firstIdx
		var backend *Backend

		for i := 0; i < total; i++ {
			b := &route.Backends[idx]
			if time.Now().UnixNano() >= b.DisabledUntil.Load() {
				backend = b
				break
			}
			slog.Info("round-robin skipping disabled backend",
				"alias", modelName,
				"provider", b.Provider.Name(),
				"model", b.Model,
			)
			idx = route.Balancer.Next()
		}

		// If all backends disabled, use the originally selected one
		if backend == nil {
			backend = &route.Backends[firstIdx]
			slog.Warn("all backends disabled, using originally selected",
				"alias", modelName,
				"provider", backend.Provider.Name(),
			)
		}

		req.Model = backend.Model
		slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s (round-robin)", modelName, backend.Provider.Name(), backend.Model))

		resp, err := r.executeBackend(ctx, backend.Provider, backend.Model, req)
		if err != nil {
			backend.DisabledUntil.Store(time.Now().Add(circuitBreakerDuration).UnixNano())
			slog.Warn("backend disabled due to error",
				"alias", modelName,
				"provider", backend.Provider.Name(),
				"disabled_for", circuitBreakerDuration.String(),
				"error", err,
			)
		}
		return resp, err

	case "fallback", "tiered":
		// Fallback with retry: try each backend in tier order
		var lastErr error
		currentTier := 0
		for i := range route.Backends {
			backend := &route.Backends[i]
			// Skip unhealthy or circuit-broken providers
			if !backend.Provider.IsHealthy() {
				slog.Warn("skipping unhealthy provider",
					"alias", modelName,
					"provider", backend.Provider.Name(),
					"model", backend.Model,
				)
				continue
			}
			if time.Now().UnixNano() < backend.DisabledUntil.Load() {
				slog.Warn("skipping circuit-broken provider",
					"alias", modelName,
					"provider", backend.Provider.Name(),
					"model", backend.Model,
				)
				continue
			}

			// Log tier transitions
			if backend.Tier != currentTier {
				if currentTier > 0 {
					slog.Warn("falling back to next tier",
						"alias", modelName,
						"from_tier", currentTier,
						"to_tier", backend.Tier,
					)
				}
				currentTier = backend.Tier
			}

			// Try this backend with retry
			reqCopy := *req
			reqCopy.Model = backend.Model

			var successResp *models.ChatCompletionResponse
			retryErr := r.retryWithBackoff(ctx, func() (bool, error) {
				resp, err := r.executeBackend(ctx, backend.Provider, backend.Model, &reqCopy)
				if err == nil {
					successResp = resp
					if i > 0 {
						slog.Info(fmt.Sprintf("ℹ️ [ROUTING] fallback succeeded: %s → %s/%s", modelName, backend.Provider.Name(), backend.Model))
					}
					return false, nil
				}
				// Only retry on retryable errors
				if pe, ok := err.(*provider.ProviderError); ok {
					return pe.IsRetryable(), err
				}
				return false, err
			})

			if retryErr == nil && successResp != nil {
				return successResp, nil
			}

			lastErr = retryErr
			// Disable this backend via circuit breaker
			backend.DisabledUntil.Store(time.Now().Add(circuitBreakerDuration).UnixNano())
			slog.Warn("fallback: backend failed after retries",
				"alias", modelName,
				"provider", backend.Provider.Name(),
				"model", backend.Model,
				"tier", backend.Tier,
				"disabled_for", circuitBreakerDuration.String(),
				"error", retryErr,
			)

			// Non-retryable error stops fallback for this backend
			if pe, ok := retryErr.(*provider.ProviderError); ok && !pe.IsRetryable() {
				// Continue to next backend in next tier
				continue
			}
		}
		return nil, fmt.Errorf("all backends failed for model %s: %w", modelName, lastErr)

	default:
		return nil, fmt.Errorf("unknown routing strategy: %s", route.Strategy)
	}
}

// HeaderWrittenChecker can check if HTTP headers have already been sent.
// Implemented by proxy.responseTracker to prevent streaming fallback corruption.
type HeaderWrittenChecker interface {
	HeaderWritten() bool
}

// ChatCompletionStream routes a streaming chat completion request.
func (r *Router) ChatCompletionStream(ctx context.Context, modelName string, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	modelName = r.resolveSmartVisionRoute(modelName, req)
	route, ok := r.routes[modelName]
	if !ok {
		// Dynamic prefix routing fallback
		if strings.HasPrefix(modelName, "oc/") {
			p, pok := r.registry.Get("opencode")
			if !pok {
				for _, prov := range r.registry.All() {
					if up, ok := prov.(provider.UpstreamConfigProvider); ok && up.ProviderType() == "opencode" {
						p = prov
						pok = true
						break
					}
				}
			}
			if pok {
				targetModel := strings.TrimPrefix(modelName, "oc/")
				if targetModel == "auto" {
					modelsList := provider.GetCachedDynamicModels(p.Name())
					if len(modelsList) == 0 {
						modelsList = []string{"deepseek-v4-flash-free", "mimo-v2.5-free", "nemotron-3-ultra-free", "north-mini-code-free"}
					}
					var lastErr error
					for _, mName := range modelsList {
						req.Model = mName
						slog.Info("routing stream oc/auto to model", "model", mName)
						err := p.ChatCompletionStream(ctx, req, w, flusher)
						if err == nil {
							return nil
						}
						slog.Warn("oc/auto stream failed model fallback", "model", mName, "error", err)
						lastErr = err
					}
					return fmt.Errorf("all opencode free models failed: %w", lastErr)
				}
				req.Model = targetModel
				return p.ChatCompletionStream(ctx, req, w, flusher)
			}
		}
		if strings.HasPrefix(modelName, "mimo/") {
			p, pok := r.registry.Get("mimo")
			if !pok {
				for _, prov := range r.registry.All() {
					if up, ok := prov.(provider.UpstreamConfigProvider); ok && up.ProviderType() == "mimo" {
						p = prov
						pok = true
						break
					}
				}
			}
			if pok {
				targetModel := strings.TrimPrefix(modelName, "mimo/")
				if targetModel == "auto" {
					modelsList := provider.GetCachedDynamicModels(p.Name())
					if len(modelsList) == 0 {
						modelsList = []string{"mimo-v2.5-free"}
					}
					var lastErr error
					for _, mName := range modelsList {
						req.Model = mName
						slog.Info("routing stream mimo/auto to model", "model", mName)
						err := p.ChatCompletionStream(ctx, req, w, flusher)
						if err == nil {
							return nil
						}
						slog.Warn("mimo/auto stream failed model fallback", "model", mName, "error", err)
						lastErr = err
					}
					return fmt.Errorf("all mimo free models failed: %w", lastErr)
				}
				req.Model = targetModel
				slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s", modelName, p.Name(), targetModel))
				return p.ChatCompletionStream(ctx, req, w, flusher)
			}
		}
		return fmt.Errorf("model not found: %s", modelName)
	}
	if route.Disabled {
		return fmt.Errorf("model is disabled: %s", modelName)
	}

	switch route.Strategy {
	case "", "direct":
		req.Model = route.Backend.Model
		slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s", modelName, route.Backend.Provider.Name(), route.Backend.Model))
		return r.executeBackendStream(ctx, route.Backend.Provider, route.Backend.Model, req, w, flusher)

	case "round-robin":
		// Round-robin with circuit breaker: try available backends
		total := len(route.Backends)
		firstIdx := route.Balancer.Next()
		idx := firstIdx
		var backend *Backend

		for i := 0; i < total; i++ {
			b := &route.Backends[idx]
			if time.Now().UnixNano() >= b.DisabledUntil.Load() {
				backend = b
				break
			}
			slog.Info("round-robin skipping disabled backend",
				"alias", modelName,
				"provider", b.Provider.Name(),
				"model", b.Model,
			)
			idx = route.Balancer.Next()
		}

		// If all backends disabled, use the originally selected one
		if backend == nil {
			backend = &route.Backends[firstIdx]
			slog.Warn("all backends disabled, using originally selected",
				"alias", modelName,
				"provider", backend.Provider.Name(),
			)
		}

		req.Model = backend.Model
		slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s (round-robin)", modelName, backend.Provider.Name(), backend.Model))

		err := r.executeBackendStream(ctx, backend.Provider, backend.Model, req, w, flusher)
		if err != nil {
			backend.DisabledUntil.Store(time.Now().Add(circuitBreakerDuration).UnixNano())
			slog.Warn("backend disabled due to error",
				"alias", modelName,
				"provider", backend.Provider.Name(),
				"disabled_for", circuitBreakerDuration.String(),
				"error", err,
			)
		}
		return err

	case "fallback", "tiered":
		// Streaming fallback with tier awareness
		var lastErr error
		currentTier := 0
		for i := range route.Backends {
			backend := &route.Backends[i]
			// CRITICAL: Check if a previous backend already started writing output
			if checker, ok := w.(HeaderWrittenChecker); ok && checker.HeaderWritten() {
				slog.Error("fallback aborted: headers already sent by previous backend",
					"alias", modelName,
					"failed_provider", lastErr,
				)
				return fmt.Errorf("streaming fallback impossible: response headers already sent: %w", lastErr)
			}

			// Skip unhealthy or circuit-broken providers
			if !backend.Provider.IsHealthy() {
				slog.Warn("skipping unhealthy provider (stream)",
					"alias", modelName,
					"provider", backend.Provider.Name(),
				)
				continue
			}
			if time.Now().UnixNano() < backend.DisabledUntil.Load() {
				slog.Warn("skipping circuit-broken provider (stream)",
					"alias", modelName,
					"provider", backend.Provider.Name(),
				)
				continue
			}

			if backend.Tier != currentTier {
				if currentTier > 0 {
					slog.Warn("stream fallback to next tier",
						"alias", modelName,
						"from_tier", currentTier,
						"to_tier", backend.Tier,
					)
				}
				currentTier = backend.Tier
			}

			reqCopy := *req
			reqCopy.Model = backend.Model

			err := r.executeBackendStream(ctx, backend.Provider, backend.Model, &reqCopy, w, flusher)
			if err == nil {
				return nil
			}

			lastErr = err
			// Disable this backend via circuit breaker
			backend.DisabledUntil.Store(time.Now().Add(circuitBreakerDuration).UnixNano())
			slog.Warn("fallback stream: backend failed",
				"alias", modelName,
				"provider", backend.Provider.Name(),
				"model", backend.Model,
				"tier", backend.Tier,
				"disabled_for", circuitBreakerDuration.String(),
				"error", err,
				"attempt", i+1,
			)

			if pe, ok := err.(*provider.ProviderError); ok && !pe.IsRetryable() {
				return err
			}
		}
		return fmt.Errorf("all backends failed for model %s: %w", modelName, lastErr)

	default:
		return fmt.Errorf("unknown routing strategy: %s", route.Strategy)
	}
}

// Registry returns the provider registry.
func (r *Router) Registry() *provider.Registry {
	return r.registry
}

// Embeddings routes an embeddings request.
func (r *Router) Embeddings(ctx context.Context, modelName string, req *models.EmbeddingsRequest) (*models.EmbeddingsResponse, error) {
	route, ok := r.routes[modelName]
	if !ok {
		// Dynamic prefix routing fallback (like for opencode)
		if strings.HasPrefix(modelName, "oc/") {
			p, pok := r.registry.Get("opencode")
			if !pok {
				for _, prov := range r.registry.All() {
					if up, ok := prov.(provider.UpstreamConfigProvider); ok && up.ProviderType() == "opencode" {
						p = prov
						pok = true
						break
					}
				}
			}
			if pok {
				targetModel := strings.TrimPrefix(modelName, "oc/")
				req.Model = targetModel
				slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s (embeddings)", modelName, p.Name(), targetModel))
				return p.Embeddings(ctx, req)
			}
		}
		if strings.HasPrefix(modelName, "mimo/") {
			p, pok := r.registry.Get("mimo")
			if !pok {
				for _, prov := range r.registry.All() {
					if up, ok := prov.(provider.UpstreamConfigProvider); ok && up.ProviderType() == "mimo" {
						p = prov
						pok = true
						break
					}
				}
			}
			if pok {
				targetModel := strings.TrimPrefix(modelName, "mimo/")
				req.Model = targetModel
				slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s (embeddings)", modelName, p.Name(), targetModel))
				return p.Embeddings(ctx, req)
			}
		}
		return nil, fmt.Errorf("model not found: %s", modelName)
	}
	if route.Disabled {
		return nil, fmt.Errorf("model is disabled: %s", modelName)
	}

	switch route.Strategy {
	case "", "direct":
		// Direct: single backend
		req.Model = route.Backend.Model
		slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s (embeddings)", modelName, route.Backend.Provider.Name(), route.Backend.Model))
		return route.Backend.Provider.Embeddings(ctx, req)

	case "round-robin":
		// Round-robin with circuit breaker: try available backends
		total := len(route.Backends)
		firstIdx := route.Balancer.Next()
		idx := firstIdx
		var backend *Backend

		for i := 0; i < total; i++ {
			b := &route.Backends[idx]
			now := time.Now().UnixNano()
			if now >= b.DisabledUntil.Load() {
				backend = b
				break
			}
			idx = (idx + 1) % total
		}

		if backend == nil {
			// All backends disabled — force try the first one
			backend = &route.Backends[firstIdx]
			slog.Warn("all backends disabled for model route, forcing first backend", "model", modelName)
		}

		var resp *models.EmbeddingsResponse
		err := r.retryWithBackoff(ctx, func() (bool, error) {
			req.Model = backend.Model
			slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s (embeddings)", modelName, backend.Provider.Name(), backend.Model))
			var callErr error
			resp, callErr = backend.Provider.Embeddings(ctx, req)
			if callErr != nil {
				if provErr, ok := callErr.(*provider.ProviderError); ok {
					if provErr.IsRetryable() {
						backend.DisabledUntil.Store(time.Now().Add(circuitBreakerDuration).UnixNano())
						slog.Warn("backend circuit broken (embeddings)", "provider", backend.Provider.Name(), "model", backend.Model)
						// Pick next backend for retry
						idx = (idx + 1) % total
						backend = &route.Backends[idx]
						return true, callErr
					}
				}
				return false, callErr
			}
			return false, nil
		})
		return resp, err

	case "fallback":
		// Sequential fallback: try backends in order of tier
		var resp *models.EmbeddingsResponse
		var lastErr error

		for i := range route.Backends {
			b := &route.Backends[i]
			now := time.Now().UnixNano()
			if now < b.DisabledUntil.Load() {
				continue // circuit broken, skip
			}

			req.Model = b.Model
			slog.Info(fmt.Sprintf("ℹ️ [ROUTING] %s → %s/%s (embeddings)", modelName, b.Provider.Name(), b.Model))
			var callErr error
			resp, callErr = b.Provider.Embeddings(ctx, req)
			if callErr == nil {
				return resp, nil
			}

			lastErr = callErr
			if provErr, ok := callErr.(*provider.ProviderError); ok && provErr.IsRetryable() {
				b.DisabledUntil.Store(time.Now().Add(circuitBreakerDuration).UnixNano())
				slog.Warn("backend circuit broken (embeddings)", "provider", b.Provider.Name(), "model", b.Model)
			} else {
				break // non-retryable error, stop fallback
			}
		}

		if resp == nil && lastErr == nil {
			lastErr = fmt.Errorf("no healthy backends available for fallback")
		}
		return resp, lastErr

	default:
		return nil, fmt.Errorf("unsupported strategy for embeddings: %s", route.Strategy)
	}
}
