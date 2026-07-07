package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/aigateway/config"
)

// ==================== Manajemen Server Menu ====================

func menuServer(cfg *config.Config, cfgPath string) {
	for {
		clearScreen()
		printSectionTitle("⚙️", "Manajemen Server")

		var choice string
		err := huh.NewSelect[string]().
			Title("").
			Options(
				huh.NewOption("📋  Lihat settings", "show"),
				huh.NewOption("📋  Lihat log server (Live Logs)", "viewlogs"),
				huh.NewOption("🌐  Edit host & port", "host"),
				huh.NewOption("🔐  Edit admin secret", "secret"),
				huh.NewOption("⚡  Edit concurrency limits", "concurrency"),
				huh.NewOption("📝  Edit log level (Tingkat Detail)", "loglevel"),
				huh.NewOption("🧠  Token Saver (RTK)", "tokensaver"),
				huh.NewOption("🌐  Quick Tunnel (Cloudflare)", "tunnel"),
				huh.NewOption("🛡️  Warp Proxy (Khusus OpenCode)", "warp"),
				huh.NewOption("📊  Web Dashboard & Playground", "dashboard"),
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
		case "show":
			clearScreen()
			showServerSettings(cfg)
			pause()
		case "viewlogs":
			viewLogs()
		case "host":
			if editHostPort(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		case "secret":
			if editAdminSecret(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		case "concurrency":
			if editConcurrency(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		case "loglevel":
			if editLogLevel(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		case "tokensaver":
			if editTokenSaver(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		case "tunnel":
			if editQuickTunnel(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		case "warp":
			if editWarpSettings(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		case "dashboard":
			if editDashboardToggle(cfg) {
				safeSave(cfg, cfgPath)
			}
			pause()
		}
	}
}

func showServerSettings(cfg *config.Config) {
	printSectionTitle("⚙️", "Server Settings")

	// Server info
	printKeyValue("🌐  Host:", cfg.Server.Host)
	printKeyValue("🔌  Port:", strconv.Itoa(cfg.Server.Port))
	printKeyValue("🔐  Admin Secret:", maskString(cfg.Server.AdminSecret))

	logLevel := cfg.Server.LogLevel
	if logLevel == "" {
		logLevel = "info (default)"
	}
	printKeyValue("  Log Level:", logLevel)
	printKeyValue("📊  Web Dashboard:", boolEmoji(cfg.Server.IsDashboardEnabled()))

	fmt.Println()
	fmt.Println(SubtitleStyle.Render("  ⚡  Concurrency Limits"))
	printKeyValue("  Max Concurrent:", strconv.Itoa(cfg.Concurrency.MaxConcurrent))
	printKeyValue("  Per Provider:", strconv.Itoa(cfg.Concurrency.PerProvider))
	printKeyValue("  Per Model:", strconv.Itoa(cfg.Concurrency.PerModel))
	printKeyValue("  Queue Depth:", strconv.Itoa(cfg.Concurrency.QueueDepth))
	printKeyValue("  Queue Timeout:", cfg.Concurrency.QueueTimeout.String())

	fmt.Println()
	fmt.Println(SubtitleStyle.Render("  💾  Cache Settings"))
	cacheStatus := "Disabled"
	if cfg.Cache.Enabled {
		cacheStatus = "Enabled"
	}
	printKeyValue("  Status:", cacheStatus)
	printKeyValue("  Max Size:", strconv.Itoa(cfg.Cache.MaxSize))
	printKeyValue("  TTL:", fmt.Sprintf("%ds", cfg.Cache.TTL))

	fmt.Println()
	fmt.Println(SubtitleStyle.Render("  🔄  Retry Settings"))
	printKeyValue("  Max Retries:", strconv.Itoa(cfg.Retry.MaxRetries))
	printKeyValue("  Initial Backoff:", fmt.Sprintf("%dms", cfg.Retry.InitialBackoff))
	printKeyValue("  Max Backoff:", fmt.Sprintf("%dms", cfg.Retry.MaxBackoff))

	fmt.Println()
	fmt.Println(SubtitleStyle.Render("  🧠  Token Saver (RTK)"))
	tsStatus := "❌ Disabled"
	if cfg.TokenSaver.Enabled {
		tsStatus = "✅ Enabled"
	}
	printKeyValue("  Status:", tsStatus)
	printKeyValue("  Max Input Bytes:", fmt.Sprintf("%d bytes", cfg.TokenSaver.MaxInputBytes))
	printKeyValue("  Caveman Mode:", boolEmoji(cfg.TokenSaver.CavemanMode))
	printKeyValue("  Compress User Msgs:", boolEmoji(cfg.TokenSaver.CompressUser))
	printKeyValue("  Minify JSON:", boolEmoji(cfg.TokenSaver.MinifyJSON))
	printKeyValue("  Strip Comments:", boolEmoji(cfg.TokenSaver.StripComments))

	fmt.Println()
	fmt.Println(SubtitleStyle.Render("  🌐  Quick Tunnel"))
	qtStatus := "❌ Disabled"
	if cfg.Server.QuickTunnel {
		qtStatus = "✅ Enabled"
	}
	printKeyValue("  Status:", qtStatus)
}

func editHostPort(cfg *config.Config) bool {
	printSectionTitle("🌐", "Edit Host & Port")

	var host string
	errH := huh.NewInput().
		Title(fmt.Sprintf("Host [%s] (Esc untuk batal)", cfg.Server.Host)).
		Placeholder(cfg.Server.Host).
		Value(&host).
		Run()
	if isAbort(errH) {
		printInfo("Dibatalkan")
		return false
	}

	if host != "" {
		cfg.Server.Host = host
	}

	var portStr string
	errP := huh.NewInput().
		Title(fmt.Sprintf("Port [%d] (Esc untuk batal)", cfg.Server.Port)).
		Placeholder(strconv.Itoa(cfg.Server.Port)).
		Value(&portStr).
		Run()
	if isAbort(errP) {
		printInfo("Dibatalkan")
		return false
	}

	if portStr != "" {
		if v, err := strconv.Atoi(portStr); err == nil {
			cfg.Server.Port = v
		}
	}

	printSuccess("Disimpan")
	printWarning("Perubahan host/port memerlukan restart server agar berlaku.")
	return true
}

func editAdminSecret(cfg *config.Config) bool {
	printSectionTitle("🔐", "Edit Admin Secret")

	var secret string
	errS := huh.NewInput().
		Title("Admin secret baru (Esc untuk batal)").
		EchoMode(huh.EchoModePassword).
		Value(&secret).
		Run()
	if isAbort(errS) {
		printInfo("Dibatalkan")
		return false
	}

	if secret != "" {
		cfg.Server.AdminSecret = secret
		printSuccess("Disimpan")
		return true
	}
	return false
}

func editConcurrency(cfg *config.Config) bool {
	printSectionTitle("⚡", "Edit Concurrency Limits")

	var maxStr, perProvStr, perModelStr, queueStr string

	errM := huh.NewInput().
		Title(fmt.Sprintf("Max concurrent [%d] (Esc untuk batal)", cfg.Concurrency.MaxConcurrent)).
		Placeholder(strconv.Itoa(cfg.Concurrency.MaxConcurrent)).
		Value(&maxStr).
		Run()
	if isAbort(errM) {
		printInfo("Dibatalkan")
		return false
	}

	errP := huh.NewInput().
		Title(fmt.Sprintf("Per provider [%d] (Esc untuk batal)", cfg.Concurrency.PerProvider)).
		Placeholder(strconv.Itoa(cfg.Concurrency.PerProvider)).
		Value(&perProvStr).
		Run()
	if isAbort(errP) {
		printInfo("Dibatalkan")
		return false
	}

	errMod := huh.NewInput().
		Title(fmt.Sprintf("Per model [%d] (Esc untuk batal)", cfg.Concurrency.PerModel)).
		Placeholder(strconv.Itoa(cfg.Concurrency.PerModel)).
		Value(&perModelStr).
		Run()
	if isAbort(errMod) {
		printInfo("Dibatalkan")
		return false
	}

	errQ := huh.NewInput().
		Title(fmt.Sprintf("Queue depth [%d] (Esc untuk batal)", cfg.Concurrency.QueueDepth)).
		Placeholder(strconv.Itoa(cfg.Concurrency.QueueDepth)).
		Value(&queueStr).
		Run()
	if isAbort(errQ) {
		printInfo("Dibatalkan")
		return false
	}

	if maxStr != "" {
		if n, err := strconv.Atoi(maxStr); err == nil {
			cfg.Concurrency.MaxConcurrent = n
		}
	}
	if perProvStr != "" {
		if n, err := strconv.Atoi(perProvStr); err == nil {
			cfg.Concurrency.PerProvider = n
		}
	}
	if perModelStr != "" {
		if n, err := strconv.Atoi(perModelStr); err == nil {
			cfg.Concurrency.PerModel = n
		}
	}
	if queueStr != "" {
		if n, err := strconv.Atoi(queueStr); err == nil {
			cfg.Concurrency.QueueDepth = n
		}
	}

	printSuccess("Disimpan")
	return true
}

func editLogLevel(cfg *config.Config) bool {
	printSectionTitle("📝", "Edit Log Level")

	current := cfg.Server.LogLevel
	if current == "" {
		current = "info"
	}

	var level string
	huh.NewSelect[string]().
		Title("Pilih Log Level").
		Options(
			huh.NewOption("debug — Detail semua aktivitas", "debug"),
			huh.NewOption("info — Informasi umum (default)", "info"),
			huh.NewOption("warn — Peringatan saja", "warn"),
			huh.NewOption("error — Error saja", "error"),
			huh.NewOption("❌  Batal / kembali", "__cancel__"),
		).
		Value(&level).
		Run()

	if level == "" || level == "__cancel__" {
		printInfo("Dibatalkan")
		return false
	}

	if level != current {
		cfg.Server.LogLevel = level
		printSuccess(fmt.Sprintf("Log level diubah ke %s", lipgloss.NewStyle().Foreground(Primary).Render(level)))
		return true
	}
	return false
}

func boolEmoji(b bool) string {
	if b {
		return "✅ Yes"
	}
	return "❌ No"
}

func editTokenSaver(cfg *config.Config) bool {
	printSectionTitle("🧠", "Edit Token Saver (RTK) Settings")

	var enabled, caveman, compressUser, minifyJSON, stripComments bool
	var maxInputBytesStr string

	enabled = cfg.TokenSaver.Enabled
	caveman = cfg.TokenSaver.CavemanMode
	compressUser = cfg.TokenSaver.CompressUser
	minifyJSON = cfg.TokenSaver.MinifyJSON
	stripComments = cfg.TokenSaver.StripComments

	// 1. Ask master toggle
	errE := huh.NewConfirm().
		Title("Enable Token Saver? (Mengompres riwayat chat/instruksi panjang untuk menghemat biaya API)").
		Value(&enabled).
		Run()
	if isAbort(errE) {
		printInfo("Dibatalkan")
		return false
	}

	// If disabled, save immediately and skip other prompts!
	if !enabled {
		cfg.TokenSaver.Enabled = false
		printSuccess("Token Saver (RTK) dinonaktifkan")
		return true
	}

	// 2. Threshold Input bytes
	errM := huh.NewInput().
		Title(fmt.Sprintf("Batas Minimum Ukuran Pesan (Bytes) - Direkomendasikan: 4096 (Ctrl+C untuk batal)\n[Hanya pesan di atas batas ini yang akan dikompres. Nilai saat ini: %d]", cfg.TokenSaver.MaxInputBytes)).
		Placeholder(strconv.Itoa(cfg.TokenSaver.MaxInputBytes)).
		Value(&maxInputBytesStr).
		Run()
	if isAbort(errM) {
		printInfo("Dibatalkan")
		return false
	}

	// 3. Compress User messages
	errU := huh.NewConfirm().
		Title("Kompres juga pesan dari User? (Direkomendasikan: Yes untuk penghematan maksimal)").
		Value(&compressUser).
		Run()
	if isAbort(errU) {
		printInfo("Dibatalkan")
		return false
	}

	// 4. Minify JSON payloads
	errJ := huh.NewConfirm().
		Title("Minify JSON payloads? (Direkomendasikan: Yes untuk membuang spasi/enter kosong pada format data)").
		Value(&minifyJSON).
		Run()
	if isAbort(errJ) {
		printInfo("Dibatalkan")
		return false
	}

	// 5. Strip comments
	errS := huh.NewConfirm().
		Title("Hapus komentar (Comments) dari Code Blocks? (Direkomendasikan: No untuk menjaga dokumentasi kode)").
		Value(&stripComments).
		Run()
	if isAbort(errS) {
		printInfo("Dibatalkan")
		return false
	}

	// 6. Caveman Mode
	errC := huh.NewConfirm().
		Title("Aktifkan Caveman Mode? (Menyingkat respon AI secara ekstrem untuk menghemat token keluaran)\n[Direkomendasikan: No - Karena dapat mengurangi kualitas jawaban AI]").
		Value(&caveman).
		Run()
	if isAbort(errC) {
		printInfo("Dibatalkan")
		return false
	}

	cfg.TokenSaver.Enabled = enabled
	cfg.TokenSaver.CavemanMode = caveman
	cfg.TokenSaver.CompressUser = compressUser
	cfg.TokenSaver.MinifyJSON = minifyJSON
	cfg.TokenSaver.StripComments = stripComments

	if maxInputBytesStr != "" {
		if v, err := strconv.Atoi(maxInputBytesStr); err == nil && v > 0 {
			cfg.TokenSaver.MaxInputBytes = v
		}
	}

	printSuccess("Token Saver (RTK) settings berhasil diperbarui!")
	return true
}

func editQuickTunnel(cfg *config.Config) bool {
	printSectionTitle("🌐", "Edit Quick Tunnel Settings")

	var enabled bool
	err := huh.NewConfirm().
		Title("Aktifkan Cloudflare Quick Tunnel? (Membuat URL publik gratis untuk gateway Anda)").
		Value(&enabled).
		Run()

	if isAbort(err) {
		printInfo("Dibatalkan")
		return false
	}

	cfg.Server.QuickTunnel = enabled
	if enabled {
		printSuccess("Quick Tunnel diaktifkan (akan terkoneksi saat server aktif)")
	} else {
		printSuccess("Quick Tunnel dinonaktifkan")
	}
	return true
}

func editWarpSettings(cfg *config.Config) bool {
	printSectionTitle("🛡️", "Warp Proxy Settings (Khusus OpenCode)")

	// Find the opencode provider config
	var opencodeIdx = -1
	for i, p := range cfg.Providers {
		if p.Type == "opencode" {
			opencodeIdx = i
			break
		}
	}

	if opencodeIdx == -1 {
		printError("Provider OpenCode tidak ditemukan di config.yaml!")
		return false
	}

	opencode := &cfg.Providers[opencodeIdx]

	// 1. Show current status
	statusStr := "❌ Nonaktif"
	if opencode.ProxyURL != "" {
		statusStr = fmt.Sprintf("✅ Aktif (%s)", opencode.ProxyURL)
	}
	fmt.Printf("  Status Saat Ini: %s\n\n", statusStr)

	// 2. Select Option
	var action string
	huh.NewSelect[string]().
		Title("Pilih Aksi Warp").
		Options(
			huh.NewOption("🔌  Aktifkan / Ubah Proxy Warp", "enable"),
			huh.NewOption("❌  Nonaktifkan Proxy Warp", "disable"),
			huh.NewOption("🔄  Reset Koneksi Warp (Rotate IP)", "restart"),
			huh.NewOption("❌  Batal / kembali", "cancel"),
		).
		Value(&action).
		Run()

	if action == "" || action == "cancel" {
		printInfo("Dibatalkan")
		return false
	}

	switch action {
	case "enable":
		var proxyAddr string
		defaultAddr := "socks5://127.0.0.1:40000"
		if opencode.ProxyURL != "" {
			defaultAddr = opencode.ProxyURL
		}
		err := huh.NewInput().
			Title("Masukkan Alamat Proxy Warp (SOCKS5/HTTP)").
			Placeholder(defaultAddr).
			Value(&proxyAddr).
			Run()
		if isAbort(err) {
			printInfo("Dibatalkan")
			return false
		}
		if proxyAddr == "" {
			proxyAddr = defaultAddr
		}
		opencode.ProxyURL = proxyAddr
		printSuccess(fmt.Sprintf("Warp Proxy untuk OpenCode diaktifkan ke: %s", proxyAddr))
		return true

	case "disable":
		opencode.ProxyURL = ""
		printSuccess("Warp Proxy untuk OpenCode dinonaktifkan")
		return true

	case "restart":
		printInfo("Mereset koneksi Cloudflare Warp (Memutar IP)...")
		// Execute warp-cli disconnect
		cmdDisc := exec.Command("warp-cli", "disconnect")
		_ = cmdDisc.Run() // ignore error, might not be connected

		time.Sleep(1 * time.Second)

		// Execute warp-cli connect
		cmdConn := exec.Command("warp-cli", "connect")
		output, err := cmdConn.CombinedOutput()
		if err != nil {
			printError(fmt.Sprintf("Gagal menjalankan warp-cli: %v. Pastikan Cloudflare Warp terpasang di sistem.", err))
			if len(output) > 0 {
				fmt.Printf("Output: %s\n", string(output))
			}
		} else {
			printSuccess("Koneksi Cloudflare Warp berhasil di-reset!")
			if len(output) > 0 {
				fmt.Printf("Output: %s\n", string(output))
			}
		}
		return false // no config change needed, just executed system command
	}

	return false
}

func viewLogs() {
	logPath := "aigateway.log"
	for {
		clearScreen()
		printSectionTitle("📋", "Lihat Log Server (aigateway.log)")

		lines, err := readLastLines(logPath, 40)
		if err != nil {
			printError(fmt.Sprintf("Gagal membaca file log: %v", err))
		} else {
			for _, line := range lines {
				fmt.Println(line)
			}
		}

		fmt.Println("\n" + lipgloss.NewStyle().Foreground(Secondary).Render("────────────────────────────────────────────────────────────"))

		var choice string
		huh.NewSelect[string]().
			Title("Aksi Log").
			Options(
				huh.NewOption("🔄  Refresh / Muat Ulang Log", "refresh"),
				huh.NewOption("←  Kembali ke menu", "back"),
			).
			Value(&choice).
			Run()

		if choice == "back" || choice == "" {
			break
		}
	}
}

func readLastLines(path string, maxLines int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if len(lines) <= maxLines {
		return lines, nil
	}
	return lines[len(lines)-maxLines:], nil
}

func editDashboardToggle(cfg *config.Config) bool {
	printSectionTitle("📊", "Web Dashboard & Playground Settings")

	enabled := cfg.Server.IsDashboardEnabled()

	err := huh.NewConfirm().
		Title("Aktifkan Web Dashboard & Playground? (Memungkinkan konfigurasi visual via browser)").
		Value(&enabled).
		Run()

	if isAbort(err) {
		printInfo("Dibatalkan")
		return false
	}

	cfg.Server.DashboardEnabled = &enabled
	if enabled {
		printSuccess("Web Dashboard & Playground diaktifkan")
	} else {
		printSuccess("Web Dashboard & Playground dinonaktifkan (Keamanan Tinggi)")
	}
	return true
}

