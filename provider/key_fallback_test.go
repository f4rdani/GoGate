package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

// keyScriptUpstream fails with 429 when the Authorization header contains
// failSubstr, otherwise returns a minimal valid chat completion.
func keyScriptUpstream(hits *atomic.Int64, failSubstr string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failSubstr != "" && strings.Contains(r.Header.Get("Authorization"), failSubstr) {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"rate limit"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}))
}

func keyFallbackProvider(t *testing.T, srv *httptest.Server, keys []string) Provider {
	t.Helper()
	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "p1", Type: "openai", BaseURL: srv.URL, APIKeys: keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func keyFallbackReq() *models.ChatCompletionRequest {
	return &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	}
}

func TestProviderKeyFallbackSuccess(t *testing.T) {
	var hits atomic.Int64
	srv := keyScriptUpstream(&hits, "key-1")
	defer srv.Close()

	resp, err := keyFallbackProvider(t, srv, []string{"key-1", "key-2"}).ChatCompletion(context.Background(), keyFallbackReq())
	if err != nil {
		t.Fatalf("expected success via second key, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits (key-1 429, key-2 ok), got %d", hits.Load())
	}
}

func TestProviderKeyExhaustedKeepsLastStatus(t *testing.T) {
	var hits atomic.Int64
	srv := keyScriptUpstream(&hits, "key-")
	defer srv.Close()

	_, err := keyFallbackProvider(t, srv, []string{"key-1", "key-2"}).ChatCompletion(context.Background(), keyFallbackReq())
	pe, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T (%v)", err, err)
	}
	if pe.StatusCode != 429 {
		t.Fatalf("expected original 429 status, got %d", pe.StatusCode)
	}
	if !pe.KeyExhausted {
		t.Fatal("expected KeyExhausted=true after all keys failed")
	}
	if hits.Load() != 2 {
		t.Fatalf("expected both keys tried (2 hits), got %d", hits.Load())
	}
}

func TestProviderKeyFallbackStream(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.Header.Get("Authorization"), "key-1") {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"rate limit"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"1\"}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	w := httptest.NewRecorder()
	err := keyFallbackProvider(t, srv, []string{"key-1", "key-2"}).ChatCompletionStream(context.Background(), keyFallbackReq(), w, w)
	if err != nil {
		t.Fatalf("expected stream success via second key, got %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits, got %d", hits.Load())
	}
}
