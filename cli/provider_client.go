package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

	// Pick the first model and do a real 1-token completion test to enforce API key verification cheaply
	modelToTest := models[0]
	testErr := testAPIKeyConnection(baseURL, apiKey, modelToTest, providerType)
	if testErr != nil {
		return false, 0, fmt.Errorf("kunci API salah / tidak memiliki akses (chat test failed: %v)", testErr)
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

// testModel sends a minimal chat completion request to verify the model works.
// Returns (response_text, latency_ms, is_reasoning, error).
func testModel(baseURL, apiKey, modelID, providerType string) (string, int64, bool, error) {
	url := strings.TrimRight(baseURL, "/")

	// Anthropic uses different endpoint and format
	if providerType == "anthropic" {
		return testModelAnthropic(url, apiKey, modelID)
	}

	url += "/chat/completions"

	reqBody := map[string]interface{}{
		"model":    modelID,
		"messages": []map[string]string{{"role": "user", "content": "Say 'OK' and nothing else."}},
	}

	mLower := strings.ToLower(modelID)
	// Reasoning models require max_completion_tokens instead of max_tokens.
	// Some also support reasoning_effort parameter.
	isOpenAIReasoning := strings.Contains(mLower, "o1") ||
		strings.Contains(mLower, "o3") ||
		strings.Contains(mLower, "gpt-oss") ||
		strings.Contains(mLower, "qwen3") ||
		strings.Contains(mLower, "qwq") ||
		strings.Contains(mLower, "command-a") ||
		strings.Contains(mLower, "north-mini")

	if isOpenAIReasoning {
		reqBody["max_completion_tokens"] = 150
	} else {
		reqBody["max_tokens"] = 10
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, false, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", 0, false, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 60 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, false, fmt.Errorf("request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", latency, false, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", latency, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	// Parse OpenAI response dynamically (content can be a string or array of blocks)
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
		return "", latency, false, fmt.Errorf("parse response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "(empty response)", latency, isReasoningModelID(modelID), nil
	}

	msg := chatResp.Choices[0].Message
	var contentText string
	isReasoning := msg.ReasoningContent != "" || isReasoningModelID(modelID)

	if len(msg.ContentRaw) > 0 {
		// Try string unmarshal first
		var strVal string
		if err := json.Unmarshal(msg.ContentRaw, &strVal); err == nil {
			contentText = strVal
		} else {
			type ContentBlock struct {
				Type        string          `json:"type"`
				Text        string          `json:"text"`
				Thinking    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"thinking"`
				ThinkingRaw json.RawMessage `json:"thinking"`
			}

			var arrayVal []ContentBlock
			var stringArray []string

			if err := json.Unmarshal(msg.ContentRaw, &arrayVal); err == nil {
				var texts []string
				var thinkingTexts []string
				for _, block := range arrayVal {
					if block.Type == "thinking" {
						isReasoning = true
						if len(block.ThinkingRaw) > 0 {
							var thinkStr string
							if err := json.Unmarshal(block.ThinkingRaw, &thinkStr); err == nil {
								if thinkStr != "" {
									thinkingTexts = append(thinkingTexts, thinkStr)
								}
							}
						}
					}
					if len(block.Thinking) > 0 {
						isReasoning = true
						for _, tBlock := range block.Thinking {
							if tBlock.Text != "" {
								thinkingTexts = append(thinkingTexts, tBlock.Text)
							}
						}
					}
					if block.Text != "" {
						texts = append(texts, block.Text)
					}
				}
				contentText = strings.Join(texts, " ")
				if len(thinkingTexts) > 0 && contentText == "" {
					// Fallback to thinking text if no final text was generated yet
					contentText = "(thinking: " + strings.Join(thinkingTexts, " ") + ")"
				}
			} else if err := json.Unmarshal(msg.ContentRaw, &stringArray); err == nil {
				// Try array of strings format: ["..."]
				contentText = strings.Join(stringArray, " ")
			} else {
				// Fallback to raw string
				contentText = string(msg.ContentRaw)
			}
		}
	}

	// Double-check: if still empty, use the raw JSON string as fallback
	if contentText == "" && len(msg.ContentRaw) > 0 {
		raw := strings.TrimSpace(string(msg.ContentRaw))
		// Skip empty JSON strings like `""` or `null`
		if raw != `""` && raw != "null" {
			contentText = raw
		}
	}

	// If content is still empty, fallback to reasoning_content preview
	if contentText == "" && msg.ReasoningContent != "" {
		contentText = "(thinking: " + truncate(msg.ReasoningContent, 80) + ")"
		isReasoning = true
	}

	// Final fallback if everything is empty
	if contentText == "" {
		contentText = "(empty — model only produced internal reasoning)"
		isReasoning = true
	}

	// Format <think> tags if present
	formattedContent, hasThinkTags := formatThinkTags(contentText)
	if hasThinkTags {
		isReasoning = true
		contentText = formattedContent
	}

	return strings.TrimSpace(contentText), latency, isReasoning, nil
}

// testModelAnthropic handles testing for Anthropic's different API format.
// Returns (response_text, latency_ms, is_reasoning, error).
func testModelAnthropic(baseURL, apiKey, modelID string) (string, int64, bool, error) {
	url := baseURL + "/v1/messages"

	reqBody := map[string]interface{}{
		"model":    modelID,
		"messages": []map[string]string{{"role": "user", "content": "Say 'OK' and nothing else."}},
	}

	mLower := strings.ToLower(modelID)
	// Anthropic Claude 3.7 supports the thinking parameter to enable reasoning.
	if strings.Contains(mLower, "3-7") || strings.Contains(mLower, "3.7") {
		reqBody["max_tokens"] = 150
		reqBody["thinking"] = map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": 100,
		}
	} else {
		reqBody["max_tokens"] = 10
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, false, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", 0, false, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 60 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, false, fmt.Errorf("request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", latency, false, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", latency, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var anthResp struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		return "", latency, false, fmt.Errorf("parse response: %w", err)
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

	return responseText, latency, isReasoning, nil
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
