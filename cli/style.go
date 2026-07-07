package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/aigateway/config"
	"github.com/aigateway/tunnel"
)

// ==================== Color Palette ====================

var (
	// Primary colors
	Primary   = lipgloss.Color("#7C3AED") // Purple
	Secondary = lipgloss.Color("#3B82F6") // Blue
	Accent    = lipgloss.Color("#10B981") // Green

	// Semantic colors
	Success = lipgloss.Color("#10B981") // Green
	Error   = lipgloss.Color("#EF4444") // Red
	Warning = lipgloss.Color("#F59E0B") // Yellow
	Info    = lipgloss.Color("#3B82F6") // Blue

	// Neutral colors
	Muted    = lipgloss.Color("#6B7280") // Gray
	Subtle   = lipgloss.Color("#374151") // Dark gray
	White    = lipgloss.Color("#FFFFFF")
	TextMain = lipgloss.Color("#E5E7EB") // Light gray text
)

// ==================== Styled Components ====================

var (
	// Banner style
	BannerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Primary).
			Padding(0, 2)

	// Title style for sections
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Primary).
			MarginTop(1).
			MarginBottom(1)

	// Subtitle style
	SubtitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Secondary).
			MarginBottom(1)

	// Info box style
	InfoBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Secondary).
			Padding(0, 1).
			MarginTop(1).
			MarginBottom(1)

	// Success message
	SuccessStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Success)

	// Error message
	ErrorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Error)

	// Warning message
	WarningStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Warning)

	// Muted text
	MutedStyle = lipgloss.NewStyle().
			Foreground(Muted)

	// Key-value label
	KeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(White)

	// Key-value value
	ValueStyle = lipgloss.NewStyle().
			Foreground(TextMain)

	// Status badge styles
	BadgeSuccess = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#000000")).
			Background(Success).
			Padding(0, 1)
)

// ==================== Helper Functions ====================

// fetchTunnelURL queries the local server status endpoint to see if Cloudflare Quick Tunnel is active and returns the URL.
func fetchTunnelURL(cfg *config.Config) string {
	client := &http.Client{Timeout: 100 * time.Millisecond}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/admin/tunnel", cfg.Server.Port), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-Admin-Secret", cfg.Server.AdminSecret)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var res struct {
		IsRunning bool   `json:"is_running"`
		URL       string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ""
	}
	if res.IsRunning {
		return res.URL
	}
	return ""
}

// printBanner displays the styled application banner.
func printBanner(cfgPath string, providerCount, keyCount, modelCount int, cfg *config.Config) {
	fmt.Println()
	fmt.Println(BannerStyle.Render("🚀  AI Gateway — Config CLI"))
	fmt.Println()

	// 1. Get Token Saver (RTK) status
	rtkStatus := "❌ Off"
	if cfg.TokenSaver.Enabled {
		rtkStatus = fmt.Sprintf("✅ On (%d B)", cfg.TokenSaver.MaxInputBytes)
	}

	// 2. Get Quick Tunnel status & URL
	tunnelStatus := "❌ Off"
	if cfg.Server.QuickTunnel {
		isRunning, url := tunnel.GetGlobalStatus()
		if isRunning && url != "" {
			tunnelStatus = fmt.Sprintf("✅ Active (%s)", url)
		} else {
			// Fallback to HTTP query (e.g. if CLI is run in a separate process from the server)
			url = fetchTunnelURL(cfg)
			if url != "" {
				tunnelStatus = fmt.Sprintf("✅ Active (%s)", url)
			} else {
				tunnelStatus = "⏳ Connecting..."
			}
		}
	}

	// 3. Get Web Dashboard status
	dashStatus := "❌ Off"
	if cfg.Server.IsDashboardEnabled() {
		dashStatus = "✅ On"
	}

	infoText := KeyStyle.Render("📁 Config: ") + ValueStyle.Render(cfgPath) + "\n" +
		KeyStyle.Render("📡 Providers: ") + ValueStyle.Render(fmt.Sprintf("%d", providerCount)) +
		"  " + KeyStyle.Render("🔑 API Keys: ") + ValueStyle.Render(fmt.Sprintf("%d", keyCount)) +
		"  " + KeyStyle.Render("🤖 Models: ") + ValueStyle.Render(fmt.Sprintf("%d", modelCount)) + "\n" +
		KeyStyle.Render("🧠 Token Saver (RTK): ") + ValueStyle.Render(rtkStatus) + "\n" +
		KeyStyle.Render("📊 Web Dashboard: ") + ValueStyle.Render(dashStatus) + "\n" +
		KeyStyle.Render("🌐 Quick Tunnel: ") + ValueStyle.Render(tunnelStatus)

	fmt.Println(InfoBoxStyle.Render(infoText))
}

// printSectionTitle prints a styled section title.
func printSectionTitle(emoji, title string) {
	fmt.Println()
	fmt.Println(TitleStyle.Render(fmt.Sprintf("%s  %s", emoji, title)))
}

// printSuccess prints a styled success message.
func printSuccess(msg string) {
	fmt.Println(SuccessStyle.Render("  ✅ " + msg))
}

// printError prints a styled error message.
func printError(msg string) {
	fmt.Println(ErrorStyle.Render("  ❌ " + msg))
}

// printWarning prints a styled warning message.
func printWarning(msg string) {
	fmt.Println(WarningStyle.Render("  ⚠  " + msg))
}

// printInfo prints a styled info message.
func printInfo(msg string) {
	fmt.Println(MutedStyle.Render("  💡 " + msg))
}

// printKeyValue prints a styled key-value pair.
func printKeyValue(key, value string) {
	fmt.Printf("  %s %s\n", KeyStyle.Render(key), ValueStyle.Render(value))
}

// ==================== Spinner ====================

// SpinnerFrames is the animation frames for a braille spinner.
var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// withSpinner runs fn while showing an animated spinner with the given message.
// Returns the result of fn. Handles panics gracefully.
func withSpinner(msg string, fn func() error) error {
	done := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- fn()
	}()

	frames := SpinnerFrames
	i := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			// Clear spinner line
			fmt.Printf("\r%s\r", strings.Repeat(" ", len(msg)+10))
			return err
		case <-ticker.C:
			frame := lipgloss.NewStyle().Foreground(Primary).Render(frames[i%len(frames)])
			fmt.Printf("\r  %s %s", frame, MutedStyle.Render(msg))
			i++
		}
	}
}

// ==================== Terminal Helpers ====================

// clearScreen clears the terminal screen.
func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

// maskString masks a string, showing only the first 4 characters.
func maskString(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:4] + strings.Repeat("*", len(s)-4)
}

// providerTypeBadge returns a styled badge for a provider type.
func providerTypeBadge(pType string) string {
	switch pType {
	case "openai":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Render("● " + pType)
	case "anthropic":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B")).Render("● " + pType)
	case "groq":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#8B5CF6")).Render("● " + pType)
	case "mistral":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#3B82F6")).Render("● " + pType)
	default:
		return lipgloss.NewStyle().Foreground(Muted).Render("● " + pType)
	}
}

// plural returns singular or plural form based on count.
// Example: plural(1, "model", "models") → "1 model"
//          plural(3, "model", "models") → "3 models"
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}

