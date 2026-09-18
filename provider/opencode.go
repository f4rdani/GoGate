package provider

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aigateway/models"
)

//go:embed opencode_tools.json
var defaultOpenCodeToolsJSON []byte

const (
	// OpenCodeDefaultUA is the canonical User-Agent required by OpenCode Zen upstream.
	OpenCodeDefaultUA = "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	base62Chars       = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// OpenCodeSessionRegex matches canonical OpenCode session IDs: ses_ + 12 hex + 14 Base62 (exactly 30 chars).
var OpenCodeSessionRegex = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var openCodeUARegex = regexp.MustCompile(`^opencode/(\d+)\.(\d+)(?:\.(\d+))?`)

var sessionCounter atomic.Uint64

type opencodeCtxKey string

const (
	ctxKeySessionID  opencodeCtxKey = "opencode_session_id"
	ctxKeyClientTool opencodeCtxKey = "opencode_client_tool"
	ctxKeyUserAgent  opencodeCtxKey = "opencode_user_agent"
)

// WithOpenCodeMeta stores client session, tool name, and user-agent into context.
func WithOpenCodeMeta(ctx context.Context, sessionID, clientTool, userAgent string) context.Context {
	if sessionID != "" {
		ctx = context.WithValue(ctx, ctxKeySessionID, sessionID)
	}
	if clientTool != "" {
		ctx = context.WithValue(ctx, ctxKeyClientTool, clientTool)
	}
	if userAgent != "" {
		ctx = context.WithValue(ctx, ctxKeyUserAgent, userAgent)
	}
	return ctx
}

// GetOpenCodeMeta extracts opencode session, client tool, and user-agent from context.
func GetOpenCodeMeta(ctx context.Context) (sessionID, clientTool, userAgent string) {
	if ctx == nil {
		return
	}
	if v, ok := ctx.Value(ctxKeySessionID).(string); ok {
		sessionID = v
	}
	if v, ok := ctx.Value(ctxKeyClientTool).(string); ok {
		clientTool = v
	}
	if v, ok := ctx.Value(ctxKeyUserAgent).(string); ok {
		userAgent = v
	}
	return
}

// GenerateOpenCodeSessionID generates a canonical 30-character session ID:
// "ses_" + 12 hex characters (time-based, descending ordering) + 14 Base62 characters.
func GenerateOpenCodeSessionID() string {
	ts := uint64(time.Now().UnixMilli())
	cnt := sessionCounter.Add(1)
	current := (ts << 12) + (cnt & 0xFFF)
	val := ^current // bitwise NOT for descending ordering

	var timeBytes [6]byte
	for i := 0; i < 6; i++ {
		timeBytes[i] = byte((val >> (40 - 8*i)) & 0xFF)
	}
	timeHex := hex.EncodeToString(timeBytes[:])

	var randBytes [14]byte
	_, _ = rand.Read(randBytes[:])
	var randPart strings.Builder
	for _, b := range randBytes {
		randPart.WriteByte(base62Chars[b%62])
	}

	return fmt.Sprintf("ses_%s%s", timeHex, randPart.String())
}

// GenerateOpenCodeRequestID generates a canonical message/request ID:
// "msg_" + 12 hex characters (descending ordering) + 14 Base62 characters.
func GenerateOpenCodeRequestID() string {
	ts := uint64(time.Now().UnixMilli())
	cnt := sessionCounter.Add(1)
	current := (ts << 12) + (cnt & 0xFFF)
	val := ^current

	var timeBytes [6]byte
	for i := 0; i < 6; i++ {
		timeBytes[i] = byte((val >> (40 - 8*i)) & 0xFF)
	}
	timeHex := hex.EncodeToString(timeBytes[:])

	var randBytes [14]byte
	_, _ = rand.Read(randBytes[:])
	var randPart strings.Builder
	for _, b := range randBytes {
		randPart.WriteByte(base62Chars[b%62])
	}

	return fmt.Sprintf("msg_%s%s", timeHex, randPart.String())
}

// OpenCodeProjectID returns a canonical 40-character sha1 project ID.
func OpenCodeProjectID() string {
	return "391e9810d99b41f1fd73141dd8258d53be303469"
}

// TranslateOpenCodeSessionID deterministically translates an arbitrary session ID
// into a canonical OpenCode session ID so multi-turn prompt caching in OpenCode
// remains consistent across turns.
func TranslateOpenCodeSessionID(rawSession, clientTool string) string {
	raw := strings.TrimSpace(rawSession)
	if OpenCodeSessionRegex.MatchString(raw) {
		return raw
	}
	if raw == "" {
		return GenerateOpenCodeSessionID()
	}
	if clientTool == "" {
		clientTool = "gogate"
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("opencode\x00%s\x00%s", clientTool, raw)))
	timeHex := hex.EncodeToString(h[:6])
	var randPart strings.Builder
	for _, b := range h[6:20] {
		randPart.WriteByte(base62Chars[b%62])
	}
	return fmt.Sprintf("ses_%s%s", timeHex, randPart.String())
}

// FormatOpenCodeUserAgent checks whether the incoming User-Agent is a valid opencode version.
// Upstream OpenCode Zen requires a user agent identifying as official OpenCode with AI SDK runtime.
func FormatOpenCodeUserAgent(incomingUA string) string {
	incomingUA = strings.TrimSpace(incomingUA)
	if incomingUA == "" {
		return OpenCodeDefaultUA
	}
	matches := openCodeUARegex.FindStringSubmatch(incomingUA)
	if len(matches) >= 3 {
		major, err1 := strconv.Atoi(matches[1])
		minor, err2 := strconv.Atoi(matches[2])
		if err1 == nil && err2 == nil {
			if major > 1 || (major == 1 && minor >= 17) {
				return incomingUA
			}
		}
	}
	return OpenCodeDefaultUA
}

var defaultChatToolsJSON = []byte(`[{"type":"function","function":{"name":"bash","description":"This tool is currently unavailable and must not be used.","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"read","description":"This tool is currently unavailable and must not be used.","parameters":{"type":"object","properties":{}}}}]`)

// ApplyOpenCodeHeaders applies all required headers for OpenCode Zen upstream requests.
func ApplyOpenCodeHeaders(req *http.Request, apiKey string, attemptCtx context.Context) {
	sess, tool, ua := GetOpenCodeMeta(attemptCtx)
	sessionID := TranslateOpenCodeSessionID(sess, tool)
	reqID := GenerateOpenCodeRequestID()

	req.Header.Set("x-opencode-client", "desktop")
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-request", reqID)
	req.Header.Set("X-Session-ID", sessionID)
	req.Header.Set("User-Agent", FormatOpenCodeUserAgent(ua))
	req.Header.Set("Accept", "text/event-stream")

	apiKey = strings.TrimSpace(apiKey)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("Authorization", "Bearer public")
	}
}

// PrepareOpenCodeRequest clones and prepares standard chat completion requests for OpenCode:
// - Injects OpenCode lightweight tools and sets tool_choice: "none" when tools are not specified.
// - Ensures stream: true upstream.
func PrepareOpenCodeRequest(req *models.ChatCompletionRequest) *models.ChatCompletionRequest {
	cloned := *req
	cloned.Model = CleanOpenCodeModel(cloned.Model)
	cloned.Stream = true
	if len(cloned.Tools) == 0 || string(cloned.Tools) == "null" || string(cloned.Tools) == "[]" {
		cloned.Tools = json.RawMessage(defaultChatToolsJSON)
		if len(cloned.ToolChoice) == 0 {
			cloned.ToolChoice = json.RawMessage(`"auto"`)
		}
	} else {
		cloned.Tools = mergeOpenCodeTools(cloned.Tools, defaultChatToolsJSON)
	}
	return &cloned
}

func mergeOpenCodeTools(userToolsRaw, defaultToolsRaw []byte) json.RawMessage {
	if len(defaultToolsRaw) == 0 {
		return json.RawMessage(userToolsRaw)
	}
	var userTools []map[string]interface{}
	if err := json.Unmarshal(userToolsRaw, &userTools); err != nil || len(userTools) == 0 {
		return json.RawMessage(defaultToolsRaw)
	}
	var defTools []map[string]interface{}
	if err := json.Unmarshal(defaultToolsRaw, &defTools); err != nil {
		return json.RawMessage(userToolsRaw)
	}
	existingNames := make(map[string]bool)
	for _, t := range userTools {
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				existingNames[name] = true
			}
		}
	}
	for _, dt := range defTools {
		if fn, ok := dt["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok && !existingNames[name] {
				userTools = append(userTools, dt)
			}
		}
	}
	merged, err := json.Marshal(userTools)
	if err != nil {
		return json.RawMessage(userToolsRaw)
	}
	return json.RawMessage(merged)
}

// ParseOpenCodeSSEStream reads SSE chunks from upstream OpenCode Zen stream
// and aggregates them into a standard non-streaming ChatCompletionResponse.
func ParseOpenCodeSSEStream(r io.Reader, requestedModel string) (*models.ChatCompletionResponse, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024) // up to 1MB lines

	type toolCallAccumulator struct {
		id       string
		callType string
		name     string
		args     strings.Builder
	}

	var (
		respID           string
		modelName        string
		createdTs        int64
		assembledContent strings.Builder
		assembledReason  strings.Builder
		finishReason     *string
		usage            *models.Usage
		toolCallsMap     = make(map[int]*toolCallAccumulator)
	)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk models.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.ID != "" && respID == "" {
			respID = chunk.ID
		}
		if chunk.Model != "" && modelName == "" {
			modelName = chunk.Model
		}
		if chunk.Created != 0 && createdTs == 0 {
			createdTs = chunk.Created
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				finishReason = choice.FinishReason
			}
			if choice.Delta != nil {
				if choice.Delta.Content != "" {
					assembledContent.WriteString(choice.Delta.Content)
				}
				if choice.Delta.ReasoningContent != "" {
					assembledReason.WriteString(choice.Delta.ReasoningContent)
				}
				if len(choice.Delta.ToolCalls) > 0 {
					var deltas []struct {
						Index    int    `json:"index"`
						ID       string `json:"id,omitempty"`
						Type     string `json:"type,omitempty"`
						Function *struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						} `json:"function,omitempty"`
					}
					if err := json.Unmarshal(choice.Delta.ToolCalls, &deltas); err == nil {
						for _, tc := range deltas {
							acc, exists := toolCallsMap[tc.Index]
							if !exists {
								acc = &toolCallAccumulator{
									id:       tc.ID,
									callType: tc.Type,
								}
								if tc.Function != nil {
									acc.name = tc.Function.Name
								}
								toolCallsMap[tc.Index] = acc
							} else {
								if tc.ID != "" {
									acc.id = tc.ID
								}
								if tc.Type != "" {
									acc.callType = tc.Type
								}
								if tc.Function != nil && tc.Function.Name != "" {
									acc.name = tc.Function.Name
								}
							}
							if tc.Function != nil && tc.Function.Arguments != "" {
								acc.args.WriteString(tc.Function.Arguments)
							}
						}
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read sse stream: %w", err)
	}

	if respID == "" {
		respID = "chatcmpl-" + GenerateOpenCodeRequestID()
	}
	if modelName == "" {
		modelName = requestedModel
	}
	if createdTs == 0 {
		createdTs = time.Now().Unix()
	}
	if finishReason == nil {
		defReason := "stop"
		if len(toolCallsMap) > 0 {
			defReason = "tool_calls"
		}
		finishReason = &defReason
	}

	msg := &models.Message{
		Role:             "assistant",
		Content:          json.RawMessage(strconv.Quote(assembledContent.String())),
		ReasoningContent: assembledReason.String(),
	}

	if len(toolCallsMap) > 0 {
		type toolCallOut struct {
			Index    int    `json:"index,omitempty"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		var tcList []toolCallOut
		for i := 0; i < len(toolCallsMap); i++ {
			if acc, ok := toolCallsMap[i]; ok {
				var tc toolCallOut
				tc.Index = i
				tc.ID = acc.id
				if tc.ID == "" {
					tc.ID = fmt.Sprintf("call_%d", i)
				}
				tc.Type = acc.callType
				if tc.Type == "" {
					tc.Type = "function"
				}
				tc.Function.Name = acc.name
				tc.Function.Arguments = acc.args.String()
				tcList = append(tcList, tc)
			}
		}
		tcBytes, _ := json.Marshal(tcList)
		msg.ToolCalls = tcBytes
	}

	return &models.ChatCompletionResponse{
		ID:      respID,
		Object:  "chat.completion",
		Created: createdTs,
		Model:   modelName,
		Choices: []models.Choice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		Usage: usage,
	}, nil
}

var museSparkRegex = regexp.MustCompile(`(?i)^muse[-_]?spark(?:$|[-_:.\s])`)

// IsOpenCodeResponsesModel checks whether the requested model must be routed to the OpenCode Responses API (/zen/v1/responses).
// Muse Spark models require the /responses endpoint with tools attached to pass the upstream free-tier check.
func IsOpenCodeResponsesModel(model string) bool {
	clean := CleanOpenCodeModel(model)
	return museSparkRegex.MatchString(clean) ||
		clean == "muse-spark-1.2-contributor-free" ||
		clean == "muse-spark-1.3-contributor-free"
}

// CleanOpenCodeModel strips any routing prefixes like "oc/" or "opencode/".
func CleanOpenCodeModel(model string) string {
	clean := strings.TrimSpace(model)
	if idx := strings.LastIndex(clean, "/"); idx != -1 {
		return clean[idx+1:]
	}
	return clean
}

type OpenCodeResponsesContent struct {
	Type     string `json:"type"` // "input_text", "output_text", "input_image"
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type OpenCodeResponsesMessage struct {
	Type      string                     `json:"type"` // "message", "function_call", "function_call_output"
	Role      string                     `json:"role,omitempty"` // "user", "assistant"
	Content   []OpenCodeResponsesContent `json:"content,omitempty"`
	CallID    string                     `json:"call_id,omitempty"`
	Name      string                     `json:"name,omitempty"`
	Arguments string                     `json:"arguments,omitempty"`
	Output    string                     `json:"output,omitempty"`
}

type OpenCodeResponsesTool struct {
	Type        string                 `json:"type"` // "function"
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
	Strict      *bool                  `json:"strict,omitempty"`
}

type OpenCodeResponsesPayload struct {
	Model           string                     `json:"model"`
	Input           []OpenCodeResponsesMessage `json:"input"`
	Instructions    string                     `json:"instructions,omitempty"`
	Stream          bool                       `json:"stream"`
	Store           bool                       `json:"store"`
	Tools           []OpenCodeResponsesTool    `json:"tools,omitempty"`
	ToolChoice      interface{}                `json:"tool_choice,omitempty"`
	Temperature     *float64                   `json:"temperature,omitempty"`
	TopP            *float64                   `json:"top_p,omitempty"`
	MaxOutputTokens *int                       `json:"max_output_tokens,omitempty"`
	Reasoning       map[string]interface{}     `json:"reasoning,omitempty"`
}

func extractMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

// BuildOpenCodeResponsesRequest translates standard ChatCompletionRequest into OpenCode Responses API payload.
func BuildOpenCodeResponsesRequest(req *models.ChatCompletionRequest) ([]byte, error) {
	model := CleanOpenCodeModel(req.Model)
	var instructionsBuilder strings.Builder
	var input []OpenCodeResponsesMessage

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			text := extractMessageContent(msg.Content)
			if text != "" {
				if instructionsBuilder.Len() > 0 {
					instructionsBuilder.WriteString("\n")
				}
				instructionsBuilder.WriteString(text)
			}
		case "user":
			text := extractMessageContent(msg.Content)
			input = append(input, OpenCodeResponsesMessage{
				Type: "message",
				Role: "user",
				Content: []OpenCodeResponsesContent{
					{Type: "input_text", Text: text},
				},
			})
		case "assistant":
			text := extractMessageContent(msg.Content)
			if text != "" {
				input = append(input, OpenCodeResponsesMessage{
					Type: "message",
					Role: "assistant",
					Content: []OpenCodeResponsesContent{
						{Type: "output_text", Text: text},
					},
				})
			}
			if len(msg.ToolCalls) > 0 {
				var tcList []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				if err := json.Unmarshal(msg.ToolCalls, &tcList); err == nil {
					for _, tc := range tcList {
						input = append(input, OpenCodeResponsesMessage{
							Type:      "function_call",
							CallID:    tc.ID,
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						})
					}
				}
			}
		case "tool":
			input = append(input, OpenCodeResponsesMessage{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: extractMessageContent(msg.Content),
			})
		default:
			text := extractMessageContent(msg.Content)
			input = append(input, OpenCodeResponsesMessage{
				Type: "message",
				Role: "user",
				Content: []OpenCodeResponsesContent{
					{Type: "input_text", Text: text},
				},
			})
		}
	}

	if len(input) == 0 {
		input = []OpenCodeResponsesMessage{
			{
				Type: "message",
				Role: "user",
				Content: []OpenCodeResponsesContent{
					{Type: "input_text", Text: "..."},
				},
			},
		}
	}

	// Format tools
	var tools []OpenCodeResponsesTool
	toolChoice := "auto"

	if len(req.Tools) > 0 && string(req.Tools) != "null" && string(req.Tools) != "[]" {
		var userTools []struct {
			Type     string `json:"type"`
			Name     string `json:"name,omitempty"`
			Function *struct {
				Name        string                 `json:"name"`
				Description string                 `json:"description"`
				Parameters  map[string]interface{} `json:"parameters"`
			} `json:"function,omitempty"`
			Description string                 `json:"description,omitempty"`
			Parameters  map[string]interface{} `json:"parameters,omitempty"`
		}
		if err := json.Unmarshal(req.Tools, &userTools); err == nil {
			for _, ut := range userTools {
				name := ut.Name
				desc := ut.Description
				params := ut.Parameters
				if ut.Function != nil {
					if ut.Function.Name != "" {
						name = ut.Function.Name
					}
					if ut.Function.Description != "" {
						desc = ut.Function.Description
					}
					if ut.Function.Parameters != nil {
						params = ut.Function.Parameters
					}
				}
				if name != "" {
					if params == nil {
						params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
					}
					tools = append(tools, OpenCodeResponsesTool{
						Type:        "function",
						Name:        name,
						Description: desc,
						Parameters:  params,
					})
				}
			}
		}
		if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
			var tc interface{}
			if err := json.Unmarshal(req.ToolChoice, &tc); err == nil {
				toolChoice = fmt.Sprintf("%v", tc)
			}
		}
	}

	// If no tools, inject default tools required by OpenCode free tier gate
	if len(tools) == 0 {
		tools = []OpenCodeResponsesTool{
			{
				Type:        "function",
				Name:        "bash",
				Description: "This tool is currently unavailable and must not be used.",
				Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
			{
				Type:        "function",
				Name:        "read",
				Description: "This tool is currently unavailable and must not be used.",
				Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
		}
		toolChoice = "auto"
	}

	payload := OpenCodeResponsesPayload{
		Model:        model,
		Input:        input,
		Instructions: instructionsBuilder.String(),
		Stream:       true,
		Store:        false,
		Tools:        tools,
		ToolChoice:   toolChoice,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		Reasoning: map[string]interface{}{
			"effort":  "xhigh",
			"summary": "auto",
		},
	}

	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		payload.MaxOutputTokens = req.MaxTokens
	} else if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		payload.MaxOutputTokens = req.MaxCompletionTokens
	}

	return json.Marshal(payload)
}

type OpenCodeResponsesEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta,omitempty"`
	Item     *struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	} `json:"item,omitempty"`
	Response *struct {
		ID        string `json:"id"`
		Model     string `json:"model"`
		CreatedAt int64  `json:"created_at"`
		Status    string `json:"status"`
		Output    []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response,omitempty"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ParseOpenCodeResponsesStream reads SSE stream from OpenCode /zen/v1/responses endpoint
// and aggregates it into a standard non-streaming ChatCompletionResponse.
func ParseOpenCodeResponsesStream(r io.Reader, requestedModel string) (*models.ChatCompletionResponse, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var (
		respID           string
		modelName        string
		createdTs        int64
		assembledContent strings.Builder
		assembledReason  strings.Builder
		usage            *models.Usage
		currentEvent     string
		upstreamErr      error
	)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataStr == "[DONE]" {
			break
		}

		var ev OpenCodeResponsesEvent
		if err := json.Unmarshal([]byte(dataStr), &ev); err != nil {
			continue
		}

		evType := ev.Type
		if evType == "" {
			evType = currentEvent
		}

		if ev.Error != nil {
			upstreamErr = fmt.Errorf("opencode upstream error: %s: %s", ev.Error.Type, ev.Error.Message)
		}

		switch evType {
		case "response.output_text.delta":
			if ev.Delta != "" {
				assembledContent.WriteString(ev.Delta)
			}
		case "response.reasoning_summary_text.delta":
			if ev.Delta != "" {
				assembledReason.WriteString(ev.Delta)
			}
		case "response.completed", "response.done":
			if ev.Response != nil {
				if ev.Response.ID != "" && respID == "" {
					respID = ev.Response.ID
				}
				if ev.Response.Model != "" && modelName == "" {
					modelName = ev.Response.Model
				}
				if ev.Response.CreatedAt != 0 && createdTs == 0 {
					createdTs = ev.Response.CreatedAt
				}
				if ev.Response.Usage != nil {
					usage = &models.Usage{
						PromptTokens:     ev.Response.Usage.InputTokens,
						CompletionTokens: ev.Response.Usage.OutputTokens,
						TotalTokens:      ev.Response.Usage.TotalTokens,
					}
				}
				if ev.Response.Error != nil {
					upstreamErr = fmt.Errorf("opencode upstream error: %s: %s", ev.Response.Error.Type, ev.Response.Error.Message)
				}
				if assembledContent.Len() == 0 {
					for _, item := range ev.Response.Output {
						if item.Type == "message" || item.Role == "assistant" {
							for _, c := range item.Content {
								if c.Text != "" {
									assembledContent.WriteString(c.Text)
								}
							}
						}
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read responses sse: %w", err)
	}
	if upstreamErr != nil {
		return nil, upstreamErr
	}

	if respID == "" {
		respID = "resp_" + GenerateOpenCodeRequestID()
	}
	if modelName == "" {
		modelName = requestedModel
	}
	if createdTs == 0 {
		createdTs = time.Now().Unix()
	}
	finishReason := "stop"

	return &models.ChatCompletionResponse{
		ID:      respID,
		Object:  "chat.completion",
		Created: createdTs,
		Model:   modelName,
		Choices: []models.Choice{
			{
				Index: 0,
				Message: &models.Message{
					Role:             "assistant",
					Content:          json.RawMessage(strconv.Quote(assembledContent.String())),
					ReasoningContent: assembledReason.String(),
				},
				FinishReason: &finishReason,
			},
		},
		Usage: usage,
	}, nil
}

// PipeOpenCodeResponsesSSE reads SSE from OpenCode /zen/v1/responses endpoint and writes
// standard OpenAI chat completion chunk SSE lines to the client.
func PipeOpenCodeResponsesSSE(r io.Reader, w http.ResponseWriter, flusher http.Flusher, requestedModel string) error {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	chatID := "chatcmpl-" + GenerateOpenCodeRequestID()
	createdTs := time.Now().Unix()
	modelName := requestedModel
	roleEmitted := false
	var currentEvent string

	writeChunk := func(delta *models.StreamDelta, finishReason *string, usage *models.Usage) error {
		chunk := models.ChatCompletionChunk{
			ID:      chatID,
			Object:  "chat.completion.chunk",
			Created: createdTs,
			Model:   modelName,
			Choices: []models.StreamChoice{
				{
					Index:        0,
					Delta:        delta,
					FinishReason: finishReason,
				},
			},
			Usage: usage,
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataStr == "[DONE]" {
			break
		}

		var ev OpenCodeResponsesEvent
		if err := json.Unmarshal([]byte(dataStr), &ev); err != nil {
			continue
		}

		evType := ev.Type
		if evType == "" {
			evType = currentEvent
		}

		switch evType {
		case "response.output_text.delta":
			if ev.Delta != "" {
				delta := &models.StreamDelta{Content: ev.Delta}
				if !roleEmitted {
					delta.Role = "assistant"
					roleEmitted = true
				}
				if err := writeChunk(delta, nil, nil); err != nil {
					return err
				}
			}
		case "response.reasoning_summary_text.delta":
			if ev.Delta != "" {
				delta := &models.StreamDelta{ReasoningContent: ev.Delta}
				if !roleEmitted {
					delta.Role = "assistant"
					roleEmitted = true
				}
				if err := writeChunk(delta, nil, nil); err != nil {
					return err
				}
			}
		case "response.completed", "response.done":
			if !roleEmitted && ev.Response != nil {
				for _, item := range ev.Response.Output {
					if item.Type == "message" || item.Role == "assistant" {
						for _, c := range item.Content {
							if c.Text != "" {
								delta := &models.StreamDelta{Role: "assistant", Content: c.Text}
								roleEmitted = true
								_ = writeChunk(delta, nil, nil)
							}
						}
					}
				}
			}
			var usage *models.Usage
			if ev.Response != nil && ev.Response.Usage != nil {
				usage = &models.Usage{
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.TotalTokens,
				}
			}
			stop := "stop"
			if err := writeChunk(&models.StreamDelta{}, &stop, usage); err != nil {
				return err
			}
		}
	}

	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
