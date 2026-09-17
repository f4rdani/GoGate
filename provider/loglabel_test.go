package provider

import (
	"encoding/json"
	"testing"

	"github.com/aigateway/models"
)

func TestMaskKeyShort(t *testing.T) {
	if got := maskKeyShort("abcdefgh12345678"); got != "abcd...5678" {
		t.Fatalf("got %q", got)
	}
	if maskKeyShort("") != "-" || maskKeyShort("short") != "****" {
		t.Fatal("edge masks broken")
	}
}

func TestKeyLabel(t *testing.T) {
	multi := &BaseProvider{name: "p", apiKeys: []*UpstreamKey{{Key: "abcdefgh12345678", Index: 0}, {Key: "zzzzzzzz87654321", Index: 1}}}
	if got := multi.keyLabel(multi.apiKeys[1]); got != "#2 zzzz...4321" {
		t.Fatalf("multi-key label: %q", got)
	}
	single := &BaseProvider{name: "p", apiKeys: []*UpstreamKey{{Key: "abcdefgh12345678", Index: 0}}}
	if got := single.keyLabel(single.apiKeys[0]); got != "abcd...5678" {
		t.Fatalf("single-key label: %q", got)
	}
	if got := multi.keyLabel(nil); got != "token" {
		t.Fatalf("nil key must be token: %q", got)
	}
	if got := multi.keyLabel(&UpstreamKey{}); got != "token" {
		t.Fatalf("empty key must be token: %q", got)
	}
}

func TestFormatTag(t *testing.T) {
	cases := map[string]string{"anthropic": "openai→anthropic", "kiro": "openai→kiro", "openai": "openai→openai", "groq": "openai→openai", "": "openai→openai"}
	for typ, want := range cases {
		if got := (&BaseProvider{providerType: typ}).formatTag(); got != want {
			t.Fatalf("type %q: got %q want %q", typ, got, want)
		}
	}
}

func TestDescribeChatRequest(t *testing.T) {
	msgs, tools := describeChatRequest(nil)
	if msgs != 0 || tools != 0 {
		t.Fatal("nil request must describe empty")
	}
	req := &models.ChatCompletionRequest{
		Messages: []models.Message{{Role: "user"}, {Role: "assistant"}},
		Tools:    json.RawMessage(`[{"type":"function"},{"type":"function"}]`),
	}
	if msgs, tools := describeChatRequest(req); msgs != 2 || tools != 2 {
		t.Fatalf("got %d msgs %d tools", msgs, tools)
	}
	req.Tools = json.RawMessage(`oops`)
	if _, tools := describeChatRequest(req); tools != 0 {
		t.Fatalf("malformed tools must count 0, got %d", tools)
	}
}
