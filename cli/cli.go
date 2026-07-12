package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"

	"github.com/aigateway/config"
)

// Run starts the interactive CLI for config management.
func Run(cfgPath string) {
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Println(WarningStyle.Render(fmt.Sprintf("  ⚠  Config belum ada atau error: %v", err)))
		cfg = defaultConfig()
	}
	setLanguage(cfg.Server.Language)

	for {
		clearScreen()
		printBanner(cfgPath, len(cfg.Providers), len(cfg.APIKeys), len(cfg.Models), cfg)

		var choice string
		menu := huh.NewSelect[string]().
			Title(T("  Pilih Menu", "  Choose Menu")).
			Options(
				huh.NewOption(T("🤖  Manajemen AI (Provider/Model/Combo)", "🤖  AI Management (Provider/Model/Combo)"), "ai"),
				huh.NewOption(T("⚙️   Manajemen Server", "⚙️   Server Management"), "server"),
				huh.NewOption(T("🔑  Manajemen API Key (User/Client)", "🔑  API Key Management (User/Client)"), "keys"),
				huh.NewOption(T("🧪  Diagnostik & Testing", "🧪  Diagnostics & Testing"), "diag"),
				huh.NewOption(T("📋  Lihat Summary", "📋  View Summary"), "summary"),
				huh.NewOption(T("💾  Simpan & Keluar", "💾  Save & Exit"), "quit"),
			).
			Value(&choice)

		if err := menu.Run(); err != nil {
			if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, huh.ErrUserAborted) {
				safeSave(cfg, cfgPath)
				fmt.Println(SuccessStyle.Render(T("\n  👋 Sampai jumpa!", "\n  👋 Goodbye!")))
				return
			}
			continue
		}

		switch choice {
		case "ai":
			menuAI(cfg, cfgPath)
		case "server":
			menuServer(cfg, cfgPath)
		case "keys":
			menuAPIKeys(cfg, cfgPath)
		case "diag":
			menuTest(cfg, cfgPath)
		case "summary":
			clearScreen()
			showSummary(cfg)
			pause()
		case "quit":
			safeSave(cfg, cfgPath)
			fmt.Println(SuccessStyle.Render(T("\n  👋 Sampai jumpa!", "\n  👋 Goodbye!")))
			return
		}
	}
}

func defaultConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Host:        "0.0.0.0",
			Port:        8080,
			AdminSecret: "change-me-" + uuid.New().String()[:8],
		},
		Concurrency: config.ConcurrencyConfig{
			MaxConcurrent: 100,
			PerProvider:   30,
			PerModel:      15,
		},
	}
}

// ==================== Summary ====================

func showSummary(cfg *config.Config) {
	printSectionTitle("📋", T("Ringkasan Konfigurasi", "Configuration Summary"))

	// Server info
	printKeyValue("🌐  Server:", fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port))
	printKeyValue("🔐  Admin Secret:", maskString(cfg.Server.AdminSecret))
	printKeyValue("📝  Log Level:", cfg.Server.LogLevel)
	fmt.Println()

	// Providers
	if len(cfg.Providers) > 0 {
		// Hitung routes aktif per provider
		routeCount := make(map[string]int)
		for _, m := range cfg.Models {
			if m.Strategy == "" && m.Provider != "" {
				routeCount[m.Provider]++
			}
		}

		fmt.Println(SubtitleStyle.Render("  📡  Providers"))
		for _, p := range cfg.Providers {
			badge := providerTypeBadge(p.Type)
			nRoutes := routeCount[p.Name]
			nRaw := len(p.Models)
			modelInfo := plural(nRoutes, "model", "models")
			if nRaw != nRoutes {
				modelInfo = fmt.Sprintf("%d/%d models", nRoutes, nRaw)
			}
			fmt.Printf("    %s %-14s %s  │  %s  │  %s\n",
				badge,
				KeyStyle.Render(p.Name),
				lipgloss.NewStyle().Foreground(Muted).Render(p.BaseURL),
				plural(len(p.APIKeys), "key", "keys"),
				modelInfo)
		}
		fmt.Println()
	}

	// Models
	if len(cfg.Models) > 0 {
		fmt.Println(SubtitleStyle.Render("  🤖  Models & Combos"))
		for _, m := range cfg.Models {
			if m.Strategy != "" {
				icon := "🔀"
				if m.Strategy == "fallback" {
					icon = "🔄"
				}
				fmt.Printf("    %s %-20s %s (%d backends)\n",
					icon,
					KeyStyle.Render(m.Name),
					lipgloss.NewStyle().Foreground(Warning).Render(m.Strategy),
					len(m.Backends))
			} else {
				fmt.Printf("    🤖 %-20s %s\n",
					KeyStyle.Render(m.Name),
					MutedStyle.Render(fmt.Sprintf("→ %s/%s", m.Provider, m.Model)))
			}
		}
		fmt.Println()
	}

	// API Keys
	if len(cfg.APIKeys) > 0 {
		fmt.Println(SubtitleStyle.Render("  🔑  API Keys"))
		for _, k := range cfg.APIKeys {
			rateStr := "unlimited"
			if k.RateLimit > 0 {
				rateStr = fmt.Sprintf("%d req/min", k.RateLimit)
			}
			models := strings.Join(k.AllowedModels, ", ")
			if len(models) > 30 {
				models = models[:30] + "..."
			}
			fmt.Printf("    🔑 %-20s %s  │  %s\n",
				KeyStyle.Render(k.Name),
				lipgloss.NewStyle().Foreground(Muted).Render(models),
				lipgloss.NewStyle().Foreground(Accent).Render(rateStr))
		}
		fmt.Println()
	}

	// Concurrency
	printKeyValue("⚡  Concurrency:",
		fmt.Sprintf("global=%d, provider=%d, model=%d",
			cfg.Concurrency.MaxConcurrent, cfg.Concurrency.PerProvider, cfg.Concurrency.PerModel))
}

// ==================== Shared Helpers ====================

// isAbort returns true if the error indicates the user pressed Esc/Ctrl+C.
// Used by sub-flows to cleanly return to the parent menu instead of exiting the CLI.
func isAbort(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, tea.ErrProgramKilled) {
		return true
	}
	// huh sometimes wraps abort errors — match by string as fallback
	msg := err.Error()
	return msg == "user aborted" || msg == "program killed"
}

// pause waits for user to press Enter.
func pause() {
	fmt.Println()
	huh.NewInput().
		Title("").
		Description(T("Tekan Enter untuk lanjut...", "Press Enter to continue...")).
		Value(new(string)).
		Run()
}

// splitTrim splits a comma-separated string and trims whitespace.
func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// detectedReasoningModels stores models that have been programmatically verified to support reasoning.
var detectedReasoningModels = make(map[string]bool)

// createModelRoutes creates direct model routes for a provider.
func createModelRoutes(cfg *config.Config, providerName string, models []string) int {
	created := 0
	for _, modelName := range models {
		m := config.ModelConfig{
			Name:      modelName,
			Provider:  providerName,
			Model:     modelName,
			Reasoning: detectedReasoningModels[modelName] || isReasoningModelID(modelName),
			Vision:    isVisionModelID(modelName),
		}
		if err := cfg.AddModel(m); err == nil {
			created++
		}
	}
	return created
}

// safeSave validates and saves the config, then triggers hot reload.
func safeSave(cfg *config.Config, path string) {
	if err := cfg.Validate(); err != nil {
		printWarning(fmt.Sprintf("Config tidak valid: %v", err))
		return
	}
	if err := cfg.SaveConfig(path); err != nil {
		printWarning(fmt.Sprintf("Gagal simpan: %v", err))
		return
	}
	printSuccess("Config tersimpan!")
	triggerHotReload(cfg)
}

// triggerHotReload sends a POST to the admin reload endpoint.
func triggerHotReload(cfg *config.Config) {
	port := cfg.Server.Port
	secret := cfg.Server.AdminSecret
	host := cfg.Server.Host
	if host == "0.0.0.0" {
		host = "localhost"
	}
	url := fmt.Sprintf("http://%s:%d/admin/config/reload", host, port)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Admin-Secret", secret)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Server might not be running — that's OK
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		printSuccess("Server hot-reload berhasil dipicu!")
	}
}
