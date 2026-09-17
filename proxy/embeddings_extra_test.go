package proxy

import (
	"encoding/json"
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

func setupEmbeddingsHandler(t *testing.T, srv *httptest.Server) *Handler {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", BaseURL: srv.URL, APIKeys: []string{"sk-up"}},
		},
		Models: []config.ModelConfig{
			{Name: "text-embedding-3", Provider: "openai", Model: "text-embedding-3"},
		},
		APIKeys: []config.APIKeyConfig{
			{Key: "sk-valid-key", Name: "k", AllowedModels: []string{"text-embedding-3"}},
		},
	}
	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("openai", p)
	r, _ := router.NewRouter(cfg, reg)
	h := NewHandler(r, auth.NewKeyStore(cfg.APIKeys), middleware.NewConcurrencyLimiter(10), config.TokenSaverConfig{})
	h.SetCache(cache.New(100, 1000000000))
	h.SetTracker(usage.NewTracker())
	return h
}

const embedOKBody = `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"text-embedding-3","usage":{"prompt_tokens":5,"total_tokens":5}}`

func TestHandleEmbeddingsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(embedOKBody))
	}))
	defer srv.Close()
	h := setupEmbeddingsHandler(t, srv)

	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"text-embedding-3","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w := httptest.NewRecorder()
	h.HandleEmbeddings(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out models.EmbeddingsResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil || len(out.Data) != 1 {
		t.Fatalf("bad embeddings response: %v %s", err, w.Body.String())
	}
}

func TestHandleEmbeddingsValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	h := setupEmbeddingsHandler(t, srv)

	// Missing auth.
	w := httptest.NewRecorder()
	h.HandleEmbeddings(w, httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401: %d", w.Code)
	}

	// Missing model.
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-valid-key")
	w2 := httptest.NewRecorder()
	h.HandleEmbeddings(w2, req)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400: %d", w2.Code)
	}

	// Upstream failure → 502.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("boom"))
	}))
	defer srv2.Close()
	h2 := setupEmbeddingsHandler(t, srv2)
	// Rebuild with srv2 URL: setup helper already did; call directly.
	req3 := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"text-embedding-3","input":"x"}`))
	req3.Header.Set("Authorization", "Bearer sk-valid-key")
	w3 := httptest.NewRecorder()
	h2.HandleEmbeddings(w3, req3)
	if w3.Code != http.StatusBadGateway {
		t.Fatalf("expected 502: %d %s", w3.Code, w3.Body.String())
	}
	_ = h
}

func TestHandlerAccessorsAndUpdate(t *testing.T) {
	h, _ := setupTestHandler()
	if h.GetTracker() != nil {
		t.Fatal("tracker starts nil")
	}
	if h.GetCache() == nil || h.GetLimiter() == nil {
		t.Fatal("cache/limiter must exist")
	}
	tr := usage.NewTracker()
	h.SetTracker(tr)
	if h.GetTracker() != tr {
		t.Fatal("tracker must swap")
	}

	r, _ := router.NewRouter(&config.Config{}, provider.NewRegistry())
	h.UpdateConfig(r, auth.NewKeyStore(nil), nil, nil)
}

func TestResponseTrackerBits(t *testing.T) {
	w := httptest.NewRecorder()
	rt := &responseTracker{ResponseWriter: w}
	if rt.HeaderWritten() {
		t.Fatal("fresh tracker unwritten")
	}
	rt.WriteHeader(201)
	if !rt.HeaderWritten() || w.Code != 201 {
		t.Fatalf("header tracking: %v %d", rt.HeaderWritten(), w.Code)
	}
	rt.Flush() // must not panic without flusher assertion issues
}

func TestGetDynamicModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"m-free"},{"id":"x-free"},{"id":"paid-pro"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "oc", Type: "opencode", BaseURL: srv.URL},
		},
	}
	reg := provider.NewRegistry()
	p, _ := provider.NewProviderFromConfig(cfg.Providers[0])
	reg.Register("oc", p)
	r, _ := router.NewRouter(cfg, reg)
	h := NewHandler(r, auth.NewKeyStore(nil), middleware.NewConcurrencyLimiter(10), config.TokenSaverConfig{})

	got := h.getDynamicModels(p)
	if len(got) != 3 || got[0] != "oc/auto" || got[1] != "oc/m-free" {
		t.Fatalf("must prepend virtual auto + filter paid: %v", got)
	}
	// Second call serves the 5-minute cache without upstream.
	got2 := h.getDynamicModels(p)
	if len(got2) != 3 {
		t.Fatalf("cached call broken: %v", got2)
	}
}

func TestResponsesContentToChat(t *testing.T) {
	// String passthrough.
	out, err := responsesContentToChat(json.RawMessage(`"hi"`))
	if err != nil || string(out) != `"hi"` {
		t.Fatalf("string: %s %v", out, err)
	}
	// Empty → empty string JSON.
	out, err = responsesContentToChat(json.RawMessage(``))
	if err != nil || string(out) != `""` {
		t.Fatalf("empty: %s %v", out, err)
	}
	// Parts with image URL.
	out, err = responsesContentToChat(json.RawMessage(`[{"type":"input_text","text":"see"},{"type":"input_image","image_url":"http://x/i.png"}]`))
	if err != nil || !strings.Contains(string(out), "image_url") {
		t.Fatalf("parts: %s %v", out, err)
	}
	// Unknown part type errors.
	if _, err := responsesContentToChat(json.RawMessage(`[{"type":"teleport"}]`)); err == nil {
		t.Fatal("unknown part must error")
	}
	// Invalid JSON errors.
	if _, err := responsesContentToChat(json.RawMessage(`{oops`)); err == nil {
		t.Fatal("invalid json must error")
	}
}

func TestResponsesStreamErrorAndHelpers(t *testing.T) {
	if nonEmptyOr("a", "b") != "a" || nonEmptyOr("", "b") != "b" {
		t.Fatal("nonEmptyOr broken")
	}
	w := httptest.NewRecorder()
	tr := newResponsesStreamTranslator(w, w, "m")
	// Not emitted → real HTTP error.
	h, _ := setupTestHandler()
	h.responsesStreamError(w, tr, nil, "m", errTest("boom"), 5)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("must 502 when silent: %d", w.Code)
	}
	// Emitted → failed event, no status change possible.
	w2 := httptest.NewRecorder()
	tr2 := newResponsesStreamTranslator(w2, w2, "m")
	tr2.emit("response.created", map[string]interface{}{"x": 1})
	h.responsesStreamError(w2, tr2, nil, "m", errTest("late"), 5)
	if !strings.Contains(w2.Body.String(), "response.failed") {
		t.Fatalf("must emit failed event: %s", w2.Body.String())
	}
	tr2.fail(nil) // nil-safe path
	// WriteHeader held until first emit.
	w3 := httptest.NewRecorder()
	tr3 := newResponsesStreamTranslator(w3, w3, "m")
	tr3.WriteHeader(200)
	if tr3.HeaderWritten() {
		t.Fatal("headers must be held")
	}
	tr3.Flush() // no-op before headers
	if w3.Code != 200 {
		t.Fatalf("recorder default must stay 200: %d", w3.Code)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
