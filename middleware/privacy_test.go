package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

func TestPrivacySanitizeSecrets(t *testing.T) {
	cfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "redact",
		MaskSecrets: true,
		MaskPII:     false,
	}

	tests := []struct {
		name     string
		input    string
		contains string
		notCont  string
	}{
		{
			name:     "OpenAI key",
			input:    "Here is my key: sk-proj-1234567890abcdef1234567890 please keep it safe",
			contains: "[REDACTED_API_KEY]",
			notCont:  "sk-proj-1234567890abcdef1234567890",
		},
		{
			name:     "Anthropic key",
			input:    "Claude key is sk-ant-api03-abcdefghijklmnopqrstuvwxyz-123456 ok",
			contains: "[REDACTED_API_KEY]",
			notCont:  "sk-ant-api03",
		},
		{
			name:     "Gemini key",
			input:    "Gemini key is AIzaSyD1234567890abcdef123456789012345 ok",
			contains: "[REDACTED_API_KEY]",
			notCont:  "AIzaSyD1234567890abcdef123456789012345",
		},
		{
			name:     "Gemini AQ key",
			input:    "API key is AQ.FakeGeminiKeyTestSample1234567890abcdef for flash lite",
			contains: "[REDACTED_API_KEY]",
			notCont:  "AQ.FakeGeminiKeyTestSample1234567890abcdef",
		},
		{
			name:     "GitHub token",
			input:    "GitHub token is ghp_123456789012345678901234567890123456",
			contains: "[REDACTED_API_KEY]",
			notCont:  "ghp_123456789012345678901234567890123456",
		},
		{
			name:     "AWS Access Key",
			input:    "AWS credentials: AKIAIOSFODNN7EXAMPLE",
			contains: "[REDACTED_API_KEY]",
			notCont:  "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:     "JWT token",
			input:    "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
			contains: "[REDACTED_JWT]",
			notCont:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:     "Password in JSON",
			input:    `{"username": "admin", "password": "supersecretpassword123"}`,
			contains: `"password": "[REDACTED_PASSWORD]"`,
			notCont:  "supersecretpassword123",
		},
		{
			name:     "Private Key",
			input:    "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0Y...\n-----END RSA PRIVATE KEY-----",
			contains: "[REDACTED_PRIVATE_KEY]",
			notCont:  "MIIEowIBAAKCAQEA0Y",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, stats := SanitizeText(tc.input, cfg)
			if stats.RedactionsCount == 0 {
				t.Fatalf("expected redactions for %s, got 0", tc.name)
			}
			if !containsSubstring(got, tc.contains) {
				t.Errorf("expected output to contain %q, got: %s", tc.contains, got)
			}
			if containsSubstring(got, tc.notCont) {
				t.Errorf("expected output to NOT contain %q, got: %s", tc.notCont, got)
			}
		})
	}
}

func TestPrivacySanitizePII(t *testing.T) {
	cfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "redact",
		MaskSecrets: false,
		MaskPII:     true,
	}

	tests := []struct {
		name     string
		input    string
		contains string
		notCont  string
	}{
		{
			name:     "Email address",
			input:    "Contact me at user.name+test@example.co.id for queries",
			contains: "[REDACTED_EMAIL]",
			notCont:  "user.name+test@example.co.id",
		},
		{
			name:     "Indonesian Phone",
			input:    "Telepon ke 081234567890 atau +6281987654321 sekarang",
			contains: "[REDACTED_PHONE]",
			notCont:  "081234567890",
		},
		{
			name:     "Valid Credit Card (Stripe test 4242424242424242)",
			input:    "My credit card is 4242 4242 4242 4242 exp 12/28",
			contains: "[REDACTED_CREDIT_CARD]",
			notCont:  "4242 4242 4242 4242",
		},
		{
			name:     "IPv4 address",
			input:    "Server is at 198.51.100.42 and database at 10.0.0.15",
			contains: "[REDACTED_IP_ADDRESS]",
			notCont:  "198.51.100.42",
		},
		{
			name:     "Preserve localhost 127.0.0.1",
			input:    "Connect to http://127.0.0.1:8080 or 0.0.0.0",
			contains: "127.0.0.1",
			notCont:  "[REDACTED_IP_ADDRESS]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := SanitizeText(tc.input, cfg)
			if !containsSubstring(got, tc.contains) {
				t.Errorf("expected output to contain %q, got: %s", tc.contains, got)
			}
			if containsSubstring(got, tc.notCont) {
				t.Errorf("expected output to NOT contain %q, got: %s", tc.notCont, got)
			}
		})
	}
}

func TestSanitizeMessagesImmutability(t *testing.T) {
	cfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "redact",
		MaskSecrets: true,
		MaskPII:     true,
	}

	originalContent := "My secret is sk-proj-12345678901234567890 and email is admin@company.org"
	raw, _ := json.Marshal(originalContent)
	msgs := []models.Message{
		{
			Role:    "user",
			Content: raw,
		},
	}

	sanitized, stats := SanitizeMessages(msgs, cfg)
	if stats.RedactionsCount != 2 {
		t.Fatalf("expected 2 redactions, got %d", stats.RedactionsCount)
	}

	// Verify original message content is NOT modified
	var origBack string
	json.Unmarshal(msgs[0].Content, &origBack)
	if origBack != originalContent {
		t.Fatalf("original message was mutated: %s", origBack)
	}

	// Verify sanitized message is clean
	var cleanBack string
	json.Unmarshal(sanitized[0].Content, &cleanBack)
	if containsSubstring(cleanBack, "sk-proj-") || containsSubstring(cleanBack, "admin@company.org") {
		t.Fatalf("sanitized message still contains sensitive data: %s", cleanBack)
	}
	if !containsSubstring(cleanBack, "[REDACTED_API_KEY]") || !containsSubstring(cleanBack, "[REDACTED_EMAIL]") {
		t.Fatalf("sanitized message missing redaction markers: %s", cleanBack)
	}
}

func TestValidateMessagesBlockMode(t *testing.T) {
	cfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "block",
		MaskSecrets: true,
		MaskPII:     true,
	}

	rawClean, _ := json.Marshal("Hello, how are you?")
	cleanMsgs := []models.Message{{Role: "user", Content: rawClean}}
	if err := ValidateMessages(cleanMsgs, cfg); err != nil {
		t.Fatalf("expected clean message to pass, got error: %v", err)
	}

	rawSensitive, _ := json.Marshal("My api key: sk-proj-1234567890abcdef1234567890")
	sensitiveMsgs := []models.Message{{Role: "user", Content: rawSensitive}}
	if err := ValidateMessages(sensitiveMsgs, cfg); err == nil {
		t.Fatalf("expected sensitive message to fail in block mode, but error was nil")
	}
}

func containsSubstring(s, substr string) bool {
	if substr == "" {
		return false
	}
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) && (searchSubstr(s, substr))))
}

func searchSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestPrivacyVaultFormatPreservingSynthetic(t *testing.T) {
	cfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "vault",
		MaskSecrets: true,
		MaskPII:     true,
	}
	vault := NewPrivacyVault()

	input := `const apiKey = "sk-proj-1234567890abcdef1234567890";
const password = "superSecretPassword123";
const email = "developer@company.org";
const localIP = "192.168.1.50";
const publicIP = "198.51.100.22";`

	got, stats := SanitizeTextWithVault(input, cfg, vault)
	if stats.RedactionsCount < 4 {
		t.Fatalf("expected at least 4 items masked, got %d", stats.RedactionsCount)
	}

	// 1. Secrets should not appear in sanitized output
	if containsSubstring(got, "sk-proj-1234567890abcdef1234567890") {
		t.Errorf("original apiKey leaked: %s", got)
	}
	if containsSubstring(got, "superSecretPassword123") {
		t.Errorf("original password leaked: %s", got)
	}
	if containsSubstring(got, "developer@company.org") {
		t.Errorf("original email leaked: %s", got)
	}

	// 2. Local IP (RFC 1918) MUST be preserved untouched
	if !containsSubstring(got, "192.168.1.50") {
		t.Errorf("local private IP was wrongly redacted: %s", got)
	}

	// 3. Synthetic dummies must look realistic and syntactically valid
	if !containsSubstring(got, `const apiKey = "sk-proj-mocksec00000000000000000001"`) {
		t.Errorf("expected realistic synthetic apiKey, got: %s", got)
	}
	if !containsSubstring(got, `const password = "MockSecretPass_0001"`) {
		t.Errorf("expected realistic synthetic password, got: %s", got)
	}
	if !containsSubstring(got, `const email = "anon_developer1@example.com"`) {
		t.Errorf("expected realistic synthetic email, got: %s", got)
	}

	// 4. Reverse replacement should restore 100% of the original text
	restored := vault.ReverseReplace(got)
	if restored != input {
		t.Fatalf("ReverseReplace failed to restore exact original.\nGot:\n%s\nExpected:\n%s", restored, input)
	}
}

func TestDeAnonymizeResponse(t *testing.T) {
	cfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "vault",
		MaskSecrets: true,
		MaskPII:     true,
	}
	vault := NewPrivacyVault()

	prompt := `var secret = "superSecretPassword123";`
	_, _ = SanitizeTextWithVault(prompt, cfg, vault)

	aiAnswer := `Here is the refactored code:
func login() {
    var secret = "MockSecretPass_0001";
}`
	rawContent, _ := json.Marshal(aiAnswer)

	resp := &models.ChatCompletionResponse{
		ID:    "chatcmpl-test",
		Model: "mimo-v2.5-free",
		Choices: []models.Choice{
			{
				Index: 0,
				Message: &models.Message{
					Role:             "assistant",
					Content:          rawContent,
					ReasoningContent: "Thinking about MockSecretPass_0001 authentication...",
					ToolCalls:        json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"save","arguments":"{\"key\":\"MockSecretPass_0001\"}"}}]`),
				},
			},
		},
	}

	DeAnonymizeResponse(resp, vault)

	// Verify Message.Content was de-anonymized
	var cleanContent string
	json.Unmarshal(resp.Choices[0].Message.Content, &cleanContent)
	if !containsSubstring(cleanContent, `var secret = "superSecretPassword123";`) {
		t.Errorf("DeAnonymizeResponse failed to restore content, got: %s", cleanContent)
	}

	// Verify ReasoningContent was de-anonymized
	if !containsSubstring(resp.Choices[0].Message.ReasoningContent, "Thinking about superSecretPassword123 authentication...") {
		t.Errorf("DeAnonymizeResponse failed to restore reasoning_content, got: %s", resp.Choices[0].Message.ReasoningContent)
	}

	// Verify ToolCalls arguments was de-anonymized
	if !containsSubstring(string(resp.Choices[0].Message.ToolCalls), "superSecretPassword123") {
		t.Errorf("DeAnonymizeResponse failed to restore tool_calls arguments, got: %s", string(resp.Choices[0].Message.ToolCalls))
	}
}

type mockResponseWriter struct {
	headers http.Header
	buf     []byte
	flushed int
}

func (m *mockResponseWriter) Header() http.Header {
	if m.headers == nil {
		m.headers = make(http.Header)
	}
	return m.headers
}

func (m *mockResponseWriter) Write(p []byte) (int, error) {
	m.buf = append(m.buf, p...)
	return len(p), nil
}

func (m *mockResponseWriter) WriteHeader(statusCode int) {}

func (m *mockResponseWriter) Flush() {
	m.flushed++
}

func TestDeAnonymizingResponseWriterStreaming(t *testing.T) {
	cfg := config.PrivacyConfig{
		Enabled:     true,
		Mode:        "vault",
		MaskSecrets: true,
	}
	vault := NewPrivacyVault()

	prompt := `key: sk-proj-1234567890abcdef1234567890`
	_, _ = SanitizeTextWithVault(prompt, cfg, vault)

	syn := vault.forward["sk-proj-1234567890abcdef1234567890"]
	if syn == "" {
		t.Fatalf("expected synthetic token in vault")
	}

	mockW := &mockResponseWriter{}
	deanonymizer := NewDeAnonymizingResponseWriter(mockW, mockW, vault)

	// Dynamically split the actual synthetic token in half across chunk 1 and chunk 2!
	mid := len(syn) / 2
	chunk1 := []byte(fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s`, syn[:mid]))
	chunk2 := []byte(fmt.Sprintf(`%s"}}]}`+"\n\n", syn[mid:]))
	chunk3 := []byte("data: [DONE]\n\n")

	// Write chunk 1
	n1, err1 := deanonymizer.Write(chunk1)
	if err1 != nil || n1 != len(chunk1) {
		t.Fatalf("write chunk 1 failed: %v", err1)
	}

	// Write chunk 2
	n2, err2 := deanonymizer.Write(chunk2)
	if err2 != nil || n2 != len(chunk2) {
		t.Fatalf("write chunk 2 failed: %v", err2)
	}

	// Write chunk 3
	n3, err3 := deanonymizer.Write(chunk3)
	if err3 != nil || n3 != len(chunk3) {
		t.Fatalf("write chunk 3 failed: %v", err3)
	}

	deanonymizer.FlushFinal()

	outputStr := string(mockW.buf)

	// Verify that the split synthetic token was seamlessly restored to the original key!
	if !containsSubstring(outputStr, "sk-proj-1234567890abcdef1234567890") {
		t.Fatalf("streaming de-anonymization failed to restore split token across chunks! Output was:\n%s", outputStr)
	}
	if containsSubstring(outputStr, "sk-proj-mocksec") {
		t.Fatalf("synthetic token leaked in streaming output! Output was:\n%s", outputStr)
	}
	if !containsSubstring(outputStr, "data: [DONE]\n\n") {
		t.Fatalf("missing [DONE] in stream output")
	}
}

