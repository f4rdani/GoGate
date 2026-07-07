package playground

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed index.html
var playgroundHTML string

// Handler serves the playground UI.
func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Replace {{HOST}} with actual request host for API calls
	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}
	// Use ws protocol for the same host
	html := strings.ReplaceAll(playgroundHTML, "{{HOST}}", host)
	w.Write([]byte(html))
}
