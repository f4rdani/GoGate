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

	url := strings.TrimRight(o.baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	keyObj, err := o.NextAPIKey()
	if err != nil {
		return nil, &ProviderError{StatusCode: 503, Body: err.Error(), Provider: o.name}
	}
	apiKey := keyObj.Key
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	keyMasked := "-"
	if len(apiKey) > 8 {
		keyMasked = apiKey[:8] + "..."
	} else if len(apiKey) > 0 {
		keyMasked = apiKey
	}
	slog.Info(fmt.Sprintf("ℹ️ [AUTH] Using %s key: %s", o.name, keyMasked))
	slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s", o.name, req.Model))

	resp, err := o.client.Do(httpReq)
	slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", o.name, req.Model))
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
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
		}
		return nil, provErr
	}

	var chatResp models.ChatCompletionResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &chatResp, nil
}

// ChatCompletionStream sends a streaming request and pipes SSE to the client.
// IMPORTANT: This checks upstream status BEFORE writing any headers,
// so fallback can work if upstream returns an error.
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

	url := strings.TrimRight(o.baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	keyObj, err := o.NextAPIKey()
	if err != nil {
		return &ProviderError{StatusCode: 503, Body: err.Error(), Provider: o.name}
	}
	apiKey := keyObj.Key
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	keyMasked := "-"
	if len(apiKey) > 8 {
		keyMasked = apiKey[:8] + "..."
	} else if len(apiKey) > 0 {
		keyMasked = apiKey
	}
	slog.Info(fmt.Sprintf("ℹ️ [AUTH] Using %s key: %s", o.name, keyMasked))
	slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s", o.name, req.Model))

	resp, err := o.client.Do(httpReq)
	slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", o.name, req.Model))
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Check status BEFORE writing to ResponseWriter (enables fallback)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		provErr := &ProviderError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			Provider:   o.name,
		}
		if provErr.IsRetryable() {
			keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
		}
		return provErr
	}

	// Set streaming headers and begin piping
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

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
