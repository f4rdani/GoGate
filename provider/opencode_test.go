package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aigateway/models"
)

func TestGenerateOpenCodeSessionID(t *testing.T) {
	for i := 0; i < 50; i++ {
		sid := GenerateOpenCodeSessionID()
		if len(sid) != 30 {
			t.Fatalf("expected session ID length 30, got %d: %q", len(sid), sid)
		}
		if !OpenCodeSessionRegex.MatchString(sid) {
			t.Fatalf("session ID %q did not match canonical OpenCode regex", sid)
		}
	}
}

func TestTranslateOpenCodeSessionID(t *testing.T) {
	// If already canonical, return as-is
	canonical := "ses_0123456789abCdefGhijKlmnOp"
	if tr := TranslateOpenCodeSessionID(canonical, "test"); tr != canonical {
		t.Fatalf("expected canonical session to be unchanged, got %s", tr)
	}

	// Deterministic translation
	raw := "arbitrary-client-session-12345"
	tr1 := TranslateOpenCodeSessionID(raw, "tool1")
	tr2 := TranslateOpenCodeSessionID(raw, "tool1")
	if tr1 != tr2 {
		t.Fatalf("expected deterministic translation, got %s != %s", tr1, tr2)
	}
	if len(tr1) != 30 {
		t.Fatalf("expected translated session length 30, got %d: %s", len(tr1), tr1)
	}
	if !OpenCodeSessionRegex.MatchString(tr1) {
		t.Fatalf("translated session %q did not match canonical OpenCode regex", tr1)
	}

	// Different tools produce different sessions
	tr3 := TranslateOpenCodeSessionID(raw, "tool2")
	if tr1 == tr3 {
		t.Fatalf("expected different sessions for different tools, got same %s", tr1)
	}
}

func TestFormatOpenCodeUserAgent(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", OpenCodeDefaultUA},
		{"Go-http-client/1.1", OpenCodeDefaultUA},
		{"curl/7.68.0", OpenCodeDefaultUA},
		{"opencode/1.16.5", OpenCodeDefaultUA}, // < 1.17 should upgrade
		{"opencode/1.17.0", "opencode/1.17.0"}, // >= 1.17 allowed
		{"opencode/1.18.31", "opencode/1.18.31"},
		{"opencode/2.0.0", "opencode/2.0.0"},
	}

	for _, tc := range tests {
		got := FormatOpenCodeUserAgent(tc.input)
		if got != tc.expected {
			t.Errorf("FormatOpenCodeUserAgent(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestOpenCodeProvider_HeadersInjected(t *testing.T) {
	var capturedUA string
	var capturedSess string
	var capturedSessAlt string
	var capturedAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUA = r.Header.Get("User-Agent")
		capturedSess = r.Header.Get("x-opencode-session")
		capturedSessAlt = r.Header.Get("X-Session-ID")
		capturedAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chat-123","choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer ts.Close()

	base := &BaseProvider{
		name:         "opencode",
		providerType: "opencode",
		baseURL:      ts.URL,
		client:       ts.Client(),
	}
	p := NewOpenAIProvider(base)

	req := &models.ChatCompletionRequest{
		Model: "mimo-v2.5-free",
		Messages: []models.Message{
			{Role: "user", Content: []byte(`"hello"`)},
		},
	}

	_, err := p.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if capturedUA != OpenCodeDefaultUA {
		t.Errorf("expected UA %q, got %q", OpenCodeDefaultUA, capturedUA)
	}
	if !OpenCodeSessionRegex.MatchString(capturedSess) {
		t.Errorf("expected session %q to match canonical regex", capturedSess)
	}
	if capturedSess != capturedSessAlt {
		t.Errorf("expected X-Session-ID %q == x-opencode-session %q", capturedSessAlt, capturedSess)
	}
	if capturedAuth != "Bearer public" {
		t.Errorf("expected Authorization 'Bearer public', got %q", capturedAuth)
	}
}

func TestMiMoProvider_HeadersInjected(t *testing.T) {
	var capturedSource string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSource = r.Header.Get("X-Mimo-Source")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chat-123","choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer ts.Close()

	base := &BaseProvider{
		name:         "mimo",
		providerType: "mimo",
		baseURL:      ts.URL,
		client:       ts.Client(),
		apiKeys:      []*UpstreamKey{{Key: "dummy-mimo-key"}},
	}
	p := NewOpenAIProvider(base)

	req := &models.ChatCompletionRequest{
		Model: "mimo-v2.5-free",
		Messages: []models.Message{
			{Role: "user", Content: []byte(`"hello"`)},
		},
	}

	_, err := p.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if capturedSource != "mimocode-cli" {
		t.Errorf("expected X-Mimo-Source 'mimocode-cli', got %q", capturedSource)
	}
}

func TestGenerateOpenCodeRequestID(t *testing.T) {
	for i := 0; i < 20; i++ {
		reqID := GenerateOpenCodeRequestID()
		if !strings.HasPrefix(reqID, "msg_") {
			t.Fatalf("expected msg_ prefix, got %q", reqID)
		}
		if len(reqID) != 30 {
			t.Fatalf("expected length 30, got %d for %q", len(reqID), reqID)
		}
	}
}

func TestOpenCodeProjectID(t *testing.T) {
	projID := OpenCodeProjectID()
	if len(projID) != 40 {
		t.Fatalf("expected 40-character sha1 project ID, got %d: %q", len(projID), projID)
	}
}

func TestPrepareOpenCodeRequest(t *testing.T) {
	// Empty tools should be populated with defaults
	req := &models.ChatCompletionRequest{
		Model: "mimo-v2.5-free",
		Messages: []models.Message{
			{Role: "user", Content: []byte(`"test"`)},
		},
	}
	prepared := PrepareOpenCodeRequest(req)
	if len(prepared.Tools) == 0 {
		t.Fatalf("expected tools to be populated")
	}
	if string(prepared.ToolChoice) != `"auto"` {
		t.Fatalf("expected tool_choice 'auto', got %s", string(prepared.ToolChoice))
	}

	// Custom tools should be preserved and merged
	customTools := []byte(`[{"type":"function","function":{"name":"custom_tool"}}]`)
	req2 := &models.ChatCompletionRequest{
		Model: "mimo-v2.5-free",
		Tools: customTools,
	}
	prepared2 := PrepareOpenCodeRequest(req2)
	if !strings.Contains(string(prepared2.Tools), "custom_tool") {
		t.Fatalf("expected custom_tool to be preserved in merged tools")
	}
	if !strings.Contains(string(prepared2.Tools), "bash") {
		t.Fatalf("expected bash tool from defaults to be present in merged tools")
	}
}

func TestParseOpenCodeSSEStream(t *testing.T) {
	sseInput := "data: {\"id\":\"gen-1\",\"model\":\"mimo-v2.5-free\",\"created\":1789695000,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Halo \"}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"model\":\"mimo-v2.5-free\",\"created\":1789695000,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"dunia!\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"

	resp, err := ParseOpenCodeSSEStream(strings.NewReader(sseInput), "mimo-v2.5-free")
	if err != nil {
		t.Fatalf("ParseOpenCodeSSEStream failed: %v", err)
	}
	if resp.ID != "gen-1" {
		t.Errorf("expected ID 'gen-1', got %q", resp.ID)
	}
	if resp.Model != "mimo-v2.5-free" {
		t.Errorf("expected Model 'mimo-v2.5-free', got %q", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.ContentString() != "Halo dunia!" {
		t.Errorf("expected content 'Halo dunia!', got %q", resp.Choices[0].Message.ContentString())
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop'")
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 15 {
		t.Errorf("expected usage total tokens 15, got %+v", resp.Usage)
	}
}

func TestIsOpenCodeResponsesModel(t *testing.T) {
	tests := []struct {
		model    string
		expected bool
	}{
		{"muse-spark-1.3-contributor-free", true},
		{"muse-spark-1.2-contributor-free", true},
		{"oc/muse-spark-1.3-contributor-free", true},
		{"opencode/muse-spark-1.2-contributor-free", true},
		{"muse-spark", true},
		{"muse_spark-1.3", true},
		{"mimo-v2.5-free", false},
		{"nemotron-3-ultra-free", false},
		{"gpt-4o", false},
	}

	for _, tt := range tests {
		got := IsOpenCodeResponsesModel(tt.model)
		if got != tt.expected {
			t.Errorf("IsOpenCodeResponsesModel(%q) = %v, want %v", tt.model, got, tt.expected)
		}
	}
}

func TestBuildOpenCodeResponsesRequest(t *testing.T) {
	req := &models.ChatCompletionRequest{
		Model: "oc/muse-spark-1.3-contributor-free",
		Messages: []models.Message{
			{Role: "system", Content: []byte(`"System prompt"`)},
			{Role: "user", Content: []byte(`"Hello"`)},
		},
	}

	body, err := BuildOpenCodeResponsesRequest(req)
	if err != nil {
		t.Fatalf("BuildOpenCodeResponsesRequest failed: %v", err)
	}

	var payload OpenCodeResponsesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}

	if payload.Model != "muse-spark-1.3-contributor-free" {
		t.Errorf("expected model 'muse-spark-1.3-contributor-free', got %q", payload.Model)
	}
	if payload.Instructions != "System prompt" {
		t.Errorf("expected instructions 'System prompt', got %q", payload.Instructions)
	}
	if len(payload.Input) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(payload.Input))
	}
	if payload.Input[0].Role != "user" || len(payload.Input[0].Content) == 0 || payload.Input[0].Content[0].Text != "Hello" {
		t.Errorf("unexpected input content: %+v", payload.Input[0])
	}
	if !payload.Stream || payload.Store {
		t.Errorf("expected stream=true, store=false")
	}
	if len(payload.Tools) != 2 {
		t.Errorf("expected 2 default tools injected, got %d", len(payload.Tools))
	}
	if payload.ToolChoice != "auto" {
		t.Errorf("expected tool_choice 'auto', got %v", payload.ToolChoice)
	}
}

func TestParseOpenCodeResponsesStream(t *testing.T) {
	sseInput := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-123\",\"model\":\"muse-spark-1.3-contributor-free\",\"created_at\":1789738000}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Halo \"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"dunia!\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-123\",\"model\":\"muse-spark-1.3-contributor-free\",\"usage\":{\"input_tokens\":50,\"output_tokens\":20,\"total_tokens\":70}}}\n\n"

	resp, err := ParseOpenCodeResponsesStream(strings.NewReader(sseInput), "muse-spark-1.3-contributor-free")
	if err != nil {
		t.Fatalf("ParseOpenCodeResponsesStream failed: %v", err)
	}
	if resp.ID != "resp-123" {
		t.Errorf("expected ID 'resp-123', got %q", resp.ID)
	}
	if resp.Choices[0].Message.ContentString() != "Halo dunia!" {
		t.Errorf("expected content 'Halo dunia!', got %q", resp.Choices[0].Message.ContentString())
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 70 {
		t.Errorf("expected total tokens 70, got %+v", resp.Usage)
	}
}

type mockFlusherResponseWriter struct {
	http.ResponseWriter
	header http.Header
	buf    strings.Builder
}

func newMockFlusherResponseWriter() *mockFlusherResponseWriter {
	return &mockFlusherResponseWriter{header: make(http.Header)}
}

func (m *mockFlusherResponseWriter) Header() http.Header {
	return m.header
}

func (m *mockFlusherResponseWriter) Write(b []byte) (int, error) {
	return m.buf.Write(b)
}

func (m *mockFlusherResponseWriter) WriteHeader(statusCode int) {}

func (m *mockFlusherResponseWriter) Flush() {}

func TestPipeOpenCodeResponsesSSE(t *testing.T) {
	sseInput := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi \"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"there!\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"

	w := newMockFlusherResponseWriter()
	err := PipeOpenCodeResponsesSSE(strings.NewReader(sseInput), w, w, "muse-spark-1.3-contributor-free")
	if err != nil {
		t.Fatalf("PipeOpenCodeResponsesSSE failed: %v", err)
	}

	out := w.buf.String()
	if !strings.Contains(out, "chatcmpl-") {
		t.Errorf("expected chatcmpl- ID in chunks")
	}
	if !strings.Contains(out, "Hi ") || !strings.Contains(out, "there!") {
		t.Errorf("expected delta chunks to be written")
	}
	if !strings.Contains(out, "data: [DONE]\n\n") {
		t.Errorf("expected [DONE] delimiter at end")
	}
}

