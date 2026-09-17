package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aigateway/middleware"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
	"github.com/aigateway/usage"
)

// newRespID generates a prefixed random ID (resp_*, msg_*, fc_*).
func newRespID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// responsesToChat converts an OpenAI Responses request into a chat completion
// request the router can serve. Returns an error for unsupported shapes.
func responsesToChat(in *models.ResponsesRequest) (*models.ChatCompletionRequest, error) {
	out := &models.ChatCompletionRequest{
		Model:       in.Model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		ToolChoice:  translateResponsesToolChoice(in.ToolChoice),
		Tools:       translateResponsesTools(in.Tools),
	}
	if in.MaxOutputTokens != nil {
		out.MaxTokens = in.MaxOutputTokens
	}

	var msgs []models.Message
	if in.Instructions != "" {
		msgs = append(msgs, models.Message{
			Role:    "system",
			Content: mustJSONMarshal(in.Instructions),
		})
	}

	trimmed := bytesTrimSpace(in.Input)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	if trimmed[0] == '"' {
		// Plain string input → single user message.
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("invalid string input: %w", err)
		}
		msgs = append(msgs, models.Message{Role: "user", Content: mustJSONMarshal(text)})
	} else {
		var items []models.ResponseInputItem
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("input must be a string or an array of items: %w", err)
		}
		for _, it := range items {
			switch it.Type {
			case "", "message":
				role := it.Role
				if role == "" {
					role = "user"
				}
				content, err := responsesContentToChat(it.Content)
				if err != nil {
					return nil, err
				}
				msgs = append(msgs, models.Message{Role: role, Content: content})
			case "function_call_output":
				// Tool result → chat tool message.
				trimmedOut := bytesTrimSpace(it.Output)
				var content json.RawMessage
				switch {
				case len(trimmedOut) == 0:
					content = json.RawMessage(`""`)
				case trimmedOut[0] == '"' || trimmedOut[0] == '{' || trimmedOut[0] == '[':
					content = trimmedOut
				default:
					content = mustJSONMarshal(string(trimmedOut))
				}
				msgs = append(msgs, models.Message{
					Role:       "tool",
					Content:    content,
					ToolCallID: it.CallID,
				})
			case "reasoning":
				continue // stateless gateway: prior reasoning is not replayed
			default:
				return nil, fmt.Errorf("unsupported input item type %q", it.Type)
			}
		}
	}

	if len(msgs) == 0 {
		return nil, fmt.Errorf("input produced no messages")
	}
	out.Messages = msgs
	return out, nil
}

// responsesContentToChat maps Responses content (string or parts) to a chat
// content payload (string or OpenAI parts array).
func responsesContentToChat(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`""`), nil
	}
	if trimmed[0] == '"' {
		return trimmed, nil
	}
	var parts []models.ResponseContentPart
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil, fmt.Errorf("invalid message content: %w", err)
	}
	chatParts := make([]map[string]interface{}, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "text", "output_text":
			chatParts = append(chatParts, map[string]interface{}{"type": "text", "text": p.Text})
		case "input_image":
			url := p.ImageURL
			chatParts = append(chatParts, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}})
		default:
			return nil, fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	out, err := json.Marshal(chatParts)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// translateResponsesTools maps Responses function tools to chat tools.
func translateResponsesTools(raw json.RawMessage) json.RawMessage {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var tools []models.ResponseTool
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return raw // unknown shape — pass through untouched
	}
	chatTools := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue // web_search etc. are not supported upstream
		}
		fn := map[string]interface{}{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(bytesTrimSpace(t.Parameters)) > 0 {
			var params interface{}
			if err := json.Unmarshal(t.Parameters, &params); err == nil {
				fn["parameters"] = params
			}
		}
		chatTools = append(chatTools, map[string]interface{}{"type": "function", "function": fn})
	}
	if len(chatTools) == 0 {
		return nil
	}
	out, err := json.Marshal(chatTools)
	if err != nil {
		return nil
	}
	return out
}

// translateResponsesToolChoice maps Responses tool_choice to chat shape.
func translateResponsesToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		return trimmed // "auto" | "none" | "required" pass through
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &choice); err != nil {
		return raw
	}
	if choice.Type == "function" && choice.Name != "" {
		out, err := json.Marshal(map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": choice.Name},
		})
		if err == nil {
			return out
		}
	}
	return raw
}

// chatToResponses maps a chat completion response to a Responses object.
func chatToResponses(model string, chat *models.ChatCompletionResponse) *models.ResponsesResponse {
	resp := &models.ResponsesResponse{
		ID:        newRespID("resp"),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     model,
		Status:    "completed",
	}
	if chat == nil || len(chat.Choices) == 0 || chat.Choices[0].Message == nil {
		resp.Status = "incomplete"
		resp.Output = []models.ResponseOutputItem{}
		return resp
	}
	msg := chat.Choices[0].Message

	var text string
	if len(msg.Content) > 0 {
		_ = json.Unmarshal(msg.Content, &text)
		if text == "" && string(bytesTrimSpace(msg.Content)) != "" && string(bytesTrimSpace(msg.Content)) != "null" {
			text = string(msg.Content)
		}
	}
	if text != "" || len(msg.ToolCalls) == 0 {
		resp.Output = append(resp.Output, models.ResponseOutputItem{
			Type: "message",
			ID:   newRespID("msg"),
			Role: "assistant",
			Content: []models.ResponseOutputPart{
				{Type: "output_text", Text: text, Annotations: []json.RawMessage{}},
			},
		})
	}

	if len(msg.ToolCalls) > 0 {
		var calls []map[string]interface{}
		if err := json.Unmarshal(msg.ToolCalls, &calls); err == nil {
			for _, c := range calls {
				fn, _ := c["function"].(map[string]interface{})
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				id, _ := c["id"].(string)
				if id == "" {
					id = newRespID("fc")
				}
				resp.Output = append(resp.Output, models.ResponseOutputItem{
					Type:      "function_call",
					ID:        id,
					CallID:    id,
					Name:      name,
					Arguments: args,
				})
			}
		}
	}

	if chat.Usage != nil {
		resp.Usage = &models.ResponsesUsage{
			InputTokens:  chat.Usage.PromptTokens,
			OutputTokens: chat.Usage.CompletionTokens,
			TotalTokens:  chat.Usage.TotalTokens,
		}
	}
	return resp
}

// HandleResponses handles POST /v1/responses (OpenAI Responses API).
// The request is translated to a chat completion served by the router, then
// mapped back. Streaming replays the completed text as response events
// (valid SSE event sequence; upstream is consumed non-streaming).
func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	h.Stats.TotalRequests.Add(1)
	h.Stats.ActiveRequests.Add(1)
	defer h.Stats.ActiveRequests.Add(-1)

	h.mu.RLock()
	routerInst := h.router
	keyStore := h.keyStore
	limiter := h.limiter
	tracker := h.tracker
	tsCfg := h.tokenSaverCfg
	h.mu.RUnlock()

	apiKey := h.extractAPIKey(r)
	if apiKey == "" {
		h.sendError(w, http.StatusUnauthorized, "Missing API key in Authorization header", "authentication_error")
		return
	}
	keyInfo, ok := keyStore.Validate(apiKey)
	if !ok {
		h.sendError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error")
		return
	}
	if !keyInfo.CheckRateLimit() {
		middleware.TooManyRequestsResponse(w)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendError(w, http.StatusBadRequest, "Failed to read request body (max 10MB)", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var req models.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request_error")
		return
	}
	if req.Model == "" {
		h.sendError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	slog.Info(fmt.Sprintf("📥 POST /v1/responses | %s | stream=%v", req.Model, req.Stream))

	if !keyInfo.IsModelAllowed(req.Model) {
		h.sendError(w, http.StatusForbidden,
			"Model '"+req.Model+"' is not allowed for this API key", "permission_error")
		return
	}

	chatReq, err := responsesToChat(&req)
	if err != nil {
		h.sendError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	if keyInfo.IsTokenSaverEnabled(tsCfg.Enabled) && !isTokenSaverBypassed(r) {
		if stats := middleware.CompressMessages(chatReq, tsCfg); stats != nil && tracker != nil {
			tracker.RecordTokenSaving(int64(stats.OriginalBytes - stats.CompressedBytes))
		}
		if tsCfg.CavemanMode {
			middleware.InjectCavemanMode(chatReq)
		} else if tsCfg.Ponytail != "" {
			middleware.InjectPonytailMode(chatReq, tsCfg.Ponytail)
		}
	}

	if !limiter.AcquireGlobalWithQueue() {
		middleware.QueueFullResponse(w)
		return
	}
	defer limiter.ReleaseGlobal()

	if !limiter.AcquireModel(chatReq.Model) {
		middleware.QueueFullResponse(w)
		return
	}
	defer limiter.ReleaseModel(chatReq.Model)

	if req.Stream {
		h.handleResponsesStream(w, r, apiKey, chatReq, req.Model)
		return
	}

	startTime := time.Now()
	resp, provName, err := routerInst.ChatCompletion(r.Context(), chatReq.Model, chatReq)
	durationMs := time.Since(startTime).Milliseconds()
	if err != nil {
		slog.Error(fmt.Sprintf("❌ [RESPONSES] error: %v | model=%s | duration=%dms", err, req.Model, durationMs))
		if tracker != nil {
			tracker.RecordError(nonEmptyOr(provName, "unknown"), req.Model)
		}
		if provider.IsLocalLimit(err) {
			middleware.TooManyRequestsResponse(w)
			return
		}
		h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
		return
	}

	out := chatToResponses(req.Model, resp)
	if tracker != nil && resp.Usage != nil {
		tracker.RecordUsage(apiKey, nonEmptyOr(provName, resp.Model), req.Model,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, false)
	}
	slog.Info(fmt.Sprintf("📊 [RESPONSES] %s | latency=%dms", req.Model, durationMs))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleResponsesStream serves a streaming Responses request with true
// live translation: upstream OpenAI SSE is converted to response events as
// it arrives. Token usage reported by upstream (via injected stream_options)
// is recorded. If upstream rejects stream_options before emitting anything,
// it degrades to buffered replay without them.
func (h *Handler) handleResponsesStream(w http.ResponseWriter, r *http.Request, apiKey string, chatReq *models.ChatCompletionRequest, routeModel string) {
	h.mu.RLock()
	routerInst := h.router
	tracker := h.tracker
	h.mu.RUnlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendError(w, http.StatusInternalServerError, "Streaming not supported by server", "server_error")
		return
	}

	chatReq.Stream = true
	chatReq.StreamOptions = json.RawMessage(`{"include_usage":true}`)

	translator := newResponsesStreamTranslator(w, flusher, routeModel)
	startTime := time.Now()
	streamReq := *chatReq
	err := routerInst.ChatCompletionStream(r.Context(), chatReq.Model, &streamReq, translator, translator)
	durationMs := time.Since(startTime).Milliseconds()

	if err == nil {
		translator.finalize()
		if tracker != nil && translator.usage != nil {
			model := translator.upstreamModel
			if model == "" {
				model = routeModel
			}
			tracker.RecordUsage(apiKey, "stream", model,
				translator.usage.InputTokens, translator.usage.OutputTokens, false)
		}
		slog.Info(fmt.Sprintf("🌊 [RESPONSES-STREAM] %s | %dms | complete", routeModel, durationMs))
		return
	}

	// Upstream rejected stream_options before emitting? Degrade to replay.
	if !translator.Emitted() && isStreamOptionsRejection(err) {
		slog.Warn("responses stream_options rejected, degrading to replay", "model", routeModel, "error", err)
		replayReq := *chatReq
		replayReq.Stream = false
		replayReq.StreamOptions = nil
		resp, provName, rerr := routerInst.ChatCompletion(r.Context(), routeModel, &replayReq)
		if rerr != nil {
			h.responsesStreamError(w, translator, tracker, routeModel, rerr, durationMs)
			return
		}
		out := chatToResponses(routeModel, resp)
		if tracker != nil && resp.Usage != nil {
			tracker.RecordUsage(apiKey, nonEmptyOr(provName, resp.Model), routeModel,
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens, false)
		}
		writeResponsesStream(w, out)
		return
	}

	h.responsesStreamError(w, translator, tracker, routeModel, err, durationMs)
}

// responsesStreamError maps a streaming failure: real HTTP error when nothing
// was emitted yet; a protocol failed-event when the stream already started.
func (h *Handler) responsesStreamError(w http.ResponseWriter, translator *responsesStreamTranslator, tracker *usage.Tracker, routeModel string, err error, durationMs int64) {
	slog.Error(fmt.Sprintf("🌊 [RESPONSES-STREAM] %s | %dms | error: %v", routeModel, durationMs, err))
	if tracker != nil {
		tracker.RecordError("stream", routeModel)
	}
	if translator.Emitted() {
		translator.fail(err)
		return
	}
	if provider.IsLocalLimit(err) {
		middleware.TooManyRequestsResponse(w)
		return
	}
	h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
}

// isStreamOptionsRejection detects upstreams that don't accept stream_options.
func isStreamOptionsRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "stream_options") || strings.Contains(msg, "streamoptions")
}

// streamToolAccum accumulates one streamed function call.
type streamToolAccum struct {
	id        string
	name      string
	args      strings.Builder
	addedSent bool
}

// responsesStreamTranslator consumes OpenAI-format SSE written by the router
// and re-emits Responses API events to the real client live (true streaming).
// Upstream status is only forwarded once the first event is ready, so router
// backend failover keeps working while headers are unsent.
type responsesStreamTranslator struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	model       string
	respID      string
	msgID       string
	headerSent  bool
	buf         strings.Builder
	createdSent bool
	itemAdded   bool
	text        strings.Builder
	tools       map[int]*streamToolAccum
	toolOrder   []int
	usage       *models.ResponsesUsage
	upstreamModel string
	finalized   bool
	emittedAny  bool
}

func newResponsesStreamTranslator(w http.ResponseWriter, flusher http.Flusher, model string) *responsesStreamTranslator {
	return &responsesStreamTranslator{
		w:       w,
		flusher: flusher,
		model:   model,
		respID:  newRespID("resp"),
		msgID:   newRespID("msg"),
		tools:   make(map[int]*streamToolAccum),
	}
}

// Header passes through (provider sets SSE headers on us; we send our own).
func (t *responsesStreamTranslator) Header() http.Header { return t.w.Header() }

// WriteHeader records the status but only forwards it with the first event,
// preserving backend failover while nothing was emitted yet.
func (t *responsesStreamTranslator) WriteHeader(status int) { /* held until first emit */ }

// HeaderWritten reports whether bytes hit the real client (failover guard).
func (t *responsesStreamTranslator) HeaderWritten() bool { return t.headerSent }

// Emitted reports whether any event reached the real client.
func (t *responsesStreamTranslator) Emitted() bool { return t.emittedAny }

// Flush forwards to the real client when headers are out.
func (t *responsesStreamTranslator) Flush() {
	if t.headerSent {
		t.flusher.Flush()
	}
}

func (t *responsesStreamTranslator) sendHeaders() {
	if t.headerSent {
		return
	}
	t.w.Header().Set("Content-Type", "text/event-stream")
	t.w.Header().Set("Cache-Control", "no-cache")
	t.w.Header().Set("Connection", "keep-alive")
	t.w.WriteHeader(http.StatusOK)
	t.headerSent = true
}

func (t *responsesStreamTranslator) emit(event string, payload interface{}) {
	data, _ := json.Marshal(payload)
	t.sendHeaders()
	fmt.Fprintf(t.w, "event: %s\ndata: %s\n\n", event, data)
	t.flusher.Flush()
	t.emittedAny = true
}

func (t *responsesStreamTranslator) ensureCreated() {
	if t.createdSent {
		return
	}
	t.createdSent = true
	t.emit("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": map[string]interface{}{"id": t.respID, "object": t.Object(), "status": "in_progress", "model": t.model},
	})
}

func (t *responsesStreamTranslator) Object() string { return "response" }

func (t *responsesStreamTranslator) ensureItemAdded() {
	if t.itemAdded {
		return
	}
	t.itemAdded = true
	t.emit("response.output_item.added", map[string]interface{}{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]interface{}{
			"id": t.msgID, "type": "message", "role": "assistant",
			"content": []interface{}{},
		},
	})
}

// Write consumes raw upstream SSE bytes and re-emits response events.
func (t *responsesStreamTranslator) Write(p []byte) (int, error) {
	t.buf.Write(p)
	for {
		raw := t.buf.String()
		idx := strings.Index(raw, "\n\n")
		if idx < 0 {
			break
		}
		frame := raw[:idx]
		t.buf.Reset()
		t.buf.WriteString(raw[idx+2:])
		t.handleFrame(frame)
		if t.finalized {
			// Drain nothing further; provider will see [DONE]/EOF next.
		}
	}
	return len(p), nil
}

func (t *responsesStreamTranslator) handleFrame(frame string) {
	var dataLines []string
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		// "event:" and ":comment" lines carry no payload for us.
	}
	for _, data := range dataLines {
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			t.finalize()
			continue
		}
		var chunk models.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		t.handleChunk(&chunk)
	}
}

func (t *responsesStreamTranslator) handleChunk(chunk *models.ChatCompletionChunk) {
	if chunk.Model != "" {
		t.upstreamModel = chunk.Model
	}
	if chunk.Usage != nil {
		t.usage = &models.ResponsesUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
	}
	for _, choice := range chunk.Choices {
		if choice.Delta == nil && choice.FinishReason == nil {
			continue
		}
		if choice.Delta != nil {
			if choice.Delta.Content != "" {
				t.ensureCreated()
				t.ensureItemAdded()
				t.text.WriteString(choice.Delta.Content)
				t.emit("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       t.msgID,
					"output_index":  0,
					"content_index": 0,
					"delta":         choice.Delta.Content,
				})
			}
			if len(choice.Delta.ToolCalls) > 0 {
				t.handleToolDeltas(choice.Delta.ToolCalls)
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			t.finalize()
		}
	}
}

func (t *responsesStreamTranslator) handleToolDeltas(raw json.RawMessage) {
	var deltas []map[string]interface{}
	if err := json.Unmarshal(raw, &deltas); err != nil {
		return
	}
	t.ensureCreated()
	for _, d := range deltas {
		idx := 0
		if f, ok := d["index"].(float64); ok {
			idx = int(f)
		}
		acc, ok := t.tools[idx]
		if !ok {
			acc = &streamToolAccum{id: newRespID("fc")}
			t.tools[idx] = acc
			t.toolOrder = append(t.toolOrder, idx)
		}
		if id, ok := d["id"].(string); ok && id != "" {
			acc.id = id
		}
		if fn, ok := d["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				acc.name = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				acc.args.WriteString(args)
			}
		}
		if !acc.addedSent {
			acc.addedSent = true
			t.emit("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": len(t.toolOrder),
				"item": map[string]interface{}{
					"id": acc.id, "type": "function_call",
					"call_id": acc.id, "name": acc.name, "arguments": "",
				},
			})
		} else if fn, ok := d["function"].(map[string]interface{}); ok {
			if args, ok := fn["arguments"].(string); ok && args != "" {
				t.emit("response.function_call_arguments.delta", map[string]interface{}{
					"type":      "response.function_call_arguments.delta",
					"item_id":   acc.id,
					"output_index": len(t.toolOrder),
					"delta":     args,
				})
			}
		}
	}
}

// finalize emits done/completed events exactly once and returns the built object.
func (t *responsesStreamTranslator) finalize() *models.ResponsesResponse {
	if t.finalized {
		return nil
	}
	t.finalized = true
	t.ensureCreated()
	out := &models.ResponsesResponse{
		ID: t.respID, Object: "response", CreatedAt: time.Now().Unix(),
		Model: t.model, Status: "completed", Usage: t.usage,
	}
	if t.itemAdded || t.text.Len() > 0 {
		t.emit("response.output_text.done", map[string]interface{}{
			"type":          "response.output_text.done",
			"item_id":       t.msgID,
			"output_index":  0,
			"content_index": 0,
			"text":          t.text.String(),
		})
		out.Output = append(out.Output, models.ResponseOutputItem{
			Type:    "message",
			ID:      t.msgID,
			Role:    "assistant",
			Content: []models.ResponseOutputPart{{Type: "output_text", Text: t.text.String(), Annotations: []json.RawMessage{}}},
		})
	}
	for _, idx := range t.toolOrder {
		acc := t.tools[idx]
		t.emit("response.function_call_arguments.done", map[string]interface{}{
			"type":      "response.function_call_arguments.done",
			"item_id":   acc.id,
			"output_index": 0,
			"arguments": acc.args.String(),
		})
		out.Output = append(out.Output, models.ResponseOutputItem{
			Type: "function_call", ID: acc.id, CallID: acc.id,
			Name: acc.name, Arguments: acc.args.String(),
		})
	}
	if out.Output == nil {
		out.Output = []models.ResponseOutputItem{}
	}
	t.emit("response.completed", map[string]interface{}{"type": "response.completed", "response": out})
	return out
}

// fail emits a terminal failed event (only valid once headers are out).
func (t *responsesStreamTranslator) fail(err error) {
	if !t.headerSent {
		return
	}
	msg := "upstream error"
	if err != nil {
		msg = err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
	}
	t.emit("response.failed", map[string]interface{}{
		"type":     "response.failed",
		"response": map[string]interface{}{"id": t.respID, "object": t.Object(), "status": "failed", "model": t.model},
		"error":    map[string]interface{}{"message": msg},
	})
}

// writeResponsesStream replays a completed response as SSE response events.
func writeResponsesStream(w http.ResponseWriter, out *models.ResponsesResponse) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, payload interface{}) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	emit("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": map[string]interface{}{"id": out.ID, "object": out.Object, "status": "in_progress", "model": out.Model},
	})

	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			// Emit text in small chunks so clients can render progressively.
			runes := []rune(part.Text)
			for i := 0; i < len(runes); i += 120 {
				end := i + 120
				if end > len(runes) {
					end = len(runes)
				}
				emit("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       item.ID,
					"output_index":  0,
					"content_index": 0,
					"delta":         string(runes[i:end]),
				})
			}
			emit("response.output_text.done", map[string]interface{}{
				"type":          "response.output_text.done",
				"item_id":       item.ID,
				"output_index":  0,
				"content_index": 0,
				"text":          part.Text,
			})
		}
	}

	emit("response.completed", map[string]interface{}{"type": "response.completed", "response": out})
}

// nonEmptyOr returns s or fallback when s is empty.
func nonEmptyOr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

func bytesTrimSpace(b json.RawMessage) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func mustJSONMarshal(s string) json.RawMessage {
	out, _ := json.Marshal(s)
	return out
}
