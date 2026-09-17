package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// maxLoggedBodyBytes caps captured response bodies in debug body logging.
const maxLoggedBodyBytes = 4096

// statusWriter wraps http.ResponseWriter to capture the status code and,
// optionally, a truncated prefix of the response body for debug logging.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	capture     *bytes.Buffer
}

func (sw *statusWriter) WriteHeader(status int) {
	if !sw.wroteHeader {
		sw.status = status
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(status)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.status = 200
		sw.wroteHeader = true
	}
	if sw.capture != nil && sw.capture.Len() < maxLoggedBodyBytes {
		room := maxLoggedBodyBytes - sw.capture.Len()
		if len(b) > room {
			sw.capture.Write(b[:room])
		} else {
			sw.capture.Write(b)
		}
	}
	return sw.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for streaming support.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// LoggingMiddleware logs every HTTP request with method, path, status, and duration.
func LoggingMiddleware(next http.Handler) http.Handler {
	return LoggingMiddlewareWithBodies(next, false)
}

// LoggingMiddlewareWithBodies is LoggingMiddleware plus optional truncated
// response-body capture at debug level (server `log_bodies: true`).
// Only response bodies are captured — request bodies stay untouched so the
// downstream body-limit middleware keeps enforcing size caps.
func LoggingMiddlewareWithBodies(next http.Handler, logBodies bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		if logBodies {
			sw.capture = &bytes.Buffer{}
		}

		next.ServeHTTP(sw, r)

		attrs := []interface{}{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start),
			"remote", r.RemoteAddr,
		}
		if logBodies && sw.capture != nil {
			body := sw.capture.String()
			if sw.capture.Len() >= maxLoggedBodyBytes {
				body += "…[truncated]"
			}
			attrs = append(attrs, "resp_body", body)
			slog.Debug("request", attrs...)
		} else {
			slog.Info("request", attrs...)
		}
	})
}

// CORSMiddleware adds CORS headers for cross-origin requests (needed for playground).
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only set CORS for public API endpoints, not admin endpoints
		if !strings.HasPrefix(r.URL.Path, "/admin") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// BodyLimitMiddleware limits the incoming request body size to protect against memory-exhaustion DoS attacks.
func BodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Limit admin API requests to 1MB
		if strings.HasPrefix(r.URL.Path, "/admin") {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
		} else {
			// Proxy routes (e.g. chat completion) get 10MB limit
			r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB
		}
		next.ServeHTTP(w, r)
	})
}
