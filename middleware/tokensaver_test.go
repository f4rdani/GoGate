package middleware

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

func msgContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func TestCompressMessagesNilRequest(t *testing.T) {
	stats := CompressMessages(nil, config.TokenSaverConfig{Enabled: true})
	if stats != nil {
		t.Error("expected nil stats for nil request")
	}
}

func TestCompressMessagesEmptyMessages(t *testing.T) {
	req := &models.ChatCompletionRequest{}
	stats := CompressMessages(req, config.TokenSaverConfig{Enabled: true})
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats.MessagesChanged != 0 {
		t.Errorf("expected 0 messages changed, got %d", stats.MessagesChanged)
	}
}

func TestCompressMessagesDisabled(t *testing.T) {
	req := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "tool", Content: msgContent("foo\nbar\nbaz\n" + strings.Repeat("long text\n", 1000))},
		},
	}
	stats := CompressMessages(req, config.TokenSaverConfig{Enabled: false})
	if stats != nil {
		t.Error("expected nil stats when disabled")
	}
}

func TestCompressMessagesSkipsSystem(t *testing.T) {
	content := "system msg\n" + strings.Repeat("long text\n", 1000)
	req := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "system", Content: msgContent(content)},
		},
	}
	stats := CompressMessages(req, config.TokenSaverConfig{Enabled: true, MaxInputBytes: 100, CompressUser: true})
	if stats.MessagesChanged != 0 {
		t.Error("system message should not be compressed")
	}
}

func TestCompressMessagesUserToggle(t *testing.T) {
	largeContent := "user msg\n" + strings.Repeat("long text\n", 1000)
	
	// Case 1: CompressUser is false -> user message is skipped
	req1 := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "user", Content: msgContent(largeContent)},
		},
	}
	stats1 := CompressMessages(req1, config.TokenSaverConfig{Enabled: true, MaxInputBytes: 100, CompressUser: false})
	if stats1.MessagesChanged != 0 {
		t.Error("user message should be skipped when CompressUser is false")
	}

	// Case 2: CompressUser is true -> user message is compressed
	req2 := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "user", Content: msgContent(largeContent)},
		},
	}
	stats2 := CompressMessages(req2, config.TokenSaverConfig{Enabled: true, MaxInputBytes: 100, CompressUser: true})
	if stats2.MessagesChanged != 1 {
		t.Error("user message should be compressed when CompressUser is true")
	}
}

func TestCompressMessagesSmall(t *testing.T) {
	req := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "tool", Content: msgContent("short message")},
		},
	}
	stats := CompressMessages(req, config.TokenSaverConfig{Enabled: true, MaxInputBytes: 100})
	if stats.MessagesChanged != 0 {
		t.Error("small content should not be compressed")
	}
}

func TestJSONMinification(t *testing.T) {
	rawJSON := `{
		"key": "value",
		"nested": {
			"array": [1, 2, 3]
		}
	}`
	req := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "tool", Content: msgContent(rawJSON)},
		},
	}
	// MinifyJSON is true
	stats := CompressMessages(req, config.TokenSaverConfig{Enabled: true, MaxInputBytes: 4000, MinifyJSON: true})
	if stats.MessagesChanged != 1 {
		t.Fatal("expected JSON to be minified")
	}
	var res string
	json.Unmarshal(req.Messages[0].Content, &res)
	if strings.Contains(res, "\n") || strings.Contains(res, "\t") || strings.Contains(res, " ") {
		t.Errorf("minified JSON should not contain whitespace, got: %q", res)
	}
}

func TestGrepCompression(t *testing.T) {
	grepOutput := "src/main.go:10:func main() {\nsrc/main.go:11:  println(\"hello\")\nsrc/main.go:12:}\nsrc/utils.go:20:func help() {\nsrc/utils.go:21:}"
	lines := strings.Split(grepOutput, "\n")
	if !isGrepOutput(lines) {
		t.Fatal("expected to match grep output pattern")
	}
	compressed := compressGrep(lines)
	if !strings.Contains(compressed, "📂 src/main.go:") || !strings.Contains(compressed, "📂 src/utils.go:") {
		t.Errorf("grep output not compressed correctly: %q", compressed)
	}
}

func TestLogCompression(t *testing.T) {
	logOutput := "2024-05-14T12:00:01Z [INFO] request started\n2024-05-14T12:00:02Z [INFO] processing...\n2024-05-14T12:00:03Z [INFO] processing...\n2024-05-14T12:00:04Z [INFO] processing...\n2024-05-14T12:00:05Z [INFO] done"
	lines := strings.Split(logOutput, "\n")
	if !isLogOutput(lines) {
		t.Fatal("expected to match log output pattern")
	}
	compressed := compressLog(lines)
	if !strings.Contains(compressed, "repeated log entries collapsed") {
		t.Errorf("expected logs to be collapsed, got: %q", compressed)
	}
}

func TestBuildCompression(t *testing.T) {
	buildOutput := "go: building...\n# github.com/aigateway\n./main.go:15:2: warning: unused variable x\n./main.go:18:5: error: undefined: y\n./main.go:20:2: warning: unused variable z"
	lines := strings.Split(buildOutput, "\n")
	if !isBuildOutput(lines) {
		t.Fatal("expected to match build output pattern")
	}
	compressed := compressBuild(lines)
	if !strings.Contains(compressed, "Compile Errors (1)") || !strings.Contains(compressed, "Warnings collapsed (2 warnings") {
		t.Errorf("build output not compressed correctly: %q", compressed)
	}
}

func TestTestCompression(t *testing.T) {
	testOutput := "=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\n=== RUN   TestBar\n--- FAIL: TestBar (0.01s)\n    bar_test.go:15: assertion failed\nFAIL"
	lines := strings.Split(testOutput, "\n")
	if !isTestOutput(lines) {
		t.Fatal("expected to match test output pattern")
	}
	compressed := compressTest(lines)
	if !strings.Contains(compressed, "Passed: 1") || !strings.Contains(compressed, "Failed: 1") || !strings.Contains(compressed, "Failed Tests Detail:") {
		t.Errorf("test output not compressed correctly: %q", compressed)
	}
}

func TestWhitespaceNormalization(t *testing.T) {
	text := "line1\n\n\n\nline2\n\n\nline3"
	normalized := normalizeWhitespace(text)
	expected := "line1\n\nline2\n\nline3"
	if normalized != expected {
		t.Errorf("expected %q, got %q", expected, normalized)
	}
}

func TestDuplicateCollapsing(t *testing.T) {
	text := "line1\nline2\nline2\nline2\nline2\nline3"
	collapsed := collapseDuplicates(text)
	if !strings.Contains(collapsed, "repeated lines collapsed") {
		t.Errorf("expected duplicates to be collapsed, got %q", collapsed)
	}
}

func TestStripComments(t *testing.T) {
	text := "Some markdown text\n```go\nfunc main() {\n\t// this is a comment\n\tx := 1\n}\n```"
	stripped := stripComments(text)
	if strings.Contains(stripped, "// this is a comment") {
		t.Errorf("comment was not stripped, got: %q", stripped)
	}
	if !strings.Contains(stripped, "x := 1") {
		t.Error("code content should be preserved")
	}
}

func TestInjectCavemanMode(t *testing.T) {
	// Case 1: prepend to existing system message
	req1 := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "system", Content: msgContent("initial instruction")},
		},
	}
	InjectCavemanMode(req1)
	var content1 string
	json.Unmarshal(req1.Messages[0].Content, &content1)
	if !strings.HasPrefix(content1, "IMPORTANT: Be extremely concise") {
		t.Errorf("expected caveman instructions to be prepended, got: %q", content1)
	}

	// Case 2: create new system message
	req2 := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "user", Content: msgContent("hello")},
		},
	}
	InjectCavemanMode(req2)
	if len(req2.Messages) != 2 || req2.Messages[0].Role != "system" {
		t.Fatal("expected system message to be prepended")
	}
}

func TestLegacyCompressToolOutputs(t *testing.T) {
	// Legacy function should compile and run without panic
	req := &models.ChatCompletionRequest{
		Messages: []models.Message{
			{Role: "tool", Content: msgContent("some output")},
		},
	}
	CompressToolOutputs(req)
}
