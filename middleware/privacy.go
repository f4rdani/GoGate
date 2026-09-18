package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

// PrivacyStats tracks how many sensitive items were redacted, masked, or detected.
type PrivacyStats struct {
	RedactionsCount int            `json:"redactions_count"`
	DetectedTypes   map[string]int `json:"detected_types"` // e.g. "api_key": 2, "email": 1
}

func newPrivacyStats() *PrivacyStats {
	return &PrivacyStats{
		DetectedTypes: make(map[string]int),
	}
}

func (s *PrivacyStats) record(category string) {
	s.RedactionsCount++
	s.DetectedTypes[category]++
}

type privacyCtxKey string

const (
	ctxKeyPrivacy privacyCtxKey = "gogate_privacy_config"
	ctxKeyVault   privacyCtxKey = "gogate_privacy_vault"
)

// WithPrivacyConfig attaches a PrivacyConfig to context.
func WithPrivacyConfig(ctx context.Context, cfg config.PrivacyConfig) context.Context {
	return context.WithValue(ctx, ctxKeyPrivacy, cfg)
}

// GetPrivacyConfig retrieves the PrivacyConfig from context.
func GetPrivacyConfig(ctx context.Context) (config.PrivacyConfig, bool) {
	if ctx == nil {
		return config.PrivacyConfig{}, false
	}
	cfg, ok := ctx.Value(ctxKeyPrivacy).(config.PrivacyConfig)
	return cfg, ok
}

// WithPrivacyVault attaches a PrivacyVault to context.
func WithPrivacyVault(ctx context.Context, v *PrivacyVault) context.Context {
	return context.WithValue(ctx, ctxKeyVault, v)
}

// GetPrivacyVault retrieves the PrivacyVault from context.
func GetPrivacyVault(ctx context.Context) *PrivacyVault {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKeyVault).(*PrivacyVault)
	return v
}

// PrivacyVault manages bidirectional mappings between original sensitive secrets
// and synthetic format-preserving dummy tokens.
type PrivacyVault struct {
	mu        sync.RWMutex
	forward   map[string]string // original -> synthetic
	reverse   map[string]string // synthetic -> original
	revKeys   []string          // synthetic keys sorted descending by length
	counters  map[string]int
	maxSynLen int
}

// NewPrivacyVault creates an initialized PrivacyVault.
func NewPrivacyVault() *PrivacyVault {
	return &PrivacyVault{
		forward:  make(map[string]string),
		reverse:  make(map[string]string),
		counters: make(map[string]int),
	}
}

// Len returns the count of mapped entities in the vault.
func (v *PrivacyVault) Len() int {
	if v == nil {
		return 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.forward)
}

// GetOrGenerate returns an existing synthetic dummy or generates a new one.
func (v *PrivacyVault) GetOrGenerate(category, original string, cfg config.PrivacyConfig) string {
	if v == nil {
		return generateSyntheticToken(category, original, 1)
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	if syn, exists := v.forward[original]; exists {
		return syn
	}

	seq := v.counters[category]
	v.counters[category]++

	syn := generateSyntheticToken(category, original, seq+1)
	v.forward[original] = syn
	v.reverse[syn] = original

	if len(syn) > v.maxSynLen {
		v.maxSynLen = len(syn)
	}

	v.revKeys = append(v.revKeys, syn)
	sort.Slice(v.revKeys, func(i, j int) bool {
		return len(v.revKeys[i]) > len(v.revKeys[j])
	})

	return syn
}

// ReverseReplace substitutes all synthetic dummy tokens in input with original secrets.
func (v *PrivacyVault) ReverseReplace(input string) string {
	if input == "" || v == nil {
		return input
	}
	v.mu.RLock()
	defer v.mu.RUnlock()

	if len(v.reverse) == 0 {
		return input
	}

	result := input
	for _, syn := range v.revKeys {
		if strings.Contains(result, syn) {
			orig := v.reverse[syn]
			result = strings.ReplaceAll(result, syn, orig)
		}
	}
	return result
}

// LongestSyntheticPrefix returns the length of the longest suffix of data that is a prefix of any synthetic key in vault.
func (v *PrivacyVault) LongestSyntheticPrefix(data []byte) int {
	if len(data) == 0 || v == nil {
		return 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()

	if len(v.reverse) == 0 {
		return 0
	}

	maxCheck := v.maxSynLen - 1
	if len(data) < maxCheck {
		maxCheck = len(data)
	}

	for l := maxCheck; l >= 1; l-- {
		suffix := string(data[len(data)-l:])
		for syn := range v.reverse {
			if strings.HasPrefix(syn, suffix) {
				return l
			}
		}
	}
	return 0
}

// Regex patterns for secrets & tokens
var (
	rePrivateKey1 = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z\s]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z\s]+ )?PRIVATE KEY-----`)
	rePrivateKey2 = regexp.MustCompile(`(?s)-----BEGIN OPENSSH PRIVATE KEY-----.*?-----END OPENSSH PRIVATE KEY-----`)
	rePrivateKey3 = regexp.MustCompile(`(?s)-----BEGIN PGP PRIVATE KEY BLOCK-----.*?-----END PGP PRIVATE KEY BLOCK-----`)

	reJWT = regexp.MustCompile(`\bey[a-zA-Z0-9_\-]{15,}\.ey[a-zA-Z0-9_\-]{15,}\.[a-zA-Z0-9_\-]+\b`)

	reOpenAIKey    = regexp.MustCompile(`\bsk-(?:proj-|live-)?[a-zA-Z0-9_\-]{20,}\b`)
	reAnthropicKey = regexp.MustCompile(`\bsk-ant-[a-zA-Z0-9_\-]{20,}\b`)
	reGeminiKey1   = regexp.MustCompile(`\bAIzaSy[a-zA-Z0-9_\-]{30,35}\b`)
	reGeminiKey2   = regexp.MustCompile(`\bAQ\.[a-zA-Z0-9_\-]{30,}\b`)
	reGitHubToken  = regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[a-zA-Z0-9]{36}\b`)
	reGitHubPAT    = regexp.MustCompile(`\bgithub_pat_[a-zA-Z0-9_]{50,}\b`)
	reSlackToken   = regexp.MustCompile(`\bxox[baprs]-[0-9a-zA-Z-]{10,}\b`)
	reAWSAccess    = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)

	rePasswordAssignment = regexp.MustCompile(`(?i)(["']?(?:password|passwd|pwd|secret|api_key|apikey|access_token|auth_token)["']?\s*[:=]\s*["'])([^"'` + "`" + `\s]{6,})(["'])`)

	// PII Patterns
	reEmail         = regexp.MustCompile(`\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`)
	rePhoneID       = regexp.MustCompile(`\b(?:\+?62|08)[0-9]{8,12}\b`)
	rePhoneIntl     = regexp.MustCompile(`\+[1-9]\d{7,14}\b`)
	reIPv4          = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b`)
	reCardCandidate = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)
)

// isPrivateOrLocalIP checks if an IP is loopback, RFC 1918 private, link-local, or common public DNS.
// These IPs are not sensitive and should NOT be redacted in programming/development contexts.
func isPrivateOrLocalIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ipStr == "255.255.255.255" {
		return true
	}
	// Common public DNS servers
	if ipStr == "8.8.8.8" || ipStr == "8.8.4.4" || ipStr == "1.1.1.1" || ipStr == "1.0.0.1" {
		return true
	}
	return false
}

// isSyntheticToken returns true if a token is a synthetic token generated by PrivacyVault.
func isSyntheticToken(s string) bool {
	return strings.Contains(s, "mocksec") ||
		strings.Contains(s, "MockSecret") ||
		strings.Contains(s, "mocktoken") ||
		strings.Contains(s, "AIzaSyMock") ||
		strings.Contains(s, "AQ.mockgemini") ||
		strings.Contains(s, "AKIAMOCK") ||
		strings.Contains(s, "mock_signature") ||
		strings.Contains(s, "mockPrivateKey") ||
		strings.Contains(s, "anon_developer") ||
		strings.Contains(s, "mockprivacy") ||
		strings.HasPrefix(s, "[REDACTED")
}

// isValidLuhn validates a credit card number using the Luhn checksum algorithm.
func isValidLuhn(digits string) bool {
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	alternate := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if alternate {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alternate = !alternate
	}
	return sum%10 == 0
}

// generateValidLuhn generates a valid 16-digit test card number passing Luhn checksum.
func generateValidLuhn(seq int) string {
	base := fmt.Sprintf("400000%09d", seq)
	for check := 0; check <= 9; check++ {
		cand := fmt.Sprintf("%s%d", base, check)
		if isValidLuhn(cand) {
			return cand
		}
	}
	return "4000001234567895"
}

// generateSyntheticToken produces format-preserving synthetic dummy values.
func generateSyntheticToken(category, original string, seq int) string {
	switch category {
	case "api_key_openai":
		if strings.HasPrefix(original, "sk-proj-") {
			return fmt.Sprintf("sk-proj-mocksec%020d", seq)
		}
		return fmt.Sprintf("sk-mocksec%024d", seq)

	case "api_key_anthropic":
		return fmt.Sprintf("sk-ant-api03-mocksec%020d", seq)

	case "api_key_gemini":
		return fmt.Sprintf("AIzaSyMock%029d", seq)

	case "api_key_gemini_oauth":
		return fmt.Sprintf("AQ.mockgeminikey%022d", seq)

	case "token_github":
		return fmt.Sprintf("ghp_mocktoken%027d", seq)

	case "token_github_pat":
		return fmt.Sprintf("github_pat_mocksec%044d", seq)

	case "token_slack":
		return fmt.Sprintf("xoxb-mocktoken-%012d-%012d-mockslacktoken", seq, seq)

	case "token_aws":
		return fmt.Sprintf("AKIAMOCK%012d", seq)

	case "jwt":
		return fmt.Sprintf("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJtb2NrX3VzZXIiLCJpYXQiOjE1MTYyMzkwMjJ9.mock_signature_%016d", seq)

	case "private_key":
		return fmt.Sprintf("-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0mockPrivateKey%016d0000000000000000000000000000\n00000000000000000000000000000000000000000000000000000000000000000001\n-----END RSA PRIVATE KEY-----", seq)

	case "password":
		return fmt.Sprintf("MockSecretPass_%04d", seq)

	case "email":
		return fmt.Sprintf("anon_developer%d@example.com", seq)

	case "phone_id":
		return fmt.Sprintf("08129999%04d", seq)

	case "phone_intl":
		return fmt.Sprintf("+1555010%04d", seq)

	case "credit_card":
		return generateValidLuhn(seq)

	case "ip_address":
		return fmt.Sprintf("203.0.113.%d", (seq%200)+10)

	default:
		return fmt.Sprintf("mock_sec_%s_%04d", category, seq)
	}
}

// SanitizeText scans a string and redacts/replaces sensitive items.
func SanitizeText(input string, cfg config.PrivacyConfig) (string, *PrivacyStats) {
	return SanitizeTextWithVault(input, cfg, nil)
}

// SanitizeTextWithVault scans a string and replaces secrets with synthetic tokens or redact markers.
func SanitizeTextWithVault(input string, cfg config.PrivacyConfig, vault *PrivacyVault) (string, *PrivacyStats) {
	stats := newPrivacyStats()
	if input == "" || (!cfg.MaskSecrets && !cfg.MaskPII) {
		return input, stats
	}

	result := input
	isRedactMode := cfg.Mode == "redact"

	// === 1. Mask Secrets ===
	if cfg.MaskSecrets {
		// Private keys
		for _, re := range []*regexp.Regexp{rePrivateKey1, rePrivateKey2, rePrivateKey3} {
			if matches := re.FindAllString(result, -1); len(matches) > 0 {
				for _, m := range matches {
					stats.record("private_key")
					if isRedactMode {
						result = strings.Replace(result, m, "[REDACTED_PRIVATE_KEY]", 1)
					} else {
						syn := vault.GetOrGenerate("private_key", m, cfg)
						result = strings.Replace(result, m, syn, 1)
					}
				}
			}
		}

		// JWT
		if matches := reJWT.FindAllString(result, -1); len(matches) > 0 {
			for _, m := range matches {
				stats.record("jwt")
				if isRedactMode {
					result = strings.Replace(result, m, "[REDACTED_JWT]", 1)
				} else {
					syn := vault.GetOrGenerate("jwt", m, cfg)
					result = strings.Replace(result, m, syn, 1)
				}
			}
		}

		// API Keys & Tokens with category mapping
		keyMatchers := []struct {
			cat string
			re  *regexp.Regexp
		}{
			{"api_key_openai", reOpenAIKey},
			{"api_key_anthropic", reAnthropicKey},
			{"api_key_gemini", reGeminiKey1},
			{"api_key_gemini_oauth", reGeminiKey2},
			{"token_github", reGitHubToken},
			{"token_github_pat", reGitHubPAT},
			{"token_slack", reSlackToken},
			{"token_aws", reAWSAccess},
		}

		for _, km := range keyMatchers {
			if matches := km.re.FindAllString(result, -1); len(matches) > 0 {
				for _, m := range matches {
					if isSyntheticToken(m) {
						continue
					}
					stats.record("api_key")
					if isRedactMode {
						result = strings.Replace(result, m, "[REDACTED_API_KEY]", 1)
					} else {
						syn := vault.GetOrGenerate(km.cat, m, cfg)
						result = strings.Replace(result, m, syn, 1)
					}
				}
			}
		}

		// Password assignments
		if matches := rePasswordAssignment.FindAllStringSubmatch(result, -1); len(matches) > 0 {
			for _, sub := range matches {
				if len(sub) >= 4 {
					val := sub[2]
					// Skip if already masked or recognized as a specific token prefix
					if strings.HasPrefix(val, "MockSecret") || strings.HasPrefix(val, "sk-") ||
						strings.HasPrefix(val, "AIzaSy") || strings.HasPrefix(val, "AQ.") ||
						strings.HasPrefix(val, "ghp_") || strings.HasPrefix(val, "github_pat_") ||
						strings.HasPrefix(val, "xox") || strings.HasPrefix(val, "AKIA") ||
						strings.HasPrefix(val, "[REDACTED") {
						continue
					}
					stats.record("password")
					fullMatch := sub[0]
					var replacement string
					if isRedactMode {
						replacement = sub[1] + "[REDACTED_PASSWORD]" + sub[3]
					} else {
						syn := vault.GetOrGenerate("password", val, cfg)
						replacement = sub[1] + syn + sub[3]
					}
					result = strings.Replace(result, fullMatch, replacement, 1)
				}
			}
		}
	}

	// === 2. Mask PII ===
	if cfg.MaskPII {
		// Credit Cards (Candidate regex + Luhn verification)
		result = reCardCandidate.ReplaceAllStringFunc(result, func(cand string) string {
			var digits strings.Builder
			for _, r := range cand {
				if unicode.IsDigit(r) {
					digits.WriteRune(r)
				}
			}
			dStr := digits.String()
			if isValidLuhn(dStr) {
				stats.record("credit_card")
				if isRedactMode {
					return "[REDACTED_CREDIT_CARD]"
				}
				return vault.GetOrGenerate("credit_card", cand, cfg)
			}
			return cand
		})

		// Emails
		if matches := reEmail.FindAllString(result, -1); len(matches) > 0 {
			for _, m := range matches {
				stats.record("email")
				if isRedactMode {
					result = strings.Replace(result, m, "[REDACTED_EMAIL]", 1)
				} else {
					syn := vault.GetOrGenerate("email", m, cfg)
					result = strings.Replace(result, m, syn, 1)
				}
			}
		}

		// Phone numbers
		phoneMatchers := []struct {
			cat string
			re  *regexp.Regexp
		}{
			{"phone_id", rePhoneID},
			{"phone_intl", rePhoneIntl},
		}
		for _, pm := range phoneMatchers {
			if matches := pm.re.FindAllString(result, -1); len(matches) > 0 {
				for _, m := range matches {
					stats.record("phone")
					if isRedactMode {
						result = strings.Replace(result, m, "[REDACTED_PHONE]", 1)
					} else {
						syn := vault.GetOrGenerate(pm.cat, m, cfg)
						result = strings.Replace(result, m, syn, 1)
					}
				}
			}
		}

		// IPv4 (skipping private, local, and public DNS servers)
		result = reIPv4.ReplaceAllStringFunc(result, func(ip string) string {
			if isPrivateOrLocalIP(ip) {
				return ip
			}
			stats.record("ip_address")
			if isRedactMode {
				return "[REDACTED_IP_ADDRESS]"
			}
			return vault.GetOrGenerate("ip_address", ip, cfg)
		})
	}

	return result, stats
}

// SanitizeMessages returns a deep-copied, sanitized slice of messages.
func SanitizeMessages(messages []models.Message, cfg config.PrivacyConfig) ([]models.Message, *PrivacyStats) {
	return SanitizeMessagesWithVault(messages, cfg, nil)
}

// SanitizeMessagesWithVault returns a deep-copied, sanitized slice of messages using the specified vault.
func SanitizeMessagesWithVault(messages []models.Message, cfg config.PrivacyConfig, vault *PrivacyVault) ([]models.Message, *PrivacyStats) {
	totalStats := newPrivacyStats()
	if len(messages) == 0 {
		return nil, totalStats
	}

	sanitized := make([]models.Message, len(messages))
	for i, m := range messages {
		sanitized[i] = m
		if len(m.Content) == 0 {
			continue
		}

		// Check if string content
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			cleanText, stats := SanitizeTextWithVault(text, cfg, vault)
			for cat, count := range stats.DetectedTypes {
				for k := 0; k < count; k++ {
					totalStats.record(cat)
				}
			}
			if stats.RedactionsCount > 0 {
				sanitized[i].Content, _ = json.Marshal(cleanText)
			} else {
				sanitized[i].Content = append([]byte(nil), m.Content...)
			}
			continue
		}

		// Check if array of parts (multimodal)
		var parts []json.RawMessage
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			modified := false
			newParts := make([]json.RawMessage, len(parts))
			for j, part := range parts {
				var contentPart struct {
					Type    string `json:"type"`
					Text    string `json:"text,omitempty"`
					Content string `json:"content,omitempty"`
				}
				if err := json.Unmarshal(part, &contentPart); err == nil {
					targetText := contentPart.Text
					if targetText == "" {
						targetText = contentPart.Content
					}
					if targetText != "" {
						cleanText, stats := SanitizeTextWithVault(targetText, cfg, vault)
						for cat, count := range stats.DetectedTypes {
							for k := 0; k < count; k++ {
								totalStats.record(cat)
							}
						}
						if stats.RedactionsCount > 0 {
							modified = true
							if contentPart.Text != "" {
								contentPart.Text = cleanText
							} else {
								contentPart.Content = cleanText
							}
							newParts[j], _ = json.Marshal(contentPart)
							continue
						}
					}
				}
				newParts[j] = append([]byte(nil), part...)
			}
			if modified {
				sanitized[i].Content, _ = json.Marshal(newParts)
			} else {
				sanitized[i].Content = append([]byte(nil), m.Content...)
			}
			continue
		}

		sanitized[i].Content = append([]byte(nil), m.Content...)
	}

	return sanitized, totalStats
}

// ValidateMessages scans messages and returns an error if any sensitive entity is detected (mode: block).
func ValidateMessages(messages []models.Message, cfg config.PrivacyConfig) error {
	_, stats := SanitizeMessages(messages, cfg)
	if stats.RedactionsCount > 0 {
		var detected []string
		for cat, count := range stats.DetectedTypes {
			detected = append(detected, fmt.Sprintf("%s (%d)", cat, count))
		}
		return fmt.Errorf("request blocked by privacy policy: sensitive data detected: %s", strings.Join(detected, ", "))
	}
	return nil
}

// DeAnonymizeResponse restores original secrets in a ChatCompletionResponse using the vault.
func DeAnonymizeResponse(resp *models.ChatCompletionResponse, vault *PrivacyVault) {
	if resp == nil || vault == nil || vault.Len() == 0 {
		return
	}
	for i := range resp.Choices {
		choice := &resp.Choices[i]
		if choice.Message != nil {
			deAnonymizeMessage(choice.Message, vault)
		}
	}
}

func deAnonymizeMessage(m *models.Message, vault *PrivacyVault) {
	if m == nil || vault == nil || vault.Len() == 0 {
		return
	}

	// 1. Message content
	if len(m.Content) > 0 {
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			restored := vault.ReverseReplace(text)
			if restored != text {
				m.Content, _ = json.Marshal(restored)
			}
		} else {
			var parts []json.RawMessage
			if err := json.Unmarshal(m.Content, &parts); err == nil {
				modified := false
				for j, part := range parts {
					var contentPart map[string]interface{}
					if err := json.Unmarshal(part, &contentPart); err == nil {
						if t, ok := contentPart["text"].(string); ok {
							restored := vault.ReverseReplace(t)
							if restored != t {
								contentPart["text"] = restored
								modified = true
							}
						}
						if c, ok := contentPart["content"].(string); ok {
							restored := vault.ReverseReplace(c)
							if restored != c {
								contentPart["content"] = restored
								modified = true
							}
						}
						if modified {
							parts[j], _ = json.Marshal(contentPart)
						}
					}
				}
				if modified {
					m.Content, _ = json.Marshal(parts)
				}
			}
		}
	}

	// 2. Reasoning content
	if m.ReasoningContent != "" {
		m.ReasoningContent = vault.ReverseReplace(m.ReasoningContent)
	}

	// 3. Tool calls
	if len(m.ToolCalls) > 0 {
		rawStr := string(m.ToolCalls)
		restored := vault.ReverseReplace(rawStr)
		if restored != rawStr {
			m.ToolCalls = json.RawMessage(restored)
		}
	}
}

// isAnySyntheticPrefix reports whether s is a non-empty prefix of any synthetic token in the vault.
func (v *PrivacyVault) isAnySyntheticPrefix(s string) bool {
	if s == "" || v == nil {
		return false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	for syn := range v.reverse {
		if strings.HasPrefix(syn, s) {
			return true
		}
	}
	return false
}

type sseHeldEvent struct {
	raw     []byte
	content string
}

// DeAnonymizingResponseWriter intercepts streaming responses (SSE) and seamlessly
// restores original secrets from synthetic dummies, handling token boundaries across chunks.
type DeAnonymizingResponseWriter struct {
	underlying http.ResponseWriter
	flusher    http.Flusher
	vault      *PrivacyVault
	pending    []byte
	deltaAcc   string
	heldEvents []sseHeldEvent
	mu         sync.Mutex
}

// NewDeAnonymizingResponseWriter creates a streaming de-anonymizer wrapping w.
func NewDeAnonymizingResponseWriter(w http.ResponseWriter, flusher http.Flusher, vault *PrivacyVault) *DeAnonymizingResponseWriter {
	if flusher == nil {
		flusher, _ = w.(http.Flusher)
	}
	return &DeAnonymizingResponseWriter{
		underlying: w,
		flusher:    flusher,
		vault:      vault,
	}
}

func (d *DeAnonymizingResponseWriter) Header() http.Header {
	return d.underlying.Header()
}

func (d *DeAnonymizingResponseWriter) WriteHeader(statusCode int) {
	d.underlying.WriteHeader(statusCode)
}

func (d *DeAnonymizingResponseWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.vault == nil || d.vault.Len() == 0 {
		return d.underlying.Write(p)
	}

	d.pending = append(d.pending, p...)

	// If pending contains complete SSE double-newline boundaries
	if bytes.Contains(d.pending, []byte("data: ")) {
		for {
			idx := bytes.Index(d.pending, []byte("\n\n"))
			if idx < 0 {
				break
			}
			rawMsg := d.pending[:idx+2]
			d.pending = append([]byte(nil), d.pending[idx+2:]...)
			d.processSSEBlock(rawMsg)
		}
	} else {
		// Non-SSE stream fallback: do raw sliding window replacement
		pendingStr := string(d.pending)
		replaced := d.vault.ReverseReplace(pendingStr)
		d.pending = []byte(replaced)

		prefixLen := d.vault.LongestSyntheticPrefix(d.pending)
		if prefixLen > 0 {
			safeLen := len(d.pending) - prefixLen
			if safeLen > 0 {
				toWrite := d.pending[:safeLen]
				d.pending = append([]byte(nil), d.pending[safeLen:]...)
				if _, err := d.underlying.Write(toWrite); err != nil {
					return len(p), err
				}
				if d.flusher != nil {
					d.flusher.Flush()
				}
			}
		} else if len(d.pending) > 0 {
			toWrite := d.pending
			d.pending = nil
			if _, err := d.underlying.Write(toWrite); err != nil {
				return len(p), err
			}
			if d.flusher != nil {
				d.flusher.Flush()
			}
		}
	}

	if d.flusher != nil {
		d.flusher.Flush()
	}

	return len(p), nil
}

func (d *DeAnonymizingResponseWriter) processSSEBlock(raw []byte) {
	str := string(raw)
	lines := strings.Split(strings.TrimSuffix(str, "\n\n"), "\n")

	var dataLine string
	var otherLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			dataLine = strings.TrimPrefix(line, "data: ")
		} else {
			otherLines = append(otherLines, line)
		}
	}

	if dataLine == "" {
		d.underlying.Write(raw)
		return
	}

	if dataLine == "[DONE]" {
		for _, ev := range d.heldEvents {
			d.underlying.Write(ev.raw)
		}
		d.heldEvents = nil
		d.deltaAcc = ""
		d.underlying.Write(raw)
		return
	}

	var chunk struct {
		ID      string `json:"id,omitempty"`
		Object  string `json:"object,omitempty"`
		Created int64  `json:"created,omitempty"`
		Model   string `json:"model,omitempty"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role             string `json:"role,omitempty"`
				Content          string `json:"content,omitempty"`
				ReasoningContent string `json:"reasoning_content,omitempty"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason,omitempty"`
		} `json:"choices"`
	}

	if err := json.Unmarshal([]byte(dataLine), &chunk); err != nil || len(chunk.Choices) == 0 {
		replaced := d.vault.ReverseReplace(str)
		d.underlying.Write([]byte(replaced))
		return
	}

	content := chunk.Choices[0].Delta.Content
	if content == "" {
		d.underlying.Write(raw)
		return
	}

	testAcc := d.deltaAcc + content
	restored := d.vault.ReverseReplace(testAcc)
	if restored != testAcc {
		// Found complete synthetic token! Replace and emit consolidated delta
		chunk.Choices[0].Delta.Content = restored
		newJSON, err := json.Marshal(chunk)
		if err == nil {
			var out strings.Builder
			for _, ol := range otherLines {
				out.WriteString(ol)
				out.WriteString("\n")
			}
			out.WriteString("data: ")
			out.Write(newJSON)
			out.WriteString("\n\n")
			d.underlying.Write([]byte(out.String()))
		} else {
			d.underlying.Write(raw)
		}
		d.deltaAcc = ""
		d.heldEvents = nil
		return
	}

	if d.vault.isAnySyntheticPrefix(testAcc) {
		// Hold event while accumulating prefix
		d.deltaAcc = testAcc
		d.heldEvents = append(d.heldEvents, sseHeldEvent{raw: append([]byte(nil), raw...), content: content})
		return
	}

	// No match and not a prefix: flush held events first
	if len(d.heldEvents) > 0 {
		for _, ev := range d.heldEvents {
			d.underlying.Write(ev.raw)
		}
		d.heldEvents = nil
		d.deltaAcc = ""
	}

	replaced := d.vault.ReverseReplace(str)
	d.underlying.Write([]byte(replaced))
}

// FlushFinal flushes any buffered bytes and completes the stream.
func (d *DeAnonymizingResponseWriter) FlushFinal() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, ev := range d.heldEvents {
		d.underlying.Write(ev.raw)
	}
	d.heldEvents = nil
	d.deltaAcc = ""

	if len(d.pending) > 0 {
		replaced := d.vault.ReverseReplace(string(d.pending))
		d.underlying.Write([]byte(replaced))
		d.pending = nil
	}

	if d.flusher != nil {
		d.flusher.Flush()
	}
}

// Flush forwards the flush call to the underlying flusher.
func (d *DeAnonymizingResponseWriter) Flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.flusher != nil {
		d.flusher.Flush()
	}
}
