package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/aigateway/config"
	"github.com/google/uuid"
)

// ==================== Provider Templates ====================

type providerTemplate struct {
	Name           string
	Type           string
	BaseURL        string
	Desc           string
	HelpURL        string
	FallbackModels []string
}

var templates = []providerTemplate{
	{
		Name:    "openai",
		Type:    "openai",
		BaseURL: "https://api.openai.com/v1",
		Desc:    "OpenAI — GPT-4o, GPT-4.1, dll",
		HelpURL: "https://platform.openai.com/api-keys",
	},
	{
		Name:    "anthropic",
		Type:    "anthropic",
		BaseURL: "https://api.anthropic.com",
		Desc:    "Anthropic — Claude Sonnet, Haiku",
		HelpURL: "https://console.anthropic.com/settings/keys",
		FallbackModels: []string{
			"claude-sonnet-4-20250514",
			"claude-haiku-4-20250514",
			"claude-opus-4-20250514",
		},
	},
	{
		Name:    "groq",
		Type:    "groq",
		BaseURL: "https://api.groq.com/openai/v1",
		Desc:    "Groq — Llama, Mixtral (super cepat)",
		HelpURL: "https://console.groq.com/keys",
	},
	{
		Name:    "mistral",
		Type:    "mistral",
		BaseURL: "https://api.mistral.ai/v1",
		Desc:    "Mistral AI — Mistral Large, Small",
		HelpURL: "https://console.mistral.ai/api-keys/",
	},
	{
		Name:    "deepseek",
		Type:    "openai",
		BaseURL: "https://api.deepseek.com/v1",
		Desc:    "DeepSeek — Chat, Reasoner",
		HelpURL: "https://platform.deepseek.com/api_keys",
	},
	{
		Name:    "openrouter",
		Type:    "openai",
		BaseURL: "https://openrouter.ai/api/v1",
		Desc:    "OpenRouter — akses 300+ model",
		HelpURL: "https://openrouter.ai/keys",
	},
	{
		Name:    "gemini",
		Type:    "openai",
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		Desc:    "Google Gemini — Gemini 1.5, 2.0, 2.5 Pro/Flash",
		HelpURL: "https://aistudio.google.com/app/apikey",
	},
	{
		Name:    "cohere",
		Type:    "cohere",
		BaseURL: "https://api.cohere.com/compatibility/v1",
		Desc:    "Cohere — Command R+, Command R, Command Light",
		HelpURL: "https://dashboard.cohere.com/api-keys",
	},
	{
		Name:    "opencode",
		Type:    "opencode",
		BaseURL: "https://opencode.ai/zen/v1",
		Desc:    "OpenCode Zen — DeepSeek V4, Claude 3.5, Gemini 3.5, dll",
		HelpURL: "https://opencode.ai",
	},
	{
		Name:    "cloudflare",
		Type:    "cloudflare",
		BaseURL: "",
		Desc:    "Cloudflare Workers AI — Llama, Mistral, Gemma",
		HelpURL: "https://developers.cloudflare.com/workers-ai/get-started/",
		FallbackModels: []string{
			"@cf/meta/llama-3-8b-instruct",
			"@cf/mistral/mistral-7b-instruct-v0.1",
			"@cf/meta/llama-3-70b-instruct",
			"@cf/qwen/qwen1.5-14b-chat",
			"@cf/baai/bge-large-en-v1.5",
		},
	},
	{
		Name:    "cerebras",
		Type:    "cerebras",
		BaseURL: "https://api.cerebras.ai/v1",
		Desc:    "Cerebras Inference — Super fast inference (GPT OSS, Gemma 4, GLM 4.7)",
		HelpURL: "https://cloud.cerebras.ai",
	},
}

// ==================== AI Management Menu ====================

func menuAI(cfg *config.Config, cfgPath string) {
	for {
		clearScreen()
		printSectionTitle("🤖", "Manajemen AI")

		var choice string
		err := huh.NewSelect[string]().
			Title("").
			Options(
				huh.NewOption("⚡  Quick Setup (Tambah Provider)", "quick"),
				huh.NewOption("📡  Kelola Providers", "providers"),
				huh.NewOption("🔑  Kelola Upstream API Keys", "keys"),
				huh.NewOption("🤖  Kelola Models & Combos", "models"),
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
		case "quick":
			quickSetup(cfg, cfgPath)
		case "providers":
			menuProviders(cfg, cfgPath)
		case "keys":
			menuUpstreamKeys(cfg, cfgPath)
		case "models":
			menuModelsCombos(cfg, cfgPath)
		}
	}
}

// ==================== Quick Setup ====================

func quickSetup(cfg *config.Config, cfgPath string) {
	clearScreen()
	printSectionTitle("⚡", "Quick Setup — Tambah Provider")

	available := filterAvailableTemplates(cfg)
	if len(available) == 0 {
		printSuccess("Semua provider sudah ditambahkan!")
		printInfo("Untuk tambah provider custom, pilih menu Provider → Tambah Custom")
		pause()
		return
	}

	// Build options — nilai index string + "__cancel__" di akhir
	options := make([]huh.Option[string], len(available)+2)
	for i, t := range available {
		options[i] = huh.NewOption(fmt.Sprintf("%-14s %s", t.Name, t.Desc), fmt.Sprintf("%d", i))
	}
	options[len(available)] = huh.NewOption("🔧  Provider lain (custom)", "custom")
	options[len(available)+1] = huh.NewOption("❌  Batal / kembali", "__cancel__")

	var choice string
	huh.NewSelect[string]().
		Title("Pilih provider").
		Options(options...).
		Value(&choice).
		Run()
	if choice == "" || choice == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	if choice == "custom" {
		addCustomProvider(cfg)
		safeSave(cfg, cfgPath)
		return
	}

	idx, err := strconv.Atoi(choice)
	if err != nil || idx < 0 || idx >= len(available) {
		return
	}

	tmpl := available[idx]
	setupFromTemplate(cfg, tmpl)
	safeSave(cfg, cfgPath)
}

func filterAvailableTemplates(cfg *config.Config) []providerTemplate {
	existing := make(map[string]bool)
	for _, p := range cfg.Providers {
		existing[p.Name] = true
	}
	var result []providerTemplate
	for _, t := range templates {
		if !existing[t.Name] {
			result = append(result, t)
		}
	}
	return result
}

func setupFromTemplate(cfg *config.Config, tmpl providerTemplate) {
	clearScreen()
	printSectionTitle(tmpl.Name, fmt.Sprintf("Setup %s", strings.ToUpper(tmpl.Name)))

	printInfo(fmt.Sprintf(T("Dapatkan API key di: %s", "Get API key at: %s"), tmpl.HelpURL))
	fmt.Println()

	var accountID string
	if tmpl.Type == "cloudflare" {
		errAcc := huh.NewInput().
			Title(T("Account ID Cloudflare", "Cloudflare Account ID")).
			Placeholder("e.g. 1a2b3c4d5e...").
			Value(&accountID).
			Run()
		if isAbort(errAcc) {
			printInfo(T("Dibatalkan", "Cancelled"))
			return
		}
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			printError(T("Account ID tidak boleh kosong", "Account ID cannot be empty"))
			pause()
			return
		}
	}

	// Get API key
	var apiKey string
	errKey := huh.NewInput().
		Title(fmt.Sprintf(T("API Key %s", "%s API Key"), tmpl.Name)).
		Placeholder("sk-...").
		EchoMode(huh.EchoModePassword).
		Value(&apiKey).
		Run()

	if isAbort(errKey) {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}
	if apiKey == "" {
		printError(T("API key tidak boleh kosong", "API key cannot be empty"))
		pause()
		return
	}

	// Fetch models with spinner
	fmt.Println()
	var fetchedModels []string
	var fetchErr error

	effectiveBaseURL := tmpl.BaseURL
	if tmpl.Type == "cloudflare" {
		effectiveBaseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", accountID)
	}

	err := withSpinner(T("Mengambil model dari provider...", "Fetching models from provider..."), func() error {
		fetchedModels, fetchErr = fetchModels(effectiveBaseURL, apiKey, tmpl.Type)
		return fetchErr
	})

	if err != nil {
		printWarning(fmt.Sprintf(T("Gagal fetch model: %v", "Failed to fetch models: %v"), err))
		if len(tmpl.FallbackModels) > 0 {
			printInfo(T("Menggunakan model default:", "Using default models:"))
			fetchedModels = tmpl.FallbackModels
		} else {
			// Manual input
			var manualModels string
			errM := huh.NewInput().
				Title("Tambah model manual").
				Placeholder("model-1, model-2, model-3").
				Value(&manualModels).
				Run()
			if isAbort(errM) {
				printInfo("Dibatalkan")
				return
			}
			fetchedModels = splitTrim(manualModels)
			if len(fetchedModels) == 0 {
				printError("Minimal 1 model")
				pause()
				return
			}
		}
	}

	// Select models with multi-select
	fmt.Printf(T("\n  🤖 %d model tersedia:\n\n", "\n  🤖 %d models available:\n\n"), len(fetchedModels))

	selectedModels := make([]string, 0)
	modelOptions := make([]huh.Option[string], len(fetchedModels))
	for i, m := range fetchedModels {
		modelOptions[i] = huh.NewOption(m, m)
	}

	huh.NewMultiSelect[string]().
		Title(T("Pilih model (space untuk select, enter untuk confirm)", "Select models (space to select, enter to confirm)")).
		Options(modelOptions...).
		Value(&selectedModels).
		Run()

	if len(selectedModels) == 0 {
		selectedModels = fetchedModels
		printInfo(T("Semua model dipilih", "All models selected"))
	}

	selectedModels = testModelsBeforeAdd(effectiveBaseURL, apiKey, tmpl.Type, selectedModels)
	if len(selectedModels) == 0 {
		printInfo(T("Tidak ada model ditambahkan", "No models added"))
		pause()
		return
	}

	// Ask for additional manual models
	var addMore bool
	huh.NewConfirm().
		Title(T("Tambah model manual?", "Add model manually?")).
		Value(&addMore).
		Run()

	if addMore {
		var manualStr string
		huh.NewInput().
			Title(T("Nama model", "Model name")).
			Placeholder("model-1, model-2").
			Value(&manualStr).
			Run()
		manual := splitTrim(manualStr)
		selectedModels = append(selectedModels, manual...)
	}

	if len(selectedModels) == 0 {
		printError(T("Tidak ada model dipilih", "No models selected"))
		pause()
		return
	}

	// Create provider
	p := config.ProviderConfig{
		Name:      tmpl.Name,
		Type:      tmpl.Type,
		BaseURL:   tmpl.BaseURL,
		AccountID: accountID,
		APIKeys:   []string{apiKey},
		Models:    selectedModels,
	}

	if err := cfg.AddProvider(p); err != nil {
		printError(fmt.Sprintf("%v", err))
		pause()
		return
	}

	created := createModelRoutes(cfg, tmpl.Name, selectedModels)

	printSuccess(fmt.Sprintf("Provider '%s' ditambahkan!", tmpl.Name))
	fmt.Printf("     📡 %s  │  🤖 %s  │  🛤  %s dibuat\n",
		plural(1, "API key", "API keys"),
		plural(len(selectedModels), "model", "models"),
		plural(created, "route", "routes"))
	fmt.Println()

	// Ask to create user API key
	var createKey bool
	huh.NewConfirm().
		Title("Buat API key user untuk akses model ini?").
		Value(&createKey).
		Run()

	if createKey {
		key := "sk-gw-" + uuid.New().String()
		k := config.APIKeyConfig{
			Key:           key,
			Name:          fmt.Sprintf("User - %s", tmpl.Name),
			AllowedModels: selectedModels,
			RateLimit:     0,
		}
		if err := cfg.AddAPIKey(k); err == nil {
			printSuccess(fmt.Sprintf("API Key dibuat: %s", key))
			printInfo("Gunakan key ini di client SDK atau playground")
		}
	}

	pause()
}

func addCustomProvider(cfg *config.Config) {
	clearScreen()
	printSectionTitle("🔧", T("Tambah Custom Provider", "Add Custom Provider"))

	var name, baseURL, apiKey string
	var providerType string

	errN := huh.NewInput().
		Title(T("Nama provider", "Provider name")).
		Placeholder("my-llm").
		Value(&name).
		Run()
	if isAbort(errN) {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}
	if name == "" {
		printError(T("Nama tidak boleh kosong", "Name cannot be empty"))
		return
	}

	errT := huh.NewSelect[string]().
		Title(T("Tipe provider (Esc untuk batal)", "Provider type (Esc to cancel)")).
		Options(
			huh.NewOption("openai     — OpenAI-compatible API", "openai"),
			huh.NewOption("cloudflare — Cloudflare Workers AI", "cloudflare"),
			huh.NewOption("cohere     — Cohere API", "cohere"),
			huh.NewOption("opencode   — OpenCode Free API", "opencode"),
			huh.NewOption("mimo       — Xiaomi MiMo API", "mimo"),
			huh.NewOption("anthropic  — Anthropic Messages API", "anthropic"),
			huh.NewOption("groq       — Groq API", "groq"),
			huh.NewOption("mistral    — Mistral API", "mistral"),
		).
		Value(&providerType).
		Run()
	if isAbort(errT) || providerType == "" {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}

	var accountID string
	if providerType == "cloudflare" {
		errAcc := huh.NewInput().
			Title(T("Account ID Cloudflare", "Cloudflare Account ID")).
			Placeholder("e.g. 1a2b3c4d5e...").
			Value(&accountID).
			Run()
		if isAbort(errAcc) {
			printInfo(T("Dibatalkan", "Cancelled"))
			return
		}
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			printError(T("Account ID tidak boleh kosong", "Account ID cannot be empty"))
			return
		}
	}

	errU := huh.NewInput().
		Title("Base URL").
		Placeholder(T("https://api.example.com/v1 (kosongkan untuk default jika Cloudflare)", "https://api.example.com/v1 (leave empty for default if Cloudflare)")).
		Value(&baseURL).
		Run()
	if isAbort(errU) {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}
	if baseURL == "" && providerType != "cloudflare" {
		printError(T("Base URL tidak boleh kosong", "Base URL cannot be empty"))
		return
	}

	errK := huh.NewInput().
		Title("API Key").
		Placeholder("sk-...").
		EchoMode(huh.EchoModePassword).
		Value(&apiKey).
		Run()
	if isAbort(errK) {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}
	if apiKey == "" {
		printError(T("API key tidak boleh kosong", "API key cannot be empty"))
		return
	}

	// Fetch models with spinner
	fmt.Println()
	var models []string
	var fetchErr error

	// If cloudflare and baseURL is empty, construct it temporarily to fetch models
	effectiveBaseURL := baseURL
	if providerType == "cloudflare" && effectiveBaseURL == "" {
		effectiveBaseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", accountID)
	}

	err := withSpinner(T("Mengambil model dari provider...", "Fetching models from provider..."), func() error {
		models, fetchErr = fetchModels(effectiveBaseURL, apiKey, providerType)
		return fetchErr
	})

	if err != nil {
		printWarning(fmt.Sprintf(T("Gagal fetch: %v", "Failed to fetch: %v"), err))
		
		// If cloudflare, suggest fallback models
		if providerType == "cloudflare" {
			models = []string{
				"@cf/meta/llama-3-8b-instruct",
				"@cf/mistral/mistral-7b-instruct-v0.1",
				"@cf/meta/llama-3-70b-instruct",
				"@cf/qwen/qwen1.5-14b-chat",
				"@cf/baai/bge-large-en-v1.5",
			}
			printInfo(T("Menggunakan model default Cloudflare:", "Using default Cloudflare models:"))
		} else {
			printInfo(T("Tambah model manual:", "Add model manually:"))

			var modelsStr string
			huh.NewInput().
				Title("Models").
				Placeholder("model-1, model-2, model-3").
				Value(&modelsStr).
				Run()
			models = splitTrim(modelsStr)
			if len(models) == 0 {
				printError(T("Minimal 1 model", "At least 1 model is required"))
				return
			}
		}
	}

	// Select models with multi-select
	if len(models) > 0 {
		fmt.Printf(T("\n  🤖 %d model ditemukan:\n\n", "\n  🤖 %d models found:\n\n"), len(models))

		selectedModels := make([]string, 0)
		modelOptions := make([]huh.Option[string], len(models))
		for i, m := range models {
			modelOptions[i] = huh.NewOption(m, m)
		}

		huh.NewMultiSelect[string]().
			Title(T("Pilih model (space untuk select, enter untuk confirm)", "Select models (space to select, enter to confirm)")).
			Options(modelOptions...).
			Value(&selectedModels).
			Run()

		if len(selectedModels) == 0 {
			selectedModels = models
		}

		selectedModels = testModelsBeforeAdd(effectiveBaseURL, apiKey, providerType, selectedModels)
		if len(selectedModels) == 0 {
			printInfo(T("Tidak ada model ditambahkan", "No models added"))
			return
		}
		models = selectedModels
	}

	p := config.ProviderConfig{
		Name:      name,
		Type:      providerType,
		BaseURL:   baseURL,
		AccountID: accountID,
		APIKeys:   []string{apiKey},
		Models:    models,
	}

	if err := cfg.AddProvider(p); err != nil {
		printError(fmt.Sprintf("%v", err))
		return
	}

	createModelRoutes(cfg, name, models)
	printSuccess(fmt.Sprintf(T("Provider '%s' ditambahkan dengan %d model", "Provider '%s' added with %d models"), name, len(models)))
}

// ==================== Providers CRUD ====================

func menuProviders(cfg *config.Config, cfgPath string) {
	for {
		clearScreen()
		printSectionTitle("📡", T("Kelola Provider", "Manage Providers"))

		var choice string
		err := huh.NewSelect[string]().
			Title("").
			Options(
				huh.NewOption(T("📋  Lihat semua provider", "📋  View all providers"), "list"),
				huh.NewOption(T("➕  Tambah provider (template)", "➕  Add provider (template)"), "add_tmpl"),
				huh.NewOption(T("➕  Tambah provider (custom)", "➕  Add provider (custom)"), "add_custom"),
				huh.NewOption(T("✏️   Edit provider", "✏️   Edit provider"), "edit"),
				huh.NewOption(T("🔑  Tambah API key ke provider", "🔑  Add API key to provider"), "add_key"),
				huh.NewOption(T("🗑️   Hapus API key dari provider", "🗑️   Delete API key from provider"), "delete_key"),
				huh.NewOption(T("➕  Tambah model ke provider", "➕  Add model to provider"), "add_model"),
				huh.NewOption(T("🗑️   Hapus provider", "🗑️   Delete provider"), "delete"),
				huh.NewOption(T("←  Kembali", "←  Back"), "back"),
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
		case "list":
			clearScreen()
			listProviders(cfg)
			pause()
		case "add_tmpl":
			quickSetup(cfg, cfgPath)
		case "add_custom":
			addCustomProvider(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "edit":
			editProvider(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "add_key":
			addUpstreamKey(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "delete_key":
			deleteUpstreamKey(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "add_model":
			addModelToProvider(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "delete":
			deleteProvider(cfg)
			safeSave(cfg, cfgPath)
			pause()
		}
	}
}

func listProviders(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider. Jalankan Quick Setup untuk tambah provider.")
		return
	}

	// Hitung routes aktif per provider
	routeCount := make(map[string]int)
	for _, m := range cfg.Models {
		if m.Strategy == "" && m.Provider != "" {
			routeCount[m.Provider]++
		}
	}

	printSectionTitle("📡", "Daftar Provider")
	for i, p := range cfg.Providers {
		keyMasked := "-"
		if len(p.APIKeys) > 0 {
			k := p.APIKeys[0]
			if len(k) > 8 {
				k = k[:8]
			}
			keyMasked = k + "..."
		}

		nRoutes := routeCount[p.Name]
		nRaw := len(p.Models)
		var modelInfo string
		if p.Type == "opencode" {
			// OpenCode is keyless & auto — always has virtual oc/auto + dynamic free models
			modelInfo = MutedStyle.Render("oc/auto (virtual, model gratis diambil otomatis)")
		} else {
			modelInfo = plural(nRoutes, "model", "models")
			if nRaw != nRoutes {
				modelInfo = fmt.Sprintf("%d/%d models (aktif/tersimpan)", nRoutes, nRaw)
			}
		}

		badge := providerTypeBadge(p.Type)
		fmt.Printf("  %s. %s %-14s %s\n", KeyStyle.Render(fmt.Sprintf("%d", i+1)),
			badge, KeyStyle.Render(p.Name),
			MutedStyle.Render(p.BaseURL))
		fmt.Printf("     🔑 %s (%s)  │  🤖 %s\n",
			plural(len(p.APIKeys), "key", "keys"), keyMasked, modelInfo)
	}
}


func providerSelectOptions(cfg *config.Config) []huh.Option[string] {
	// Hitung jumlah direct routes aktif per provider dari cfg.Models
	routeCount := make(map[string]int)
	for _, m := range cfg.Models {
		if m.Strategy == "" && m.Provider != "" {
			routeCount[m.Provider]++
		}
	}

	options := make([]huh.Option[string], len(cfg.Providers))
	for i, p := range cfg.Providers {
		nRoutes := routeCount[p.Name]  // routes aktif (sinkron dengan Daftar Model & Combo)
		nRaw := len(p.Models)          // model tersimpan di provider list
		nKeys := len(p.APIKeys)

		var modelInfo string
		if nRaw == nRoutes {
			// Sinkron: cukup tampilkan satu angka
			modelInfo = plural(nRoutes, "model", "models")
		} else {
			// Ada gap: tampilkan keduanya agar jelas
			modelInfo = fmt.Sprintf("%d/%d models", nRoutes, nRaw)
		}

		options[i] = huh.NewOption(
			fmt.Sprintf("%s [%s] — %s, %s",
				p.Name, p.Type,
				modelInfo,
				plural(nKeys, "key", "keys")),
			fmt.Sprintf("%d", i),
		)
	}
	return options
}


// providerSelectOptionsWithCancel returns provider options plus a cancel sentinel at the bottom.
func providerSelectOptionsWithCancel(cfg *config.Config) []huh.Option[string] {
	opts := providerSelectOptions(cfg)
	return append(opts, huh.NewOption("❌  Batal / kembali", "__cancel__"))
}

func editProvider(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	listProviders(cfg)
	fmt.Println()

	var idxStr string
	huh.NewSelect[string]().
		Title("Pilih provider yang akan diedit").
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

	pv := cfg.Providers[idx]
	if pv.Type == "opencode" {
		printWarning("OpenCode adalah provider gratis otomatis (Keyless) dan tidak memerlukan konfigurasi/setup!")
		pause()
		return
	}

	var newURL, newKey string
	var newAccountID string

	if pv.Type == "cloudflare" {
		huh.NewInput().
			Title(fmt.Sprintf(T("Account ID Cloudflare [%s]", "Cloudflare Account ID [%s]"), pv.AccountID)).
			Placeholder(pv.AccountID).
			Value(&newAccountID).
			Run()
		if newAccountID != "" {
			pv.AccountID = strings.TrimSpace(newAccountID)
			if strings.Contains(pv.BaseURL, "api.cloudflare.com") || pv.BaseURL == "" {
				pv.BaseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", pv.AccountID)
			}
		}
	}

	huh.NewInput().
		Title(fmt.Sprintf("Base URL [%s]", pv.BaseURL)).
		Placeholder(pv.BaseURL).
		Value(&newURL).
		Run()

	if newURL != "" {
		pv.BaseURL = newURL
	}

	huh.NewInput().
		Title(fmt.Sprintf("Tambah API key [%d key, kosongkan=tidak ubah]", len(pv.APIKeys))).
		EchoMode(huh.EchoModePassword).
		Value(&newKey).
		Run()

	if newKey != "" {
		newKey = strings.TrimSpace(newKey)
		exists := false
		for _, k := range pv.APIKeys {
			if k == newKey {
				exists = true
				break
			}
		}
		if exists {
			printError("API Key ini sudah terdaftar di provider ini!")
			time.Sleep(1500 * time.Millisecond)
		} else {
			pv.APIKeys = append(pv.APIKeys, newKey)
		}
	}

	if err := cfg.UpdateProvider(pv); err != nil {
		printError(fmt.Sprintf("%v", err))
		return
	}
	printSuccess(fmt.Sprintf("Provider '%s' diupdate", pv.Name))
}

func addModelToProvider(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	listProviders(cfg)
	fmt.Println()

	var idxStr string
	huh.NewSelect[string]().
		Title("Pilih provider").
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
	if p.Type == "opencode" {
		printWarning("OpenCode adalah provider gratis otomatis (Keyless) dan tidak memerlukan konfigurasi/setup!")
		pause()
		return
	}

	var method string
	huh.NewSelect[string]().
		Title("Cara tambah model").
		Options(
			huh.NewOption("🔄 Fetch dari API provider", "fetch"),
			huh.NewOption("✏️  Input manual", "manual"),
			huh.NewOption("❌  Batal / kembali", "__cancel__"),
		).
		Value(&method).
		Run()
	if method == "" || method == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	switch method {
	case "fetch":
		if len(p.APIKeys) == 0 {
			printError("Provider tidak punya API key")
			return
		}

		var fetchedModels []string
		var fetchErr error
		err := withSpinner("Fetching model...", func() error {
			fetchedModels, fetchErr = fetchModels(p.BaseURL, p.APIKeys[0], p.Type)
			return fetchErr
		})
		if err != nil {
			printError(fmt.Sprintf("Gagal fetch: %v", err))
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
			printSuccess("Semua model sudah ditambahkan")
			return
		}

		fmt.Printf("\n  📦 %d model baru ditemukan\n\n", len(newModels))

		// 3 options: take all, select, or test first
		var action string
		errA := huh.NewSelect[string]().
			Title("Apa yang ingin kamu lakukan? (Esc untuk batal)").
			Options(
				huh.NewOption(fmt.Sprintf("✅  Ambil semua %d model", len(newModels)), "all"),
				huh.NewOption("📋  Pilih model sendiri (checkbox)", "select"),
				huh.NewOption("🧪  Test model dulu baru tambahkan", "test"),
			).
			Value(&action).
			Run()
		if isAbort(errA) || action == "" {
			printInfo("Dibatalkan")
			return
		}

		var selected []string

		switch action {
		case "all":
			selected = testModelsBeforeAdd(p.BaseURL, p.APIKeys[0], p.Type, newModels)
			if len(selected) == 0 {
				printInfo("Tidak ada model ditambahkan")
				return
			}

		case "select":
			opts := make([]huh.Option[string], len(newModels))
			for i, m := range newModels {
				opts[i] = huh.NewOption(m, m)
			}
			huh.NewMultiSelect[string]().
				Title("Pilih model (space untuk select, enter untuk confirm)").
				Options(opts...).
				Value(&selected).
				Run()
			if len(selected) == 0 {
				printInfo("Tidak ada model dipilih")
				return
			}
			selected = testModelsBeforeAdd(p.BaseURL, p.APIKeys[0], p.Type, selected)
			if len(selected) == 0 {
				printInfo("Tidak ada model ditambahkan")
				return
			}

		case "test":
			// Select which models to test
			var toTest []string
			opts := make([]huh.Option[string], len(newModels))
			for i, m := range newModels {
				opts[i] = huh.NewOption(m, m)
			}
			errM := huh.NewMultiSelect[string]().
				Title("Pilih model-model untuk ditest (space untuk select, enter untuk confirm)").
				Options(opts...).
				Value(&toTest).
				Run()

			if isAbort(errM) || len(toTest) == 0 {
				printInfo("Dibatalkan")
				return
			}

			// Run tests and save models using the robust testModelsBeforeAdd function
			selected = testModelsBeforeAdd(p.BaseURL, p.APIKeys[0], p.Type, toTest)
			if len(selected) == 0 {
				printInfo("Tidak ada model ditambahkan")
				return
			}

		default:
			return
		}

		p.Models = append(p.Models, selected...)
		cfg.Providers[idx] = p
		created := createModelRoutes(cfg, p.Name, selected)
		printSuccess(fmt.Sprintf("%s ditambahkan, %s dibuat",
			plural(len(selected), "model", "models"),
			plural(created, "route", "routes")))

	case "manual":
		var modelsStr string
		huh.NewInput().
			Title("Model name (pisahkan koma)").
			Placeholder("model-1, model-2").
			Value(&modelsStr).
			Run()

		models := splitTrim(modelsStr)
		if len(models) == 0 {
			printInfo("Tidak ada model ditambahkan")
			return
		}

		p.Models = append(p.Models, models...)
		cfg.Providers[idx] = p
		created := createModelRoutes(cfg, p.Name, models)
		printSuccess(fmt.Sprintf("%s ditambahkan, %s dibuat",
			plural(len(models), "model", "models"),
			plural(created, "route", "routes")))
	}
}

func deleteProvider(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	listProviders(cfg)
	fmt.Println()

	// Build options with explicit "Batal" entry
	opts := providerSelectOptions(cfg)
	opts = append(opts, huh.NewOption("❌  Batal / kembali", "__cancel__"))

	var idxStr string
	err := huh.NewSelect[string]().
		Title("Pilih provider yang akan dihapus (Esc untuk batal)").
		Options(opts...).
		Value(&idxStr).
		Run()

	if isAbort(err) || idxStr == "__cancel__" || idxStr == "" {
		printInfo("Dibatalkan")
		return
	}

	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(cfg.Providers) {
		return
	}

	name := cfg.Providers[idx].Name

	var confirm bool
	huh.NewConfirm().
		Title(fmt.Sprintf("Yakin hapus '%s'?", name)).
		Description("Semua model route dari provider ini juga akan dihapus.").
		Value(&confirm).
		Run()

	if !confirm {
		printInfo("Dibatalkan")
		return
	}

	routes, combos, err := cfg.DeleteProvider(name)
	if err != nil {
		printError(fmt.Sprintf("%v", err))
		return
	}
	printSuccess(fmt.Sprintf("Provider '%s' dihapus", name))
	if routes > 0 || combos > 0 {
		printInfo(fmt.Sprintf("  ↳ %d model route & %d combo juga dihapus (cascade)", routes, combos))
	}
}

// ==================== Upstream API Keys ====================

func menuUpstreamKeys(cfg *config.Config, cfgPath string) {
	for {
		clearScreen()
		printSectionTitle("🔑", "Kelola Upstream API Keys")

		var choice string
		err := huh.NewSelect[string]().
			Title("").
			Options(
				huh.NewOption("📋  Lihat keys per provider", "list"),
				huh.NewOption("➕  Tambah key ke provider", "add"),
				huh.NewOption("🗑️   Hapus key dari provider", "delete"),
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
		case "list":
			clearScreen()
			listUpstreamKeys(cfg)
			pause()
		case "add":
			addUpstreamKey(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "delete":
			deleteUpstreamKey(cfg)
			safeSave(cfg, cfgPath)
			pause()
		}
	}
}

func listUpstreamKeys(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	printSectionTitle("🔑", "Upstream API Keys per Provider")
	for _, p := range cfg.Providers {
		badge := providerTypeBadge(p.Type)
		fmt.Printf("  %s %s\n", badge, KeyStyle.Render(p.Name))
		if len(p.APIKeys) == 0 {
			fmt.Println("    (tidak ada key)")
		} else {
			for idx, key := range p.APIKeys {
				masked := key
				if len(key) > 12 {
					masked = key[:6] + "..." + key[len(key)-6:]
				}
				fmt.Printf("    %d. %s\n", idx+1, masked)
			}
		}
	}
}

func addUpstreamKey(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	var pIdxStr string
	huh.NewSelect[string]().
		Title("Pilih Provider").
		Options(providerSelectOptionsWithCancel(cfg)...).
		Value(&pIdxStr).
		Run()
	if pIdxStr == "" || pIdxStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	pIdx, err := strconv.Atoi(pIdxStr)
	if err != nil || pIdx < 0 || pIdx >= len(cfg.Providers) {
		return
	}

	p := &cfg.Providers[pIdx]
	if p.Type == "opencode" {
		printWarning("OpenCode adalah provider gratis otomatis (Keyless) dan tidak memerlukan konfigurasi/setup!")
		pause()
		return
	}

	var key string
	huh.NewInput().
		Title("API Key baru").
		Placeholder("sk-...").
		EchoMode(huh.EchoModePassword).
		Value(&key).
		Run()

	if key == "" {
		printError("API key tidak boleh kosong")
		return
	}

	// Test key connectivity
	var ok bool
	var count int
	var testErr error

	spinnerErr := withSpinner("Menguji konektivitas API key...", func() error {
		ok, count, testErr = testAPIKey(p.BaseURL, key, p.Type)
		return testErr
	})

	if spinnerErr != nil || !ok {
		printError(fmt.Sprintf("Koneksi API key gagal: %v", spinnerErr))

		// Ask the user if they still want to save it
		var keep bool
		errConfirm := huh.NewConfirm().
			Title("Koneksi gagal. Apakah Anda yakin tetap ingin menyimpan API key ini?").
			Value(&keep).
			Run()

		if isAbort(errConfirm) || !keep {
			printInfo("Penambahan API key dibatalkan.")
			return
		}
	} else {
		printSuccess(fmt.Sprintf("Koneksi sukses! API key aktif (ditemukan %d model).", count))
	}

	key = strings.TrimSpace(key)
	exists := false
	for _, k := range p.APIKeys {
		if k == key {
			exists = true
			break
		}
	}
	if exists {
		printError("API Key ini sudah terdaftar di provider ini!")
		return
	}

	p.APIKeys = append(p.APIKeys, key)
	printSuccess(fmt.Sprintf("API key ditambahkan ke provider %s", p.Name))
}

func deleteUpstreamKey(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	var pIdxStr string
	huh.NewSelect[string]().
		Title("Pilih Provider").
		Options(providerSelectOptionsWithCancel(cfg)...).
		Value(&pIdxStr).
		Run()
	if pIdxStr == "" || pIdxStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	pIdx, err := strconv.Atoi(pIdxStr)
	if err != nil || pIdx < 0 || pIdx >= len(cfg.Providers) {
		return
	}

	p := &cfg.Providers[pIdx]
	if p.Type == "opencode" {
		printWarning("OpenCode adalah provider gratis otomatis (Keyless) dan tidak memerlukan konfigurasi/setup!")
		pause()
		return
	}

	if len(p.APIKeys) == 0 {
		printInfo("Provider tidak memiliki API key")
		return
	}

	// Build key options
	keyOptions := make([]huh.Option[string], len(p.APIKeys))
	for i, key := range p.APIKeys {
		masked := key
		if len(key) > 12 {
			masked = key[:6] + "..." + key[len(key)-6:]
		}
		keyOptions[i] = huh.NewOption(fmt.Sprintf("%d. %s", i+1, masked), fmt.Sprintf("%d", i))
	}

	var kIdxStr string
	huh.NewSelect[string]().
		Title("Pilih API Key yang akan dihapus").
		Options(append(keyOptions, huh.NewOption("❌  Batal / kembali", "__cancel__"))...).
		Value(&kIdxStr).
		Run()
	if kIdxStr == "" || kIdxStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	kIdx, err := strconv.Atoi(kIdxStr)
	if err != nil || kIdx < 0 || kIdx >= len(p.APIKeys) {
		return
	}

	var confirm bool
	huh.NewConfirm().
		Title("Yakin hapus API key ini?").
		Value(&confirm).
		Run()

	if !confirm {
		printInfo("Dibatalkan")
		return
	}

	p.APIKeys = append(p.APIKeys[:kIdx], p.APIKeys[kIdx+1:]...)
	printSuccess("API key berhasil dihapus")
}

// ==================== Models & Combos ====================

func menuModelsCombos(cfg *config.Config, cfgPath string) {
	for {
		clearScreen()
		printSectionTitle("🤖", T("Kelola Models & Combos", "Manage Models & Combos"))

		var choice string
		err := huh.NewSelect[string]().
			Title("").
			Options(
				huh.NewOption(T("📋  Lihat semua model & combo", "📋  View all models & combos"), "list"),
				huh.NewOption(T("➕  Tambah model direct", "➕  Add direct model"), "add_direct"),
				huh.NewOption(T("➕  Tambah model combo/balancer", "➕  Add combo/balancer model"), "add_combo"),
				huh.NewOption(T("✏️   Edit model combo", "✏️   Edit combo model"), "edit_combo"),
				huh.NewOption(T("🔌  Enable/Disable model route", "🔌  Enable/Disable model route"), "toggle_enable"),
				huh.NewOption(T("❌  Hapus model / combo", "❌  Delete model / combo"), "delete"),
				huh.NewOption(T("🗑️  Hapus semua model dari satu provider", "🗑️  Delete all models of a provider"), "delete_by_provider"),
				huh.NewOption(T(" ←  Kembali", " ←  Back"), "cancel"),
			).
			Value(&choice).
			Run()

		if isAbort(err) || choice == "cancel" {
			return
		}

		switch choice {
		case "list":
			listModelsAndCombos(cfg)
			pause()
		case "add_direct":
			addDirectModel(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "add_combo":
			addComboModel(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "edit_combo":
			editComboModel(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "toggle_enable":
			toggleModelEnable(cfg, cfgPath)
			pause()
		case "delete":
			deleteModel(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "delete_by_provider":
			deleteModelsByProvider(cfg)
			safeSave(cfg, cfgPath)
			pause()
		}
	}
}

func listModelsAndCombos(cfg *config.Config) {
	// Count virtual models from opencode
	hasOpencode := false
	for _, p := range cfg.Providers {
		if p.Type == "opencode" {
			hasOpencode = true
			break
		}
	}

	if len(cfg.Models) == 0 && !hasOpencode {
		printInfo("Belum ada model/combo.")
		return
	}

	printSectionTitle("🤖", "Daftar Model & Combo")

	// 1. Print Combo / Fallback models at the very top
	totalIdx := 1
	hasCombos := false
	for _, m := range cfg.Models {
		if m.Strategy != "" {
			if !hasCombos {
				hasCombos = true
			}
			statusStr := ""
			if m.Disabled {
				statusStr = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(" [DISABLED]")
			}
			icon := "🔀"
			if m.Strategy == "fallback" {
				icon = "🔄"
			}
			fmt.Printf("  %d. %s %-20s [%s, %s]%s\n", totalIdx, icon,
				KeyStyle.Render(m.Name),
				lipgloss.NewStyle().Foreground(Warning).Render(m.Strategy),
				plural(len(m.Backends), "backend", "backends"),
				statusStr)
			for _, b := range m.Backends {
				tierStr := ""
				if b.Tier > 0 {
					tierStr = fmt.Sprintf(" (tier %d)", b.Tier)
				}
				fmt.Printf("     └─ %s / %s%s\n", b.Provider, b.Model,
					lipgloss.NewStyle().Foreground(Muted).Render(tierStr))
			}
			totalIdx++
		}
	}

	// 2. Print Direct/Individual models grouped by Provider
	for _, p := range cfg.Providers {
		var provModels []config.ModelConfig
		for _, m := range cfg.Models {
			if m.Strategy == "" && m.Provider == p.Name {
				provModels = append(provModels, m)
			}
		}

		isOpencodeProvider := p.Type == "opencode"
		if len(provModels) == 0 && !(isOpencodeProvider && hasOpencode) {
			continue // skip provider if it has no direct models
		}

		// Print provider header
		fmt.Printf("\n  ● %s %s\n", p.Type, KeyStyle.Render(p.Name))

		subIdx := 1
		for _, m := range provModels {
			statusStr := ""
			if m.Disabled {
				statusStr = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(" [DISABLED]")
			}
			icon := "🤖"
			if m.Reasoning && m.Vision {
				icon = "🧠👁️"
			} else if m.Reasoning {
				icon = "🧠"
			} else if m.Vision {
				icon = "👁️"
			}

			// Show target info if it differs from the alias name
			targetInfo := ""
			if m.Name != m.Model {
				targetInfo = MutedStyle.Render(fmt.Sprintf(" (target: %s)", m.Model))
			}

			fmt.Printf("    %d. %s %s%s%s\n", subIdx, icon, KeyStyle.Render(m.Name), targetInfo, statusStr)
			subIdx++
		}

		// Print virtual oc/auto if opencode
		if isOpencodeProvider && hasOpencode {
			virtualLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("[VIRTUAL — auto fallback gratis]")
			fmt.Printf("    %d. ⚡ %s %s\n", subIdx, KeyStyle.Render("oc/auto"), virtualLabel)
			fmt.Printf("       └─ opencode / fallback otomatis ke model gratis yang tersedia\n")
		}
	}

	// 3. Print Orphaned models (models whose provider doesn't exist anymore)
	var orphanedModels []config.ModelConfig
	for _, m := range cfg.Models {
		if m.Strategy == "" {
			found := false
			for _, p := range cfg.Providers {
				if m.Provider == p.Name {
					found = true
					break
				}
			}
			if !found {
				orphanedModels = append(orphanedModels, m)
			}
		}
	}

	if len(orphanedModels) > 0 {
		fmt.Printf("\n  ● unknown orphaned\n")
		for i, m := range orphanedModels {
			statusStr := ""
			if m.Disabled {
				statusStr = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(" [DISABLED]")
			}
			icon := "🤖"
			if m.Reasoning {
				icon = "🧠"
			} else if m.Vision {
				icon = "👁️"
			}
			fmt.Printf("    %d. %s %s (provider %s tidak ditemukan)%s\n", i+1, icon, KeyStyle.Render(m.Name), m.Provider, statusStr)
		}
	}
}

func addDirectModel(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider. Tambah provider dahulu.")
		return
	}

	var name string
	err := huh.NewInput().
		Title("Nama model alias").
		Placeholder("gpt-4o").
		Value(&name).
		Run()
	if isAbort(err) {
		printInfo("Dibatalkan")
		return
	}

	if name == "" {
		printError("Nama tidak boleh kosong")
		return
	}

	// Select provider
	var pIdxStr string
	err = huh.NewSelect[string]().
		Title("Pilih Provider (Esc untuk batal)").
		Options(providerSelectOptions(cfg)...).
		Value(&pIdxStr).
		Run()
	if isAbort(err) || pIdxStr == "" {
		printInfo("Dibatalkan")
		return
	}

	pIdx, err := strconv.Atoi(pIdxStr)
	if err != nil || pIdx < 0 || pIdx >= len(cfg.Providers) {
		return
	}
	p := cfg.Providers[pIdx]

	// Select model
	modelOptions := make([]huh.Option[string], len(p.Models)+1)
	for i, m := range p.Models {
		modelOptions[i] = huh.NewOption(m, m)
	}
	modelOptions[len(p.Models)] = huh.NewOption("✏️  Input manual", "__manual__")

	var modelID string
	err = huh.NewSelect[string]().
		Title("Pilih Model").
		Options(modelOptions...).
		Value(&modelID).
		Run()
	if isAbort(err) || modelID == "" {
		printInfo("Dibatalkan")
		return
	}

	if modelID == "__manual__" {
		err = huh.NewInput().
			Title("Model ID").
			Placeholder("gpt-4o").
			Value(&modelID).
			Run()
		if isAbort(err) || modelID == "" {
			printInfo("Dibatalkan")
			return
		}
	}

	if modelID == "" {
		printError("Model ID tidak boleh kosong")
		return
	}

	m := config.ModelConfig{
		Name:      name,
		Provider:  p.Name,
		Model:     modelID,
		Reasoning: detectedReasoningModels[modelID] || isReasoningModelID(modelID) || isReasoningModelID(name),
		Vision:    isVisionModelID(modelID) || isVisionModelID(name),
	}

	if err := cfg.AddModel(m); err != nil {
		printError(fmt.Sprintf("Gagal tambah model: %v", err))
	} else {
		printSuccess(fmt.Sprintf("Model direct '%s' berhasil ditambahkan", name))
	}
}

func buildModelOptionsForProvider(p config.ProviderConfig, selected map[string]bool) []huh.Option[string] {
	var modelOpts []huh.Option[string]
	for _, m := range p.Models {
		key := p.Name + "/" + m
		label := m
		if selected != nil && selected[key] {
			label += "  ✅ " + T("sudah dipilih", "already selected")
		}
		modelOpts = append(modelOpts, huh.NewOption(label, m))
	}
	// Add virtual models
	if p.Type == "opencode" {
		key := p.Name + "/oc/auto"
		label := "oc/auto [VIRTUAL]"
		if selected != nil && selected[key] {
			label += "  ✅ " + T("sudah dipilih", "already selected")
		}
		modelOpts = append(modelOpts, huh.NewOption(label, "oc/auto"))
	}
	if p.Type == "mimo" {
		key := p.Name + "/mimo/auto"
		label := "mimo/auto [VIRTUAL]"
		if selected != nil && selected[key] {
			label += "  ✅ " + T("sudah dipilih", "already selected")
		}
		modelOpts = append(modelOpts, huh.NewOption(label, "mimo/auto"))
	}
	return modelOpts
}

func addComboModel(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo("Belum ada provider.")
		return
	}

	var name string
	errN := huh.NewInput().
		Title("Nama model combo/balancer").
		Placeholder("budget-mix").
		Value(&name).
		Run()
	if isAbort(errN) {
		printInfo("Dibatalkan")
		return
	}
	if name == "" {
		printError("Nama tidak boleh kosong")
		return
	}

	var strategy string
	errS := huh.NewSelect[string]().
		Title("Pilih Strategi (Esc untuk batal)").
		Options(
			huh.NewOption("round-robin  — rotasi request bergantian", "round-robin"),
			huh.NewOption("fallback     — jika utama gagal, coba berikutnya", "fallback"),
		).
		Value(&strategy).
		Run()
	if isAbort(errS) || strategy == "" {
		printInfo("Dibatalkan")
		return
	}

	var backends []config.BackendConfig
	selected := make(map[string]bool)

	for {
		fmt.Printf("\n  ── Backend ke-%d ──\n", len(backends)+1)
		if len(backends) > 0 {
			fmt.Println("  Backend terpilih:")
			for _, b := range backends {
				fmt.Printf("    ✓ %s / %s\n", b.Provider, b.Model)
			}
		}

		var pIdxStr string
		huh.NewSelect[string]().
			Title("Pilih Provider (❌ Batal untuk selesai)").
			Options(append(providerSelectOptions(cfg), huh.NewOption("❌  Selesai / batal", "__cancel__"))...).
			Value(&pIdxStr).
			Run()

		// __cancel__ atau kosong = selesai memilih backend
		if pIdxStr == "" || pIdxStr == "__cancel__" {
			break
		}

		pIdx, err := strconv.Atoi(pIdxStr)
		if err != nil || pIdx < 0 || pIdx >= len(cfg.Providers) {
			break
		}
		p := cfg.Providers[pIdx]

		// Build model options
		modelOpts := buildModelOptionsForProvider(p, selected)

		var modelID string
		err = huh.NewSelect[string]().
			Title(fmt.Sprintf("Pilih Model dari %s (Esc untuk batal)", p.Name)).
			Options(modelOpts...).
			Value(&modelID).
			Run()

		if isAbort(err) {
			printInfo("Dibatalkan")
			return
		}
		if modelID == "" {
			break
		}

		key := p.Name + "/" + modelID
		if selected[key] {
			printWarning(fmt.Sprintf("%s sudah dipilih sebelumnya!", key))
			continue
		}

		selected[key] = true
		backends = append(backends, config.BackendConfig{
			Provider: p.Name,
			Model:    modelID,
		})
		printSuccess(fmt.Sprintf("%s / %s ditambahkan", p.Name, modelID))

		var more bool
		huh.NewConfirm().
			Title("Tambah backend lagi?").
			Value(&more).
			Run()

		if !more {
			break
		}
	}

	if len(backends) < 2 {
		printError("Minimal harus ada 2 backend untuk model combo/balancer")
		return
	}

	m := config.ModelConfig{
		Name:     name,
		Strategy: strategy,
		Backends: backends,
	}

	if err := cfg.AddModel(m); err != nil {
		printError(fmt.Sprintf("Gagal tambah combo: %v", err))
	} else {
		printSuccess(fmt.Sprintf("Combo model '%s' [%s] berhasil ditambahkan", name, strategy))
	}
}

func editComboModel(cfg *config.Config) {
	var combos []config.ModelConfig
	var comboIndices []int
	for i, m := range cfg.Models {
		if m.Strategy != "" {
			combos = append(combos, m)
			comboIndices = append(comboIndices, i)
		}
	}
	if len(combos) == 0 {
		printInfo("Belum ada model combo untuk diedit.")
		return
	}

	opts := make([]huh.Option[string], len(combos)+1)
	for i, c := range combos {
		icon := "🔀"
		if c.Strategy == "fallback" {
			icon = "🔄"
		}
		opts[i] = huh.NewOption(fmt.Sprintf("%s %s [%s]", icon, c.Name, c.Strategy), strconv.Itoa(i))
	}
	opts[len(combos)] = huh.NewOption(" ←  Kembali", "__cancel__")

	var choiceStr string
	err := huh.NewSelect[string]().
		Title("Pilih Model Combo untuk diedit").
		Options(opts...).
		Value(&choiceStr).
		Run()

	if isAbort(err) || choiceStr == "" || choiceStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	cIdxLocal, _ := strconv.Atoi(choiceStr)
	cIdx := comboIndices[cIdxLocal]
	m := &cfg.Models[cIdx]

	for {
		clearScreen()
		printSectionTitle("✏️", fmt.Sprintf("Edit Combo '%s' [%s]", m.Name, m.Strategy))

		fmt.Println("  Daftar Backend Saat Ini:")
		for idx, b := range m.Backends {
			tierStr := ""
			if b.Tier > 0 {
				tierStr = fmt.Sprintf(" (tier %d)", b.Tier)
			}
			fmt.Printf("    %d. %s / %s%s\n", idx+1, b.Provider, b.Model, tierStr)
		}
		fmt.Println()

		var action string
		err = huh.NewSelect[string]().
			Title("Pilih Tindakan").
			Options(
				huh.NewOption("⚙️   Ubah Strategi", "strategy"),
				huh.NewOption("📝  Ubah Nama Combo", "rename"),
				huh.NewOption("➕  Tambah backend baru", "add_backend"),
				huh.NewOption("✏️   Edit backend terdaftar", "edit_backend"),
				huh.NewOption("↕️   Ubah urutan backend", "move_backend"),
				huh.NewOption("🗑️   Hapus backend", "delete_backend"),
				huh.NewOption(" ←  Kembali & Simpan", "cancel"),
			).
			Value(&action).
			Run()

		if isAbort(err) || action == "cancel" {
			if len(m.Backends) < 2 {
				printError("Minimal harus ada 2 backend untuk model combo/balancer! Harap tambahkan backend baru.")
				pause()
				continue
			}
			break
		}

		switch action {
		case "strategy":
			var strategy string
			errS := huh.NewSelect[string]().
				Title("Pilih Strategi Baru").
				Options(
					huh.NewOption("round-robin  - rotasi request bergantian", "round-robin"),
					huh.NewOption("fallback     — jika utama gagal, coba berikutnya", "fallback"),
				).
				Value(&strategy).
				Run()
			if !isAbort(errS) && strategy != "" {
				m.Strategy = strategy
				printSuccess(fmt.Sprintf("Strategi combo '%s' diubah menjadi %s", m.Name, strategy))
				pause()
			}

		case "rename":
			var newName string
			errR := huh.NewInput().
				Title("Masukkan nama baru untuk combo").
				Placeholder(m.Name).
				Value(&newName).
				Run()

			if !isAbort(errR) && newName != "" && newName != m.Name {
				// Check for clashes
				clash := false
				for _, mdl := range cfg.Models {
					if mdl.Name == newName {
						clash = true
						break
					}
				}
				if clash {
					printError(fmt.Sprintf("Nama '%s' sudah digunakan oleh model/combo lain!", newName))
					pause()
				} else {
					oldName := m.Name
					m.Name = newName
					cfg.RenameModelCascade(oldName, newName)
					printSuccess(fmt.Sprintf("Combo '%s' berhasil diubah namanya menjadi '%s' (cascade API keys terupdate)", oldName, newName))
					pause()
				}
			}

		case "add_backend":
			var pIdxStr string
			huh.NewSelect[string]().
				Title("Pilih Provider untuk backend baru").
				Options(append(providerSelectOptions(cfg), huh.NewOption(" ←  Kembali", "__cancel__"))...).
				Value(&pIdxStr).
				Run()

			if pIdxStr == "" || pIdxStr == "__cancel__" {
				continue
			}

			pIdx, _ := strconv.Atoi(pIdxStr)
			p := cfg.Providers[pIdx]

			selectedBackends := make(map[string]bool)
			for _, b := range m.Backends {
				selectedBackends[b.Provider+"/"+b.Model] = true
			}
			modelOpts := buildModelOptionsForProvider(p, selectedBackends)

			var modelID string
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Pilih Model dari %s", p.Name)).
				Options(modelOpts...).
				Value(&modelID).
				Run()

			if modelID != "" {
				m.Backends = append(m.Backends, config.BackendConfig{
					Provider: p.Name,
					Model:    modelID,
				})
				printSuccess(fmt.Sprintf("Backend '%s / %s' ditambahkan.", p.Name, modelID))
				pause()
			}

		case "edit_backend":
			if len(m.Backends) == 0 {
				printInfo("Belum ada backend.")
				pause()
				continue
			}

			bOpts := make([]huh.Option[string], len(m.Backends)+1)
			for idx, b := range m.Backends {
				bOpts[idx] = huh.NewOption(fmt.Sprintf("%d. %s / %s", idx+1, b.Provider, b.Model), strconv.Itoa(idx))
			}
			bOpts[len(m.Backends)] = huh.NewOption(" ←  Kembali", "__cancel__")

			var bChoice string
			huh.NewSelect[string]().
				Title("Pilih backend yang ingin diganti").
				Options(bOpts...).
				Value(&bChoice).
				Run()

			if bChoice == "" || bChoice == "__cancel__" {
				continue
			}

			bIdx, _ := strconv.Atoi(bChoice)

			var pIdxStr string
			huh.NewSelect[string]().
				Title("Pilih Provider baru").
				Options(append(providerSelectOptions(cfg), huh.NewOption(" ←  Kembali", "__cancel__"))...).
				Value(&pIdxStr).
				Run()

			if pIdxStr == "" || pIdxStr == "__cancel__" {
				continue
			}

			pIdx, _ := strconv.Atoi(pIdxStr)
			p := cfg.Providers[pIdx]

			selectedBackends := make(map[string]bool)
			for _, b := range m.Backends {
				selectedBackends[b.Provider+"/"+b.Model] = true
			}
			modelOpts := buildModelOptionsForProvider(p, selectedBackends)

			var modelID string
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Pilih Model baru dari %s", p.Name)).
				Options(modelOpts...).
				Value(&modelID).
				Run()

			if modelID != "" {
				m.Backends[bIdx] = config.BackendConfig{
					Provider: p.Name,
					Model:    modelID,
				}
				printSuccess(fmt.Sprintf("Backend %d diubah menjadi '%s / %s'.", bIdx+1, p.Name, modelID))
				pause()
			}

		case "move_backend":
			if len(m.Backends) < 2 {
				printInfo("Minimal harus ada 2 backend untuk memindahkan urutan.")
				pause()
				continue
			}

			bOpts := make([]huh.Option[string], len(m.Backends)+1)
			for idx, b := range m.Backends {
				bOpts[idx] = huh.NewOption(fmt.Sprintf("%d. %s / %s", idx+1, b.Provider, b.Model), strconv.Itoa(idx))
			}
			bOpts[len(m.Backends)] = huh.NewOption(" ←  Kembali", "__cancel__")

			var bChoice string
			huh.NewSelect[string]().
				Title("Pilih backend yang ingin dipindahkan posisinya").
				Options(bOpts...).
				Value(&bChoice).
				Run()

			if bChoice == "" || bChoice == "__cancel__" {
				continue
			}
			fromIdx, _ := strconv.Atoi(bChoice)

			posOpts := make([]huh.Option[string], len(m.Backends))
			for idx := range m.Backends {
				posOpts[idx] = huh.NewOption(fmt.Sprintf("Posisi ke-%d", idx+1), strconv.Itoa(idx))
			}

			var posChoice string
			huh.NewSelect[string]().
				Title("Pilih posisi tujuan baru").
				Options(posOpts...).
				Value(&posChoice).
				Run()

			if posChoice != "" {
				toIdx, _ := strconv.Atoi(posChoice)
				if fromIdx != toIdx {
					backendToMove := m.Backends[fromIdx]
					m.Backends = append(m.Backends[:fromIdx], m.Backends[fromIdx+1:]...)

					// Insert at new position
					tail := append([]config.BackendConfig{backendToMove}, m.Backends[toIdx:]...)
					m.Backends = append(m.Backends[:toIdx], tail...)

					printSuccess(fmt.Sprintf("Backend berhasil dipindahkan ke posisi ke-%d.", toIdx+1))
					pause()
				}
			}

		case "delete_backend":
			if len(m.Backends) == 0 {
				printInfo("Belum ada backend.")
				pause()
				continue
			}

			bOpts := make([]huh.Option[string], len(m.Backends)+1)
			for idx, b := range m.Backends {
				bOpts[idx] = huh.NewOption(fmt.Sprintf("%d. %s / %s", idx+1, b.Provider, b.Model), strconv.Itoa(idx))
			}
			bOpts[len(m.Backends)] = huh.NewOption(" ←  Kembali", "__cancel__")

			var bChoice string
			huh.NewSelect[string]().
				Title("Pilih backend yang ingin dihapus dari combo").
				Options(bOpts...).
				Value(&bChoice).
				Run()

			if bChoice == "" || bChoice == "__cancel__" {
				continue
			}
			bIdx, _ := strconv.Atoi(bChoice)

			deletedBackend := m.Backends[bIdx]
			m.Backends = append(m.Backends[:bIdx], m.Backends[bIdx+1:]...)
			printSuccess(fmt.Sprintf("Backend '%s / %s' berhasil dihapus.", deletedBackend.Provider, deletedBackend.Model))
			pause()
		}
	}
}

func deleteModel(cfg *config.Config) {
	if len(cfg.Models) == 0 {
		printInfo("Belum ada model/combo.")
		return
	}

	var opts []huh.Option[string]

	// 1. Add combos at the top
	for _, m := range cfg.Models {
		if m.Strategy != "" {
			label := fmt.Sprintf("🔀 %s [%s]", m.Name, m.Strategy)
			opts = append(opts, huh.NewOption(label, m.Name))
		}
	}

	// 2. Add individual models grouped by provider
	for _, p := range cfg.Providers {
		var provModels []config.ModelConfig
		for _, m := range cfg.Models {
			if m.Strategy == "" && m.Provider == p.Name {
				provModels = append(provModels, m)
			}
		}

		if len(provModels) == 0 {
			continue
		}

		// Add visual separator header
		headerLabel := fmt.Sprintf("── ● %s %s ──", p.Type, p.Name)
		opts = append(opts, huh.NewOption(headerLabel, "__header__"))

		for _, m := range provModels {
			icon := "🤖"
			if m.Reasoning {
				icon = "🧠"
			} else if m.Vision {
				icon = "👁️"
			}
			label := fmt.Sprintf("   %s %s", icon, m.Name)
			opts = append(opts, huh.NewOption(label, m.Name))
		}
	}

	opts = append(opts, huh.NewOption("❌  Batal / kembali", "__cancel__"))

	selectedNames := make([]string, 0)
	err := huh.NewMultiSelect[string]().
		Title("Pilih model/combo yang akan dihapus (space untuk select, enter untuk confirm)").
		Options(opts...).
		Value(&selectedNames).
		Run()

	if isAbort(err) {
		printInfo("Dibatalkan")
		return
	}

	// Filter out cancel and header sentinels if selected
	filtered := make([]string, 0, len(selectedNames))
	for _, name := range selectedNames {
		if name != "__cancel__" && name != "__header__" {
			filtered = append(filtered, name)
		}
	}
	selectedNames = filtered

	if len(selectedNames) == 0 {
		printInfo("Tidak ada model dipilih")
		return
	}

	var confirm bool
	huh.NewConfirm().
		Title(fmt.Sprintf("Yakin hapus %d model?", len(selectedNames))).
		Description(fmt.Sprintf("Model: %s", strings.Join(selectedNames, ", "))).
		Value(&confirm).
		Run()

	if !confirm {
		printInfo("Dibatalkan")
		return
	}

	deleted := 0
	for _, name := range selectedNames {
		if err := cfg.DeleteModel(name); err != nil {
			printError(fmt.Sprintf("Gagal hapus '%s': %v", name, err))
		} else {
			deleted++
		}
	}
	printSuccess(fmt.Sprintf("%d model berhasil dihapus", deleted))
	printInfo("  ↳ Model juga dihapus dari provider list & allowed_models API key (cascade)")
}

func deleteModelsByProvider(cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		printInfo(T("Belum ada provider.", "No providers configured."))
		return
	}

	opts := providerSelectOptions(cfg)
	opts = append(opts, huh.NewOption(T("❌  Batal / kembali", "❌  Cancel / back"), "__cancel__"))

	var idxStr string
	err := huh.NewSelect[string]().
		Title(T("Pilih provider yang modelnya akan dihapus semua (Esc untuk batal)", "Select provider to delete all its models (Esc to cancel)")).
		Options(opts...).
		Value(&idxStr).
		Run()

	if isAbort(err) || idxStr == "__cancel__" || idxStr == "" {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}

	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(cfg.Providers) {
		return
	}

	p := &cfg.Providers[idx]

	// Find all models belonging to this provider
	var modelsToDelete []string
	for _, m := range cfg.Models {
		if m.Strategy == "" && m.Provider == p.Name {
			modelsToDelete = append(modelsToDelete, m.Name)
		}
	}

	if len(modelsToDelete) == 0 {
		printInfo(T("Tidak ada model ditemukan untuk provider ini.", "No models found for this provider."))
		return
	}

	var confirm bool
	huh.NewConfirm().
		Title(fmt.Sprintf(T("Yakin hapus %d model dari '%s'?", "Are you sure you want to delete %d models from '%s'?"), len(modelsToDelete), p.Name)).
		Value(&confirm).
		Run()

	if !confirm {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}

	deleted := 0
	for _, name := range modelsToDelete {
		if err := cfg.DeleteModel(name); err != nil {
			printError(fmt.Sprintf(T("Gagal hapus '%s': %v", "Failed to delete '%s': %v"), name, err))
		} else {
			deleted++
		}
	}

	printSuccess(fmt.Sprintf(T("%d model dari provider '%s' berhasil dihapus", "%d models from provider '%s' successfully deleted"), deleted, p.Name))
}


func testModelsBeforeAdd(baseURL, apiKey, providerType string, selectedModels []string) []string {
	if len(selectedModels) == 0 {
		return selectedModels
	}

	var testMore bool
	err := huh.NewConfirm().
		Title(fmt.Sprintf("Apakah Anda ingin mengetes %s terlebih dahulu?", plural(len(selectedModels), "model", "models"))).
		Value(&testMore).
		Run()
	if isAbort(err) || !testMore {
		return selectedModels
	}

	var testType string
	errType := huh.NewSelect[string]().
		Title("Pilih jenis pengetesan model:").
		Options(
			huh.NewOption("📝  Test Teks Biasa (Hemat Token)", "text"),
			huh.NewOption("👁️  Test Vision / Multimodal (Kirim Gambar)", "vision"),
		).
		Value(&testType).
		Run()

	if isAbort(errType) || testType == "" {
		return selectedModels
	}

	forceVision := testType == "vision"

	fmt.Println()
	printInfo("Memulai pengetesan model...")

	successModels := make([]string, 0)
	failedModels := make([]string, 0)

	type testResult struct {
		modelID  string
		response string
		latency  int64
		err      error
	}

	results := make([]testResult, len(selectedModels))

	for i, modelID := range selectedModels {
		var response string
		var latency int64
		var isReasoning bool
		var isVision bool
		var testErr error

		spinnerErr := withSpinner(fmt.Sprintf("Testing '%s'...", modelID), func() error {
			response, latency, isReasoning, isVision, testErr = testModel(baseURL, apiKey, modelID, providerType, forceVision)
			return testErr
		})

		results[i] = testResult{
			modelID:  modelID,
			response: response,
			latency:  latency,
			err:      spinnerErr,
		}

		if spinnerErr != nil {
			fmt.Printf("  ❌  %-40s GAGAL: %v\n", modelID, spinnerErr)
			failedModels = append(failedModels, modelID)
		} else {
			if isReasoning && isVision {
				detectedReasoningModels[modelID] = true
				fmt.Printf("  🧠👁️  %-40s OK (%dms) [Reasoning & Vision] -> Response: %q\n", modelID, latency, truncate(response, 45))
			} else if isReasoning {
				detectedReasoningModels[modelID] = true
				fmt.Printf("  🧠  %-40s OK (%dms) [Reasoning] -> Response: %q\n", modelID, latency, truncate(response, 45))
			} else if isVision {
				fmt.Printf("  👁️  %-40s OK (%dms) [Vision] -> Response: %q\n", modelID, latency, truncate(response, 45))
			} else {
				fmt.Printf("  ✅  %-40s OK (%dms) -> Response: %q\n", modelID, latency, truncate(response, 45))
			}
			successModels = append(successModels, modelID)
		}
	}

	fmt.Println()
	printInfo(fmt.Sprintf("Hasil: %d sukses, %d gagal", len(successModels), len(failedModels)))

	if len(successModels) == 0 && len(failedModels) == 0 {
		return nil
	}

	// Choose which models to keep
	options := make([]huh.Option[string], len(selectedModels))
	for i, r := range results {
		status := "✅ OK"
		if r.err != nil {
			status = "❌ GAGAL"
		}
		label := fmt.Sprintf("%-35s [%s]", r.modelID, status)
		options[i] = huh.NewOption(label, r.modelID)
	}

	// We pre-select all models that successfully responded
	finalModels := successModels

	err = huh.NewMultiSelect[string]().
		Title("Pilih model yang ingin disimpan (space untuk select, enter untuk confirm)").
		Options(options...).
		Value(&finalModels).
		Run()

	if isAbort(err) {
		printInfo("Batal menyimpan model")
		return nil
	}

	return finalModels
}

func toggleModelEnable(cfg *config.Config, cfgPath string) {
	if len(cfg.Models) == 0 {
		printInfo("Belum ada model/combo.")
		return
	}

	var opts []huh.Option[string]

	// 1. Add combos at the top
	for i, m := range cfg.Models {
		if m.Strategy != "" {
			status := "Active"
			if m.Disabled {
				status = "Disabled"
			}
			icon := "🔀"
			if m.Strategy == "fallback" {
				icon = "🔄"
			}
			label := fmt.Sprintf("%s %s (%s)", icon, m.Name, status)
			opts = append(opts, huh.NewOption(label, strconv.Itoa(i)))
		}
	}

	// 2. Add individual models grouped by provider
	for _, p := range cfg.Providers {
		type modelItem struct {
			model config.ModelConfig
			index int
		}
		var provModels []modelItem
		for i, m := range cfg.Models {
			if m.Strategy == "" && m.Provider == p.Name {
				provModels = append(provModels, modelItem{model: m, index: i})
			}
		}

		if len(provModels) == 0 {
			continue
		}

		// Add visual separator header
		headerLabel := fmt.Sprintf("── ● %s %s ──", p.Type, p.Name)
		opts = append(opts, huh.NewOption(headerLabel, "__header__"))

		for _, item := range provModels {
			m := item.model
			status := "Active"
			if m.Disabled {
				status = "Disabled"
			}
			icon := "🤖"
			if m.Reasoning {
				icon = "🧠"
			} else if m.Vision {
				icon = "👁️"
			}
			label := fmt.Sprintf("   %s %s (%s)", icon, m.Name, status)
			opts = append(opts, huh.NewOption(label, strconv.Itoa(item.index)))
		}
	}

	opts = append(opts, huh.NewOption("❌  Batal / kembali", "__cancel__"))

	var choiceStr string
	err := huh.NewSelect[string]().
		Title("Pilih model/combo untuk di-enable/disable").
		Options(opts...).
		Value(&choiceStr).
		Run()

	if isAbort(err) || choiceStr == "" || choiceStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	if choiceStr == "__header__" {
		printWarning("Itu adalah header kelompok provider. Silakan pilih model di bawahnya.")
		pause()
		return
	}

	idx, err := strconv.Atoi(choiceStr)
	if err != nil || idx < 0 || idx >= len(cfg.Models) {
		return
	}

	cfg.Models[idx].Disabled = !cfg.Models[idx].Disabled
	status := "diaktifkan"
	if cfg.Models[idx].Disabled {
		status = "dinonaktifkan"
	}

	safeSave(cfg, cfgPath)
	printSuccess(fmt.Sprintf("Model '%s' berhasil %s", cfg.Models[idx].Name, status))
}

