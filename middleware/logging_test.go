package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoggingMiddlewarePassthrough(t *testing.T) {
	for _, withBodies := range []bool{false, true} {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: hello\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		})
		req := httptest.NewRequest("GET", "/v1/models", nil)
		w := httptest.NewRecorder()
		LoggingMiddlewareWithBodies(next, withBodies).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("bodies=%v: expected 200, got %d", withBodies, w.Code)
		}
		if w.Body.String() != "data: hello\n\n" {
			t.Fatalf("bodies=%v: body must pass through verbatim: %q", withBodies, w.Body.String())
		}
		if !w.Flushed {
			t.Fatalf("bodies=%v: flusher must keep working (streaming)", withBodies)
		}
	}
}
