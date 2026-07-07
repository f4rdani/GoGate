package models

import "encoding/json"

// ==================== Anthropic Request ====================

// AnthropicRequest represents a request to the Anthropic Messages API.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []AnthropicMessage `json:"messages"`
	System        string             `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         json.RawMessage    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

// AnthropicMessage represents a message in the Anthropic format.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or array of content blocks
}

// ==================== Anthropic Response ====================

// AnthropicResponse represents a response from the Anthropic Messages API.
type AnthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []AnthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason *string                 `json:"stop_reason,omitempty"`
	Usage      AnthropicUsage          `json:"usage"`
}

// AnthropicContentBlock represents a content block in the response.
type AnthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// AnthropicUsage represents token usage in the Anthropic format.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ==================== Anthropic Streaming ====================

// AnthropicStreamEvent represents a streaming event from Anthropic.
type AnthropicStreamEvent struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index,omitempty"`
	Delta        json.RawMessage        `json:"delta,omitempty"`
	Message      *AnthropicResponse     `json:"message,omitempty"`
	ContentBlock *AnthropicContentBlock `json:"content_block,omitempty"`
	Usage        *AnthropicUsage        `json:"usage,omitempty"`
}

// AnthropicDeltaText represents a text delta in streaming.
type AnthropicDeltaText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// AnthropicMessageDelta represents a message delta in streaming.
type AnthropicMessageDelta struct {
	StopReason string `json:"stop_reason"`
}

// AnthropicInputJSONDelta represents an input JSON delta for tool use streaming.
type AnthropicInputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}
