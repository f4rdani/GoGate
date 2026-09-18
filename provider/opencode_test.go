package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
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
