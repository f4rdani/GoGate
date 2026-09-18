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

// ApplyOpenCodeHeaders applies all required headers for OpenCode Zen upstream requests.
func ApplyOpenCodeHeaders(req *http.Request, apiKey string, attemptCtx context.Context) {
	sess, tool, ua := GetOpenCodeMeta(attemptCtx)
	sessionID := TranslateOpenCodeSessionID(sess, tool)
	reqID := GenerateOpenCodeRequestID()

	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", OpenCodeProjectID())
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-request", reqID)
	req.Header.Set("X-Session-ID", sessionID)
	req.Header.Set("User-Agent", FormatOpenCodeUserAgent(ua))

	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("Authorization", "Bearer public")
	}
}

// PrepareOpenCodeRequest clones and prepares the request for OpenCode Zen:
// - Injects OpenCode default tools so the upstream free tier gate accepts the request.
// - Ensures tool_choice is set to "auto" if tools are injected.
func PrepareOpenCodeRequest(req *models.ChatCompletionRequest) *models.ChatCompletionRequest {
	cloned := *req
	if len(cloned.Tools) == 0 || string(cloned.Tools) == "null" || string(cloned.Tools) == "[]" {
		if len(defaultOpenCodeToolsJSON) > 0 {
			cloned.Tools = json.RawMessage(defaultOpenCodeToolsJSON)
			if len(cloned.ToolChoice) == 0 {
				cloned.ToolChoice = json.RawMessage(`"auto"`)
			}
		}
	} else {
		cloned.Tools = mergeOpenCodeTools(cloned.Tools, defaultOpenCodeToolsJSON)
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
