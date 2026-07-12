package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ProviderModel represents a model returned by a provider's API.
type ProviderModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// fetchModels calls the provider's /models endpoint to get available models.
// Returns model IDs sorted by name.
func fetchModels(baseURL, apiKey, providerType string) ([]string, error) {
	// Anthropic doesn't have a public models endpoint
	if providerType == "anthropic" {
		return nil, fmt.Errorf("Anthropic tidak punya endpoint /models — tambah model manual")
	}

	// Normalize URL
	url := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(url, "/models") {
		url += "/models"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Auth header — most use Bearer, Anthropic uses x-api-key (but we skip Anthropic)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request gagal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Parse response — handle both {data: [...]} and [...] formats
	var models []string

	// Try OpenAI format first: {"object": "list", "data": [...]}
	var listResp struct {
		Data []ProviderModel `json:"data"`
	}
	if err := json.Unmarshal(body, &listResp); err == nil && len(listResp.Data) > 0 {
		for _, m := range listResp.Data {
			models = append(models, m.ID)
		}
	} else {
		// Try direct array format
		var arrResp []ProviderModel
		if err := json.Unmarshal(body, &arrResp); err == nil && len(arrResp) > 0 {
			for _, m := range arrResp {
				models = append(models, m.ID)
			}
		}
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("format response tidak dikenali atau model kosong: %s", truncate(string(body), 200))
	}

	// Deduplicate by ID — beberapa provider (mis. Mistral) mengembalikan
	// ID yang sama lebih dari sekali (model alias)
	seen := make(map[string]bool, len(models))
	var uniq []string
	for _, id := range models {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	return uniq, nil
}

// isChatModel returns true if the model ID is likely to support chat completions.
// Excludes audio, embedding, image generation, and moderation models.
func isChatModel(modelID string) bool {
	m := strings.ToLower(modelID)
	// Exclude known non-chat models
	if strings.Contains(m, "embed") ||
		strings.Contains(m, "whisper") ||
		strings.Contains(m, "tts") ||
		strings.Contains(m, "dall-e") ||
		strings.Contains(m, "moderation") ||
		strings.Contains(m, "audio") {
		return false
	}
	return true
}

// testAPIKey verifies if an API key is valid by calling the /models endpoint AND performing a 1-token test completion call.
// Returns (ok, modelCount, error).
func testAPIKey(baseURL, apiKey, providerType string) (bool, int, error) {
	// Anthropic special case (no /models endpoint)
	if providerType == "anthropic" {
		err := testAPIKeyConnection(baseURL, apiKey, "claude-3-5-haiku-20241022", "anthropic")
		if err != nil {
			return false, 0, err
		}
		return true, 1, nil
	}

	models, err := fetchModels(baseURL, apiKey, providerType)
	if err != nil {
		return false, 0, err
	}

	if len(models) == 0 {
		return false, 0, fmt.Errorf("provider tidak mengembalikan model apa pun")
	}

	// Pick the first chat-compatible model to enforce API key verification cheaply
	var modelToTest string
	for _, m := range models {
		if isChatModel(m) {
			modelToTest = m
			break
		}
	}
	if modelToTest == "" {
		// Fallback to the first model if no model is explicitly detected as chat
		modelToTest = models[0]
	}

	testErr := testAPIKeyConnection(baseURL, apiKey, modelToTest, providerType)
	if testErr != nil {
		return false, 0, fmt.Errorf("kunci API salah / tidak memiliki akses (chat test model '%s' failed: %v)", modelToTest, testErr)
	}

	return true, len(models), nil
}

// isReasoningModelID checks if the model ID matches common reasoning keywords.
func isReasoningModelID(modelID string) bool {
	m := strings.ToLower(modelID)
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

// isVisionModelID checks if the model ID matches common vision keywords.
func isVisionModelID(modelID string) bool {
	m := strings.ToLower(modelID)
	return strings.Contains(m, "vision") ||
		strings.Contains(m, "pixtral") ||
		strings.Contains(m, "vl") || // like Llama-3.2-11B-Vision-Instruct or Qwen2-VL
		strings.Contains(m, "gpt-4o") || // gpt-4o and gpt-4o-mini support vision
		strings.Contains(m, "claude-3-5-sonnet") ||
		strings.Contains(m, "claude-3-5-haiku") ||
		strings.Contains(m, "claude-3-opus") ||
		strings.Contains(m, "claude-3-sonnet") ||
		strings.Contains(m, "gemini-1.5") ||
		strings.Contains(m, "gemini-2.5") ||
		strings.Contains(m, "gemini-3.5") ||
		strings.Contains(m, "gemini-flash")
}

// testModel sends a minimal chat completion request to verify the model works.
// If the model supports vision, it will perform a vision test.
// Returns (response_text, latency_ms, is_reasoning, is_vision, error).
func testModel(baseURL, apiKey, modelID, providerType string) (string, int64, bool, bool, error) {
	url := strings.TrimRight(baseURL, "/")

	// Anthropic uses different endpoint and format
	if providerType == "anthropic" {
		return testModelAnthropic(url, apiKey, modelID)
	}

	url += "/chat/completions"

	isVision := isVisionModelID(modelID)
	response, latency, isReasoning, successVision, err := runActualModelTest(url, apiKey, modelID, providerType, isVision)
	if err != nil && isVision {
		slog.Warn("vision test failed, falling back to text test", "model", modelID, "error", err)
		return runActualModelTest(url, apiKey, modelID, providerType, false)
	}
	return response, latency, isReasoning, successVision, err
}

func runActualModelTest(url, apiKey, modelID, providerType string, tryVision bool) (string, int64, bool, bool, error) {
	var reqBody map[string]interface{}

	if tryVision {
		reqBody = map[string]interface{}{
			"model": modelID,
			"messages": []map[string]interface{}{
				{
					"role": "user",
					"content": []map[string]interface{}{
						{"type": "text", "text": "Say 'OK'"},
						{
							"type": "image_url",
							"image_url": map[string]string{
								"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
							},
						},
					},
				},
			},
			"max_tokens": 10,
		}
	} else {
		reqBody = map[string]interface{}{
			"model":    modelID,
			"messages": []map[string]string{{"role": "user", "content": "Say 'OK' and nothing else."}},
		}
		mLower := strings.ToLower(modelID)
		if strings.Contains(mLower, "o1") || strings.Contains(mLower, "o3") ||
			strings.Contains(mLower, "gpt-oss") || strings.Contains(mLower, "qwen3") ||
			strings.Contains(mLower, "qwq") || strings.Contains(mLower, "command-a") ||
			strings.Contains(mLower, "north-mini") {
			reqBody["max_completion_tokens"] = 150
		} else {
			reqBody["max_tokens"] = 10
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, false, false, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", 0, false, false, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 60 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, false, false, fmt.Errorf("request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", latency, false, false, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", latency, false, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	// Parse OpenAI response dynamically
	type OpenAIMessage struct {
		ContentRaw       json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
	}
	var chatResp struct {
		Choices []struct {
			Message OpenAIMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", latency, false, false, fmt.Errorf("parse response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "(empty response)", latency, isReasoningModelID(modelID), tryVision, nil
	}

	msg := chatResp.Choices[0].Message
	var contentText string
	isReasoning := msg.ReasoningContent != "" || isReasoningModelID(modelID)

	if len(msg.ContentRaw) > 0 {
		var strVal string
		if err := json.Unmarshal(msg.ContentRaw, &strVal); err == nil {
			contentText = strVal
		} else {
			type ContentBlock struct {
				Type        string          `json:"type"`
				Text        string          `json:"text"`
				ThinkingRaw json.RawMessage `json:"thinking"`
			}
			var arrayVal []ContentBlock
			if err := json.Unmarshal(msg.ContentRaw, &arrayVal); err == nil {
				var texts []string
				var thinkingTexts []string
				for _, block := range arrayVal {
					if block.Type == "thinking" {
						isReasoning = true
						if len(block.ThinkingRaw) > 0 {
							var thinkStr string
							if err := json.Unmarshal(block.ThinkingRaw, &thinkStr); err == nil && thinkStr != "" {
								thinkingTexts = append(thinkingTexts, thinkStr)
							}
						}
					}
					if block.Text != "" {
						texts = append(texts, block.Text)
					}
				}
				contentText = strings.Join(texts, " ")
				if len(thinkingTexts) > 0 && contentText == "" {
					contentText = "(thinking: " + strings.Join(thinkingTexts, " ") + ")"
				}
			} else {
				contentText = string(msg.ContentRaw)
			}
		}
	}

	if contentText == "" && msg.ReasoningContent != "" {
		contentText = "(thinking: " + truncate(msg.ReasoningContent, 80) + ")"
		isReasoning = true
	}

	if contentText == "" {
		contentText = "(empty response)"
	}

	formattedContent, hasThinkTags := formatThinkTags(contentText)
	if hasThinkTags {
		isReasoning = true
		contentText = formattedContent
	}

	return strings.TrimSpace(contentText), latency, isReasoning, tryVision, nil
}

// testModelAnthropic handles testing for Anthropic's different API format.
func testModelAnthropic(baseURL, apiKey, modelID string) (string, int64, bool, bool, error) {
	url := baseURL + "/v1/messages"
	isVision := isVisionModelID(modelID)
	response, latency, isReasoning, successVision, err := runActualAnthropicTest(url, apiKey, modelID, isVision)
	if err != nil && isVision {
		slog.Warn("anthropic vision test failed, falling back to text test", "model", modelID, "error", err)
		return runActualAnthropicTest(url, apiKey, modelID, false)
	}
	return response, latency, isReasoning, successVision, err
}

func runActualAnthropicTest(url, apiKey, modelID string, tryVision bool) (string, int64, bool, bool, error) {
	reqBody := map[string]interface{}{
		"model": modelID,
	}

	if tryVision {
		reqBody["messages"] = []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Say 'OK'"},
					{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
						},
					},
				},
			},
		}
		reqBody["max_tokens"] = 10
	} else {
		reqBody["messages"] = []map[string]string{{"role": "user", "content": "Say 'OK' and nothing else."}}
		mLower := strings.ToLower(modelID)
		if strings.Contains(mLower, "3-7") || strings.Contains(mLower, "3.7") {
			reqBody["max_tokens"] = 150
			reqBody["thinking"] = map[string]interface{}{
				"type":          "enabled",
				"budget_tokens": 100,
			}
		} else {
			reqBody["max_tokens"] = 10
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, false, false, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", 0, false, false, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 60 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, false, false, fmt.Errorf("request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", latency, false, false, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", latency, false, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var anthResp struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		return "", latency, false, false, fmt.Errorf("parse response: %w", err)
	}

	isReasoning := isReasoningModelID(modelID)
	var responseText string

	for _, c := range anthResp.Content {
		if c.Type == "thinking" || c.Thinking != "" {
			isReasoning = true
		}
		if c.Type == "text" {
			responseText = strings.TrimSpace(c.Text)
		}
	}

	if responseText == "" {
		responseText = "(empty response)"
	}

	// Format <think> tags if present
	formattedContent, hasThinkTags := formatThinkTags(responseText)
	if hasThinkTags {
		isReasoning = true
		responseText = formattedContent
	}

	return responseText, latency, isReasoning, tryVision, nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// testAPIKeyConnection sends a super lightweight 1-token request to verify key authentication cheaply.
func testAPIKeyConnection(baseURL, apiKey, modelID, providerType string) error {
	url := strings.TrimRight(baseURL, "/")

	if providerType == "anthropic" {
		url += "/v1/messages"
		reqBody := map[string]interface{}{
			"model":      modelID,
			"messages":   []map[string]string{{"role": "user", "content": "y"}},
			"max_tokens": 1,
		}
		body, _ := json.Marshal(reqBody)
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			respBody, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		return nil
	}

	// OpenAI-compatible
	url += "/chat/completions"
	reqBody := map[string]interface{}{
		"model":      modelID,
		"messages":   []map[string]string{{"role": "user", "content": "y"}},
		"max_tokens": 1,
	}
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// formatThinkTags converts <think>...</think> XML tags inside text into (thinking: ...) format.
// It handles both complete blocks and cut-off blocks.
func formatThinkTags(text string) (string, bool) {
	if strings.Contains(text, "<think>") || strings.Contains(text, "</think>") {
		cleaned := text
		if strings.Contains(cleaned, "<think>") {
			cleaned = strings.Replace(cleaned, "<think>", "(thinking: ", 1)
			if strings.Contains(cleaned, "</think>") {
				cleaned = strings.Replace(cleaned, "</think>", ")", 1)
			} else {
				cleaned = cleaned + ")"
			}
		} else if strings.Contains(cleaned, "</think>") {
			cleaned = "(thinking: " + strings.Replace(cleaned, "</think>", ")", 1)
		}
		return cleaned, true
	}
	return text, false
}
