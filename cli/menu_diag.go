package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/aigateway/config"
)

// ==================== Diagnostik & Testing Menu ====================

func menuTest(cfg *config.Config, cfgPath string) {
	for {
		clearScreen()
		printSectionTitle("🧪", "Diagnostik & Testing")

		var choice string
		err := huh.NewSelect[string]().
			Title("").
			Options(
				huh.NewOption("🔑  Test API Key (verifikasi)", "testkey"),
				huh.NewOption("🤖  Test Model (kirim request)", "testmodel"),
				huh.NewOption("🔄  Sync Models (re-fetch dari API)", "sync"),
				huh.NewOption("←  Kembali", "back"),
			).
			Value(&choice).
			Run()

		if isAbort(err) || choice == "back" {
			return
		}
		if err != nil {
			continue
		}

		switch choice {
		case "testkey":
			testAPIKeyMenu(cfg)
			pause()
		case "testmodel":
			testModelMenu(cfg, cfgPath)
			pause()
		case "sync":
			syncModelsMenu(cfg, cfgPath)
			pause()
		}
	}
}

func testAPIKeyMenu(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider. Tambah dulu di Manajemen AI.")
		return
	}

	printSectionTitle("🔑", "Test API Key")

	var idxStr string
	huh.NewSelect[string]().
		Title("Pilih provider yang mau di-test").
		Options(providerSelectOptionsWithCancel(cfg)...).
		Value(&idxStr).
		Run()
	if idxStr == "" || idxStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(cfg.Providers) {
		return
	}

	p := cfg.Providers[idx]
	if len(p.APIKeys) == 0 {
		printError("Provider ini tidak punya API key")
		return
	}

	fmt.Println()

	// If there is only 1 API key, test it directly
	if len(p.APIKeys) == 1 {
		runSingleKeyTest(p.Name, p.BaseURL, p.APIKeys[0], p.Type, 1)
		return
	}

	// If there are multiple API keys, ask user what to do
	var choice string
	errChoice := huh.NewSelect[string]().
		Title(fmt.Sprintf("Provider %s memiliki %d API Key. Pilih metode test:", p.Name, len(p.APIKeys))).
		Options(
			huh.NewOption(fmt.Sprintf("🔍  Test Semua API Key (%d keys)", len(p.APIKeys)), "all"),
			huh.NewOption("🔑  Test API Key Spesifik", "specific"),
			huh.NewOption("❌  Batal / kembali", "back"),
		).
		Value(&choice).
		Run()

	if isAbort(errChoice) || choice == "back" || choice == "" {
		printInfo("Dibatalkan")
		return
	}

	if choice == "all" {
		fmt.Println()
		printInfo(fmt.Sprintf("Memulai pengetesan %d API key untuk %s...", len(p.APIKeys), p.Name))
		for i, key := range p.APIKeys {
			fmt.Println()
			runSingleKeyTest(p.Name, p.BaseURL, key, p.Type, i+1)
		}
	} else if choice == "specific" {
		var selectedKeyIdx int
		options := make([]huh.Option[int], len(p.APIKeys))
		for i, key := range p.APIKeys {
			options[i] = huh.NewOption(fmt.Sprintf("Key #%d (%s)", i+1, maskAPIKey(key)), i)
		}
		options = append(options, huh.NewOption("❌  Batal / kembali", -1))

		errKeySelect := huh.NewSelect[int]().
			Title("Pilih API key yang ingin di-test").
			Options(options...).
			Value(&selectedKeyIdx).
			Run()

		if isAbort(errKeySelect) || selectedKeyIdx == -1 {
			printInfo("Dibatalkan")
			return
		}

		fmt.Println()
		runSingleKeyTest(p.Name, p.BaseURL, p.APIKeys[selectedKeyIdx], p.Type, selectedKeyIdx+1)
	}
}

func runSingleKeyTest(providerName, baseURL, apiKey, providerType string, keyNum int) {
	masked := maskAPIKey(apiKey)
	var ok bool
	var count int
	var testErr error

	spinnerErr := withSpinner(fmt.Sprintf("Testing key #%d (%s) untuk %s...", keyNum, masked, providerName), func() error {
		ok, count, testErr = testAPIKey(baseURL, apiKey, providerType)
		return testErr
	})

	if spinnerErr != nil {
		printError(fmt.Sprintf("Key #%d (%s) GAGAL: %v", keyNum, masked, spinnerErr))
		return
	}

	if ok {
		fmt.Println(BadgeSuccess.Render(fmt.Sprintf(" VALID (Key #%d) ", keyNum)))
		fmt.Printf("  %s %s\n",
			KeyStyle.Render("Model tersedia:"),
			lipgloss.NewStyle().Foreground(Accent).Render(fmt.Sprintf("%d model", count)))
	}
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "••••••••"
	}
	return key[:4] + "••••" + key[len(key)-4:]
}

// updateModelReasoningInCfg updates the reasoning flag on all model routes
// that match the given provider+modelID pair. Returns true if anything changed.
func updateModelReasoningInCfg(cfg *config.Config, providerName, modelID string, isReasoning bool) bool {
	changed := false
	for i, m := range cfg.Models {
		if m.Provider == providerName && m.Model == modelID && m.Reasoning != isReasoning {
			cfg.Models[i].Reasoning = isReasoning
			detectedReasoningModels[modelID] = isReasoning
			changed = true
		}
	}
	return changed
}

func testModelMenu(cfg *config.Config, cfgPath string) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider. Tambah dulu di Manajemen AI.")
		return
	}

	printSectionTitle("🤖", "Test Model")

	// Build options with "Test Semua" at the top
	options := []huh.Option[string]{
		huh.NewOption("🔍  Test semua model dari semua provider", "all_providers"),
	}
	options = append(options, providerSelectOptions(cfg)...)
	options = append(options, huh.NewOption("❌  Batal / kembali", "__cancel__"))

	var pIdxStr string
	huh.NewSelect[string]().
		Title("Pilih provider").
		Options(options...).
		Value(&pIdxStr).
		Run()
	if pIdxStr == "" || pIdxStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	if pIdxStr == "all_providers" {
		testAllModelsAllProviders(cfg, cfgPath)
		return
	}

	pIdx, err := strconv.Atoi(pIdxStr)
	if err != nil || pIdx < 0 || pIdx >= len(cfg.Providers) {
		return
	}

	p := cfg.Providers[pIdx]

	// Special handling for OpenCode — Keyless auto routing provider
	if p.Type == "opencode" {
		printSectionTitle("🤖", "Test OpenCode — Model Virtual oc/auto")
		fmt.Println()
		printInfo("OpenCode adalah provider Keyless otomatis.")
		printInfo("Menjalankan test menggunakan model virtual oc/auto (fallback otomatis ke model gratis tersedia)...")
		fmt.Println()

		var response string
		var latency int64
		var testErr error

		spinnerErr := withSpinner("Testing oc/auto via AI Gateway...", func() error {
			// Call the gateway directly using user's api key
			response, latency, _, _, testErr = testGatewayModel(cfg, "oc/auto")
			return testErr
		})

		if spinnerErr != nil {
			printError(fmt.Sprintf("oc/auto GAGAL: %v", spinnerErr))
		} else {
			fmt.Printf("  ✅  Model '%s' %s\n", "oc/auto", BadgeSuccess.Render(" BERFUNGSI "))
			printKeyValue("     📥 Response:", response)
			printKeyValue("     ⏱  Latency:", fmt.Sprintf("%dms", latency))
		}
		return
	}

	if len(p.APIKeys) == 0 {
		printError("Provider ini tidak punya API key")
		return
	}
	if len(p.Models) == 0 {
		printError("Provider ini tidak punya model")
		return
	}

	// Select model
	modelOptions := make([]huh.Option[string], len(p.Models))
	for i, m := range p.Models {
		modelOptions[i] = huh.NewOption(m, m)
	}
	modelOptions = append(modelOptions, huh.NewOption("❌  Batal / kembali", "__cancel__"))

	var selectedModels []string
	// Pre-select all models by default so the user can just hit Enter to test all
	for _, m := range p.Models {
		selectedModels = append(selectedModels, m)
	}

	errSelect := huh.NewMultiSelect[string]().
		Title("Pilih model-model untuk ditest (space untuk select, enter untuk confirm)").
		Options(modelOptions...).
		Value(&selectedModels).
		Run()
	if isAbort(errSelect) || len(selectedModels) == 0 {
		printInfo("Dibatalkan")
		return
	}

	// Filter out cancel sentinel if selected
	var finalModels []string
	for _, m := range selectedModels {
		if m != "__cancel__" {
			finalModels = append(finalModels, m)
		}
	}

	if len(finalModels) == 0 {
		printInfo("Dibatalkan")
		return
	}

	fmt.Println()
	printInfo(fmt.Sprintf("Memulai pengetesan %s...", plural(len(finalModels), "model", "models")))

	for _, modelID := range finalModels {
		fmt.Println()
		// Run test with spinner
		var response string
		var latency int64
		var isReasoning bool
		var isVision bool
		var testErr error

		spinnerErr := withSpinner(fmt.Sprintf("Testing model '%s' di %s...", modelID, p.Name), func() error {
			response, latency, isReasoning, isVision, testErr = testModel(p.BaseURL, p.APIKeys[0], modelID, p.Type)
			return testErr
		})

		if spinnerErr != nil {
			printError(fmt.Sprintf("Model '%s' GAGAL: %v", modelID, spinnerErr))
		} else {
			if isReasoning && isVision {
				fmt.Printf("  🧠👁️  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI & REASONING & VISION "))
			} else if isReasoning {
				fmt.Printf("  🧠  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI & REASONING "))
			} else if isVision {
				fmt.Printf("  👁️  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI & VISION "))
			} else {
				fmt.Printf("  ✅  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI "))
			}
			printKeyValue("     📥 Response:", response)
			printKeyValue("     ⏱  Latency:", fmt.Sprintf("%dms", latency))
			updateModelReasoningInCfg(cfg, p.Name, modelID, isReasoning)
		}
	}
	// Persist any reasoning flag changes back to disk
	safeSave(cfg, cfgPath)
}

func syncModelsMenu(cfg *config.Config, cfgPath string) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	printSectionTitle("🔄", "Sync Models dari Provider API")

	// Options: sync all or specific provider
	options := make([]huh.Option[string], len(cfg.Providers)+1)
	options[0] = huh.NewOption("🔄  Sync semua provider", "all")
	for i, p := range cfg.Providers {
		options[i+1] = huh.NewOption(
			fmt.Sprintf("%s [%d model saat ini]", p.Name, len(p.Models)),
			fmt.Sprintf("%d", i),
		)
	}

	options = append(options, huh.NewOption("❌  Batal / kembali", "__cancel__"))

	var choice string
	huh.NewSelect[string]().
		Title("Pilih provider untuk sync").
		Options(options...).
		Value(&choice).
		Run()
	if choice == "" || choice == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	if choice == "all" {
		for i := range cfg.Providers {
			syncProviderModels(cfg, i)
		}
	} else {
		idx, err := strconv.Atoi(choice)
		if err != nil || idx < 0 || idx >= len(cfg.Providers) {
			return
		}
		syncProviderModels(cfg, idx)
	}

	safeSave(cfg, cfgPath)
}

func syncProviderModels(cfg *config.Config, providerIdx int) {
	p := cfg.Providers[providerIdx]
	if len(p.APIKeys) == 0 {
		printWarning(fmt.Sprintf("%s: tidak punya API key, skip", p.Name))
		return
	}

	// Fetch models with spinner
	var fetchedModels []string
	var fetchErr error

	spinnerErr := withSpinner(fmt.Sprintf("Sync model %s...", p.Name), func() error {
		fetchedModels, fetchErr = fetchModels(p.BaseURL, p.APIKeys[0], p.Type)
		return fetchErr
	})

	if spinnerErr != nil {
		printWarning(fmt.Sprintf("%s: Gagal fetch: %v", p.Name, spinnerErr))
		return
	}

	existing := make(map[string]bool)
	for _, m := range p.Models {
		existing[m] = true
	}

	var newModels []string
	for _, m := range fetchedModels {
		if !existing[m] {
			newModels = append(newModels, m)
		}
	}

	if len(newModels) == 0 {
		printSuccess(fmt.Sprintf("%s: tidak ada model baru (%d model sudah ada)", p.Name, len(p.Models)))
		return
	}

	fmt.Printf("\n  📦 %d model baru ditemukan:\n\n", len(newModels))

	// Multi-select which models to add
	selected := make([]string, 0)
	opts := make([]huh.Option[string], len(newModels))
	for i, m := range newModels {
		opts[i] = huh.NewOption(m, m)
	}

	huh.NewMultiSelect[string]().
		Title(fmt.Sprintf("Model baru dari %s (space untuk select)", p.Name)).
		Options(opts...).
		Value(&selected).
		Run()

	if len(selected) == 0 {
		selected = newModels
		printInfo("Semua model baru dipilih")
	}

	selected = testModelsBeforeAdd(p.BaseURL, p.APIKeys[0], p.Type, selected)
	if len(selected) == 0 {
		printInfo("Tidak ada model ditambahkan")
		return
	}

	p.Models = append(p.Models, selected...)
	cfg.Providers[providerIdx] = p
	createModelRoutes(cfg, p.Name, selected)
	printSuccess(fmt.Sprintf("%s: %s ditambahkan", p.Name, plural(len(selected), "model", "models")))
}

func testAllModelsAllProviders(cfg *config.Config, cfgPath string) {
	fmt.Println()
	printInfo("Memulai pengetesan semua model dari semua provider...")

	totalTested := 0
	totalSuccess := 0

	for _, p := range cfg.Providers {
		if len(p.APIKeys) == 0 || len(p.Models) == 0 {
			continue
		}

		fmt.Println()
		fmt.Println(SubtitleStyle.Render(fmt.Sprintf("📡  Provider: %s", p.Name)))

		for _, modelID := range p.Models {
			totalTested++
			var response string
			var latency int64
			var isReasoning bool
			var isVision bool
			var testErr error

			spinnerErr := withSpinner(fmt.Sprintf("Testing model '%s'...", modelID), func() error {
				response, latency, isReasoning, isVision, testErr = testModel(p.BaseURL, p.APIKeys[0], modelID, p.Type)
				return testErr
			})

			if spinnerErr != nil {
				printError(fmt.Sprintf("Model '%s' GAGAL: %v", modelID, spinnerErr))
			} else {
				totalSuccess++
				if isReasoning && isVision {
					fmt.Printf("  🧠👁️  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI & REASONING & VISION "))
				} else if isReasoning {
					fmt.Printf("  🧠  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI & REASONING "))
				} else if isVision {
					fmt.Printf("  👁️  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI & VISION "))
				} else {
					fmt.Printf("  ✅  Model '%s' %s\n", modelID, BadgeSuccess.Render(" BERFUNGSI "))
				}
				printKeyValue("     📥 Response:", response)
				printKeyValue("     ⏱  Latency:", fmt.Sprintf("%dms", latency))
				updateModelReasoningInCfg(cfg, p.Name, modelID, isReasoning)
			}
		}
	}

	fmt.Println()
	printInfo(fmt.Sprintf("Selesai! Berhasil mengetes %d model (%d sukses, %d gagal)",
		totalTested, totalSuccess, totalTested-totalSuccess))
	// Persist reasoning flag updates
	safeSave(cfg, cfgPath)
}

// testGatewayModel calls the local AI Gateway chat/completions endpoint to test a virtual model like oc/auto.
func testGatewayModel(cfg *config.Config, modelID string) (response string, latencyMs int64, isReasoning bool, isVision bool, err error) {
	host := cfg.Server.Host
	if host == "0.0.0.0" {
		host = "localhost"
	}
	url := fmt.Sprintf("http://%s:%d/v1/chat/completions", host, cfg.Server.Port)

	// Find a valid gateway api key
	apiKey := ""
	for _, k := range cfg.APIKeys {
		if k.Key != "" {
			apiKey = k.Key
			break
		}
	}
	if apiKey == "" {
		return "", 0, false, false, fmt.Errorf("tidak ada API key gateway yang dikonfigurasi")
	}

	body := map[string]interface{}{
		"model": modelID,
		"messages": []map[string]string{
			{"role": "user", "content": "Say 'OK' in one word."},
		},
		"max_tokens": 50,
	}
	body_bytes, _ := json.Marshal(body)

	client := &http.Client{Timeout: 60 * time.Second}
	req, err2 := http.NewRequest("POST", url, bytes.NewReader(body_bytes))
	if err2 != nil {
		return "", 0, false, false, err2
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err3 := client.Do(req)
	latencyMs = time.Since(start).Milliseconds()
	if err3 != nil {
		return "", latencyMs, false, false, err3
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", latencyMs, false, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err4 := json.Unmarshal(respBody, &result); err4 != nil {
		return "", latencyMs, false, false, err4
	}
	if len(result.Choices) == 0 {
		return "", latencyMs, false, false, fmt.Errorf("tidak ada response dari gateway")
	}
	content := strings.TrimSpace(result.Choices[0].Message.Content)
	if len(content) > 200 {
		content = content[:200] + "..."
	}
	reasoningUsed := result.Choices[0].Message.ReasoningContent != ""
	return content, latencyMs, reasoningUsed, false, nil
}

