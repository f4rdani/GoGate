package middleware

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aigateway/models"
)

func ponytailReq() *models.ChatCompletionRequest {
	return &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

func firstSystemText(t *testing.T, req *models.ChatCompletionRequest) string {
	t.Helper()
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
		t.Fatalf("expected leading system message: %+v", req.Messages)
	}
	var s string
	if err := json.Unmarshal(req.Messages[0].Content, &s); err != nil {
		t.Fatalf("system content must be a string: %v", err)
	}
	return s
}

func TestPonytailLevels(t *testing.T) {
	for _, level := range []string{"lite", "full", "ultra", "ULTRA"} {
		req := ponytailReq()
		InjectPonytailMode(req, level)
		if got := firstSystemText(t, req); !strings.Contains(strings.ToLower(got), "yagni") && level != "lite" {
			t.Fatalf("level %s must mention YAGNI: %q", level, got)
		}
	}
	lite := ponytailReq()
	InjectPonytailMode(lite, "lite")
	full := ponytailReq()
	InjectPonytailMode(full, "full")
	if firstSystemText(t, lite) == firstSystemText(t, full) {
		t.Fatal("levels must produce different prompts")
	}
}

func TestPonytailUnknownNoop(t *testing.T) {
	req := ponytailReq()
	InjectPonytailMode(req, "extreme")
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("unknown level must not touch messages: %+v", req.Messages)
	}
	InjectPonytailMode(nil, "full") // must not panic
}
