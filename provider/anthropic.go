package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aigateway/models"
)

// AnthropicProvider handles requests to the Anthropic Messages API
// with full request/response translation from/to OpenAI format.
type AnthropicProvider struct {
	*BaseProvider
}

// NewAnthropicProvider creates a new Anthropic provider.
func NewAnthropicProvider(base *BaseProvider) *AnthropicProvider {
	return &AnthropicProvider{BaseProvider: base}
}

// translateMessageContent translates OpenAI content parts to Anthropic content parts.
// Specifically handles "image_url" conversion to Anthropic's "image" block format.
func translateMessageContent(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}

	// 1. Try to unmarshal as string first (standard text message)
	var textStr string
	if err := json.Unmarshal(raw, &textStr); err == nil {
		return raw, nil // plain text string, no translation needed
	}

	// 2. Try to unmarshal as array of interfaces (multi-part content)
	var parts []interface{}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, nil // not an array, keep as is
	}

	translatedParts := make([]interface{}, 0, len(parts))
	for _, part := range parts {
		partMap, ok := part.(map[string]interface{})
		if !ok {
			translatedParts = append(translatedParts, part)
			continue
		}

		partType, _ := partMap["type"].(string)
		if partType == "image_url" {
			imgURLObj, _ := partMap["image_url"].(map[string]interface{})
			imgURL, _ := imgURLObj["url"].(string)

			if strings.HasPrefix(imgURL, "data:image/") {
				// Base64 Data URI: data:image/png;base64,iVBORw0KGgoAAAANS...
				commaIdx := strings.Index(imgURL, ",")
				if commaIdx != -1 {
					prefix := imgURL[:commaIdx]
					base64Data := imgURL[commaIdx+1:]
					
					// Extract media type from prefix (e.g. "data:image/png;base64")
					mediaType := "image/jpeg" // default fallback
					if semiIdx := strings.Index(prefix, ";"); semiIdx != -1 {
						if colonIdx := strings.Index(prefix, ":"); colonIdx != -1 {
							mediaType = prefix[colonIdx+1 : semiIdx]
						}
					}

					translatedParts = append(translatedParts, map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": mediaType,
							"data":       base64Data,
						},
					})
					continue
				}
			} else if strings.HasPrefix(imgURL, "http://") || strings.HasPrefix(imgURL, "https://") {
				// Public URL: validate safety to prevent SSRF
				if !isSafeURL(imgURL) {
					slog.Warn("SSRF protection blocked image URL fetch", "url", imgURL)
					continue
				}

				// Public URL: download and convert to base64
				client := &http.Client{Timeout: 10 * time.Second}
				resp, err := client.Get(imgURL)
				if err == nil {
					defer resp.Body.Close()
					// Limit reading to 5MB to prevent memory exhaustion (OOM)
					data, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
					if err == nil {
						mediaType := resp.Header.Get("Content-Type")
						if mediaType == "" {
							mediaType = "image/jpeg"
						}
						// Strip parameters like charset from Content-Type if present
						if idx := strings.Index(mediaType, ";"); idx != -1 {
							mediaType = strings.TrimSpace(mediaType[:idx])
						}

						base64Data := base64.StdEncoding.EncodeToString(data)
						translatedParts = append(translatedParts, map[string]interface{}{
							"type": "image",
							"source": map[string]interface{}{
								"type":       "base64",
								"media_type": mediaType,
								"data":       base64Data,
							},
						})
						continue
					}
				}
				slog.Warn("failed to download image URL for Anthropic vision translation", "url", imgURL, "error", err)
			}
		}

		// Keep any other parts (like type: "text") as they are
		translatedParts = append(translatedParts, part)
	}

	result, err := json.Marshal(translatedParts)
	if err != nil {
		return raw, err
	}
	return json.RawMessage(result), nil
}

// isSafeURL checks if a URL is safe to download from (prevents SSRF).
// It resolves the host's IP address and verifies it is not loopback, private, link-local, multicast, or unspecified.
func isSafeURL(urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	// Resolve IP addresses
	ips, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
			return false
		}
	}
	return true
}

// translateRequest converts an OpenAI-format request to Anthropic format.
func (a *AnthropicProvider) translateRequest(req *models.ChatCompletionRequest) (*models.AnthropicRequest, error) {
	anthReq := &models.AnthropicRequest{
		Model:  req.Model,
		Stream: req.Stream,
	}

	// Extract system messages and convert message format
	var systemParts []string
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemParts = append(systemParts, msg.ContentString())
		} else {
			translatedContent, err := translateMessageContent(msg.Content)
			if err != nil {
				slog.Warn("failed to translate message content for vision/multi-part", "error", err)
				translatedContent = msg.Content // fallback to raw
			}
			anthMsg := models.AnthropicMessage{
				Role:    msg.Role,
				Content: translatedContent,
			}
			anthReq.Messages = append(anthReq.Messages, anthMsg)
		}
	}

	if len(systemParts) > 0 {
		anthReq.System = strings.Join(systemParts, "\n\n")
	}

	// max_tokens is REQUIRED by Anthropic (default 4096)
	if req.MaxTokens != nil {
		anthReq.MaxTokens = *req.MaxTokens
	} else {
		anthReq.MaxTokens = 4096
	}

	anthReq.Temperature = req.Temperature
	anthReq.TopP = req.TopP

	// Convert "stop" field to "stop_sequences"
	if len(req.Stop) > 0 {
		var stop interface{}
		if err := json.Unmarshal(req.Stop, &stop); err == nil {
			switch v := stop.(type) {
			case string:
				anthReq.StopSequences = []string{v}
			case []interface{}:
				for _, s := range v {
					if str, ok := s.(string); ok {
						anthReq.StopSequences = append(anthReq.StopSequences, str)
					}
				}
			}
		}
	}

	// Pass through tools if present
	anthReq.Tools = req.Tools
	anthReq.ToolChoice = req.ToolChoice

	return anthReq, nil
}

// translateResponse converts an Anthropic response to OpenAI format.
func (a *AnthropicProvider) translateResponse(anthResp *models.AnthropicResponse) *models.ChatCompletionResponse {
	// Extract text content and tool_use blocks from content blocks
	var contentParts []string
	var toolCalls []map[string]interface{}

	for _, block := range anthResp.Content {
		switch block.Type {
		case "text":
			contentParts = append(contentParts, block.Text)
		case "tool_use":
			// Convert Anthropic tool_use to OpenAI tool_calls format
			tc := map[string]interface{}{
				"id":   block.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      block.Name,
					"arguments": string(block.Input),
				},
			}
			toolCalls = append(toolCalls, tc)
		}
	}

	// Build content — empty string if only tool calls, nil content if no text
	var contentJSON json.RawMessage
	if len(contentParts) > 0 {
		content, _ := json.Marshal(strings.Join(contentParts, ""))
		contentJSON = content
	} else if len(toolCalls) == 0 {
		contentJSON = json.RawMessage(`""`) 
	} else {
		contentJSON = json.RawMessage(`null`) // content is null when only tool_calls
	}

	// Serialize tool_calls to json.RawMessage
	var toolCallsJSON json.RawMessage
	if len(toolCalls) > 0 {
		toolCallsJSON, _ = json.Marshal(toolCalls)
	}

	// Map Anthropic stop_reason to OpenAI finish_reason
	var finishReason string
	if anthResp.StopReason != nil {
		switch *anthResp.StopReason {
		case "end_turn":
			finishReason = "stop"
		case "max_tokens":
			finishReason = "length"
		case "tool_use":
			finishReason = "tool_calls"
		default:
			finishReason = "stop"
		}
	}

	return &models.ChatCompletionResponse{
		ID:      anthResp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   anthResp.Model,
		Choices: []models.Choice{
			{
				Index: 0,
				Message: &models.Message{
					Role:      "assistant",
					Content:   contentJSON,
					ToolCalls: toolCallsJSON,
				},
				FinishReason: &finishReason,
			},
		},
		Usage: &models.Usage{
			PromptTokens:     anthResp.Usage.InputTokens,
			CompletionTokens: anthResp.Usage.OutputTokens,
			TotalTokens:      anthResp.Usage.InputTokens + anthResp.Usage.OutputTokens,
		},
	}
}

// ChatCompletion sends a non-streaming request to Anthropic.
func (a *AnthropicProvider) ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	anthReq, err := a.translateRequest(req)
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
	}
	anthReq.Stream = false

	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(a.baseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	keyObj, err := a.NextAPIKey()
	if err != nil {
		return nil, &ProviderError{StatusCode: 503, Body: err.Error(), Provider: a.name}
	}
	apiKey := keyObj.Key
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	keyMasked := "-"
	if len(apiKey) > 8 {
		keyMasked = apiKey[:8] + "..."
	} else if len(apiKey) > 0 {
		keyMasked = apiKey
	}
	slog.Info(fmt.Sprintf("ℹ️ [AUTH] Using %s key: %s", a.name, keyMasked))
	slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s", a.name, req.Model))

	resp, err := a.client.Do(httpReq)
	slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", a.name, req.Model))
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
			Provider:   a.name,
		}
		if provErr.IsRetryable() {
			keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
		}
		return nil, provErr
	}

	var anthResp models.AnthropicResponse
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return a.translateResponse(&anthResp), nil
}

// ChatCompletionStream sends a streaming request to Anthropic and translates
// Anthropic SSE events to OpenAI SSE format on-the-fly.
func (a *AnthropicProvider) ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	anthReq, err := a.translateRequest(req)
	if err != nil {
		return fmt.Errorf("translate request: %w", err)
	}
	anthReq.Stream = true

	body, err := json.Marshal(anthReq)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(a.baseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	keyObj, err := a.NextAPIKey()
	if err != nil {
		return &ProviderError{StatusCode: 503, Body: err.Error(), Provider: a.name}
	}
	apiKey := keyObj.Key
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	keyMasked := "-"
	if len(apiKey) > 8 {
		keyMasked = apiKey[:8] + "..."
	} else if len(apiKey) > 0 {
		keyMasked = apiKey
	}
	slog.Info(fmt.Sprintf("ℹ️ [AUTH] Using %s key: %s", a.name, keyMasked))
	slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s", a.name, req.Model))

	resp, err := a.client.Do(httpReq)
	slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", a.name, req.Model))
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Check status BEFORE writing to ResponseWriter
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		provErr := &ProviderError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			Provider:   a.name,
		}
		if provErr.IsRetryable() {
			keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
		}
		return provErr
	}

	// Set streaming headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Translate Anthropic SSE → OpenAI SSE
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var msgID string
	var model string
	var toolCallIndex int  // tracks the current tool_call index for OpenAI format
	var currentIsToolUse bool // true if the current content_block is a tool_use

	// writeSSE is a helper that writes an SSE data line and flushes.
	// Returns an error if the client disconnected (write failed).
	writeSSE := func(format string, args ...interface{}) error {
		_, err := fmt.Fprintf(w, format, args...)
		if err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// writeToolCallDelta is a helper to emit an OpenAI streaming tool_calls chunk.
	writeToolCallDelta := func(toolCallJSON map[string]interface{}) error {
		toolCallsArr, _ := json.Marshal([]map[string]interface{}{toolCallJSON})
		chunk := models.ChatCompletionChunk{
			ID:      msgID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []models.StreamChoice{
				{Index: 0, Delta: &models.StreamDelta{ToolCalls: toolCallsArr}},
			},
		}
		chunkJSON, _ := json.Marshal(chunk)
		return writeSSE("data: %s\n\n", chunkJSON)
	}

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			if err := writeSSE("data: [DONE]\n\n"); err != nil {
				return nil // client disconnected, stop gracefully
			}
			break
		}

		var event models.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				msgID = event.Message.ID
				model = event.Message.Model
			}
			toolCallIndex = 0
			// Send initial role chunk
			chunk := models.ChatCompletionChunk{
				ID:      msgID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []models.StreamChoice{
					{Index: 0, Delta: &models.StreamDelta{Role: "assistant"}},
				},
			}
			chunkJSON, _ := json.Marshal(chunk)
			if err := writeSSE("data: %s\n\n", chunkJSON); err != nil {
				return nil
			}

		case "content_block_start":
			// Handle tool_use content block start — emit initial tool_calls chunk with id/name
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				currentIsToolUse = true
				tc := map[string]interface{}{
					"index": toolCallIndex,
					"id":    event.ContentBlock.ID,
					"type":  "function",
					"function": map[string]interface{}{
						"name":      event.ContentBlock.Name,
						"arguments": "",
					},
				}
				if err := writeToolCallDelta(tc); err != nil {
					return nil
				}
			}

		case "content_block_delta":
			var deltaType struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Delta, &deltaType); err != nil {
				continue
			}

			switch deltaType.Type {
			case "text_delta":
				var textDelta models.AnthropicDeltaText
				if err := json.Unmarshal(event.Delta, &textDelta); err != nil {
					continue
				}
				if textDelta.Text != "" {
					chunk := models.ChatCompletionChunk{
						ID:      msgID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   model,
						Choices: []models.StreamChoice{
							{Index: 0, Delta: &models.StreamDelta{Content: textDelta.Text}},
						},
					}
					chunkJSON, _ := json.Marshal(chunk)
					if err := writeSSE("data: %s\n\n", chunkJSON); err != nil {
						return nil
					}
				}

			case "input_json_delta":
				// Tool arguments streaming — emit partial JSON as function.arguments delta
				var inputDelta models.AnthropicInputJSONDelta
				if err := json.Unmarshal(event.Delta, &inputDelta); err != nil {
					continue
				}
				if inputDelta.PartialJSON != "" {
					tc := map[string]interface{}{
						"index": toolCallIndex,
						"function": map[string]interface{}{
							"arguments": inputDelta.PartialJSON,
						},
					}
					if err := writeToolCallDelta(tc); err != nil {
						return nil
					}
				}
			}

		case "content_block_stop":
			// A content block ended — advance tool call index if this was a tool_use block
			if currentIsToolUse {
				toolCallIndex++
				currentIsToolUse = false
			}

		case "message_delta":
			var msgDelta models.AnthropicMessageDelta
			if err := json.Unmarshal(event.Delta, &msgDelta); err != nil {
				continue
			}

			finishReason := "stop"
			if msgDelta.StopReason == "max_tokens" {
				finishReason = "length"
			} else if msgDelta.StopReason == "tool_use" {
				finishReason = "tool_calls"
			}

			chunk := models.ChatCompletionChunk{
				ID:      msgID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []models.StreamChoice{
					{Index: 0, Delta: &models.StreamDelta{}, FinishReason: &finishReason},
				},
			}
			chunkJSON, _ := json.Marshal(chunk)
			if err := writeSSE("data: %s\n\n", chunkJSON); err != nil {
				return nil
			}

		case "message_stop":
			if err := writeSSE("data: [DONE]\n\n"); err != nil {
				return nil
			}
		}
	}

	return scanner.Err()
}

// Embeddings is not supported by Anthropic.
func (a *AnthropicProvider) Embeddings(ctx context.Context, req *models.EmbeddingsRequest) (*models.EmbeddingsResponse, error) {
	return nil, fmt.Errorf("embeddings not supported by Anthropic")
}
