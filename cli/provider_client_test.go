package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Anthropic key tests must use the caller-supplied configured model,
// never a hardcoded one.
func TestTestAPIKeyAnthropicUsesConfiguredModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	ok, count, err := testAPIKey(srv.URL, "sk-x", "anthropic", []string{"my-configured-model"})
	if err != nil || !ok {
		t.Fatalf("expected success, got ok=%v err=%v", ok, err)
	}
	if gotModel != "my-configured-model" {
		t.Fatalf("expected configured model to be tested, got %q", gotModel)
	}
	if count != 1 {
		t.Fatalf("expected count 1, got %d", count)
	}
}

func TestTestAPIKeyAnthropicNeedsConfiguredModel(t *testing.T) {
	_, _, err := testAPIKey("http://127.0.0.1:9", "sk-x", "anthropic", nil)
	if err == nil {
		t.Fatal("expected error when no configured models exist")
	}
}

// Cloudflare key tests must pick the test target from the live catalog.
func TestTestAPIKeyCloudflareUsesLiveCatalog(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":true,"result":[{"name":"zz-live-catalog-model"}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	ok, _, err := testAPIKey(srv.URL, "plain-token", "cloudflare", nil)
	if err != nil || !ok {
		t.Fatalf("expected success, got ok=%v err=%v", ok, err)
	}
	if gotModel != "zz-live-catalog-model" {
		t.Fatalf("expected live catalog model to be tested, got %q", gotModel)
	}
}
