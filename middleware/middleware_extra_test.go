package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoggingMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	})
	w := httptest.NewRecorder()
	LoggingMiddleware(next).ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != http.StatusTeapot || w.Body.String() != "hi" {
		t.Fatalf("passthrough broken: %d %q", w.Code, w.Body.String())
	}
}

func TestCORSMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	// Preflight on public path short-circuits.
	w := httptest.NewRecorder()
	CORSMiddleware(next).ServeHTTP(w, httptest.NewRequest("OPTIONS", "/v1/models", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight must 204: %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("CORS headers missing on public path")
	}

	// Admin paths get no CORS headers.
	w2 := httptest.NewRecorder()
	CORSMiddleware(next).ServeHTTP(w2, httptest.NewRequest("GET", "/admin/keys", nil))
	if w2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("admin path must not expose CORS")
	}
	if w2.Code != 200 {
		t.Fatalf("admin must pass through: %d", w2.Code)
	}
}

func TestBodyLimitMiddleware(t *testing.T) {
	// Oversized admin body must be rejected by MaxBytesReader.
	big := strings.NewReader(strings.Repeat("a", 1<<20+100))
	req := httptest.NewRequest("POST", "/admin/providers", big)
	w := httptest.NewRecorder()
	BodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			t.Error("expected body-too-large error")
		}
		w.WriteHeader(200)
	})).ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("handler must run: %d", w.Code)
	}

	// Small bodies pass through untouched.
	small := strings.NewReader(`{"a":1}`)
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", small)
	w2 := httptest.NewRecorder()
	BodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != `{"a":1}` {
			t.Errorf("small body must pass: %v %q", err, b)
		}
		w.WriteHeader(200)
	})).ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("handler must run: %d", w2.Code)
	}
}

func TestLimiterBasics(t *testing.T) {
	l := NewConcurrencyLimiter(2)
	if l.Capacity() != 2 {
		t.Fatalf("capacity: %d", l.Capacity())
	}
	if !l.AcquireGlobal() || !l.AcquireGlobal() {
		t.Fatal("two acquires must succeed")
	}
	if l.ActiveCount() != 2 {
		t.Fatalf("active: %d", l.ActiveCount())
	}
	if l.AcquireGlobal() {
		t.Fatal("third acquire must fail")
	}
	l.ReleaseGlobal()
	if !l.AcquireGlobal() {
		t.Fatal("acquire after release must succeed")
	}
	l.ReleaseGlobal()
	l.ReleaseGlobal()
}

func TestErrorResponses(t *testing.T) {
	w := httptest.NewRecorder()
	TooManyRequestsResponse(w)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("429: %d", w.Code)
	}
	w2 := httptest.NewRecorder()
	QueueFullResponse(w2)
	if w2.Code != http.StatusTooManyRequests || !strings.Contains(w2.Body.String(), "queue") {
		t.Fatalf("queue_full: %d %s", w2.Code, w2.Body.String())
	}
}

func TestTokensaverCompressVariants(t *testing.T) {
	diff := compressDiff([]string{" a", "-b", "+c"}, 1000)
	if diff == "" {
		t.Fatal("diff must produce output")
	}
	tree := compressTree([]string{"root", "  child", "    leaf"})
	if tree == "" {
		t.Fatal("tree must produce output")
	}
	stack := compressStackTrace([]string{"Error: x", "    at foo (a.js:1:2)", "    at foo (a.js:1:2)", "    at bar (b.js:3:4)"})
	if stack == "" {
		t.Fatal("stack must produce output")
	}
	if got := truncateMiddle("0123456789", 6); len(got) > len("0123456789") {
		t.Fatalf("truncate must shorten: %q", got)
	}
	if got := truncateMiddle("abc", 100); got != "abc" {
		t.Fatalf("short strings untouched: %q", got)
	}
	_ = time.Second
}
