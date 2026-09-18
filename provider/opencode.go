package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// OpenCodeDefaultUA is the canonical User-Agent required by OpenCode Zen upstream.
	OpenCodeDefaultUA = "opencode/1.18.31"
	base62Chars       = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// OpenCodeSessionRegex matches canonical OpenCode session IDs: ses_ + 12 hex + 14 Base62 (exactly 30 chars).
var OpenCodeSessionRegex = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var openCodeUARegex = regexp.MustCompile(`^opencode/(\d+)\.(\d+)(?:\.(\d+))?`)

var sessionCounter atomic.Uint64

type opencodeCtxKey string

const (
	ctxKeySessionID  opencodeCtxKey = "opencode_session_id"
	ctxKeyClientTool opencodeCtxKey = "opencode_client_tool"
	ctxKeyUserAgent  opencodeCtxKey = "opencode_user_agent"
)

// WithOpenCodeMeta stores client session, tool name, and user-agent into context.
func WithOpenCodeMeta(ctx context.Context, sessionID, clientTool, userAgent string) context.Context {
	if sessionID != "" {
		ctx = context.WithValue(ctx, ctxKeySessionID, sessionID)
	}
	if clientTool != "" {
		ctx = context.WithValue(ctx, ctxKeyClientTool, clientTool)
	}
	if userAgent != "" {
		ctx = context.WithValue(ctx, ctxKeyUserAgent, userAgent)
	}
	return ctx
}

// GetOpenCodeMeta extracts opencode session, client tool, and user-agent from context.
func GetOpenCodeMeta(ctx context.Context) (sessionID, clientTool, userAgent string) {
	if ctx == nil {
		return
	}
	if v, ok := ctx.Value(ctxKeySessionID).(string); ok {
		sessionID = v
	}
	if v, ok := ctx.Value(ctxKeyClientTool).(string); ok {
		clientTool = v
	}
	if v, ok := ctx.Value(ctxKeyUserAgent).(string); ok {
		userAgent = v
	}
	return
}

// GenerateOpenCodeSessionID generates a canonical 30-character session ID:
// "ses_" + 12 hex characters (time-based, descending ordering) + 14 Base62 characters.
func GenerateOpenCodeSessionID() string {
	ts := uint64(time.Now().UnixMilli())
	cnt := sessionCounter.Add(1)
	current := (ts << 12) + (cnt & 0xFFF)
	val := ^current // bitwise NOT for descending ordering

	var timeBytes [6]byte
	for i := 0; i < 6; i++ {
		timeBytes[i] = byte((val >> (40 - 8*i)) & 0xFF)
	}
	timeHex := hex.EncodeToString(timeBytes[:])

	var randBytes [14]byte
	_, _ = rand.Read(randBytes[:])
	var randPart strings.Builder
	for _, b := range randBytes {
		randPart.WriteByte(base62Chars[b%62])
	}

	return fmt.Sprintf("ses_%s%s", timeHex, randPart.String())
}

// TranslateOpenCodeSessionID deterministically translates an arbitrary session ID
// into a canonical OpenCode session ID so multi-turn prompt caching in OpenCode
// remains consistent across turns.
func TranslateOpenCodeSessionID(rawSession, clientTool string) string {
	raw := strings.TrimSpace(rawSession)
	if OpenCodeSessionRegex.MatchString(raw) {
		return raw
	}
	if raw == "" {
		return GenerateOpenCodeSessionID()
	}
	if clientTool == "" {
		clientTool = "gogate"
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("opencode\x00%s\x00%s", clientTool, raw)))
	timeHex := hex.EncodeToString(h[:6])
	var randPart strings.Builder
	for _, b := range h[6:20] {
		randPart.WriteByte(base62Chars[b%62])
	}
	return fmt.Sprintf("ses_%s%s", timeHex, randPart.String())
}

// FormatOpenCodeUserAgent checks whether the incoming User-Agent is a valid opencode version (>= 1.17.0).
// If valid, returns it; otherwise returns OpenCodeDefaultUA.
func FormatOpenCodeUserAgent(incomingUA string) string {
	incomingUA = strings.TrimSpace(incomingUA)
	if incomingUA == "" {
		return OpenCodeDefaultUA
	}
	matches := openCodeUARegex.FindStringSubmatch(incomingUA)
	if len(matches) >= 3 {
		major, err1 := strconv.Atoi(matches[1])
		minor, err2 := strconv.Atoi(matches[2])
		if err1 == nil && err2 == nil {
			if major > 1 || (major == 1 && minor >= 17) {
				return incomingUA
			}
		}
	}
	return OpenCodeDefaultUA
}
