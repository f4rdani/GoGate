package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aigateway/auth"
	"github.com/aigateway/middleware"
	"github.com/aigateway/provider"
)

// audioForwarder is implemented by *provider.BaseProvider (embedded in every
// real provider) for raw upstream passthrough with key rotation.
type audioForwarder interface {
	ForwardUpstream(ctx context.Context, method, endpointPath string, body []byte, contentType string) (*http.Response, error)
}

// forwardAudio sends a raw request to the provider with full key rotation.
func forwardAudio(ctx context.Context, p provider.Provider, endpointPath string, body []byte, contentType string) (*http.Response, error) {
	fwd, ok := p.(audioForwarder)
	if !ok {
		return nil, fmt.Errorf("audio passthrough unsupported by provider %s", p.Name())
	}
	return fwd.ForwardUpstream(ctx, "POST", endpointPath, body, contentType)
}

// audioModelSupporter is implemented by *provider.BaseProvider (embedded in
// every real provider) to test configured model support.
type audioModelSupporter interface {
	SupportsModel(model string) bool
}

// resolveAudioProvider picks the upstream provider for an audio model:
// first healthy provider configured for the model (deterministic by name),
// else an error. Self-hosted STT/TTS/embedding boxes work by listing their
// model IDs in the provider's models list.
func (h *Handler) resolveAudioProvider(model string) (provider.Provider, string, error) {
	h.mu.RLock()
	routerInst := h.router
	h.mu.RUnlock()
	if routerInst == nil || routerInst.Registry() == nil {
		return nil, "", fmt.Errorf("router not initialized")
	}
	names := make([]string, 0)
	for name := range routerInst.Registry().All() {
		names = append(names, name)
	}
	sort.Strings(names)
	var fallback provider.Provider
	var fallbackName string
	for _, name := range names {
		p, _ := routerInst.Registry().Get(name)
		if p == nil || !p.IsHealthy() {
			continue
		}
		if ms, ok := p.(audioModelSupporter); ok && ms.SupportsModel(model) {
			return p, name, nil
		}
		if fallback == nil {
			if up, ok := p.(provider.UpstreamConfigProvider); ok && len(up.APIKeys()) > 0 {
				fallback, fallbackName = p, name
			}
		}
	}
	if fallback != nil {
		return fallback, fallbackName, nil
	}
	return nil, "", fmt.Errorf("no healthy provider serves audio model %q", model)
}

// proxyAudioUpstream streams an upstream response back to the client verbatim.
func proxyAudioUpstream(w http.ResponseWriter, upstream *http.Response) {
	defer upstream.Body.Close()
	if ct := upstream.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(upstream.StatusCode)
	_, _ = io.Copy(w, upstream.Body)
}

// audioPreflight runs auth + gateway rate limiting for audio endpoints.
func (h *Handler) audioPreflight(w http.ResponseWriter, r *http.Request, bodyLimit int64) (string, *auth.KeyInfo, bool) {
	h.mu.RLock()
	keyStore := h.keyStore
	h.mu.RUnlock()

	apiKey := h.extractAPIKey(r)
	if apiKey == "" {
		h.sendError(w, http.StatusUnauthorized, "Missing API key in Authorization header", "authentication_error")
		return "", nil, false
	}
	keyInfo, ok := keyStore.Validate(apiKey)
	if !ok {
		h.sendError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error")
		return "", nil, false
	}
	if !keyInfo.CheckRateLimit() {
		middleware.TooManyRequestsResponse(w)
		return "", nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	return apiKey, keyInfo, true
}

// HandleAudioTranscriptions handles POST /v1/audio/transcriptions.
// The multipart body is forwarded byte-for-byte (auth header swapped), so any
// OpenAI-compatible STT server works — cloud or self-hosted.
func (h *Handler) HandleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	h.Stats.TotalRequests.Add(1)
	h.Stats.ActiveRequests.Add(1)
	defer h.Stats.ActiveRequests.Add(-1)

	apiKey, keyInfo, ok := h.audioPreflight(w, r, 25<<20) // 25MB audio cap
	if !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendError(w, http.StatusBadRequest, "Failed to read request body (max 25MB)", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	model, err := audioFormModel(r.Header.Get("Content-Type"), body)
	if err != nil || model == "" {
		h.sendError(w, http.StatusBadRequest, "audio 'model' field is required", "invalid_request_error")
		return
	}
	if !keyInfo.IsModelAllowed(model) {
		h.sendError(w, http.StatusForbidden,
			"Model '"+model+"' is not allowed for this API key", "permission_error")
		return
	}
	slog.Info(fmt.Sprintf("📥 POST /v1/audio/transcriptions | %s | %d bytes", model, len(body)))

	h.mu.RLock()
	limiter := h.limiter
	tracker := h.tracker
	h.mu.RUnlock()

	if !limiter.AcquireGlobalWithQueue() {
		middleware.QueueFullResponse(w)
		return
	}
	defer limiter.ReleaseGlobal()
	if !limiter.AcquireModel(model) {
		middleware.QueueFullResponse(w)
		return
	}
	defer limiter.ReleaseModel(model)

	p, provName, err := h.resolveAudioProvider(model)
	if err != nil {
		h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
		return
	}
	if tracker != nil && tracker.OverBudget(provName) {
		middleware.TooManyRequestsResponse(w)
		return
	}
	if !limiter.AcquireProvider(provName) {
		middleware.TooManyRequestsResponse(w)
		return
	}
	defer limiter.ReleaseProvider(provName)

	startTime := time.Now()
	upstream, fwdErr := forwardAudio(r.Context(), p, "/audio/transcriptions", body, r.Header.Get("Content-Type"))
	durationMs := time.Since(startTime).Milliseconds()
	if fwdErr != nil {
		slog.Error(fmt.Sprintf("❌ [AUDIO] transcriptions error: %v | model=%s | duration=%dms", fwdErr, model, durationMs))
		if tracker != nil {
			tracker.RecordError(provName, model)
		}
		if provider.IsLocalLimit(fwdErr) {
			middleware.TooManyRequestsResponse(w)
			return
		}
		h.sendError(w, http.StatusBadGateway, "Provider error: "+fwdErr.Error(), "upstream_error")
		return
	}
	if tracker != nil {
		tracker.RecordUsage(apiKey, provName, model, 0, 0, false)
	}
	proxyAudioUpstream(w, upstream)
}

// HandleAudioSpeech handles POST /v1/audio/speech (JSON in, audio bytes out).
func (h *Handler) HandleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	h.Stats.TotalRequests.Add(1)
	h.Stats.ActiveRequests.Add(1)
	defer h.Stats.ActiveRequests.Add(-1)

	apiKey, keyInfo, ok := h.audioPreflight(w, r, 10<<20) // 10MB JSON cap
	if !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendError(w, http.StatusBadRequest, "Failed to read request body (max 10MB)", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var speechReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &speechReq); err != nil || speechReq.Model == "" {
		h.sendError(w, http.StatusBadRequest, "audio 'model' is required", "invalid_request_error")
		return
	}
	model := speechReq.Model
	if !keyInfo.IsModelAllowed(model) {
		h.sendError(w, http.StatusForbidden,
			"Model '"+model+"' is not allowed for this API key", "permission_error")
		return
	}
	slog.Info(fmt.Sprintf("📥 POST /v1/audio/speech | %s", model))

	h.mu.RLock()
	limiter := h.limiter
	tracker := h.tracker
	h.mu.RUnlock()

	if !limiter.AcquireGlobalWithQueue() {
		middleware.QueueFullResponse(w)
		return
	}
	defer limiter.ReleaseGlobal()
	if !limiter.AcquireModel(model) {
		middleware.QueueFullResponse(w)
		return
	}
	defer limiter.ReleaseModel(model)

	p, provName, err := h.resolveAudioProvider(model)
	if err != nil {
		h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
		return
	}
	if tracker != nil && tracker.OverBudget(provName) {
		middleware.TooManyRequestsResponse(w)
		return
	}
	if !limiter.AcquireProvider(provName) {
		middleware.TooManyRequestsResponse(w)
		return
	}
	defer limiter.ReleaseProvider(provName)

	startTime := time.Now()
	upstream, fwdErr := forwardAudio(r.Context(), p, "/audio/speech", body, "application/json")
	durationMs := time.Since(startTime).Milliseconds()
	if fwdErr != nil {
		slog.Error(fmt.Sprintf("❌ [AUDIO] speech error: %v | model=%s | duration=%dms", fwdErr, model, durationMs))
		if tracker != nil {
			tracker.RecordError(provName, model)
		}
		if provider.IsLocalLimit(fwdErr) {
			middleware.TooManyRequestsResponse(w)
			return
		}
		h.sendError(w, http.StatusBadGateway, "Provider error: "+fwdErr.Error(), "upstream_error")
		return
	}
	if tracker != nil {
		tracker.RecordUsage(apiKey, provName, model, 0, 0, false)
	}
	slog.Info(fmt.Sprintf("📊 [AUDIO] %s | latency=%dms", model, durationMs))
	proxyAudioUpstream(w, upstream)
}

// audioFormModel extracts the "model" form field from a multipart body
// without disturbing the original bytes (which are forwarded verbatim).
func audioFormModel(contentType string, body []byte) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return "", fmt.Errorf("expected multipart body")
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", fmt.Errorf("model field not found")
}
