package models

import "encoding/json"

// ==================== OpenAI Responses API Types ====================
// Minimal subset of the Responses API (enough for Codex-style clients):
// text in/out, function tools, and SSE streaming. Conversational state
// (previous_response_id) is intentionally stateless — each call stands alone.

// ResponsesRequest is a POST /v1/responses payload.
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"` // string or []ResponseInputItem
	Instructions       string          `json:"instructions,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

// ResponseInputItem is one element of an array-form input.
type ResponseInputItem struct {
	Type    string          `json:"type"` // message | function_call_output | reasoning | ...
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
}

// ResponseContentPart is one content part inside a message item.
type ResponseContentPart struct {
	Type     string `json:"type"` // input_text | text | output_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// ResponseTool is a function tool definition in Responses shape.
type ResponseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ResponseOutputItem is one element of a response output array.
type ResponseOutputItem struct {
	Type      string               `json:"type"` // message | function_call
	ID        string               `json:"id,omitempty"`
	Role      string               `json:"role,omitempty"`
	Content   []ResponseOutputPart `json:"content,omitempty"`
	CallID    string               `json:"call_id,omitempty"`
	Name      string               `json:"name,omitempty"`
	Arguments string               `json:"arguments,omitempty"`
}

// ResponseOutputPart is one content part of an output message.
type ResponseOutputPart struct {
	Type        string            `json:"type"` // output_text
	Text        string            `json:"text,omitempty"`
	Annotations []json.RawMessage `json:"annotations,omitempty"`
}

// ResponsesResponse is a completed response object.
type ResponsesResponse struct {
	ID           string               `json:"id"`
	Object       string               `json:"object"`
	CreatedAt    int64                `json:"created_at"`
	Model        string               `json:"model"`
	Status       string               `json:"status"`
	Output       []ResponseOutputItem `json:"output"`
	Usage        *ResponsesUsage      `json:"usage,omitempty"`
	Instructions string               `json:"instructions,omitempty"`
}

// ResponsesUsage mirrors the Responses API usage object.
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
