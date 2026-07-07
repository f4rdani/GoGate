package middleware

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

const (
	// truncationMarker is appended when content is truncated.
	truncationMarker = "\n... [truncated by AI Gateway Token Saver]"
)

// TokenSaverStats tracks compression metrics for a single request.
type TokenSaverStats struct {
	OriginalBytes   int
	CompressedBytes int
	MessagesChanged int
}

// CompressMessages applies RTK-style compression to request messages based on config.
// Returns stats about what was compressed. Modifies request in-place.
func CompressMessages(req *models.ChatCompletionRequest, cfg config.TokenSaverConfig) *TokenSaverStats {
	if !cfg.Enabled || req == nil {
		return nil
	}

	stats := &TokenSaverStats{}

	if len(req.Messages) == 0 {
		return stats
	}

	for i := range req.Messages {
		msg := &req.Messages[i]
		
		// Never compress system messages (they usually carry prompt instructions)
		if msg.Role == "system" {
			continue
		}

		// Role checks:
		// If CompressUser is false: only compress tool and assistant.
		// If CompressUser is true: compress user, tool, assistant, etc.
		if !cfg.CompressUser && msg.Role != "tool" && msg.Role != "assistant" {
			continue
		}

		origLen := len(msg.Content)
		stats.OriginalBytes += origLen

		changed := compressMessageContent(msg, cfg)
		newLen := len(msg.Content)
		stats.CompressedBytes += newLen

		if changed {
			stats.MessagesChanged++
		}
	}

	return stats
}

// CompressToolOutputs is the legacy function (always-on, default 4KB threshold).
func CompressToolOutputs(req *models.ChatCompletionRequest) {
	CompressMessages(req, config.TokenSaverConfig{
		Enabled:       true,
		MaxInputBytes: 4096,
		CompressUser:  true,
		MinifyJSON:    true,
	})
}

// InjectCavemanMode prepends or adds a system message instructing the LLM to output tersely.
func InjectCavemanMode(req *models.ChatCompletionRequest) {
	if req == nil {
		return
	}

	const cavemanPrompt = "IMPORTANT: Be extremely concise in your responses. Use short sentences. No unnecessary words. Skip pleasantries and preamble. Provide code-only answers when possible. Avoid verbose markdown formatting unless essential. Think step-by-step internally but output only the final answer."

	// Check if there is already a system message at the beginning
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		sysMsg := &req.Messages[0]
		var content string
		if err := json.Unmarshal(sysMsg.Content, &content); err == nil {
			newContent := cavemanPrompt + "\n\n" + content
			sysMsg.Content, _ = json.Marshal(newContent)
			return
		}
	}

	// Otherwise, prepend a new system message
	newMsg := models.Message{
		Role:    "system",
		Content: json.RawMessage(strconv.Quote(cavemanPrompt)),
	}
	req.Messages = append([]models.Message{newMsg}, req.Messages...)
}

// compressMessageContent compresses the content of a single message and returns true if modified.
func compressMessageContent(msg *models.Message, cfg config.TokenSaverConfig) bool {
	if len(msg.Content) == 0 {
		return false
	}

	// Try to parse as string first
	var content string
	if err := json.Unmarshal(msg.Content, &content); err == nil {
		compressed := compressText(content, cfg)
		if compressed != content {
			msg.Content, _ = json.Marshal(compressed)
			return true
		}
		return false
	}

	// Try to parse as array of content parts (Claude/OpenAI multi-modal format)
	var parts []json.RawMessage
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		return false
	}

	modified := false
	for j, part := range parts {
		// Try to parse each part as an object with type/text or type/content fields
		var contentPart struct {
			Type    string `json:"type"`
			Text    string `json:"text,omitempty"`
			Content string `json:"content,omitempty"`
		}
		if err := json.Unmarshal(part, &contentPart); err != nil {
			continue
		}

		text := contentPart.Text
		if text == "" {
			text = contentPart.Content
		}
		if text == "" {
			continue
		}

		compressed := compressText(text, cfg)
		if compressed != text {
			var m map[string]json.RawMessage
			json.Unmarshal(part, &m)
			textJSON, _ := json.Marshal(compressed)
			if _, ok := m["text"]; ok {
				m["text"] = textJSON
			} else if _, ok := m["content"]; ok {
				m["content"] = textJSON
			}
			newPart, _ := json.Marshal(m)
			parts[j] = newPart
			modified = true
		}
	}

	if modified {
		msg.Content, _ = json.Marshal(parts)
	}
	return modified
}

// compressText applies various compression patterns.
func compressText(text string, cfg config.TokenSaverConfig) string {
	// First, check if JSON minification is requested and try that.
	// JSON minification can be run even on smaller payloads as it strips whitespace safely.
	if cfg.MinifyJSON && isJSON(text) {
		text = minifyJSON(text)
	}

	// If length is below threshold, stop compressing (except JSON minify which we already did)
	if len(text) <= cfg.MaxInputBytes {
		return text
	}

	lines := strings.Split(text, "\n")

	// Apply comment stripping if requested
	if cfg.StripComments {
		text = stripComments(text)
		lines = strings.Split(text, "\n")
	}

	// Check and compress known patterns:
	if isDiffOutput(lines) {
		text = compressDiff(lines, cfg.MaxInputBytes)
	} else if isTreeOutput(lines) {
		text = compressTree(lines)
	} else if isStackTrace(lines) {
		text = compressStackTrace(lines)
	} else if isGrepOutput(lines) {
		text = compressGrep(lines)
	} else if isTestOutput(lines) {
		text = compressTest(lines)
	} else if isBuildOutput(lines) {
		text = compressBuild(lines)
	} else if isLogOutput(lines) {
		text = compressLog(lines)
	} else {
		// Generic fallback: collapse duplicates, then truncate middle if still too large
		text = collapseDuplicates(text)
		if len(text) > cfg.MaxInputBytes {
			text = truncateMiddle(text, cfg.MaxInputBytes)
		}
	}

	// Final step: whitespace normalization
	text = normalizeWhitespace(text)

	return text
}

// ---------------------------------------------------------
// Git Diff Pattern
// ---------------------------------------------------------

func isDiffOutput(lines []string) bool {
	diffIndicators := 0
	for _, line := range lines[:min(20, len(lines))] {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") ||
			strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "diff --git") {
			diffIndicators++
		}
	}
	return diffIndicators >= 2
}

func compressDiff(lines []string, maxBytes int) string {
	if len(lines) <= 30 {
		return strings.Join(lines, "\n")
	}

	var result []string
	headerLines := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			result = append(result, line)
			headerLines++
		}
	}

	hunkCount := 0
	inHunk := false
	for _, line := range lines[headerLines:] {
		if strings.HasPrefix(line, "@@") {
			hunkCount++
			if hunkCount > 3 {
				result = append(result, truncationMarker)
				break
			}
			inHunk = true
		}
		if inHunk || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			result = append(result, line)
		}
	}

	compressed := strings.Join(result, "\n")
	if len(compressed) > maxBytes {
		compressed = truncateMiddle(compressed, maxBytes)
	}
	return compressed
}

// ---------------------------------------------------------
// Directory Tree Pattern
// ---------------------------------------------------------

func isTreeOutput(lines []string) bool {
	if len(lines) < 5 {
		return false
	}
	treeIndicators := 0
	for _, line := range lines[:min(15, len(lines))] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "├──") || strings.HasPrefix(trimmed, "└──") ||
			strings.HasPrefix(trimmed, "│") {
			treeIndicators++
		}
	}
	return treeIndicators >= 3
}

func compressTree(lines []string) string {
	if len(lines) <= 40 {
		return strings.Join(lines, "\n")
	}

	var result []string
	count := 0
	for _, line := range lines {
		result = append(result, line)
		count++
		if count >= 40 {
			result = append(result, truncationMarker)
			break
		}
	}
	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// Stack Trace Pattern
// ---------------------------------------------------------

func isStackTrace(lines []string) bool {
	if len(lines) < 3 {
		return false
	}
	traceIndicators := 0
	for _, line := range lines[:min(10, len(lines))] {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "at ") && strings.Contains(trimmed, "(") {
			traceIndicators++
		}
		if strings.HasPrefix(trimmed, "Traceback") {
			traceIndicators++
		}
	}
	return traceIndicators >= 2
}

func compressStackTrace(lines []string) string {
	if len(lines) <= 20 {
		return strings.Join(lines, "\n")
	}

	var result []string
	result = append(result, lines[0])
	frameCount := 0
	for _, line := range lines[1:] {
		result = append(result, line)
		if strings.TrimSpace(line) != "" {
			frameCount++
		}
		if frameCount >= 10 {
			result = append(result, truncationMarker)
			break
		}
	}
	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// Grep / Find Pattern
// ---------------------------------------------------------

var grepRegex = regexp.MustCompile(`^(\.?\/?[a-zA-Z0-9_\-\.\/]+):(\d+):(.*)$`)

func isGrepOutput(lines []string) bool {
	matches := 0
	for _, line := range lines[:min(10, len(lines))] {
		if grepRegex.MatchString(line) {
			matches++
		}
	}
	return matches >= 2
}

func compressGrep(lines []string) string {
	type fileMatch struct {
		lineNum string
		content string
	}
	fileGroups := make(map[string][]fileMatch)
	var fileOrder []string

	for _, line := range lines {
		m := grepRegex.FindStringSubmatch(line)
		if len(m) == 4 {
			file := m[1]
			lineNum := m[2]
			content := m[3]
			
			if _, ok := fileGroups[file]; !ok {
				fileOrder = append(fileOrder, file)
			}
			fileGroups[file] = append(fileGroups[file], fileMatch{lineNum: lineNum, content: content})
		}
	}

	if len(fileOrder) == 0 {
		return strings.Join(lines, "\n")
	}

	var result []string
	// Limit to max 20 files, and max 5 matches per file
	maxFiles := min(20, len(fileOrder))
	for i := 0; i < maxFiles; i++ {
		file := fileOrder[i]
		matches := fileGroups[file]
		result = append(result, "📂 "+file+":")
		
		maxMatches := min(5, len(matches))
		for j := 0; j < maxMatches; j++ {
			result = append(result, "  Line "+matches[j].lineNum+": "+strings.TrimSpace(matches[j].content))
		}
		if len(matches) > 5 {
			result = append(result, "  ... and "+strconv.Itoa(len(matches)-5)+" more matches")
		}
	}
	if len(fileOrder) > 20 {
		result = append(result, truncationMarker)
	}

	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// Log Output Pattern
// ---------------------------------------------------------

// logRegex detects simple timestamp formats or logging severity badges.
var logRegex = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}|\d{2}:\d{2}:\d{2}|\[\d{2}|\[INFO\]|\[ERROR\]|\[WARN\]|INFO|ERROR|WARN|DEBUG)`)

func isLogOutput(lines []string) bool {
	matches := 0
	for _, line := range lines[:min(10, len(lines))] {
		if logRegex.MatchString(line) {
			matches++
		}
	}
	return matches >= 3
}

func compressLog(lines []string) string {
	if len(lines) < 5 {
		return strings.Join(lines, "\n")
	}

	var result []string
	lastLine := ""
	dupCount := 0

	for _, line := range lines {
		// Strip timestamps to find similar lines for collapsing
		cleaned := stripTimestamp(line)
		if cleaned == lastLine && cleaned != "" {
			dupCount++
			continue
		}

		if dupCount > 0 {
			result = append(result, "  [×"+strconv.Itoa(dupCount+1)+" repeated log entries collapsed]")
			dupCount = 0
		}
		result = append(result, line)
		lastLine = cleaned
	}

	if dupCount > 0 {
		result = append(result, "  [×"+strconv.Itoa(dupCount+1)+" repeated log entries collapsed]")
	}

	// If still too long, keep first 30 lines and last 10 lines
	if len(result) > 50 {
		var truncated []string
		truncated = append(truncated, result[:30]...)
		truncated = append(truncated, truncationMarker)
		truncated = append(truncated, result[len(result)-10:]...)
		return strings.Join(truncated, "\n")
	}

	return strings.Join(result, "\n")
}

func stripTimestamp(s string) string {
	// Crude removal of timestamp prefix to detect repeated log patterns
	if len(s) > 25 {
		// e.g. "2024-05-14T12:34:56.789Z [INFO] msg" -> "[INFO] msg"
		// Just take substring after first few spaces or bracket
		if idx := strings.Index(s, " ]"); idx != -1 {
			return s[idx+2:]
		}
		if idx := strings.Index(s, "] "); idx != -1 {
			return s[idx+2:]
		}
		// Split by spaces, skip first 2 tokens (often date and time)
		parts := strings.Fields(s)
		if len(parts) > 2 {
			return strings.Join(parts[2:], " ")
		}
	}
	return s
}

// ---------------------------------------------------------
// Build / Compile Pattern
// ---------------------------------------------------------

func isBuildOutput(lines []string) bool {
	indicators := 0
	for _, line := range lines[:min(20, len(lines))] {
		l := strings.ToLower(line)
		if strings.Contains(l, "error:") || strings.Contains(l, "warning:") ||
			strings.Contains(l, "failed") || strings.Contains(l, "build") ||
			strings.Contains(l, "compil") {
			indicators++
		}
	}
	return indicators >= 3
}

func compressBuild(lines []string) string {
	var errors []string
	var warnings []string
	var others []string

	for _, line := range lines {
		l := strings.ToLower(line)
		if strings.Contains(l, "error:") || strings.Contains(l, "failed:") || strings.Contains(l, "err:") {
			errors = append(errors, line)
		} else if strings.Contains(l, "warning:") || strings.Contains(l, "warn:") {
			warnings = append(warnings, line)
		} else if len(others) < 15 {
			others = append(others, line)
		}
	}

	var result []string
	result = append(result, "🛠️ Build Output Summary:")
	result = append(result, others...)

	if len(errors) > 0 {
		result = append(result, "\n🔴 Compile Errors ("+strconv.Itoa(len(errors))+"):")
		// Limit errors output to max 10
		maxErrs := min(10, len(errors))
		result = append(result, errors[:maxErrs]...)
		if len(errors) > 10 {
			result = append(result, "  ... and "+strconv.Itoa(len(errors)-10)+" more errors")
		}
	}

	if len(warnings) > 0 {
		result = append(result, "\n🟡 Warnings collapsed ("+strconv.Itoa(len(warnings))+" warnings omitted to save tokens)")
		// Show just top 2 warnings
		maxWarns := min(2, len(warnings))
		for i := 0; i < maxWarns; i++ {
			result = append(result, "  - "+warnings[i])
		}
	}

	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// Test Output Pattern
// ---------------------------------------------------------

func isTestOutput(lines []string) bool {
	indicators := 0
	for _, line := range lines[:min(20, len(lines))] {
		if strings.HasPrefix(line, "=== RUN") || strings.HasPrefix(line, "--- PASS") ||
			strings.HasPrefix(line, "--- FAIL") || strings.HasPrefix(line, "PASS") ||
			strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "ok ") {
			indicators++
		}
	}
	return indicators >= 2
}

func compressTest(lines []string) string {
	var failures []string
	runCount := 0
	passCount := 0

	for _, line := range lines {
		if strings.HasPrefix(line, "=== RUN") {
			runCount++
		} else if strings.HasPrefix(line, "--- PASS") || strings.HasPrefix(line, "PASS") {
			passCount++
		} else if strings.HasPrefix(line, "--- FAIL") || (strings.HasPrefix(line, "FAIL") && len(line) > 4) || strings.Contains(line, "FAIL:") {
			failures = append(failures, line)
		}
	}

	var result []string
	result = append(result, "🧪 Test Summary:")
	result = append(result, "  Total Runs: "+strconv.Itoa(runCount))
	result = append(result, "  Passed: "+strconv.Itoa(passCount))
	result = append(result, "  Failed: "+strconv.Itoa(len(failures)))

	if len(failures) > 0 {
		result = append(result, "\n🔴 Failed Tests Detail:")
		// limit to first 10 failures
		maxFails := min(10, len(failures))
		result = append(result, failures[:maxFails]...)
		if len(failures) > 10 {
			result = append(result, "  ... and "+strconv.Itoa(len(failures)-10)+" more failures")
		}
	}

	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// JSON Minification
// ---------------------------------------------------------

func isJSON(s string) bool {
	s = strings.TrimSpace(s)
	if !((strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"))) {
		return false
	}
	var js json.RawMessage
	return json.Unmarshal([]byte(s), &js) == nil
}

func minifyJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err == nil {
		return buf.String()
	}
	return s
}

// ---------------------------------------------------------
// Duplicate Line Collapsing
// ---------------------------------------------------------

func collapseDuplicates(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) < 5 {
		return text
	}

	var result []string
	lastLine := ""
	dupCount := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == lastLine && trimmed != "" {
			dupCount++
			continue
		}

		if dupCount >= 2 {
			result = append(result, "  [×"+strconv.Itoa(dupCount+1)+" repeated lines collapsed]")
			dupCount = 0
		} else if dupCount == 1 {
			result = append(result, line) // output it normally since it's just 2 lines
			dupCount = 0
		}

		result = append(result, line)
		lastLine = trimmed
	}

	if dupCount >= 2 {
		result = append(result, "  [×"+strconv.Itoa(dupCount+1)+" repeated lines collapsed]")
	} else if dupCount == 1 {
		result = append(result, lastLine)
	}

	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// Comments Stripping
// ---------------------------------------------------------

var pythonComment = regexp.MustCompile(`#.*$`)
var cComment      = regexp.MustCompile(`\/\/.*$`)

func stripComments(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	inCodeBlock := false

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCodeBlock = !inCodeBlock
			result = append(result, line)
			continue
		}

		if inCodeBlock {
			// Strip single line comments
			// Check if it's Python/Shell
			line = pythonComment.ReplaceAllString(line, "")
			// Check if it's Go/JS/C
			line = cComment.ReplaceAllString(line, "")
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// Whitespace Normalization
// ---------------------------------------------------------

func normalizeWhitespace(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	consecutiveBlank := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			consecutiveBlank++
			if consecutiveBlank == 1 {
				result = append(result, "")
			}
		} else {
			consecutiveBlank = 0
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// ---------------------------------------------------------
// Truncation
// ---------------------------------------------------------

func truncateMiddle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	keep := (maxLen - len(truncationMarker)) / 2
	if keep < 10 {
		keep = 10
	}
	if keep*2 >= len(s) {
		return s
	}
	return s[:keep] + truncationMarker + s[len(s)-keep:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
