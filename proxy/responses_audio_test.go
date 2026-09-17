package proxy

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aigateway/auth"
	"github.com/aigateway/cache"
	"github.com/aigateway/config"
	"github.com/aigateway/middleware"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
	"github.com/aigateway/router"
	"github.com/aigateway/usage"
)

// chatUpstream serves a fixed chat completion for any POST.
func chatUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func setupResponsesHandler(t *testing.T, srv *httptest.Server, gatewayModels []string) *Handler {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"sk-up"}},
		},
		Models: []config.ModelConfig{
			{Name: "gpt-4o", Provider: "openai", Model: "gpt-4o"},
		},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-valid-key", Name: "User Key", AllowedModels: gatewayModels, RateLimit: 100},
		},
	}
	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("openai", p)
	r, _ := router.NewRouter(cfg, reg)
	ks := auth.NewKeyStore(cfg.APIKeys)
	h := NewHandler(r, ks, middleware.NewConcurrencyLimiter(10), config.TokenSaverConfig{})
	h.SetCache(cache.New(100, 1000000000))
	return h
}

const chatOKBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`

func TestHandleResponsesStringInput(t *testing.T) {
	srv := chatUpstream(t, chatOKBody)
	defer srv.Close()
	h := setupResponsesHandler(t, srv, []string{"gpt-4o"})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"Say hi"}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out models.ResponsesResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("unexpected response envelope: %+v", out)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" {
		t.Fatalf("expected one message item: %+v", out.Output)
	}
	if out.Output[0].Content[0].Text != "hello there" {
		t.Fatalf("unexpected text: %+v", out.Output[0].Content)
	}
	if out.Usage == nil || out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 2 {
		t.Fatalf("usage must map prompt/completion tokens: %+v", out.Usage)
	}
}

func TestHandleResponsesStreamEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseStreamBody))
	}))
	defer srv.Close()
	h := setupResponsesHandler(t, srv, []string{"gpt-4o"})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"Say hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.output_text.done", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing SSE event %q in:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"delta":"Hel"`) || !strings.Contains(body, `"delta":"lo"`) {
		t.Fatalf("deltas must stream live:\n%s", body)
	}
}

func TestHandleResponsesToolsMapping(t *testing.T) {
	var gotTools string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var chatReq struct {
			Tools json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&chatReq)
		gotTools = string(chatReq.Tools)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(chatOKBody))
	}))
	defer srv.Close()
	h := setupResponsesHandler(t, srv, []string{"gpt-4o"})

	payload := `{"model":"gpt-4o","input":"x","tools":[{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"}}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotTools, `"get_weather"`) || !strings.Contains(gotTools, `"type":"function"`) {
		t.Fatalf("tools must map to chat shape, got %s", gotTools)
	}
}

func TestHandleResponsesValidation(t *testing.T) {
	srv := chatUpstream(t, chatOKBody)
	defer srv.Close()
	h := setupResponsesHandler(t, srv, []string{"other-model"})

	// Missing auth
	w := httptest.NewRecorder()
	h.HandleResponses(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// Forbidden model
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"x"}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w2 := httptest.NewRecorder()
	h.HandleResponses(w2, req)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w2.Code)
	}

	// Bad input shape
	hOK := setupResponsesHandler(t, srv, []string{"gpt-4o"})
	req3 := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":[{"type":"teleport"}]}`))
	req3.Header.Set("Authorization", "Bearer sk-valid-key")
	w3 := httptest.NewRecorder()
	hOK.HandleResponses(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported item, got %d", w3.Code)
	}
}

func TestHandleAudioSpeechPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-up" {
			t.Errorf("upstream auth must be swapped, got %q", got)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("FAKEAUDIO"))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"sk-up"}, Models: []string{"tts-1"}},
		},
		Models:  []config.ModelConfig{{Name: "tts-1", Provider: "openai", Model: "tts-1"}},
		APIKeys: []config.APIKeyConfig{{Key: "sk-valid-key", Name: "k", AllowedModels: []string{"tts-1"}}},
	}
	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("openai", p)
	r, _ := router.NewRouter(cfg, reg)
	h := NewHandler(r, auth.NewKeyStore(cfg.APIKeys), middleware.NewConcurrencyLimiter(10), config.TokenSaverConfig{})

	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"hi","voice":"alloy"}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleAudioSpeech(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "FAKEAUDIO" || w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("audio bytes must proxy verbatim: %q %q", w.Body.String(), w.Header().Get("Content-Type"))
	}
}

func TestHandleAudioTranscriptionsPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upstream must receive multipart: %v", err)
		}
		if r.FormValue("model") != "whisper-1" {
			t.Errorf("model field must survive, got %q", r.FormValue("model"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"hello"}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"sk-up"}, Models: []string{"whisper-1"}},
		},
		Models:  []config.ModelConfig{{Name: "whisper-1", Provider: "openai", Model: "whisper-1"}},
		APIKeys: []config.APIKeyConfig{{Key: "sk-valid-key", Name: "k", AllowedModels: []string{"whisper-1"}}},
	}
	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("openai", p)
	r, _ := router.NewRouter(cfg, reg)
	h := NewHandler(r, auth.NewKeyStore(cfg.APIKeys), middleware.NewConcurrencyLimiter(10), config.TokenSaverConfig{})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper-1")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write([]byte("WAVEDATA"))
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleAudioTranscriptions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"hello"`) {
		t.Fatalf("transcription must proxy: %s", w.Body.String())
	}
}

func TestMaxCompletionTokensNormalized(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(chatOKBody))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"sk-up"}},
		},
		Models:  []config.ModelConfig{{Name: "gpt-4o", Provider: "openai", Model: "gpt-4o"}},
		APIKeys: []config.APIKeyConfig{{Key: "sk-valid-key", Name: "k", AllowedModels: []string{"gpt-4o"}}},
	}
	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("openai", p)
	r, _ := router.NewRouter(cfg, reg)
	h := NewHandler(r, auth.NewKeyStore(cfg.APIKeys), middleware.NewConcurrencyLimiter(10), config.TokenSaverConfig{})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":150}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleChatCompletion(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotBody, `"max_tokens":150`) {
		t.Fatalf("upstream must receive max_tokens: %s", gotBody)
	}
	if strings.Contains(gotBody, "max_completion_tokens") {
		t.Fatalf("max_completion_tokens must be stripped upstream: %s", gotBody)
	}
}

const sseStreamBody = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\ndata: [DONE]\n\n"

func TestTranslatorTrueStreaming(t *testing.T) {
	w := httptest.NewRecorder()
	tr := newResponsesStreamTranslator(w, w, "gpt-4o")

	// Feed in odd-sized chunks (splitting mid-line) to prove incremental
	// buffering works.
	raw := []byte(sseStreamBody)
	for i := 0; i < len(raw); i += 37 {
		end := i + 37
		if end > len(raw) {
			end = len(raw)
		}
		if _, err := tr.Write(raw[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if !tr.Emitted() || !tr.HeaderWritten() {
		t.Fatal("events must reach the client")
	}
	body := w.Body.String()
	for _, want := range []string{
		"event: response.created", "event: response.output_text.delta",
		"event: response.output_text.done", "event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"delta":"Hel"`) || !strings.Contains(body, `"delta":"lo"`) {
		t.Fatalf("deltas must stream live:\n%s", body)
	}
	if tr.usage == nil || tr.usage.InputTokens != 5 || tr.usage.OutputTokens != 2 {
		t.Fatalf("usage must be captured: %+v", tr.usage)
	}
	if tr.upstreamModel != "gpt-4o" {
		t.Fatalf("upstream model must be captured: %q", tr.upstreamModel)
	}
	if n := strings.Count(w.Body.String(), "event: response.completed"); n != 1 {
		t.Fatalf("completed must be emitted exactly once, got %d", n)
	}
	if tr.finalize() != nil {
		t.Fatal("finalize after [DONE] must be a no-op")
	}
}

func TestHandleResponsesTrueStream(t *testing.T) {
	var sawStreamOptions bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var chatReq struct {
			StreamOptions json.RawMessage `json:"stream_options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&chatReq)
		sawStreamOptions = len(chatReq.StreamOptions) > 0
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseStreamBody))
	}))
	defer srv.Close()
	h := setupResponsesHandler(t, srv, []string{"gpt-4o"})
	tr := usage.NewTracker()
	h.SetTracker(tr)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !sawStreamOptions {
		t.Fatal("upstream must be asked for usage via stream_options")
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, `"delta":"Hel"`) {
		t.Fatalf("live events expected:\n%s", body)
	}
	stats := tr.GetStats()
	snap, ok := stats.ByModel["stream/gpt-4o"]
	if !ok || snap.TotalTokens != 7 {
		t.Fatalf("stream usage must be recorded: %+v", stats.ByModel)
	}
}

func TestHandleResponsesStreamDowngrade(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var chatReq struct {
			StreamOptions json.RawMessage `json:"stream_options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&chatReq)
		if len(chatReq.StreamOptions) > 0 {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"unsupported field: stream_options"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(chatOKBody))
	}))
	defer srv.Close()
	h := setupResponsesHandler(t, srv, []string{"gpt-4o"})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 via replay downgrade, got %d: %s", w.Code, w.Body.String())
	}
	if hits != 2 {
		t.Fatalf("expected stream attempt + replay attempt, got %d", hits)
	}
	if !strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatalf("replay must emit events:\n%s", w.Body.String())
	}
}

func TestHandleResponsesStreamTools(t *testing.T) {
	sse := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"x\\\"}\"}}]}}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer srv.Close()
	h := setupResponsesHandler(t, srv, []string{"gpt-4o"})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "get_weather"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestIsTokenSaverBypassed(t *testing.T) {
	for _, v := range []string{"off", "OFF", "0", "false", "disabled", " off "} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("X-Token-Saver", v)
		if !isTokenSaverBypassed(r) {
			t.Fatalf("expected bypass for %q", v)
		}
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if isTokenSaverBypassed(r) {
		t.Fatal("absent header must not bypass")
	}
}
