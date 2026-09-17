package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aigateway/models"
)

// OpenAIProvider handles requests to OpenAI and OpenAI-compatible APIs.
type OpenAIProvider struct {
	*BaseProvider
}

// NewOpenAIProvider creates a new OpenAI provider.
func NewOpenAIProvider(base *BaseProvider) *OpenAIProvider {
	return &OpenAIProvider{BaseProvider: base}
}

// ChatCompletion sends a non-streaming request to the OpenAI API.
// Tries each available API key in turn: on a retryable error (e.g. 429) the
// failed key is circuit-broken and the next key is attempted immediately.
func (o *OpenAIProvider) ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	req.Stream = false

	// Automatically prepend "models/" for Google Gemini OpenAI-compatible endpoint if missing
	if strings.Contains(o.baseURL, "generativelanguage.googleapis.com") {
		if !strings.HasPrefix(req.Model, "models/") {
			req.Model = "models/" + req.Model
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < o.keyAttempts(); attempt++ {
		keyObj, err := o.NextAPIKey()
		if err != nil {
			// All keys are circuit-broken — surface the last real upstream
			// error (flagged exhausted) instead of a generic 503.
			if lastErr != nil {
				return nil, markKeyExhausted(lastErr)
			}
			return nil, &ProviderError{StatusCode: 503, Body: err.Error(), Provider: o.name}
		}

		apiKey, targetURL := o.ResolveKeyAndURL(keyObj.Key, "/chat/completions")

		egressProxy := o.checkoutEgress()
		attemptCtx := ctx
		if egressProxy != "" {
			attemptCtx = WithEgressProxy(ctx, egressProxy)
		}
		httpReq, err := http.NewRequestWithContext(attemptCtx, "POST", targetURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if o.RelaySecret() != "" {
			httpReq.Header.Set("X-Relay-Secret", o.RelaySecret())
		}
		if o.RelayURL() != "" && o.baseURL != "" {
			httpReq.Header.Set("X-Target-URL", o.baseURL)
		}
		if o.providerType == "opencode" {
			sessionID := fmt.Sprintf("ses_%d", time.Now().UnixNano())
			httpReq.Header.Set("x-opencode-session", sessionID)
			httpReq.Header.Set("X-Session-ID", sessionID)
		}
		if o.providerType == "mimo" {
			httpReq.Header.Set("X-Mimo-Source", "mimocode-cli")
		}
		if tok, err := o.oauthBearer(ctx, ""); err != nil {
			return nil, fmt.Errorf("oauth token: %w", err)
		} else if tok != "" {
			httpReq.Header.Set("Authorization", "Bearer "+tok)
		}

		keyMasked := "-"
		if len(apiKey) > 8 {
			keyMasked = apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
		} else if len(apiKey) > 0 {
			keyMasked = "****"
		}
		slog.Debug(fmt.Sprintf("ℹ️ [AUTH] Using %s key: %s (attempt %d)", o.name, keyMasked, attempt+1))
		slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s", o.name, req.Model))

		resp, err := o.client.Do(httpReq)
		slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", o.name, req.Model))
		if err != nil {
			o.reportEgress(egressProxy, true)
			// A dead egress proxy must not sink the request while other
			// keys (likely via other proxies) are still untried.
			if egressProxy != "" && ctx.Err() == nil {
				lastErr = fmt.Errorf("do request: %w", err)
				continue
			}
			return nil, fmt.Errorf("do request: %w", err)
		}
		o.reportEgress(egressProxy, false)

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			provErr := &ProviderError{
				StatusCode: resp.StatusCode,
				Body:       string(respBody),
				Provider:   o.name,
			}
			if provErr.IsRetryable() {
				keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
				lastErr = provErr
				continue
			}
			return nil, provErr
		}

		var chatResp models.ChatCompletionResponse
		if err := json.Unmarshal(respBody, &chatResp); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		return &chatResp, nil
	}

	if lastErr != nil {
		return nil, markKeyExhausted(lastErr)
	}
	return nil, &ProviderError{StatusCode: 503, Body: "no API keys available for provider " + o.name, Provider: o.name}
}

// ChatCompletionStream sends a streaming request and pipes SSE to the client.
// IMPORTANT: This checks upstream status BEFORE writing any headers,
// so fallback can work if upstream returns an error.
// Tries each available API key in turn on retryable errors.
func (o *OpenAIProvider) ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	req.Stream = true

	// Automatically prepend "models/" for Google Gemini OpenAI-compatible endpoint if missing
	if strings.Contains(o.baseURL, "generativelanguage.googleapis.com") {
		if !strings.HasPrefix(req.Model, "models/") {
			req.Model = "models/" + req.Model
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	var resp *http.Response
	for attempt := 0; attempt < o.keyAttempts(); attempt++ {
		keyObj, keyErr := o.NextAPIKey()
		if keyErr != nil {
			if lastErr != nil {
				return markKeyExhausted(lastErr)
			}
			return &ProviderError{StatusCode: 503, Body: keyErr.Error(), Provider: o.name}
		}

		apiKey, targetURL := o.ResolveKeyAndURL(keyObj.Key, "/chat/completions")

		egressProxy := o.checkoutEgress()
		attemptCtx := ctx
		if egressProxy != "" {
			attemptCtx = WithEgressProxy(ctx, egressProxy)
		}
		httpReq, err := http.NewRequestWithContext(attemptCtx, "POST", targetURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if o.RelaySecret() != "" {
			httpReq.Header.Set("X-Relay-Secret", o.RelaySecret())
		}
		if o.RelayURL() != "" && o.baseURL != "" {
			httpReq.Header.Set("X-Target-URL", o.baseURL)
		}
		if o.providerType == "opencode" {
			sessionID := fmt.Sprintf("ses_%d", time.Now().UnixNano())
			httpReq.Header.Set("x-opencode-session", sessionID)
			httpReq.Header.Set("X-Session-ID", sessionID)
		}
		if o.providerType == "mimo" {
			httpReq.Header.Set("X-Mimo-Source", "mimocode-cli")
		}
		if tok, err := o.oauthBearer(ctx, ""); err != nil {
			return fmt.Errorf("oauth token: %w", err)
		} else if tok != "" {
			httpReq.Header.Set("Authorization", "Bearer "+tok)
		}

		keyMasked := "-"
		if len(apiKey) > 8 {
			keyMasked = apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
		} else if len(apiKey) > 0 {
			keyMasked = "****"
		}
		slog.Debug(fmt.Sprintf("ℹ️ [AUTH] Using %s key: %s (attempt %d)", o.name, keyMasked, attempt+1))
		slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s", o.name, req.Model))

		resp, err = o.client.Do(httpReq)
		slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", o.name, req.Model))
		if err != nil {
			resp = nil
			o.reportEgress(egressProxy, true)
			if egressProxy != "" && ctx.Err() == nil {
				lastErr = fmt.Errorf("do request: %w", err)
				continue
			}
			return fmt.Errorf("do request: %w", err)
		}
		o.reportEgress(egressProxy, false)

		// Check status BEFORE writing to ResponseWriter (enables fallback)
		if resp.StatusCode != http.StatusOK {
			status := resp.StatusCode
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			resp = nil
			provErr := &ProviderError{
				StatusCode: status,
				Body:       string(respBody),
				Provider:   o.name,
			}
			if provErr.IsRetryable() {
				keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
				lastErr = provErr
				continue
			}
			return provErr
		}
		break
	}
	if resp == nil {
		if lastErr != nil {
			return markKeyExhausted(lastErr)
		}
		return &ProviderError{StatusCode: 503, Body: "no API keys available for provider " + o.name, Provider: o.name}
	}

	// Set streaming headers and begin piping
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	defer resp.Body.Close()

	// Pipe upstream SSE directly to client (zero translation needed)
	bufPtr := streamBufPool.Get().(*[]byte)
	buf := *bufPtr
	defer streamBufPool.Put(bufPtr)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	return nil
}

// Embeddings sends a non-streaming embeddings request.
func (o *OpenAIProvider) Embeddings(ctx context.Context, req *models.EmbeddingsRequest) (*models.EmbeddingsResponse, error) {
	// Automatically prepend "models/" for Google Gemini OpenAI-compatible endpoint if missing
	if strings.Contains(o.baseURL, "generativelanguage.googleapis.com") {
		if !strings.HasPrefix(req.Model, "models/") {
			req.Model = "models/" + req.Model
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < o.keyAttempts(); attempt++ {
		keyObj, err := o.NextAPIKey()
		if err != nil {
			if lastErr != nil {
				return nil, markKeyExhausted(lastErr)
			}
			return nil, &ProviderError{StatusCode: 503, Body: err.Error(), Provider: o.name}
		}

		apiKey, targetURL := o.ResolveKeyAndURL(keyObj.Key, "/embeddings")

		egressProxy := o.checkoutEgress()
		attemptCtx := ctx
		if egressProxy != "" {
			attemptCtx = WithEgressProxy(ctx, egressProxy)
		}
		httpReq, err := http.NewRequestWithContext(attemptCtx, "POST", targetURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if o.RelaySecret() != "" {
			httpReq.Header.Set("X-Relay-Secret", o.RelaySecret())
		}
		if o.RelayURL() != "" && o.baseURL != "" {
			httpReq.Header.Set("X-Target-URL", o.baseURL)
		}
		if o.providerType == "mimo" {
			httpReq.Header.Set("X-Mimo-Source", "mimocode-cli")
		}
		if tok, err := o.oauthBearer(ctx, ""); err != nil {
			return nil, fmt.Errorf("oauth token: %w", err)
		} else if tok != "" {
			httpReq.Header.Set("Authorization", "Bearer "+tok)
		}

		keyMasked := "-"
		if len(apiKey) > 8 {
			keyMasked = apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
		} else if len(apiKey) > 0 {
			keyMasked = "****"
		}
		slog.Debug(fmt.Sprintf("ℹ️ [AUTH] Using %s key: %s (attempt %d)", o.name, keyMasked, attempt+1))
		slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s", o.name, req.Model))

		resp, err := o.client.Do(httpReq)
		slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", o.name, req.Model))
		if err != nil {
			o.reportEgress(egressProxy, true)
			if egressProxy != "" && ctx.Err() == nil {
				lastErr = fmt.Errorf("do request: %w", err)
				continue
			}
			return nil, fmt.Errorf("do request: %w", err)
		}
		o.reportEgress(egressProxy, false)

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			provErr := &ProviderError{
				StatusCode: resp.StatusCode,
				Body:       string(respBody),
				Provider:   o.name,
			}
			if provErr.IsRetryable() {
				keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
				lastErr = provErr
				continue
			}
			return nil, provErr
		}

		var embedResp models.EmbeddingsResponse
		if err := json.Unmarshal(respBody, &embedResp); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		return &embedResp, nil
	}

	if lastErr != nil {
		return nil, markKeyExhausted(lastErr)
	}
	return nil, &ProviderError{StatusCode: 503, Body: "no API keys available for provider " + o.name, Provider: o.name}
}
