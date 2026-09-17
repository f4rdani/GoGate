package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/aigateway/config"
	"github.com/aigateway/provider"
)

// KiroLoginResult is a completed Kiro login (any method).
type KiroLoginResult struct {
	APIKey       string
	RefreshToken string
	ProfileARN   string
	ClientID     string
	ClientSecret string
	Region       string
	AuthMethod   string // builder-id | google | github | import | api_key
}

// KiroLogin runs the interactive Kiro free-tier login and saves it to cfgPath.
func KiroLogin(cfgPath string) {
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Printf("Failed to load configuration: %v\n", err)
		return
	}
	clearScreen()
	printSectionTitle("🔑", "Kiro AI Login (Free Tier)")

	var method string
	errChoice := huh.NewSelect[string]().
		Title(T("Pilih metode login Kiro", "Choose Kiro login method")).
		Options(
			huh.NewOption("🖥️  AWS Builder ID (device code, buka browser)", "builder-id"),
			huh.NewOption("🔵  Google (login browser + paste callback)", "google"),
			huh.NewOption("🐙  GitHub (login browser + paste callback)", "github"),
			huh.NewOption("📋  Paste refresh token (diawali aorAAAAAG)", "import"),
			huh.NewOption("🔑  Paste API key", "api_key"),
			huh.NewOption("❌  Batal / kembali", "back"),
		).
		Value(&method).
		Run()
	if isAbort(errChoice) || method == "back" || method == "" {
		printInfo(T("Dibatalkan", "Cancelled"))
		return
	}

	var res KiroLoginResult
	var loginErr error
	switch method {
	case "builder-id":
		res, loginErr = kiroLoginDevice(cfg, "us-east-1", "https://view.awsapps.com/start", method)
	case "google", "github":
		res, loginErr = kiroLoginSocial(method)
	case "import":
		res, loginErr = kiroLoginImport()
	case "api_key":
		res, loginErr = kiroLoginAPIKey()
	}
	if loginErr != nil {
		printError(fmt.Sprintf(T("Login Kiro gagal: %v", "Kiro login failed: %v"), loginErr))
		pause()
		return
	}

	name := kiroProviderDisplayName(cfg); upsertKiroProvider(cfg, name, res)
	if err := cfg.SaveConfig(cfgPath); err != nil {
		printError(fmt.Sprintf(T("Gagal menyimpan config: %v", "Failed to save config: %v"), err))
		pause()
		return
	}
	printSuccess(fmt.Sprintf(T("Provider '%s' tersimpan!", "Provider '%s' saved!"), name))

	// Verify live: fetch catalog + quota.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cred := provider.KiroCredentials{APIKey: res.APIKey, RefreshToken: res.RefreshToken, ProfileARN: res.ProfileARN}
	models, err := provider.KiroListModels(ctx, nil, cred)
	if err != nil {
		printWarning(fmt.Sprintf(T("Katalog model gagal diambil: %v", "Model catalog fetch failed: %v"), err))
	} else {
		printSuccess(fmt.Sprintf(T("Ditemukan %d model Kiro", "Found %d Kiro models"), len(models)))
		// Register model routes for everything found.
		added := 0
		for _, m := range models {
			if cfg.GetModel(m) == nil {
				if cfg.AddModel(config.ModelConfig{Name: m, Provider: name, Model: m}) == nil {
					added++
				}
			}
		}
		if added > 0 {
			if err := cfg.SaveConfig(cfgPath); err == nil {
				printSuccess(fmt.Sprintf(T("%d route model ditambahkan", "%d model routes added"), added))
			}
		}
	}
	if usage, err := provider.FetchKiroUsage(ctx, nil, cred); err == nil {
		printKiroQuota(usage)
	} else {
		printInfo(fmt.Sprintf(T("Kuota tidak terbaca: %v", "Quota unreadable: %v"), err))
	}
	pause()
}

// upsertKiroProvider creates or updates the kiro provider entry. Pure config
// mutation (no network) — also used by tests.
func upsertKiroProvider(cfg *config.Config, name string, res KiroLoginResult) {
	if name == "" {
		name = "kiro"
	}
	existing := cfg.GetProvider(name)
	entry := config.ProviderConfig{
		Name:         name,
		Type:         "kiro",
		BaseURL:      "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		RefreshToken: res.RefreshToken,
		ProfileARN:   res.ProfileARN,
		ClientID:     res.ClientID,
		ClientSecret: res.ClientSecret,
		Region:       res.Region,
	}
	if res.APIKey != "" {
		entry.APIKeys = []string{res.APIKey}
	}
	if existing != nil {
		// Merge: keep existing keys unless replaced.
		if res.APIKey != "" {
			existing.APIKeys = []string{res.APIKey}
		}
		if res.RefreshToken != "" {
			existing.RefreshToken = res.RefreshToken
		}
		if res.ProfileARN != "" {
			existing.ProfileARN = res.ProfileARN
		}
		if res.ClientID != "" {
			existing.ClientID = res.ClientID
		}
		if res.ClientSecret != "" {
			existing.ClientSecret = res.ClientSecret
		}
		if res.Region != "" {
			existing.Region = res.Region
		}
		if existing.BaseURL == "" {
			existing.BaseURL = entry.BaseURL
		}
		existing.Type = "kiro"
		existing.Disabled = false
		return
	}
	_ = cfg.AddProvider(entry)
}

func kiroProviderDisplayName(cfg *config.Config) string {
	if cfg.GetProvider("kiro") == nil {
		return "kiro"
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("kiro-%d", i)
		if cfg.GetProvider(name) == nil {
			return name
		}
	}
}

func kiroLoginDevice(cfg *config.Config, region, startURL, authMethod string) (KiroLoginResult, error) {
	var out KiroLoginResult
	ctx := context.Background()

	reg, err := provider.KiroRegisterClient(ctx, nil, region)
	if err != nil {
		return out, err
	}
	dev, err := provider.KiroStartDeviceAuth(ctx, nil, region, reg.ClientID, reg.ClientSecret, startURL)
	if err != nil {
		return out, err
	}

	fmt.Println()
	printInfo(T("Buka URL ini di browser dan masukkan kode:", "Open this URL in your browser and enter the code:"))
	fmt.Printf("  %s\n", KeyStyle.Render(dev.VerificationURIComplete))
	fmt.Printf("  %s %s\n", T("Kode:", "Code:"), KeyStyle.Render(dev.UserCode))
	fmt.Println()

	deadline := time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	if time.Until(deadline) > 10*time.Minute {
		deadline = time.Now().Add(10 * time.Minute)
	}
	interval := time.Duration(dev.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		if time.Now().After(deadline) {
			return out, errors.New(T("waktu habis — silakan ulangi", "timed out — please retry"))
		}
		pollCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		res, err := provider.KiroPollDeviceToken(pollCtx, nil, region, reg.ClientID, reg.ClientSecret, dev.DeviceCode)
		cancel()
		if err != nil {
			return out, err
		}
		if res.Done {
			out = KiroLoginResult{
				RefreshToken: res.Tokens.RefreshToken,
				ClientID:     reg.ClientID,
				ClientSecret: reg.ClientSecret,
				Region:       region,
				AuthMethod:   authMethod,
			}
			printSuccess(T("Login disetujui!", "Login approved!"))
			return out, nil
		}
		if !res.Pending {
			return out, errors.New(T("aliran login dibatalkan", "login flow cancelled"))
		}
		time.Sleep(interval)
	}
}

func kiroPKCE() (verifier, challenge, state string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	st := make([]byte, 16)
	if _, err = rand.Read(st); err != nil {
		return "", "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(st)
	return verifier, challenge, state, nil
}

func kiroLoginSocial(idp string) (KiroLoginResult, error) {
	var out KiroLoginResult
	verifier, challenge, state, err := kiroPKCE()
	if err != nil {
		return out, err
	}
	loginURL, err := provider.KiroSocialLoginURL(idp, challenge, state)
	if err != nil {
		return out, err
	}
	fmt.Println()
	printInfo(T("Buka URL ini, login, lalu paste kembali URL callback-nya:", "Open this URL, log in, then paste the callback URL back:"))
	fmt.Printf("  %s\n", KeyStyle.Render(loginURL))
	fmt.Println()

	var callback string
	if err := huh.NewInput().
		Title(T("Paste callback URL (diawali kiro://...)", "Paste callback URL (starts with kiro://...)")).
		Value(&callback).
		Run(); err != nil || isAbort(err) {
		return out, errors.New(T("dibatalkan", "cancelled"))
	}
	code, err := kiroExtractCode(strings.TrimSpace(callback), state)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tokens, err := provider.KiroExchangeSocialCode(ctx, nil, code, verifier)
	if err != nil {
		return out, err
	}
	out = KiroLoginResult{
		RefreshToken: tokens.RefreshToken,
		ProfileARN:   tokens.ProfileARN,
		AuthMethod:   idp,
	}
	printSuccess(T("Login disetujui!", "Login approved!"))
	return out, nil
}

// kiroExtractCode pulls the authorization code from a pasted callback URL
// (or accepts a bare code), verifying state when present.
func kiroExtractCode(callback, wantState string) (string, error) {
	callback = strings.TrimSpace(callback)
	if callback == "" {
		return "", errors.New(T("callback kosong", "empty callback"))
	}
	if !strings.Contains(callback, "://") && !strings.Contains(callback, "code=") {
		return callback, nil // bare code pasted
	}
	u, err := url.Parse(callback)
	if err != nil {
		return "", fmt.Errorf(T("callback tidak valid: %v", "invalid callback: %v"), err)
	}
	q := u.Query()
	if st := q.Get("state"); wantState != "" && st != "" && st != wantState {
		return "", errors.New(T("state tidak cocok — kemungkinan salah tempel", "state mismatch — possible paste error"))
	}
	if code := q.Get("code"); code != "" {
		return code, nil
	}
	return "", errors.New(T("tidak ada code di callback", "no code in callback"))
}

func kiroLoginImport() (KiroLoginResult, error) {
	var out KiroLoginResult
	var token string
	if err := huh.NewInput().
		Title(T("Paste refresh token (aorAAAAAG...)", "Paste refresh token (aorAAAAAG...)")).
		EchoMode(huh.EchoModePassword).
		Value(&token).
		Run(); err != nil || isAbort(err) {
		return out, errors.New(T("dibatalkan", "cancelled"))
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "aorAAAAAG") {
		return out, errors.New(T("format token salah — harus diawali aorAAAAAG", "bad token format — must start with aorAAAAAG"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := provider.KiroListModels(ctx, nil, provider.KiroCredentials{RefreshToken: token}); err != nil {
		// Validate via refresh instead (catalog may need a profile).
		src, serr := provider.NewKiroTokenSource(token, "", nil)
		if serr != nil {
			return out, serr
		}
		if _, serr := src.Token(ctx); serr != nil {
			return out, fmt.Errorf(T("token tidak valid: %v", "invalid token: %v"), err)
		}
	}
	out = KiroLoginResult{RefreshToken: token, AuthMethod: "import"}
	printSuccess(T("Token valid!", "Token valid!"))
	return out, nil
}

func kiroLoginAPIKey() (KiroLoginResult, error) {
	var out KiroLoginResult
	var key string
	if err := huh.NewInput().
		Title("Kiro API Key").
		EchoMode(huh.EchoModePassword).
		Value(&key).
		Run(); err != nil || isAbort(err) {
		return out, errors.New(T("dibatalkan", "cancelled"))
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return out, fmt.Errorf("API key tidak boleh kosong")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := provider.KiroListModels(ctx, nil, provider.KiroCredentials{APIKey: key})
	if err != nil {
		return out, fmt.Errorf(T("API key tidak valid: %v", "invalid API key: %v"), err)
	}
	out = KiroLoginResult{APIKey: key, AuthMethod: "api_key"}
	printSuccess(fmt.Sprintf(T("API key valid! (%d model)", "API key valid! (%d models)"), len(models)))
	return out, nil
}

func printKiroQuota(usage *provider.KiroUsage) {
	fmt.Printf("  %s %s\n", KeyStyle.Render(T("Paket:", "Plan:")), usage.Plan)
	for name, q := range usage.Quotas {
		fmt.Printf("  %s %s: %.0f / %.0f (sisa %.0f, reset %s)\n",
			KeyStyle.Render("Kuota"), name, q.Used, q.Total, q.Remaining, q.ResetAt)
	}
}
