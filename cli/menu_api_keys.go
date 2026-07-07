package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/aigateway/config"
	"github.com/google/uuid"
)

// ==================== Manajemen API Key (User/Client) Menu ====================

func menuAPIKeys(cfg *config.Config, cfgPath string) {
	for {
		clearScreen()
		printSectionTitle("🔑", "Manajemen API Key (User/Client)")

		var choice string
		err := huh.NewSelect[string]().
			Title("").
			Options(
				huh.NewOption("📋  Lihat semua API key", "list"),
				huh.NewOption("➕  Buat API key baru", "add"),
				huh.NewOption("✏️   Edit API key (model/rate limit)", "edit"),
				huh.NewOption("🔌  Enable/Disable API key", "toggle_enable"),
				huh.NewOption("🗑️   Hapus API key", "delete"),
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
			listAPIKeys(cfg)
			pause()
		case "add":
			addAPIKey(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "edit":
			editAPIKey(cfg)
			safeSave(cfg, cfgPath)
			pause()
		case "toggle_enable":
			toggleAPIKeyEnable(cfg, cfgPath)
			pause()
		case "delete":
			deleteAPIKey(cfg)
			safeSave(cfg, cfgPath)
			pause()
		}
	}
}

func listAPIKeys(cfg *config.Config) {
	if len(cfg.APIKeys) == 0 {
		printInfo("Belum ada API key. Buat API key lewat menu ini atau Quick Setup.")
		return
	}

	printSectionTitle("🔑", "API Key (User/Client)")
	for i, k := range cfg.APIKeys {
		masked := k.Key
		if len(masked) > 24 {
			masked = masked[:24] + "..."
		}
		rateStr := "unlimited"
		if k.RateLimit > 0 {
			rateStr = fmt.Sprintf("%d req/min", k.RateLimit)
		}
		tsStr := "inherit"
		if k.TokenSaver != nil {
			if *k.TokenSaver {
				tsStr = "enabled"
			} else {
				tsStr = "disabled"
			}
		}

		statusStr := ""
		if k.Disabled {
			statusStr = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(" [DISABLED]")
		}

		fmt.Printf("  %s. %s%s\n", KeyStyle.Render(fmt.Sprintf("%d", i+1)), KeyStyle.Render(k.Name), statusStr)
		fmt.Printf("     🔑 %s\n", MutedStyle.Render(masked))
		fmt.Printf("     📦 Model: %s  │  ⏱  %s  │  🧠 Token Saver: %s\n",
			strings.Join(k.AllowedModels, ", "),
			lipgloss.NewStyle().Foreground(Accent).Render(rateStr),
			tsStr)
	}
}

func apiKeySelectOptions(cfg *config.Config) []huh.Option[string] {
	options := make([]huh.Option[string], len(cfg.APIKeys))
	for i, k := range cfg.APIKeys {
		rateStr := "unlimited"
		if k.RateLimit > 0 {
			rateStr = fmt.Sprintf("%d req/min", k.RateLimit)
		}
		options[i] = huh.NewOption(
			fmt.Sprintf("%s [%s] — %s", k.Name, strings.Join(k.AllowedModels, ", "), rateStr),
			fmt.Sprintf("%d", i),
		)
	}
	return options
}

// apiKeySelectOptionsWithCancel returns API key options plus a cancel sentinel at the bottom.
func apiKeySelectOptionsWithCancel(cfg *config.Config) []huh.Option[string] {
	return append(apiKeySelectOptions(cfg), huh.NewOption("❌  Batal / kembali", "__cancel__"))
}

func addAPIKey(cfg *config.Config) {
	if len(cfg.Models) == 0 {
		printError("Belum ada model. Tambah provider dulu di Manajemen AI.")
		return
	}

	printSectionTitle("➕", "Buat API Key Baru")

	var name string
	errN := huh.NewInput().
		Title("Nama (misal: User 1)").
		Placeholder("User").
		Value(&name).
		Run()
	if isAbort(errN) {
		printInfo("Dibatalkan")
		return
	}
	if name == "" {
		name = "User"
	}

	key := "sk-gw-" + uuid.New().String()

	// Select allowed models — opsi "Semua" tersedia via enter tanpa pilih apapun atau select *
	selectedModels := make([]string, 0)
	modelOptions := buildModelOptions(cfg)

	errMS := huh.NewMultiSelect[string]().
		Title("Akses model (space untuk select, enter untuk confirm, kosong = semua)").
		Options(modelOptions...).
		Value(&selectedModels).
		Run()
	if isAbort(errMS) {
		printInfo("Dibatalkan")
		return
	}
	hasWildcard := false
	for _, m := range selectedModels {
		if m == "*" {
			hasWildcard = true
			break
		}
	}
	if len(selectedModels) == 0 || hasWildcard {
		selectedModels = []string{"*"}
		printInfo("Semua model dipilih (wildcard)")
	}

	// Rate limit
	var rateStr string
	errR := huh.NewInput().
		Title("Rate limit per menit (0 = unlimited, Esc untuk batal)").
		Placeholder("0").
		Value(&rateStr).
		Run()
	if isAbort(errR) {
		printInfo("Dibatalkan")
		return
	}

	rateLimit := 0
	if rateStr != "" {
		if v, err := strconv.Atoi(rateStr); err == nil {
			rateLimit = v
		}
	}

	var tsChoice string
	errTS := huh.NewSelect[string]().
		Title("Token Saver (RTK)").
		Options(
			huh.NewOption("Inherit global setting (default)", "inherit"),
			huh.NewOption("Force Enable (always on for this key)", "enable"),
			huh.NewOption("Force Disable (always off for this key)", "disable"),
		).
		Value(&tsChoice).
		Run()
	if isAbort(errTS) {
		printInfo("Dibatalkan")
		return
	}

	var tsPtr *bool
	if tsChoice == "enable" {
		t := true
		tsPtr = &t
	} else if tsChoice == "disable" {
		f := false
		tsPtr = &f
	}

	k := config.APIKeyConfig{
		Key:           key,
		Name:          name,
		AllowedModels: selectedModels,
		RateLimit:     rateLimit,
		TokenSaver:    tsPtr,
	}
	if err := cfg.AddAPIKey(k); err != nil {
		printError(fmt.Sprintf("%v", err))
		return
	}

	fmt.Println()
	printSuccess("API Key dibuat!")
	printKeyValue("🔑 Key:", key)
	printKeyValue("📦 Model:", strings.Join(selectedModels, ", "))
	tsVal := "inherit"
	if tsPtr != nil {
		if *tsPtr {
			tsVal = "enabled"
		} else {
			tsVal = "disabled"
		}
	}
	printKeyValue("🧠 Token Saver:", tsVal)
}

func editAPIKey(cfg *config.Config) {
	if len(cfg.APIKeys) == 0 {
		printInfo("Belum ada API key.")
		return
	}

	listAPIKeys(cfg)
	fmt.Println()

	var idxStr string
	huh.NewSelect[string]().
		Title("Pilih API key yang akan diedit").
		Options(apiKeySelectOptionsWithCancel(cfg)...).
		Value(&idxStr).
		Run()
	if idxStr == "" || idxStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(cfg.APIKeys) {
		return
	}

	k := cfg.APIKeys[idx]
	printSectionTitle("✏️", fmt.Sprintf("Edit '%s'", k.Name))

	// Edit name
	var newName string
	errN := huh.NewInput().
		Title(fmt.Sprintf("Name [%s] (Esc untuk batal)", k.Name)).
		Placeholder(k.Name).
		Value(&newName).
		Run()
	if isAbort(errN) {
		printInfo("Dibatalkan")
		return
	}
	if newName != "" {
		k.Name = newName
	}

	// Edit allowed models
	fmt.Printf("  Model saat ini: %s\n\n", strings.Join(k.AllowedModels, ", "))

	newModels := make([]string, 0)
	modelOptions := buildModelOptions(cfg)

	errM := huh.NewMultiSelect[string]().
		Title("Allowed models (space untuk select, enter untuk confirm, Esc untuk batal)").
		Options(modelOptions...).
		Value(&newModels).
		Run()
	if isAbort(errM) {
		printInfo("Dibatalkan")
		return
	}

	if len(newModels) > 0 {
		hasWildcard := false
		for _, m := range newModels {
			if m == "*" {
				hasWildcard = true
				break
			}
		}
		if hasWildcard {
			newModels = []string{"*"}
		}
		k.AllowedModels = newModels
	}

	// Edit rate limit
	var newRate string
	errR := huh.NewInput().
		Title(fmt.Sprintf("Rate limit per menit [%d, 0=unlimited, Esc untuk batal]", k.RateLimit)).
		Placeholder(strconv.Itoa(k.RateLimit)).
		Value(&newRate).
		Run()
	if isAbort(errR) {
		printInfo("Dibatalkan")
		return
	}
	if newRate != "" {
		if v, err := strconv.Atoi(newRate); err == nil {
			k.RateLimit = v
		}
	}

	// Edit token saver
	currentTSVal := "inherit"
	if k.TokenSaver != nil {
		if *k.TokenSaver {
			currentTSVal = "enable"
		} else {
			currentTSVal = "disable"
		}
	}

	var newTSChoice string
	errTS := huh.NewSelect[string]().
		Title(fmt.Sprintf("Token Saver [%s]", currentTSVal)).
		Options(
			huh.NewOption("Inherit global setting (default)", "inherit"),
			huh.NewOption("Force Enable (always on for this key)", "enable"),
			huh.NewOption("Force Disable (always off for this key)", "disable"),
		).
		Value(&newTSChoice).
		Run()
	if isAbort(errTS) {
		printInfo("Dibatalkan")
		return
	}

	if newTSChoice != "" {
		if newTSChoice == "inherit" {
			k.TokenSaver = nil
		} else if newTSChoice == "enable" {
			t := true
			k.TokenSaver = &t
		} else if newTSChoice == "disable" {
			f := false
			k.TokenSaver = &f
		}
	}

	cfg.APIKeys[idx] = k
	printSuccess(fmt.Sprintf("API Key '%s' diupdate", k.Name))
}

func deleteAPIKey(cfg *config.Config) {
	if len(cfg.APIKeys) == 0 {
		printInfo("Belum ada API key.")
		return
	}

	var idxStr string
	huh.NewSelect[string]().
		Title("Pilih API key yang akan dihapus").
		Options(apiKeySelectOptionsWithCancel(cfg)...).
		Value(&idxStr).
		Run()
	if idxStr == "" || idxStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(cfg.APIKeys) {
		return
	}

	name := cfg.APIKeys[idx].Name

	var confirm bool
	errC := huh.NewConfirm().
		Title(fmt.Sprintf("Yakin hapus key '%s'?", name)).
		Description("API key yang dihapus tidak bisa dipulihkan.").
		Value(&confirm).
		Run()

	if isAbort(errC) || !confirm {
		printInfo("Dibatalkan")
		return
	}

	if err := cfg.DeleteAPIKey(cfg.APIKeys[idx].Key); err != nil {
		printError(fmt.Sprintf("%v", err))
		return
	}
	printSuccess(fmt.Sprintf("API Key '%s' dihapus", name))
}

// buildModelOptions groups models by combo first, then by provider under unified label format.
// Prepend "*" option for allowing all models.
func buildModelOptions(cfg *config.Config) []huh.Option[string] {
	var options []huh.Option[string]

	// 1. Allow All option
	options = append(options, huh.NewOption("* (Semua Model / Allow All)", "*"))

	// 2. Combo models
	for _, m := range cfg.Models {
		if m.Strategy != "" {
			label := fmt.Sprintf("  • [COMBO] %s (%s)", m.Name, m.Strategy)
			options = append(options, huh.NewOption(label, m.Name))
		}
	}

	// 3. Direct models grouped by provider
	providers := make(map[string]bool)
	for _, m := range cfg.Models {
		if m.Strategy == "" && m.Provider != "" {
			providers[m.Provider] = true
		}
	}

	var providerList []string
	for p := range providers {
		providerList = append(providerList, p)
	}
	sort.Strings(providerList)

	for _, p := range providerList {
		for _, m := range cfg.Models {
			if m.Strategy == "" && m.Provider == p {
				label := fmt.Sprintf("  • [%s] %s", p, m.Name)
				options = append(options, huh.NewOption(label, m.Name))
			}
		}
	}

	return options
}

func toggleAPIKeyEnable(cfg *config.Config, cfgPath string) {
	if len(cfg.APIKeys) == 0 {
		printInfo("Belum ada API key.")
		return
	}

	opts := make([]huh.Option[string], len(cfg.APIKeys)+1)
	for i, k := range cfg.APIKeys {
		status := "Active"
		if k.Disabled {
			status = "Disabled"
		}
		label := fmt.Sprintf("%s (%s)", k.Name, status)
		opts[i] = huh.NewOption(label, strconv.Itoa(i))
	}
	opts[len(cfg.APIKeys)] = huh.NewOption("❌  Batal / kembali", "__cancel__")

	var choiceStr string
	err := huh.NewSelect[string]().
		Title("Pilih API Key untuk di-enable/disable").
		Options(opts...).
		Value(&choiceStr).
		Run()

	if isAbort(err) || choiceStr == "" || choiceStr == "__cancel__" {
		printInfo("Dibatalkan")
		return
	}

	idx, err := strconv.Atoi(choiceStr)
	if err != nil || idx < 0 || idx >= len(cfg.APIKeys) {
		return
	}

	cfg.APIKeys[idx].Disabled = !cfg.APIKeys[idx].Disabled
	status := "diaktifkan"
	if cfg.APIKeys[idx].Disabled {
		status = "dinonaktifkan"
	}

	safeSave(cfg, cfgPath)
	printSuccess(fmt.Sprintf("API Key '%s' berhasil %s", cfg.APIKeys[idx].Name, status))
}
