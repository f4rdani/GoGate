package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/middleware"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

func TestChatCompletionStreamDirect(t *testing.T) {
	registry := provider.NewRegistry()
	mp := newMockProvider("p1")
	registry.Register("p1", mp)

	cfg := &config.Config{
		Models: []config.ModelConfig{{Name: "m", Provider: "p1", Model: "m"}},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	err = r.ChatCompletionStream(context.Background(), "m",
		&models.ChatCompletionRequest{Model: "m"}, w, w)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE headers missing: %q", ct)
	}
	if !strings.Contains(w.Body.String(), "hello") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("SSE body broken: %q", w.Body.String())
	}
	if mp.callCount.Load() != 1 {
		t.Fatalf("calls: %d", mp.callCount.Load())
	}
}

func TestChatCompletionStreamError(t *testing.T) {
	registry := provider.NewRegistry()
	mp := newMockProvider("p1")
	mp.errors["m"] = errStreamBoom()
	registry.Register("p1", mp)

	cfg := &config.Config{
		Models: []config.ModelConfig{{Name: "m", Provider: "p1", Model: "m"}},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	tracker := &headerTracker{ResponseWriter: w}
	err = r.ChatCompletionStream(context.Background(), "m",
		&models.ChatCompletionRequest{Model: "m"}, tracker, w)
	if err == nil {
		t.Fatal("expected stream error")
	}
	if tracker.written {
		t.Fatal("headers must not leak on pre-write failure")
	}
}

type headerTracker struct {
	http.ResponseWriter
	written bool
}

func (h *headerTracker) WriteHeader(code int) {
	h.written = true
	h.ResponseWriter.WriteHeader(code)
}

func (h *headerTracker) HeaderWritten() bool { return h.written }

type errStreamBoomT struct{}

func (errStreamBoomT) Error() string { return "boom" }

func errStreamBoom() error { return errStreamBoomT{} }

func TestRouterSaturationFailsOver(t *testing.T) {
	registry := provider.NewRegistry()
	mp := newMockProvider("p1")
	registry.Register("p1", mp)

	cfg := &config.Config{
		Models: []config.ModelConfig{{Name: "m", Provider: "p1", Model: "m"}},
	}
	r, err := NewRouter(cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	// Saturate the only provider slot.
	r.SetLimiter(middleware.NewConcurrencyLimiterWithQueue(10, 10, 1, 0, 1000000000))
	if !r.limiter.AcquireProvider("p1") {
		t.Fatal("setup acquire failed")
	}
	defer r.limiter.ReleaseProvider("p1")

	_, _, err = r.ChatCompletion(context.Background(), "m",
		&models.ChatCompletionRequest{Model: "m"})
	if err == nil || !provider.IsSaturationError(err) {
		t.Fatalf("expected saturation error, got %v", err)
	}
	if mp.callCount.Load() != 0 {
		t.Fatalf("saturated provider must not be called: %d", mp.callCount.Load())
	}
}

func TestRouterRegistryAccessor(t *testing.T) {
	registry := provider.NewRegistry()
	r, err := NewRouter(&config.Config{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if r.Registry() != registry {
		t.Fatal("Registry() must return the registry")
	}
	r.SetLimiter(nil) // must not panic on later use
}
