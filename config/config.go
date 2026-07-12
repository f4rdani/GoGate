package config

import (
	"fmt"
	"os"
	"regexp"
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
	Providers   []ProviderConfig  `yaml:"providers" json:"providers"`
	Models      []ModelConfig     `yaml:"models" json:"models"`
	APIKeys     []APIKeyConfig    `yaml:"api_keys" json:"api_keys"`
}

// TokenSaverConfig holds settings for the RTK-style input token compression.
type TokenSaverConfig struct {
	Enabled       bool `yaml:"enabled" json:"enabled"`                // master toggle (default: true)
	MaxInputBytes int  `yaml:"max_input_bytes" json:"max_input_bytes"` // per-message threshold in bytes (default: 4096)
	CavemanMode   bool `yaml:"caveman_mode" json:"caveman_mode"`       // inject terse output prompt to save output tokens
	CompressUser  bool `yaml:"compress_user" json:"compress_user"`     // also compress user messages (default: true)
	MinifyJSON    bool `yaml:"minify_json" json:"minify_json"`         // minify JSON blobs in messages (default: true)
	StripComments bool `yaml:"strip_comments" json:"strip_comments"`   // strip code comments (default: false)
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host        string `yaml:"host" json:"host"`
	Port        int    `yaml:"port" json:"port"`
	AdminSecret string `yaml:"admin_secret" json:"admin_secret"`
	LogLevel         string `yaml:"log_level,omitempty" json:"log_level,omitempty"` // "info", "debug", "warn", "error"
	QuickTunnel      bool   `yaml:"quick_tunnel,omitempty" json:"quick_tunnel,omitempty"`
	DashboardEnabled *bool  `yaml:"dashboard_enabled,omitempty" json:"dashboard_enabled,omitempty"` // enable or disable admin web dashboard (default: true)
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
	Type                string        `yaml:"type" json:"type"`                // "openai", "anthropic", "groq", "mistral", "custom"
	BaseURL             string        `yaml:"base_url" json:"base_url"`
	APIKeys             []string      `yaml:"api_keys" json:"api_keys"`
	Models              []string      `yaml:"models" json:"models"`
	Tier                int           `yaml:"tier,omitempty" json:"tier,omitempty"`      // 1=subscription, 2=cheap, 3=free (default=1)
	HealthCheckURL      string        `yaml:"health_check_url,omitempty" json:"health_check_url,omitempty"` // custom health check endpoint
	HealthCheckInterval time.Duration `yaml:"health_check_interval,omitempty" json:"health_check_interval,omitempty"` // e.g. 30s, 0=disabled
	ProxyURL            string        `yaml:"proxy_url,omitempty" json:"proxy_url,omitempty"` // proxy for outbound HTTP requests (e.g. socks5://127.0.0.1:4000)
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

// APIKeyConfig defines a user-facing API key with permissions.
type APIKeyConfig struct {
	Key           string   `yaml:"key" json:"key"`
	Name          string   `yaml:"name" json:"name"`
	AllowedModels []string `yaml:"allowed_models" json:"allowed_models"`
	RateLimit     int      `yaml:"rate_limit" json:"rate_limit"`                       // requests per minute, 0 = unlimited
	TokenSaver    *bool    `yaml:"token_saver,omitempty" json:"token_saver,omitempty"` // per-key toggle (nil = follow global, true/false = override)
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

	// Apply provider tier defaults
	for i := range cfg.Providers {
		if cfg.Providers[i].Tier == 0 {
			cfg.Providers[i].Tier = 1
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
			"opencode": true, "cerebras": true,
		}
		if !validTypes[p.Type] {
			return fmt.Errorf("provider %s: invalid type %q (valid: openai, cohere, opencode, cerebras, anthropic, groq, mistral, custom)", p.Name, p.Type)
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
		allModelNames[m.Name] = true
		if m.Strategy != "" {
			// Combo model
			if m.Strategy != "round-robin" && m.Strategy != "fallback" {
				return fmt.Errorf("model %s: invalid strategy %q (valid: round-robin, fallback)", m.Name, m.Strategy)
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
			return fmt.Errorf("duplicate api_key: %s", k.Key[:12]+"...")
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
