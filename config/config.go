package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	Server      ServerConfig      `yaml:"server" json:"server"`
	Concurrency ConcurrencyConfig `yaml:"concurrency" json:"concurrency"`
	Cache       CacheConfig       `yaml:"cache" json:"cache"`
	Retry       RetryConfig       `yaml:"retry" json:"retry"`
	TokenSaver  TokenSaverConfig  `yaml:"token_saver" json:"token_saver"`
	Privacy     PrivacyConfig     `yaml:"privacy,omitempty" json:"privacy,omitempty"`
	ProxyPool   ProxyPoolConfig        `yaml:"proxy_pool,omitempty" json:"proxy_pool,omitempty"`
	CFRelays    []CFRelay              `yaml:"cf_relays,omitempty" json:"cf_relays,omitempty"`
	Providers   []ProviderConfig       `yaml:"providers" json:"providers"`
	Models      []ModelConfig          `yaml:"models" json:"models"`
	APIKeys     []APIKeyConfig         `yaml:"api_keys" json:"api_keys"`
	Prices      map[string]ModelPrice  `yaml:"prices,omitempty" json:"prices,omitempty"`
	Budgets     []BudgetConfig         `yaml:"budgets,omitempty" json:"budgets,omitempty"`
}

// CFRelay represents a Cloudflare reverse proxy relay or AI Gateway configuration.
type CFRelay struct {
	Name      string `yaml:"name" json:"name"`                         // Nama relay (e.g. "cf-main", "my-gateway")
	AccountID string `yaml:"account_id" json:"account_id"`             // Cloudflare Account ID (id)
	Token     string `yaml:"token" json:"token"`                       // Cloudflare API Token / Secret
	Type      string `yaml:"type,omitempty" json:"type,omitempty"`     // "ai-gateway" (default), "tunnel", "worker"
	BaseURL   string `yaml:"base_url,omitempty" json:"base_url,omitempty"` // Computed or custom base URL
}

// GetEffectiveBaseURL returns the computed base URL for routing upstream AI requests.
func (r *CFRelay) GetEffectiveBaseURL() string {
	if r.BaseURL != "" {
		return strings.TrimRight(r.BaseURL, "/")
	}
	if r.AccountID != "" && r.Name != "" {
		return fmt.Sprintf("https://gateway.ai.cloudflare.com/v1/%s/%s", r.AccountID, r.Name)
	}
	return ""
}

// BudgetConfig caps monthly token spend per upstream provider. When a
// provider exceeds its budget the router fails over to other backends;
// with no backend left the request fails with 429 (budget exceeded).
type BudgetConfig struct {
	Provider      string `yaml:"provider" json:"provider"`
	MonthlyTokens int64  `yaml:"monthly_tokens" json:"monthly_tokens"`
}

// WebshareAccountConfig defines a managed Webshare account with proxies and monthly bandwidth tracking.
type WebshareAccountConfig struct {
	Name           string   `yaml:"name" json:"name"`
	IsFreeTier     bool     `yaml:"is_free_tier" json:"is_free_tier"`
	BandwidthLimit int64    `yaml:"bandwidth_limit" json:"bandwidth_limit"` // bytes per month (default: 1GB = 1,000,000,000)
	BandwidthUsed  int64    `yaml:"bandwidth_used" json:"bandwidth_used"`   // bytes used in current month
	CurrentMonth   string   `yaml:"current_month,omitempty" json:"current_month,omitempty"` // e.g. "2026-09"
	Proxies        []string `yaml:"proxies" json:"proxies"`                 // list of proxy URLs (normalized)
	Status         string   `yaml:"status,omitempty" json:"status,omitempty"` // "active", "exhausted", "disabled"
	CreatedAt      string   `yaml:"created_at,omitempty" json:"created_at,omitempty"`
	UpdatedAt      string   `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`
}

// ProxyPoolConfig holds settings for the free public proxy pool rotator.
type ProxyPoolConfig struct {
	Enabled          bool                    `yaml:"enabled" json:"enabled"`
	Sources          []string                `yaml:"sources,omitempty" json:"sources,omitempty"`
	ManualProxies    []string                `yaml:"manual_proxies,omitempty" json:"manual_proxies,omitempty"`
	WebshareAccounts []WebshareAccountConfig `yaml:"webshare_accounts,omitempty" json:"webshare_accounts,omitempty"`
	CheckInterval    time.Duration           `yaml:"check_interval,omitempty" json:"check_interval,omitempty"`
	CheckTimeout     time.Duration           `yaml:"check_timeout,omitempty" json:"check_timeout,omitempty"`
	TestURL          string                  `yaml:"test_url,omitempty" json:"test_url,omitempty"`
	MaxProxies       int                     `yaml:"max_proxies,omitempty" json:"max_proxies,omitempty"`
}

// TokenSaverConfig holds settings for the RTK-style input token compression.
type TokenSaverConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`                // master toggle (default: true)
	MaxInputBytes int    `yaml:"max_input_bytes" json:"max_input_bytes"` // per-message threshold in bytes (default: 4096)
	CavemanMode   bool   `yaml:"caveman_mode" json:"caveman_mode"`       // inject terse output prompt to save output tokens
	Ponytail      string `yaml:"ponytail,omitempty" json:"ponytail,omitempty"` // lazy-senior-dev output style: "", "lite", "full", "ultra" (ignored when caveman_mode is on)
	CompressUser  bool   `yaml:"compress_user" json:"compress_user"`     // also compress user messages (default: true)
	MinifyJSON    bool   `yaml:"minify_json" json:"minify_json"`         // minify JSON blobs in messages (default: true)
	StripComments bool   `yaml:"strip_comments" json:"strip_comments"`   // strip code comments (default: false)
}

// PrivacyConfig holds settings for DLP / PII and sensitive secret masking.
type PrivacyConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`                   // master toggle (default: false)
	Mode        string `yaml:"mode,omitempty" json:"mode,omitempty"`     // "vault" (reversible synthetic dummy) or "redact" or "block" (default: "vault")
	Scope       string `yaml:"scope,omitempty" json:"scope,omitempty"`   // "free-only" (default: only Tier 3/free) or "all"
	MaskSecrets bool   `yaml:"mask_secrets" json:"mask_secrets"`         // mask API keys, private keys, passwords (default: true)
	MaskPII     bool   `yaml:"mask_pii" json:"mask_pii"`                 // mask email, phone, IP, credit cards (default: true)
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host             string `yaml:"host" json:"host"`
	Port             int    `yaml:"port" json:"port"`
	AdminSecret      string `yaml:"admin_secret" json:"admin_secret"`
	LogLevel         string `yaml:"log_level,omitempty" json:"log_level,omitempty"` // "info", "debug", "warn", "error"
	QuickTunnel      bool   `yaml:"quick_tunnel,omitempty" json:"quick_tunnel,omitempty"`
	DashboardEnabled *bool  `yaml:"dashboard_enabled,omitempty" json:"dashboard_enabled,omitempty"` // enable or disable admin web dashboard (default: true)
	Language         string `yaml:"language,omitempty" json:"language,omitempty"`                   // "id" or "en"
	UsageFile        string `yaml:"usage_file,omitempty" json:"usage_file,omitempty"`               // persist usage stats here (default: "usage.json", "-" = disabled)
	LogBodies        bool   `yaml:"log_bodies,omitempty" json:"log_bodies,omitempty"`               // debug: log truncated response bodies
}

// ConcurrencyConfig holds concurrency limit settings.
type ConcurrencyConfig struct {
	MaxConcurrent int           `yaml:"max_concurrent" json:"max_concurrent"`
	PerProvider   int           `yaml:"per_provider" json:"per_provider"`
	PerModel      int           `yaml:"per_model" json:"per_model"`
	QueueDepth    int           `yaml:"queue_depth" json:"queue_depth"`    // max queued requests (0 = no queue)
	QueueTimeout  time.Duration `yaml:"queue_timeout" json:"queue_timeout"`  // max wait in queue (e.g. 30s)
}

// ProviderConfig defines an upstream AI provider.
type ProviderConfig struct {
	Name                string        `yaml:"name" json:"name"`
	Type                string        `yaml:"type" json:"type"` // "openai", "anthropic", "groq", "mistral", "custom", "cloudflare"
	BaseURL             string        `yaml:"base_url" json:"base_url"`
	APIKeys             []string      `yaml:"api_keys" json:"api_keys"`
	DisabledKeys        []string      `yaml:"disabled_keys,omitempty" json:"disabled_keys,omitempty"`
	AccountID           string        `yaml:"account_id,omitempty" json:"account_id,omitempty"` // for Cloudflare compatibility
	Models              []string      `yaml:"models" json:"models"`
	Tier                int           `yaml:"tier,omitempty" json:"tier,omitempty"`                         // 1=subscription, 2=cheap, 3=free (default=1)
	HealthCheckURL      string        `yaml:"health_check_url,omitempty" json:"health_check_url,omitempty"` // custom health check endpoint
	HealthCheckInterval time.Duration `yaml:"health_check_interval,omitempty" json:"health_check_interval,omitempty"` // e.g. 30s, 0=disabled
	ProxyURL            string        `yaml:"proxy_url,omitempty" json:"proxy_url,omitempty"`               // proxy for outbound HTTP requests (e.g. socks5://127.0.0.1:4000)
	KeyRotation         string        `yaml:"key_rotation,omitempty" json:"key_rotation,omitempty"`       // "round-robin" (default: rotate keys per request) or "sticky" (primary key first, failover on error)
	RelayURL            string        `yaml:"relay_url,omitempty" json:"relay_url,omitempty"`               // Cloudflare Worker or reverse proxy relay URL (e.g. https://my-worker.workers.dev)
	RelaySecret         string        `yaml:"relay_secret,omitempty" json:"relay_secret,omitempty"`         // optional secret passed in X-Relay-Secret header
	TokenURL            string        `yaml:"token_url,omitempty" json:"token_url,omitempty"`               // OAuth2 token endpoint (type oauth)
	ClientID            string        `yaml:"client_id,omitempty" json:"client_id,omitempty"`               // OAuth2 client id (type oauth)
	ClientSecret        string        `yaml:"client_secret,omitempty" json:"client_secret,omitempty"`       // OAuth2 client secret, optional (type oauth)
	RefreshToken        string        `yaml:"refresh_token,omitempty" json:"refresh_token,omitempty"`       // OAuth2 refresh token (type oauth) / Kiro refresh token (type kiro)
	ProfileARN          string        `yaml:"profile_arn,omitempty" json:"profile_arn,omitempty"`           // Kiro CodeWhisperer profile ARN (type kiro, optional)
	Region              string        `yaml:"region,omitempty" json:"region,omitempty"`                       // AWS region for OIDC/Kiro endpoints (default us-east-1)
	Disabled            bool          `yaml:"disabled,omitempty" json:"disabled,omitempty"`                 // true if provider is disabled / turned off
}

// KeyRotationMode returns the normalized API-key rotation strategy:
// "sticky" (always the first healthy key, fail over on error) or
// "round-robin" (default: rotate keys on every request).
func (p *ProviderConfig) KeyRotationMode() string {
	if strings.EqualFold(strings.TrimSpace(p.KeyRotation), "sticky") {
		return "sticky"
	}
	return "round-robin"
}

// HasCredentials reports whether the provider can authenticate requests:
// static keys, an OAuth/Kiro refresh token, or a keyless type.
func (p *ProviderConfig) HasCredentials() bool {
	if len(p.APIKeys) > 0 || p.RefreshToken != "" {
		return true
	}
	return p.Type == "opencode" || p.Type == "mimo"
}

// ModelConfig defines a model route (direct or combo).
type ModelConfig struct {
	Name      string          `yaml:"name" json:"name"`
	Provider  string          `yaml:"provider,omitempty" json:"provider,omitempty"`  // for direct models
	Model     string          `yaml:"model,omitempty" json:"model,omitempty"`     // for direct models
	Strategy  string          `yaml:"strategy,omitempty" json:"strategy,omitempty"`  // "round-robin" or "fallback"
	Backends  []BackendConfig `yaml:"backends,omitempty" json:"backends,omitempty"`  // for combo models
	Reasoning bool            `yaml:"reasoning,omitempty" json:"reasoning,omitempty"` // indicates if model supports reasoning (thinking process)
	Vision    bool            `yaml:"vision,omitempty" json:"vision,omitempty"`       // indicates if model supports vision (multimodal image input)
	Disabled  bool            `yaml:"disabled,omitempty" json:"disabled,omitempty"` // indicates if model route is disabled
}

// BackendConfig defines a backend in a combo model.
type BackendConfig struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	Tier     int    `yaml:"tier,omitempty" json:"tier,omitempty"` // 1=subscription, 2=cheap, 3=free (default=1)
}

// CacheConfig holds response cache settings.
type CacheConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	MaxSize int  `yaml:"max_size" json:"max_size"`  // max cached entries (default 1000)
	TTL     int  `yaml:"ttl" json:"ttl"`       // TTL in seconds (default 300)
}

// RetryConfig holds retry with backoff settings.
type RetryConfig struct {
	MaxRetries     int `yaml:"max_retries" json:"max_retries"`      // max retry attempts (default 2)
	InitialBackoff int `yaml:"initial_backoff" json:"initial_backoff"`   // initial backoff in ms (default 500)
	MaxBackoff     int `yaml:"max_backoff" json:"max_backoff"`       // max backoff in ms (default 10000)
}

// ModelPrice defines per-1M-token prices in USD for cost estimation.
// Prices are user-configured (providers change them often); when a model has
// no entry, its cost is simply reported as zero.
type ModelPrice struct {
	InputPer1M  float64 `yaml:"input_per_1m" json:"input_per_1m"`
	OutputPer1M float64 `yaml:"output_per_1m" json:"output_per_1m"`
}

// APIKeyConfig defines a user-facing API key with permissions.
type APIKeyConfig struct {
	Key           string   `yaml:"key" json:"key"`
	Name          string   `yaml:"name" json:"name"`
	AllowedModels []string `yaml:"allowed_models" json:"allowed_models"`
	RateLimit     int      `yaml:"rate_limit" json:"rate_limit"`                       // requests per minute, 0 = unlimited
	TokenSaver    *bool    `yaml:"token_saver,omitempty" json:"token_saver,omitempty"` // per-key toggle (nil = follow global, true/false = override)
	Privacy       *bool    `yaml:"privacy,omitempty" json:"privacy,omitempty"`         // per-key privacy toggle (nil = follow global, true/false = override)
	Disabled      bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

var envVarRegex = regexp.MustCompile(`\$\{([^}]+)\}`)

// expandEnv replaces ${VAR_NAME} patterns with environment variable values.
func expandEnv(s string) string {
	return envVarRegex.ReplaceAllStringFunc(s, func(match string) string {
		key := envVarRegex.FindStringSubmatch(match)[1]
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return match
	})
}

// LoadConfig reads and parses the YAML configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	// Expand environment variables in config
	expanded := expandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.LogLevel == "" {
		cfg.Server.LogLevel = "info"
	}
	if cfg.Server.UsageFile == "" {
		cfg.Server.UsageFile = "usage.json"
	}
	if cfg.Concurrency.MaxConcurrent == 0 {
		cfg.Concurrency.MaxConcurrent = 50
	}
	if cfg.Concurrency.PerProvider == 0 {
		cfg.Concurrency.PerProvider = 15
	}
	if cfg.Concurrency.PerModel == 0 {
		cfg.Concurrency.PerModel = 8
	}
	if cfg.Concurrency.QueueDepth == 0 {
		cfg.Concurrency.QueueDepth = 30
	}
	if cfg.Concurrency.QueueTimeout == 0 {
		cfg.Concurrency.QueueTimeout = 30 * time.Second
	}
	// Cache defaults
	if cfg.Cache.MaxSize == 0 {
		cfg.Cache.MaxSize = 200
	}
	if cfg.Cache.TTL == 0 {
		cfg.Cache.TTL = 300
	}
	// Retry defaults
	if cfg.Retry.MaxRetries == 0 {
		cfg.Retry.MaxRetries = 2
	}
	if cfg.Retry.InitialBackoff == 0 {
		cfg.Retry.InitialBackoff = 500
	}
	if cfg.Retry.MaxBackoff == 0 {
		cfg.Retry.MaxBackoff = 10000
	}
	// Token saver defaults
	// Use a sentinel: if MaxInputBytes is 0, it means the section was not set at all → apply defaults
	if cfg.TokenSaver.MaxInputBytes == 0 {
		cfg.TokenSaver.Enabled = true
		cfg.TokenSaver.MaxInputBytes = 4096
		cfg.TokenSaver.CompressUser = true
		cfg.TokenSaver.MinifyJSON = true
	}

	// Privacy defaults
	if cfg.Privacy.Mode == "" {
		cfg.Privacy.Mode = "vault"
	}
	if cfg.Privacy.Scope == "" {
		cfg.Privacy.Scope = "free-only"
	}

	// Apply provider tier defaults
	for i := range cfg.Providers {
		if cfg.Providers[i].Tier == 0 {
			cfg.Providers[i].Tier = 1
		}
	}

	// Proxy pool defaults
	if cfg.ProxyPool.CheckInterval == 0 {
		cfg.ProxyPool.CheckInterval = 10 * time.Minute
	}
	if cfg.ProxyPool.CheckTimeout == 0 {
		cfg.ProxyPool.CheckTimeout = 5 * time.Second
	}
	if cfg.ProxyPool.TestURL == "" {
		cfg.ProxyPool.TestURL = "https://www.google.com"
	}
	if cfg.ProxyPool.MaxProxies == 0 {
		cfg.ProxyPool.MaxProxies = 50
	}
	if len(cfg.ProxyPool.Sources) == 0 && cfg.ProxyPool.Enabled {
		cfg.ProxyPool.Sources = []string{
			"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt",
			"https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/http.txt",
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return &cfg, nil
}

// Validate checks the configuration for correctness.
func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}

	// Build map of active models per provider from the routes (c.Models)
	activeModels := make(map[string]map[string]bool)
	for _, p := range c.Providers {
		activeModels[p.Name] = make(map[string]bool)
	}

	for _, m := range c.Models {
		if m.Strategy == "" {
			// Direct model
			if m.Provider != "" && activeModels[m.Provider] != nil {
				activeModels[m.Provider][m.Model] = true
			}
		} else {
			// Combo model backends
			for _, b := range m.Backends {
				if b.Provider != "" && activeModels[b.Provider] != nil {
					activeModels[b.Provider][b.Model] = true
				}
			}
		}
	}

	providerNames := make(map[string]bool)
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "" {
			return fmt.Errorf("provider missing name")
		}
		if providerNames[p.Name] {
			return fmt.Errorf("duplicate provider name: %s", p.Name)
		}
		providerNames[p.Name] = true

		if p.Type == "" {
			return fmt.Errorf("provider %s: missing type", p.Name)
		}
		validTypes := map[string]bool{
			"openai": true, "anthropic": true, "groq": true,
			"mistral": true, "custom": true, "cohere": true,
			"opencode": true, "cerebras": true, "cloudflare": true,
			"oauth": true, "kiro": true, "mimo": true,
		}
		if !validTypes[p.Type] {
			return fmt.Errorf("provider %s: invalid type %q (valid: openai, cohere, opencode, cerebras, anthropic, groq, mistral, custom, cloudflare, oauth, kiro, mimo)", p.Name, p.Type)
		}

		if kr := strings.TrimSpace(p.KeyRotation); kr != "" && !strings.EqualFold(kr, "round-robin") && !strings.EqualFold(kr, "sticky") {
			return fmt.Errorf("provider %s: invalid key_rotation %q (valid: round-robin, sticky)", p.Name, p.KeyRotation)
		}

		if p.Type == "kiro" {
			if len(p.APIKeys) == 0 && p.RefreshToken == "" {
				return fmt.Errorf("provider %s: kiro type requires api_keys or refresh_token", p.Name)
			}
		}

		if p.Type == "oauth" {
			if p.TokenURL == "" {
				return fmt.Errorf("provider %s: oauth type requires token_url", p.Name)
			}
			if p.RefreshToken == "" {
				return fmt.Errorf("provider %s: oauth type requires refresh_token", p.Name)
			}
		}

		if p.Type == "cloudflare" {
			if p.AccountID == "" {
				return fmt.Errorf("provider %s: cloudflare type requires account_id", p.Name)
			}
			if p.BaseURL == "" {
				p.BaseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", p.AccountID)
			}
		}

		if p.BaseURL == "" {
			return fmt.Errorf("provider %s: missing base_url", p.Name)
		}

		// Deduplicate and sync provider models: only keep models that are actually used in active routes (c.Models)
		if len(p.Models) > 0 {
			seenModels := make(map[string]bool)
			var uniqModels []string
			for _, m := range p.Models {
				if activeModels[p.Name][m] {
					if !seenModels[m] {
						seenModels[m] = true
						uniqModels = append(uniqModels, m)
					}
				}
			}
			p.Models = uniqModels
		}
	}

	// Collect all valid model names for AllowedModels cross-validation
	allModelNames := make(map[string]bool)

	for _, m := range c.Models {
		if m.Name == "" {
			return fmt.Errorf("model entry missing name")
		}
		if allModelNames[m.Name] {
			return fmt.Errorf("duplicate model name: %s", m.Name)
		}
		allModelNames[m.Name] = true
		if m.Strategy != "" {
			// Combo model
			if m.Strategy != "round-robin" && m.Strategy != "fallback" && m.Strategy != "tiered" {
				return fmt.Errorf("model %s: invalid strategy %q (valid: round-robin, fallback, tiered)", m.Name, m.Strategy)
			}
			if len(m.Backends) == 0 {
				return fmt.Errorf("model %s: strategy set but no backends defined", m.Name)
			}
			if len(m.Backends) < 2 {
				return fmt.Errorf("model %s: combo needs at least 2 backends, got %d (hapus model ini atau tambahkan backend)", m.Name, len(m.Backends))
			}
			for _, b := range m.Backends {
				if !providerNames[b.Provider] {
					return fmt.Errorf("model %s: backend references unknown provider %q", m.Name, b.Provider)
				}
			}
		} else {
			// Direct model
			if m.Provider == "" {
				return fmt.Errorf("model %s: missing provider (set provider or strategy)", m.Name)
			}
			if !providerNames[m.Provider] {
				return fmt.Errorf("model %s: unknown provider %q", m.Name, m.Provider)
			}
			if m.Model == "" {
				return fmt.Errorf("model %s: missing model (the actual model ID at the provider)", m.Name)
			}
		}
	}

	keySet := make(map[string]bool)
	for keyIdx, k := range c.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("api_key entry missing key value")
		}
		if keySet[k.Key] {
			return fmt.Errorf("duplicate api_key: %s", shortKey(k.Key))
		}
		keySet[k.Key] = true

		// Auto-clean orphaned AllowedModels that reference non-existent model routes.
		// This handles cascade deletes where models are removed and API keys are updated.
		var cleaned []string
		for _, am := range k.AllowedModels {
			if am == "*" || allModelNames[am] {
				cleaned = append(cleaned, am)
			}
		}
		if len(cleaned) != len(k.AllowedModels) {
			// Some models were orphaned — update the key in-place
			c.APIKeys[keyIdx].AllowedModels = cleaned
		}

		// If AllowedModels is empty (initial creation or all models cascade-deleted),
		// fall back to wildcard ["*"] so the key remains usable.
		if len(c.APIKeys[keyIdx].AllowedModels) == 0 {
			c.APIKeys[keyIdx].AllowedModels = []string{"*"}
		}
	}

	return nil
}

// shortKey safely truncates a key for display in error messages.
func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12] + "..."
	}
	return k
}

// IsDashboardEnabled checks if the admin web dashboard is enabled.
func (s *ServerConfig) IsDashboardEnabled() bool {
	if s.DashboardEnabled == nil {
		return true // default to enabled
	}
	return *s.DashboardEnabled
}

// RenameModelCascade updates all references to an old model name in the AllowedModels list of API keys.
func (c *Config) RenameModelCascade(oldName, newName string) {
	for idx, k := range c.APIKeys {
		var updated []string
		changed := false
		for _, am := range k.AllowedModels {
			if am == oldName {
				updated = append(updated, newName)
				changed = true
			} else {
				updated = append(updated, am)
			}
		}
		if changed {
			c.APIKeys[idx].AllowedModels = updated
		}
	}
}

// FindCFRelay searches for a configured Cloudflare Relay by name.
func (c *Config) FindCFRelay(name string) *CFRelay {
	for i := range c.CFRelays {
		if strings.EqualFold(c.CFRelays[i].Name, name) {
			return &c.CFRelays[i]
		}
	}
	return nil
}

// NormalizeProxyURL normalizes and converts proxy representations:
// - Strips whitespace
// - Handles socks5://, https://, http:// prefixes
// - Converts "host:port:user:pass" (standard provider export format like Webshare)
//   into "http://user:pass@host:port"
// - Ensures an "http://" prefix if no scheme is specified
func NormalizeProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	scheme := "http://"
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "socks5://") {
		scheme = "socks5://"
		raw = raw[len("socks5://"):]
	} else if strings.HasPrefix(lower, "https://") {
		scheme = "https://"
		raw = raw[len("https://"):]
	} else if strings.HasPrefix(lower, "http://") {
		scheme = "http://"
		raw = raw[len("http://"):]
	}

	if strings.Contains(raw, "@") {
		return scheme + raw
	}

	// Check if raw matches "host:port:user:pass" (e.g. Webshare export)
	parts := strings.Split(raw, ":")
	if len(parts) == 4 {
		userInfo := url.UserPassword(parts[2], parts[3]).String()
		return fmt.Sprintf("%s%s@%s:%s", scheme, userInfo, parts[0], parts[1])
	}

	return scheme + raw
}

