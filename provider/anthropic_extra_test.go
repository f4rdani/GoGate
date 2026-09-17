package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

func anthropicTestProvider(t *testing.T, srv *httptest.Server, keys []string) Provider {
	t.Helper()
	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "anth", Type: "anthropic", BaseURL: srv.URL, APIKeys: keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func anthropicReq() *models.ChatCompletionRequest {
	return &models.ChatCompletionRequest{
		Model:    "claude-x",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
}

const anthropicOKBody = `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":3}}`

func TestAnthropicKeyFallback(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); strings.Contains(got, "k1") {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"slow"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(anthropicOKBody))
	}))
	defer srv.Close()

	resp, err := anthropicTestProvider(t, srv, []string{"k1", "k2"}).ChatCompletion(context.Background(), anthropicReq())
	if err != nil {
		t.Fatalf("expected fallback success: %v", err)
	}
	if resp == nil || hits.Load() != 2 {
		t.Fatalf("expected 2 hits, got %d", hits.Load())
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 4 {
		t.Fatalf("usage must translate: %+v", resp.Usage)
	}
}

func TestAnthropicKeyExhausted(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"busy"}`))
	}))
	defer srv.Close()

	_, err := anthropicTestProvider(t, srv, []string{"k1", "k2"}).ChatCompletion(context.Background(), anthropicReq())
	pe, ok := err.(*ProviderError)
	if !ok || pe.StatusCode != 503 || !pe.KeyExhausted {
		t.Fatalf("expected exhausted 503: %+v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("both keys must be tried: %d", hits.Load())
	}
}

func TestAnthropicStreamFallback(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.Header.Get("x-api-key"), "k1") {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"slow"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-x\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	w := httptest.NewRecorder()
	err := anthropicTestProvider(t, srv, []string{"k1", "k2"}).ChatCompletionStream(context.Background(), anthropicReq(), w, w)
	if err != nil {
		t.Fatalf("stream fallback failed: %v", err)
	}
	if hits.Load() != 2 || !strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("hits=%d body=%q", hits.Load(), w.Body.String())
	}
}

func TestTranslateRequestSystemAndTools(t *testing.T) {
	maxTok := 50
	p, _ := NewProviderFromConfig(config.ProviderConfig{Name: "a", Type: "anthropic", BaseURL: "http://x", APIKeys: []string{"k"}})
	ap := p.(*AnthropicProvider)
	anthReq, err := ap.translateRequest(&models.ChatCompletionRequest{
		Model:     "m",
		Messages:  []models.Message{{Role: "system", Content: []byte(`"sys"`)}, {Role: "user", Content: []byte(`"hi"`)}},
		MaxTokens: &maxTok,
		Stop:      []byte(`["STOP"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if anthReq.System != "sys" || anthReq.MaxTokens != 50 {
		t.Fatalf("bad translation: %+v", anthReq)
	}
	if len(anthReq.StopSequences) != 1 || anthReq.StopSequences[0] != "STOP" {
		t.Fatalf("stop must map: %+v", anthReq.StopSequences)
	}
	if len(anthReq.Messages) != 1 || anthReq.Messages[0].Role != "user" {
		t.Fatalf("system must be extracted: %+v", anthReq.Messages)
	}
}

func TestTranslateResponseToolUse(t *testing.T) {
	p, _ := NewProviderFromConfig(config.ProviderConfig{Name: "a", Type: "anthropic", BaseURL: "http://x", APIKeys: []string{"k"}})
	ap := p.(*AnthropicProvider)
	stop := "tool_use"
	out := ap.translateResponse(&models.AnthropicResponse{
		ID: "m1", Model: "m", StopReason: &stop,
		Content: []models.AnthropicContentBlock{
			{Type: "tool_use", ID: "t1", Name: "fn", Input: json.RawMessage(`{"a":1}`)},
		},
		Usage: models.AnthropicUsage{InputTokens: 1, OutputTokens: 2},
	})
	if out.Choices[0].FinishReason == nil || *out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish must map: %+v", out.Choices[0].FinishReason)
	}
	if !strings.Contains(string(out.Choices[0].Message.ToolCalls), "fn") {
		t.Fatalf("tool calls must map: %s", out.Choices[0].Message.ToolCalls)
	}
	if out.Usage.TotalTokens != 3 {
		t.Fatalf("usage must sum: %+v", out.Usage)
	}
}
