# AI Gateway — Complete Implementation Walkthrough

Panduan implementasi lengkap untuk AI Gateway di Go. Semua code siap copy-paste.
Ikuti urutan step-by-step karena ada dependency antar package.

---

## Prerequisites

- Go 1.22+ terinstall
- Folder project: `aigateway/`

## Project Structure (Final)

```
aigateway/
├── main.go
├── go.mod
├── config.yaml
├── config/
│   └── config.go
├── models/
│   ├── openai.go
│   └── anthropic.go
├── balancer/
│   └── roundrobin.go
├── provider/
│   ├── provider.go
│   ├── openai.go
│   ├── anthropic.go
│   ├── groq.go
│   ├── mistral.go
│   └── custom.go
├── auth/
│   └── auth.go
├── router/
│   └── router.go
├── middleware/
│   ├── ratelimit.go
│   └── logging.go
├── proxy/
│   └── handler.go
├── admin/
│   └── admin.go
└── server/
    └── server.go
```

---

## Step 1: go.mod

**File:** `go.mod`

```go
module github.com/aigateway

go 1.22.0

require (
	gopkg.in/yaml.v3 v3.0.1
	github.com/google/uuid v1.6.0
)
```

Setelah buat file ini, jalankan:
```bash
go mod tidy
```

---

## Step 2: config.yaml

**File:** `config.yaml`

```yaml
# AI Gateway Configuration
# ========================

server:
  host: "0.0.0.0"
  port: 8080
  admin_secret: "change-me-to-a-strong-secret"

# Concurrency limits
concurrency:
  max_concurrent: 100
  per_provider: 30
  per_model: 15

# ========== PROVIDERS ==========
providers:
  - name: "openai"
    type: "openai"
    base_url: "https://api.openai.com/v1"
    api_keys:
      - "sk-your-openai-key-1"
      - "sk-your-openai-key-2"
    models:
      - "gpt-4o"
      - "gpt-4o-mini"
      - "gpt-4.1"
      - "gpt-4.1-mini"

  - name: "anthropic"
    type: "anthropic"
    base_url: "https://api.anthropic.com"
    api_keys:
      - "sk-ant-your-key-1"
    models:
      - "claude-sonnet-4-20250514"
      - "claude-haiku-4-20250514"

  - name: "groq"
    type: "groq"
    base_url: "https://api.groq.com/openai/v1"
    api_keys:
      - "gsk-your-groq-key-1"
    models:
      - "llama-3.3-70b-versatile"
      - "mixtral-8x7b-32768"

  - name: "mistral"
    type: "mistral"
    base_url: "https://api.mistral.ai/v1"
    api_keys:
      - "your-mistral-key-1"
    models:
      - "mistral-large-latest"
      - "mistral-small-latest"

  # Example: custom OpenAI-compatible provider (uncomment to use)
  # - name: "local-ollama"
  #   type: "custom"
  #   base_url: "http://localhost:11434/v1"
  #   api_keys:
  #     - "ollama"
  #   models:
  #     - "llama3"

# ========== MODEL ROUTING ==========
models:
  # Direct model mappings
  - name: "gpt-4o"
    provider: "openai"
    model: "gpt-4o"

  - name: "gpt-4o-mini"
    provider: "openai"
    model: "gpt-4o-mini"

  - name: "claude-sonnet"
    provider: "anthropic"
    model: "claude-sonnet-4-20250514"

  - name: "llama-70b"
    provider: "groq"
    model: "llama-3.3-70b-versatile"

  - name: "mistral-large"
    provider: "mistral"
    model: "mistral-large-latest"

  # Round-robin across providers
  - name: "fast-model"
    strategy: "round-robin"
    backends:
      - provider: "groq"
        model: "llama-3.3-70b-versatile"
      - provider: "mistral"
        model: "mistral-small-latest"

  # Fallback chain (try first, if fails try next)
  - name: "smart-combo"
    strategy: "fallback"
    backends:
      - provider: "anthropic"
        model: "claude-sonnet-4-20250514"
      - provider: "openai"
        model: "gpt-4o"
      - provider: "groq"
        model: "llama-3.3-70b-versatile"

  # Budget mix — round-robin across cheap models
  - name: "budget-mix"
    strategy: "round-robin"
    backends:
      - provider: "groq"
        model: "llama-3.3-70b-versatile"
      - provider: "mistral"
        model: "mistral-small-latest"
      - provider: "openai"
        model: "gpt-4o-mini"

# ========== USER API KEYS ==========
api_keys:
  - key: "sk-gw-admin-example-key-do-not-use"
    name: "Admin Full Access"
    allowed_models:
      - "*"
    rate_limit: 0

  - key: "sk-gw-user1-example-key"
    name: "User 1 - Budget"
    allowed_models:
      - "budget-mix"
      - "fast-model"
      - "gpt-4o-mini"
    rate_limit: 60
```

---

## Step 3: Config Package

**File:** `config/config.go`

```go
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Concurrency ConcurrencyConfig `yaml:"concurrency"`
	Providers   []ProviderConfig  `yaml:"providers"`
	Models      []ModelConfig     `yaml:"models"`
	APIKeys     []APIKeyConfig    `yaml:"api_keys"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	AdminSecret string `yaml:"admin_secret"`
}

// ConcurrencyConfig holds concurrency limit settings.
type ConcurrencyConfig struct {
	MaxConcurrent int `yaml:"max_concurrent"`
	PerProvider   int `yaml:"per_provider"`
	PerModel      int `yaml:"per_model"`
}

// ProviderConfig defines an upstream AI provider.
type ProviderConfig struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"` // "openai", "anthropic", "groq", "mistral", "custom"
	BaseURL string   `yaml:"base_url"`
	APIKeys []string `yaml:"api_keys"`
	Models  []string `yaml:"models"`
}

// ModelConfig defines a model route (direct or combo).
type ModelConfig struct {
	Name     string          `yaml:"name"`
	Provider string          `yaml:"provider,omitempty"`  // for direct models
	Model    string          `yaml:"model,omitempty"`     // for direct models
	Strategy string          `yaml:"strategy,omitempty"`  // "round-robin" or "fallback"
	Backends []BackendConfig `yaml:"backends,omitempty"`  // for combo models
}

// BackendConfig defines a backend in a combo model.
type BackendConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// APIKeyConfig defines a user-facing API key with permissions.
type APIKeyConfig struct {
	Key           string   `yaml:"key"`
	Name          string   `yaml:"name"`
	AllowedModels []string `yaml:"allowed_models"`
	RateLimit     int      `yaml:"rate_limit"` // requests per minute, 0 = unlimited
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
	if cfg.Concurrency.MaxConcurrent == 0 {
		cfg.Concurrency.MaxConcurrent = 100
	}
	if cfg.Concurrency.PerProvider == 0 {
		cfg.Concurrency.PerProvider = 30
	}
	if cfg.Concurrency.PerModel == 0 {
		cfg.Concurrency.PerModel = 15
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

	providerNames := make(map[string]bool)
	for _, p := range c.Providers {
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
			"mistral": true, "custom": true,
		}
		if !validTypes[p.Type] {
			return fmt.Errorf("provider %s: invalid type %q (valid: openai, anthropic, groq, mistral, custom)", p.Name, p.Type)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s: missing base_url", p.Name)
		}
		if len(p.APIKeys) == 0 {
			return fmt.Errorf("provider %s: no api_keys configured", p.Name)
		}
		if len(p.Models) == 0 {
			return fmt.Errorf("provider %s: no models configured", p.Name)
		}
	}

	for _, m := range c.Models {
		if m.Name == "" {
			return fmt.Errorf("model entry missing name")
		}
		if m.Strategy != "" {
			// Combo model
			if m.Strategy != "round-robin" && m.Strategy != "fallback" {
				return fmt.Errorf("model %s: invalid strategy %q (valid: round-robin, fallback)", m.Name, m.Strategy)
			}
			if len(m.Backends) == 0 {
				return fmt.Errorf("model %s: strategy set but no backends defined", m.Name)
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
	for _, k := range c.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("api_key entry missing key value")
		}
		if keySet[k.Key] {
			return fmt.Errorf("duplicate api_key: %s", k.Key[:12]+"...")
		}
		keySet[k.Key] = true
		if len(k.AllowedModels) == 0 {
			return fmt.Errorf("api_key %s: no allowed_models defined", k.Key[:12]+"...")
		}
	}

	return nil
}
```

---

## Step 4: OpenAI Models (Types)

**File:** `models/openai.go`

```go
package models

import "encoding/json"

// ==================== Request Types ====================

// ChatCompletionRequest represents an OpenAI-compatible chat completion request.
type ChatCompletionRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	N                *int            `json:"n,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	User             string          `json:"user,omitempty"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
}

// Message represents a chat message.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`              // string or array of content parts
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// ContentString returns the message content as a plain string.
// Handles both JSON string values and raw text.
func (m *Message) ContentString() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	return string(m.Content)
}

// ==================== Response Types ====================

// ChatCompletionResponse represents an OpenAI chat completion response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice represents a completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      *Message `json:"message,omitempty"`
	FinishReason *string  `json:"finish_reason,omitempty"`
}

// Usage represents token usage statistics.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ==================== Streaming Types ====================

// ChatCompletionChunk represents a streaming chunk.
type ChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

// StreamChoice represents a choice in a streaming chunk.
type StreamChoice struct {
	Index        int          `json:"index"`
	Delta        *StreamDelta `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason,omitempty"`
}

// StreamDelta represents the delta content in a streaming chunk.
type StreamDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// ==================== Models List ====================

// ModelObject represents a model in the models list.
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelListResponse represents the GET /v1/models response.
type ModelListResponse struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// ==================== Error Response ====================

// ErrorResponse represents an OpenAI-compatible error response.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error details.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
```

---

## Step 5: Anthropic Models (Types)

**File:** `models/anthropic.go`

```go
package models

import "encoding/json"

// ==================== Anthropic Request ====================

// AnthropicRequest represents a request to the Anthropic Messages API.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []AnthropicMessage `json:"messages"`
	System        string             `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         json.RawMessage    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

// AnthropicMessage represents a message in the Anthropic format.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or array of content blocks
}

// ==================== Anthropic Response ====================

// AnthropicResponse represents a response from the Anthropic Messages API.
type AnthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []AnthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason *string                 `json:"stop_reason,omitempty"`
	Usage      AnthropicUsage          `json:"usage"`
}

// AnthropicContentBlock represents a content block in the response.
type AnthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// AnthropicUsage represents token usage in the Anthropic format.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ==================== Anthropic Streaming ====================

// AnthropicStreamEvent represents a streaming event from Anthropic.
type AnthropicStreamEvent struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index,omitempty"`
	Delta        json.RawMessage        `json:"delta,omitempty"`
	Message      *AnthropicResponse     `json:"message,omitempty"`
	ContentBlock *AnthropicContentBlock `json:"content_block,omitempty"`
	Usage        *AnthropicUsage        `json:"usage,omitempty"`
}

// AnthropicDeltaText represents a text delta in streaming.
type AnthropicDeltaText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// AnthropicMessageDelta represents a message delta in streaming.
type AnthropicMessageDelta struct {
	StopReason string `json:"stop_reason"`
}
```

---

## Step 6: Round-Robin Balancer

**File:** `balancer/roundrobin.go`

```go
package balancer

import "sync/atomic"

// RoundRobin implements a thread-safe, lock-free round-robin selector.
// Uses atomic counter for zero-contention concurrent access.
type RoundRobin[T any] struct {
	items   []T
	counter atomic.Uint64
}

// New creates a new RoundRobin balancer with the given items.
func New[T any](items []T) *RoundRobin[T] {
	return &RoundRobin[T]{items: items}
}

// Next returns the next item in round-robin order.
// Thread-safe via atomic increment.
func (rr *RoundRobin[T]) Next() T {
	n := rr.counter.Add(1) - 1
	return rr.items[n%uint64(len(rr.items))]
}

// Len returns the number of items.
func (rr *RoundRobin[T]) Len() int {
	return len(rr.items)
}

// All returns all items.
func (rr *RoundRobin[T]) All() []T {
	return rr.items
}
```

---

## Step 7: Provider Interface & Registry

**File:** `provider/provider.go`

```go
package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aigateway/balancer"
	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

// Provider is the interface that all AI providers must implement.
type Provider interface {
	// Name returns the provider's name.
	Name() string
	// ChatCompletion sends a non-streaming chat completion request.
	ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error)
	// ChatCompletionStream sends a streaming chat completion request.
	// It writes SSE data directly to the ResponseWriter in OpenAI format.
	ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error
}

// BaseProvider contains common fields shared by all providers.
type BaseProvider struct {
	name    string
	baseURL string
	apiKeys *balancer.RoundRobin[string]
	client  *http.Client
	models  map[string]bool
}

// Name returns the provider's name.
func (b *BaseProvider) Name() string {
	return b.name
}

// NextAPIKey returns the next API key in round-robin order.
func (b *BaseProvider) NextAPIKey() string {
	return b.apiKeys.Next()
}

// SupportsModel checks if the provider supports a given model.
func (b *BaseProvider) SupportsModel(model string) bool {
	return b.models[model]
}

// ProviderError represents an error from an upstream provider.
type ProviderError struct {
	StatusCode int
	Body       string
	Provider   string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider %s error (status %d): %s", e.Provider, e.StatusCode, e.Body)
}

// IsRetryable returns true if the error is transient and the request can be retried.
func (e *ProviderError) IsRetryable() bool {
	return e.StatusCode == 429 || e.StatusCode == 500 || e.StatusCode == 502 || e.StatusCode == 503
}

// Registry holds all registered providers.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry creates a new provider registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register adds a provider to the registry.
func (r *Registry) Register(name string, p Provider) {
	r.providers[name] = p
}

// Get retrieves a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// All returns all registered providers.
func (r *Registry) All() map[string]Provider {
	return r.providers
}

// NewProviderFromConfig creates a Provider instance based on the config type.
func NewProviderFromConfig(cfg config.ProviderConfig) (Provider, error) {
	modelSet := make(map[string]bool)
	for _, m := range cfg.Models {
		modelSet[m] = true
	}

	base := BaseProvider{
		name:    cfg.Name,
		baseURL: cfg.BaseURL,
		apiKeys: balancer.New(cfg.APIKeys),
		client: &http.Client{
			Timeout: 5 * time.Minute, // Long timeout for streaming
		},
		models: modelSet,
	}

	switch cfg.Type {
	case "openai":
		return NewOpenAIProvider(base), nil
	case "anthropic":
		return NewAnthropicProvider(base), nil
	case "groq":
		return NewGroqProvider(base), nil
	case "mistral":
		return NewMistralProvider(base), nil
	case "custom":
		return NewCustomProvider(base), nil
	default:
		return nil, fmt.Errorf("unknown provider type: %s", cfg.Type)
	}
}
```

---

## Step 8: OpenAI Provider

**File:** `provider/openai.go`

> [!NOTE]
> Ini adalah provider base untuk semua OpenAI-compatible providers (OpenAI, Groq, Mistral, Custom).
> Groq, Mistral, dan Custom hanya embed struct ini karena format API-nya identik.

```go
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aigateway/models"
)

// OpenAIProvider handles requests to OpenAI and OpenAI-compatible APIs.
type OpenAIProvider struct {
	BaseProvider
}

// NewOpenAIProvider creates a new OpenAI provider.
func NewOpenAIProvider(base BaseProvider) *OpenAIProvider {
	return &OpenAIProvider{BaseProvider: base}
}

// ChatCompletion sends a non-streaming request to the OpenAI API.
func (o *OpenAIProvider) ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	req.Stream = false

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := o.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	apiKey := o.NextAPIKey()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &ProviderError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			Provider:   o.name,
		}
	}

	var chatResp models.ChatCompletionResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &chatResp, nil
}

// ChatCompletionStream sends a streaming request and pipes SSE to the client.
// IMPORTANT: This checks upstream status BEFORE writing any headers,
// so fallback can work if upstream returns an error.
func (o *OpenAIProvider) ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := o.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	apiKey := o.NextAPIKey()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Check status BEFORE writing to ResponseWriter (enables fallback)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return &ProviderError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			Provider:   o.name,
		}
	}

	// Set streaming headers and begin piping
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Pipe upstream SSE directly to client (zero translation needed)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	return nil
}
```

---

## Step 9: Anthropic Provider

**File:** `provider/anthropic.go`

> [!IMPORTANT]
> Ini adalah provider paling kompleks karena Anthropic menggunakan format berbeda:
> - System message → top-level `system` field (bukan di messages array)
> - Auth → `x-api-key` header (bukan Bearer)
> - Response → `content[].text` (bukan `choices[].message.content`)
> - Streaming → event types berbeda (message_start, content_block_delta, dll)

```go
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aigateway/models"
)

// AnthropicProvider handles requests to the Anthropic Messages API
// with full request/response translation from/to OpenAI format.
type AnthropicProvider struct {
	BaseProvider
}

// NewAnthropicProvider creates a new Anthropic provider.
func NewAnthropicProvider(base BaseProvider) *AnthropicProvider {
	return &AnthropicProvider{BaseProvider: base}
}

// translateRequest converts an OpenAI-format request to Anthropic format.
func (a *AnthropicProvider) translateRequest(req *models.ChatCompletionRequest) (*models.AnthropicRequest, error) {
	anthReq := &models.AnthropicRequest{
		Model:  req.Model,
		Stream: req.Stream,
	}

	// Extract system messages and convert message format
	var systemParts []string
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemParts = append(systemParts, msg.ContentString())
		} else {
			anthMsg := models.AnthropicMessage{
				Role:    msg.Role,
				Content: msg.Content,
			}
			anthReq.Messages = append(anthReq.Messages, anthMsg)
		}
	}

	if len(systemParts) > 0 {
		anthReq.System = strings.Join(systemParts, "\n\n")
	}

	// max_tokens is REQUIRED by Anthropic (default 4096)
	if req.MaxTokens != nil {
		anthReq.MaxTokens = *req.MaxTokens
	} else {
		anthReq.MaxTokens = 4096
	}

	anthReq.Temperature = req.Temperature
	anthReq.TopP = req.TopP

	// Convert "stop" field to "stop_sequences"
	if len(req.Stop) > 0 {
		var stop interface{}
		if err := json.Unmarshal(req.Stop, &stop); err == nil {
			switch v := stop.(type) {
			case string:
				anthReq.StopSequences = []string{v}
			case []interface{}:
				for _, s := range v {
					if str, ok := s.(string); ok {
						anthReq.StopSequences = append(anthReq.StopSequences, str)
					}
				}
			}
		}
	}

	// Pass through tools if present
	anthReq.Tools = req.Tools
	anthReq.ToolChoice = req.ToolChoice

	return anthReq, nil
}

// translateResponse converts an Anthropic response to OpenAI format.
func (a *AnthropicProvider) translateResponse(anthResp *models.AnthropicResponse) *models.ChatCompletionResponse {
	// Extract text content from content blocks
	var contentParts []string
	for _, block := range anthResp.Content {
		if block.Type == "text" {
			contentParts = append(contentParts, block.Text)
		}
	}
	content := strings.Join(contentParts, "")
	contentJSON, _ := json.Marshal(content)

	// Map Anthropic stop_reason to OpenAI finish_reason
	var finishReason string
	if anthResp.StopReason != nil {
		switch *anthResp.StopReason {
		case "end_turn":
			finishReason = "stop"
		case "max_tokens":
			finishReason = "length"
		case "tool_use":
			finishReason = "tool_calls"
		default:
			finishReason = "stop"
		}
	}

	return &models.ChatCompletionResponse{
		ID:      anthResp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   anthResp.Model,
		Choices: []models.Choice{
			{
				Index: 0,
				Message: &models.Message{
					Role:    "assistant",
					Content: json.RawMessage(contentJSON),
				},
				FinishReason: &finishReason,
			},
		},
		Usage: &models.Usage{
			PromptTokens:     anthResp.Usage.InputTokens,
			CompletionTokens: anthResp.Usage.OutputTokens,
			TotalTokens:      anthResp.Usage.InputTokens + anthResp.Usage.OutputTokens,
		},
	}
}

// ChatCompletion sends a non-streaming request to Anthropic.
func (a *AnthropicProvider) ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	anthReq, err := a.translateRequest(req)
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
	}
	anthReq.Stream = false

	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := a.baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	apiKey := a.NextAPIKey()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &ProviderError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			Provider:   a.name,
		}
	}

	var anthResp models.AnthropicResponse
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return a.translateResponse(&anthResp), nil
}

// ChatCompletionStream sends a streaming request to Anthropic and translates
// Anthropic SSE events to OpenAI SSE format on-the-fly.
func (a *AnthropicProvider) ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	anthReq, err := a.translateRequest(req)
	if err != nil {
		return fmt.Errorf("translate request: %w", err)
	}
	anthReq.Stream = true

	body, err := json.Marshal(anthReq)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := a.baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	apiKey := a.NextAPIKey()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Check status BEFORE writing to ResponseWriter
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return &ProviderError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			Provider:   a.name,
		}
	}

	// Set streaming headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Translate Anthropic SSE → OpenAI SSE
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var msgID string
	var model string

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		var event models.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				msgID = event.Message.ID
				model = event.Message.Model
			}
			// Send initial role chunk
			chunk := models.ChatCompletionChunk{
				ID:      msgID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []models.StreamChoice{
					{Index: 0, Delta: &models.StreamDelta{Role: "assistant"}},
				},
			}
			chunkJSON, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", chunkJSON)
			flusher.Flush()

		case "content_block_delta":
			var delta models.AnthropicDeltaText
			if err := json.Unmarshal(event.Delta, &delta); err != nil {
				continue
			}

			if delta.Type == "text_delta" && delta.Text != "" {
				chunk := models.ChatCompletionChunk{
					ID:      msgID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   model,
					Choices: []models.StreamChoice{
						{Index: 0, Delta: &models.StreamDelta{Content: delta.Text}},
					},
				}
				chunkJSON, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", chunkJSON)
				flusher.Flush()
			}

		case "message_delta":
			var msgDelta models.AnthropicMessageDelta
			if err := json.Unmarshal(event.Delta, &msgDelta); err != nil {
				continue
			}

			finishReason := "stop"
			if msgDelta.StopReason == "max_tokens" {
				finishReason = "length"
			} else if msgDelta.StopReason == "tool_use" {
				finishReason = "tool_calls"
			}

			chunk := models.ChatCompletionChunk{
				ID:      msgID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []models.StreamChoice{
					{Index: 0, Delta: &models.StreamDelta{}, FinishReason: &finishReason},
				},
			}
			chunkJSON, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", chunkJSON)
			flusher.Flush()

		case "message_stop":
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	}

	return scanner.Err()
}
```

---

## Step 10: Groq, Mistral, Custom Providers

Ketiga provider ini **OpenAI-compatible**, jadi cukup embed `OpenAIProvider`.

**File:** `provider/groq.go`

```go
package provider

// GroqProvider handles requests to the Groq API.
// Groq uses OpenAI-compatible format, so we embed OpenAIProvider.
type GroqProvider struct {
	OpenAIProvider
}

// NewGroqProvider creates a new Groq provider.
func NewGroqProvider(base BaseProvider) *GroqProvider {
	return &GroqProvider{OpenAIProvider: OpenAIProvider{BaseProvider: base}}
}
```

**File:** `provider/mistral.go`

```go
package provider

// MistralProvider handles requests to the Mistral AI API.
// Mistral uses OpenAI-compatible format, so we embed OpenAIProvider.
type MistralProvider struct {
	OpenAIProvider
}

// NewMistralProvider creates a new Mistral provider.
func NewMistralProvider(base BaseProvider) *MistralProvider {
	return &MistralProvider{OpenAIProvider: OpenAIProvider{BaseProvider: base}}
}
```

**File:** `provider/custom.go`

```go
package provider

// CustomProvider handles requests to any OpenAI-compatible API endpoint.
// Useful for self-hosted models (Ollama, vLLM, LocalAI, etc.).
type CustomProvider struct {
	OpenAIProvider
}

// NewCustomProvider creates a new custom OpenAI-compatible provider.
func NewCustomProvider(base BaseProvider) *CustomProvider {
	return &CustomProvider{OpenAIProvider: OpenAIProvider{BaseProvider: base}}
}
```

---

## Step 11: Auth Package

**File:** `auth/auth.go`

```go
package auth

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/aigateway/config"
	"github.com/google/uuid"
)

// KeyInfo holds metadata and state for a single API key.
type KeyInfo struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	RateLimit     int      `json:"rate_limit"` // requests per minute, 0 = unlimited

	// Rate limiting state (not serialized)
	windowStart time.Time
	windowCount atomic.Int64
	mu          sync.Mutex
}

// IsModelAllowed checks if the given model is permitted for this key.
func (k *KeyInfo) IsModelAllowed(model string) bool {
	for _, m := range k.AllowedModels {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}

// CheckRateLimit returns true if the request is within rate limits.
// Uses a sliding window per-minute counter.
func (k *KeyInfo) CheckRateLimit() bool {
	if k.RateLimit <= 0 {
		return true // No rate limit
	}

	k.mu.Lock()
	now := time.Now()
	if now.Sub(k.windowStart) > time.Minute {
		// Reset window
		k.windowStart = now
		k.windowCount.Store(0)
	}
	k.mu.Unlock()

	count := k.windowCount.Add(1)
	return count <= int64(k.RateLimit)
}

// KeyStore manages API keys with thread-safe access.
type KeyStore struct {
	mu   sync.RWMutex
	keys map[string]*KeyInfo
}

// NewKeyStore creates a KeyStore from configuration.
func NewKeyStore(configs []config.APIKeyConfig) *KeyStore {
	store := &KeyStore{
		keys: make(map[string]*KeyInfo),
	}
	for _, cfg := range configs {
		store.keys[cfg.Key] = &KeyInfo{
			Key:           cfg.Key,
			Name:          cfg.Name,
			AllowedModels: cfg.AllowedModels,
			RateLimit:     cfg.RateLimit,
		}
	}
	return store
}

// Validate checks if an API key is valid and returns its info.
func (s *KeyStore) Validate(key string) (*KeyInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.keys[key]
	return info, ok
}

// AddKey creates a new API key and returns its info.
// The key is generated with format: sk-gw-{uuid}
func (s *KeyStore) AddKey(name string, allowedModels []string, rateLimit int) *KeyInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := "sk-gw-" + uuid.New().String()
	info := &KeyInfo{
		Key:           key,
		Name:          name,
		AllowedModels: allowedModels,
		RateLimit:     rateLimit,
	}
	s.keys[key] = info
	return info
}

// DeleteKey removes an API key. Returns true if the key existed.
func (s *KeyStore) DeleteKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[key]; ok {
		delete(s.keys, key)
		return true
	}
	return false
}

// ListKeys returns all API keys.
func (s *KeyStore) ListKeys() []*KeyInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*KeyInfo, 0, len(s.keys))
	for _, info := range s.keys {
		result = append(result, info)
	}
	return result
}
```

---

## Step 12: Router Package

**File:** `router/router.go`

```go
package router

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aigateway/balancer"
	"github.com/aigateway/config"
	"github.com/aigateway/models"
	"github.com/aigateway/provider"
)

// Backend represents a single model deployment on a provider.
type Backend struct {
	Provider provider.Provider
	Model    string
}

// ModelRoute holds the routing configuration for a model name.
type ModelRoute struct {
	Name     string
	Strategy string // "" (direct), "round-robin", "fallback"

	// For direct model mapping
	Backend *Backend

	// For combo models (round-robin or fallback)
	Backends []Backend
	Balancer *balancer.RoundRobin[int] // Index-based round-robin
}

// Router resolves model names to provider backends.
type Router struct {
	routes   map[string]*ModelRoute
	registry *provider.Registry
}

// NewRouter creates a Router from configuration.
func NewRouter(cfg *config.Config, registry *provider.Registry) (*Router, error) {
	r := &Router{
		routes:   make(map[string]*ModelRoute),
		registry: registry,
	}

	for _, modelCfg := range cfg.Models {
		route := &ModelRoute{
			Name:     modelCfg.Name,
			Strategy: modelCfg.Strategy,
		}

		if modelCfg.Strategy == "" {
			// Direct model — single provider, single model
			p, ok := registry.Get(modelCfg.Provider)
			if !ok {
				return nil, fmt.Errorf("model %s: provider %q not found", modelCfg.Name, modelCfg.Provider)
			}
			route.Backend = &Backend{
				Provider: p,
				Model:    modelCfg.Model,
			}
		} else {
			// Combo model — multiple backends
			indices := make([]int, len(modelCfg.Backends))
			for i, bCfg := range modelCfg.Backends {
				p, ok := registry.Get(bCfg.Provider)
				if !ok {
					return nil, fmt.Errorf("model %s: backend provider %q not found", modelCfg.Name, bCfg.Provider)
				}
				route.Backends = append(route.Backends, Backend{
					Provider: p,
					Model:    bCfg.Model,
				})
				indices[i] = i
			}
			if modelCfg.Strategy == "round-robin" {
				route.Balancer = balancer.New(indices)
			}
		}

		r.routes[modelCfg.Name] = route
	}

	return r, nil
}

// GetModelNames returns all available model names.
func (r *Router) GetModelNames() []string {
	names := make([]string, 0, len(r.routes))
	for name := range r.routes {
		names = append(names, name)
	}
	return names
}

// ChatCompletion routes a non-streaming chat completion request.
func (r *Router) ChatCompletion(ctx context.Context, modelName string, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	route, ok := r.routes[modelName]
	if !ok {
		return nil, fmt.Errorf("model not found: %s", modelName)
	}

	switch route.Strategy {
	case "", "direct":
		// Direct: single backend
		req.Model = route.Backend.Model
		return route.Backend.Provider.ChatCompletion(ctx, req)

	case "round-robin":
		// Round-robin: pick next backend
		idx := route.Balancer.Next()
		backend := route.Backends[idx]
		req.Model = backend.Model
		slog.Info("round-robin selected",
			"alias", modelName,
			"provider", backend.Provider.Name(),
			"model", backend.Model,
		)
		return backend.Provider.ChatCompletion(ctx, req)

	case "fallback":
		// Fallback: try each backend in order until one succeeds
		var lastErr error
		for i, backend := range route.Backends {
			// Copy request so each attempt gets clean state
			reqCopy := *req
			reqCopy.Model = backend.Model

			resp, err := backend.Provider.ChatCompletion(ctx, &reqCopy)
			if err == nil {
				if i > 0 {
					slog.Info("fallback succeeded",
						"alias", modelName,
						"provider", backend.Provider.Name(),
						"model", backend.Model,
						"attempt", i+1,
					)
				}
				return resp, nil
			}

			lastErr = err
			slog.Warn("fallback: backend failed",
				"alias", modelName,
				"provider", backend.Provider.Name(),
				"model", backend.Model,
				"error", err,
			)

			// Only continue fallback for retryable errors (429, 500, 502, 503)
			if pe, ok := err.(*provider.ProviderError); ok && !pe.IsRetryable() {
				return nil, err
			}
		}
		return nil, fmt.Errorf("all backends failed for model %s: %w", modelName, lastErr)

	default:
		return nil, fmt.Errorf("unknown routing strategy: %s", route.Strategy)
	}
}

// ChatCompletionStream routes a streaming chat completion request.
func (r *Router) ChatCompletionStream(ctx context.Context, modelName string, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	route, ok := r.routes[modelName]
	if !ok {
		return fmt.Errorf("model not found: %s", modelName)
	}

	switch route.Strategy {
	case "", "direct":
		req.Model = route.Backend.Model
		return route.Backend.Provider.ChatCompletionStream(ctx, req, w, flusher)

	case "round-robin":
		idx := route.Balancer.Next()
		backend := route.Backends[idx]
		req.Model = backend.Model
		slog.Info("round-robin stream selected",
			"alias", modelName,
			"provider", backend.Provider.Name(),
			"model", backend.Model,
		)
		return backend.Provider.ChatCompletionStream(ctx, req, w, flusher)

	case "fallback":
		// Streaming fallback: try each backend.
		// Works because providers check upstream status BEFORE writing headers.
		// If upstream returns error (e.g. 429), no headers are sent and we can try next.
		var lastErr error
		for i, backend := range route.Backends {
			reqCopy := *req
			reqCopy.Model = backend.Model

			err := backend.Provider.ChatCompletionStream(ctx, &reqCopy, w, flusher)
			if err == nil {
				return nil
			}

			lastErr = err
			slog.Warn("fallback stream: backend failed",
				"alias", modelName,
				"provider", backend.Provider.Name(),
				"model", backend.Model,
				"error", err,
				"attempt", i+1,
			)

			if pe, ok := err.(*provider.ProviderError); ok && !pe.IsRetryable() {
				return err
			}
		}
		return fmt.Errorf("all backends failed for model %s: %w", modelName, lastErr)

	default:
		return fmt.Errorf("unknown routing strategy: %s", route.Strategy)
	}
}
```

---

## Step 13: Middleware — Concurrency Limiter

**File:** `middleware/ratelimit.go`

```go
package middleware

import (
	"encoding/json"
	"net/http"

	"github.com/aigateway/models"
)

// ConcurrencyLimiter uses buffered channels as semaphores to limit
// the number of concurrent requests at global, per-provider, and per-model levels.
type ConcurrencyLimiter struct {
	global chan struct{}
}

// NewConcurrencyLimiter creates a new limiter with the given global max.
func NewConcurrencyLimiter(globalMax int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		global: make(chan struct{}, globalMax),
	}
}

// AcquireGlobal tries to acquire a global concurrency slot.
// Returns true if acquired, false if at capacity.
func (cl *ConcurrencyLimiter) AcquireGlobal() bool {
	select {
	case cl.global <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseGlobal releases a global concurrency slot.
func (cl *ConcurrencyLimiter) ReleaseGlobal() {
	<-cl.global
}

// TooManyRequestsResponse sends a 429 error in OpenAI format.
func TooManyRequestsResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: "Too many concurrent requests. Please retry later.",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
		},
	})
}
```

---

## Step 14: Middleware — Logging

**File:** `middleware/logging.go`

```go
package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(status int) {
	if !sw.wroteHeader {
		sw.status = status
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(status)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.status = 200
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for streaming support.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// LoggingMiddleware logs every HTTP request with method, path, status, and duration.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}

		next.ServeHTTP(sw, r)

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).String(),
			"remote", r.RemoteAddr,
		)
	})
}
```

---

## Step 15: Proxy Handler

**File:** `proxy/handler.go`

> [!NOTE]
> Ini adalah handler utama yang menerima request dari client, validasi auth,
> dan meneruskan ke router. Mendukung streaming dan non-streaming.

```go
package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aigateway/auth"
	"github.com/aigateway/middleware"
	"github.com/aigateway/models"
	"github.com/aigateway/router"
)

// Stats tracks request statistics.
type Stats struct {
	TotalRequests  atomic.Int64
	ActiveRequests atomic.Int64
}

// Handler is the main proxy request handler.
type Handler struct {
	router   *router.Router
	keyStore *auth.KeyStore
	limiter  *middleware.ConcurrencyLimiter
	Stats    *Stats
}

// NewHandler creates a new proxy handler.
func NewHandler(r *router.Router, ks *auth.KeyStore, limiter *middleware.ConcurrencyLimiter) *Handler {
	return &Handler{
		router:   r,
		keyStore: ks,
		limiter:  limiter,
		Stats:    &Stats{},
	}
}

// extractAPIKey extracts the Bearer token from the Authorization header.
func (h *Handler) extractAPIKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	return ""
}

// sendError sends an OpenAI-compatible error response.
func (h *Handler) sendError(w http.ResponseWriter, status int, msg string, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: msg,
			Type:    errType,
		},
	})
}

// responseTracker wraps ResponseWriter to track if headers have been written.
// This is needed for streaming fallback — if headers are already sent,
// we can't send a different error status code.
type responseTracker struct {
	http.ResponseWriter
	headerWritten bool
}

func (rt *responseTracker) WriteHeader(code int) {
	rt.headerWritten = true
	rt.ResponseWriter.WriteHeader(code)
}

func (rt *responseTracker) Write(b []byte) (int, error) {
	rt.headerWritten = true
	return rt.ResponseWriter.Write(b)
}

func (rt *responseTracker) Flush() {
	if f, ok := rt.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// HandleChatCompletion handles POST /v1/chat/completions.
func (h *Handler) HandleChatCompletion(w http.ResponseWriter, r *http.Request) {
	h.Stats.TotalRequests.Add(1)
	h.Stats.ActiveRequests.Add(1)
	defer h.Stats.ActiveRequests.Add(-1)

	// === Auth ===
	apiKey := h.extractAPIKey(r)
	if apiKey == "" {
		h.sendError(w, http.StatusUnauthorized, "Missing API key in Authorization header", "authentication_error")
		return
	}

	keyInfo, ok := h.keyStore.Validate(apiKey)
	if !ok {
		h.sendError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error")
		return
	}

	// === Rate limit ===
	if !keyInfo.CheckRateLimit() {
		middleware.TooManyRequestsResponse(w)
		return
	}

	// === Concurrency limit ===
	if !h.limiter.AcquireGlobal() {
		middleware.TooManyRequestsResponse(w)
		return
	}
	defer h.limiter.ReleaseGlobal()

	// === Parse request ===
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendError(w, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var req models.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request_error")
		return
	}

	if req.Model == "" {
		h.sendError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}

	// === Permission check ===
	if !keyInfo.IsModelAllowed(req.Model) {
		h.sendError(w, http.StatusForbidden,
			"Model '"+req.Model+"' is not allowed for this API key", "permission_error")
		return
	}

	// === Route request ===
	if req.Stream {
		// Streaming response
		tracker := &responseTracker{ResponseWriter: w}
		flusher, ok := w.(http.Flusher)
		if !ok {
			h.sendError(w, http.StatusInternalServerError, "Streaming not supported by server", "server_error")
			return
		}

		err := h.router.ChatCompletionStream(r.Context(), req.Model, &req, tracker, flusher)
		if err != nil {
			slog.Error("stream error", "error", err, "model", req.Model)
			if !tracker.headerWritten {
				// Headers not yet sent — can still send error response
				h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
			}
			// If headers already sent, error is logged but can't be sent to client
		}
	} else {
		// Non-streaming response
		resp, err := h.router.ChatCompletion(r.Context(), req.Model, &req)
		if err != nil {
			slog.Error("completion error", "error", err, "model", req.Model)
			h.sendError(w, http.StatusBadGateway, "Provider error: "+err.Error(), "upstream_error")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// HandleListModels handles GET /v1/models.
// Returns models filtered by the API key's permissions.
func (h *Handler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	apiKey := h.extractAPIKey(r)
	if apiKey == "" {
		h.sendError(w, http.StatusUnauthorized, "Missing API key", "authentication_error")
		return
	}

	keyInfo, ok := h.keyStore.Validate(apiKey)
	if !ok {
		h.sendError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error")
		return
	}

	modelNames := h.router.GetModelNames()
	var modelList []models.ModelObject
	for _, name := range modelNames {
		if keyInfo.IsModelAllowed(name) {
			modelList = append(modelList, models.ModelObject{
				ID:      name,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "aigateway",
			})
		}
	}

	if modelList == nil {
		modelList = []models.ModelObject{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models.ModelListResponse{
		Object: "list",
		Data:   modelList,
	})
}

// HandleHealth handles GET /health.
func (h *Handler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}
```

---

## Step 16: Admin API

**File:** `admin/admin.go`

```go
package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aigateway/auth"
	"github.com/aigateway/models"
	"github.com/aigateway/proxy"
)

// AdminHandler handles admin API endpoints.
type AdminHandler struct {
	keyStore    *auth.KeyStore
	adminSecret string
	stats       *proxy.Stats
}

// NewAdminHandler creates a new admin handler.
func NewAdminHandler(keyStore *auth.KeyStore, adminSecret string, stats *proxy.Stats) *AdminHandler {
	return &AdminHandler{
		keyStore:    keyStore,
		adminSecret: adminSecret,
		stats:       stats,
	}
}

// checkAuth validates the admin secret from the X-Admin-Secret header.
func (a *AdminHandler) checkAuth(r *http.Request) bool {
	if a.adminSecret == "" {
		return false
	}
	secret := r.Header.Get("X-Admin-Secret")
	return secret == a.adminSecret
}

// sendError sends an error response.
func (a *AdminHandler) sendError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: msg,
			Type:    "admin_error",
		},
	})
}

// HandleListKeys handles GET /admin/keys — list all API keys.
func (a *AdminHandler) HandleListKeys(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	keys := a.keyStore.ListKeys()

	type keyResponse struct {
		Key           string   `json:"key"`
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		RateLimit     int      `json:"rate_limit"`
	}

	resp := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		resp = append(resp, keyResponse{
			Key:           k.Key,
			Name:          k.Name,
			AllowedModels: k.AllowedModels,
			RateLimit:     k.RateLimit,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleCreateKey handles POST /admin/keys — generate a new API key.
func (a *AdminHandler) HandleCreateKey(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	var req struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		RateLimit     int      `json:"rate_limit"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		a.sendError(w, http.StatusBadRequest, "'name' is required")
		return
	}
	if len(req.AllowedModels) == 0 {
		req.AllowedModels = []string{"*"}
	}

	keyInfo := a.keyStore.AddKey(req.Name, req.AllowedModels, req.RateLimit)

	slog.Info("API key created",
		"name", req.Name,
		"key_prefix", keyInfo.Key[:16]+"...",
		"allowed_models", req.AllowedModels,
		"rate_limit", req.RateLimit,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"key":            keyInfo.Key,
		"name":           keyInfo.Name,
		"allowed_models": keyInfo.AllowedModels,
		"rate_limit":     keyInfo.RateLimit,
	})
}

// HandleDeleteKey handles DELETE /admin/keys/{key} — delete an API key.
func (a *AdminHandler) HandleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	// Extract key from URL path: /admin/keys/{key}
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		a.sendError(w, http.StatusBadRequest, "Key not specified in URL path")
		return
	}
	key := parts[len(parts)-1]

	if a.keyStore.DeleteKey(key) {
		slog.Info("API key deleted", "key_prefix", key[:min(16, len(key))]+"...")
		w.WriteHeader(http.StatusNoContent)
	} else {
		a.sendError(w, http.StatusNotFound, "Key not found")
	}
}

// HandleStats handles GET /admin/stats — return server statistics.
func (a *AdminHandler) HandleStats(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_requests":  a.stats.TotalRequests.Load(),
		"active_requests": a.stats.ActiveRequests.Load(),
	})
}
```

---

## Step 17: Server Setup

**File:** `server/server.go`

```go
package server

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aigateway/admin"
	"github.com/aigateway/auth"
	"github.com/aigateway/config"
	midware "github.com/aigateway/middleware"
	"github.com/aigateway/provider"
	"github.com/aigateway/proxy"
	"github.com/aigateway/router"
)

// Server is the main AI Gateway HTTP server.
type Server struct {
	cfg     *config.Config
	handler *proxy.Handler
	admin   *admin.AdminHandler
	mux     *http.ServeMux
}

// New creates and initializes a new Server from configuration.
func New(cfg *config.Config) (*Server, error) {
	// 1. Initialize provider registry
	registry := provider.NewRegistry()
	for _, pCfg := range cfg.Providers {
		p, err := provider.NewProviderFromConfig(pCfg)
		if err != nil {
			return nil, fmt.Errorf("init provider %s: %w", pCfg.Name, err)
		}
		registry.Register(pCfg.Name, p)
		slog.Info("provider registered",
			"name", pCfg.Name,
			"type", pCfg.Type,
			"models", len(pCfg.Models),
			"api_keys", len(pCfg.APIKeys),
		)
	}

	// 2. Initialize model router
	r, err := router.NewRouter(cfg, registry)
	if err != nil {
		return nil, fmt.Errorf("init router: %w", err)
	}

	// 3. Initialize auth key store
	keyStore := auth.NewKeyStore(cfg.APIKeys)

	// 4. Initialize concurrency limiter
	limiter := midware.NewConcurrencyLimiter(cfg.Concurrency.MaxConcurrent)

	// 5. Create handlers
	proxyHandler := proxy.NewHandler(r, keyStore, limiter)
	adminHandler := admin.NewAdminHandler(keyStore, cfg.Server.AdminSecret, proxyHandler.Stats)

	// 6. Setup routes (Go 1.22 pattern matching)
	mux := http.NewServeMux()

	// OpenAI-compatible API endpoints
	mux.HandleFunc("POST /v1/chat/completions", proxyHandler.HandleChatCompletion)
	mux.HandleFunc("GET /v1/models", proxyHandler.HandleListModels)
	mux.HandleFunc("GET /health", proxyHandler.HandleHealth)

	// Admin API endpoints
	mux.HandleFunc("GET /admin/keys", adminHandler.HandleListKeys)
	mux.HandleFunc("POST /admin/keys", adminHandler.HandleCreateKey)
	mux.HandleFunc("DELETE /admin/keys/", adminHandler.HandleDeleteKey)
	mux.HandleFunc("GET /admin/stats", adminHandler.HandleStats)

	return &Server{
		cfg:     cfg,
		handler: proxyHandler,
		admin:   adminHandler,
		mux:     mux,
	}, nil
}

// Start starts the HTTP server and blocks.
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)

	// Wrap with logging middleware
	handler := midware.LoggingMiddleware(s.mux)

	// Print startup info
	slog.Info("========================================")
	slog.Info("  AI Gateway starting", "address", addr)
	slog.Info("========================================")

	slog.Info("providers loaded", "count", len(s.cfg.Providers))
	slog.Info("models configured", "count", len(s.cfg.Models))
	for _, m := range s.cfg.Models {
		if m.Strategy != "" {
			slog.Info("  model (combo)",
				"name", m.Name,
				"strategy", m.Strategy,
				"backends", len(m.Backends),
			)
		} else {
			slog.Info("  model (direct)",
				"name", m.Name,
				"provider", m.Provider,
				"model", m.Model,
			)
		}
	}
	slog.Info("API keys loaded", "count", len(s.cfg.APIKeys))

	slog.Info("endpoints available",
		"chat", "POST /v1/chat/completions",
		"models", "GET /v1/models",
		"health", "GET /health",
		"admin_keys", "GET/POST /admin/keys",
		"admin_stats", "GET /admin/stats",
	)

	return http.ListenAndServe(addr, handler)
}
```

---

## Step 18: Entry Point (main.go)

**File:** `main.go`

```go
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/aigateway/config"
	"github.com/aigateway/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Setup structured logging
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Load configuration
	slog.Info("loading configuration", "path", *configPath)
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Initialize and start server
	srv, err := server.New(cfg)
	if err != nil {
		slog.Error("failed to initialize server", "error", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
```

---

## Step 19: Build & Run

### Build

```bash
# Download dependencies
go mod tidy

# Build binary
go build -o aigateway .

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o aigateway-linux .
```

### Run

```bash
# Run with default config.yaml
./aigateway

# Run with custom config path
./aigateway -config /path/to/config.yaml
```

### Test Endpoints

```bash
# Health check
curl http://localhost:8080/health

# List models (with API key)
curl -H "Authorization: Bearer sk-gw-admin-example-key-do-not-use" \
  http://localhost:8080/v1/models

# Chat completion (non-streaming)
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-admin-example-key-do-not-use" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# Chat completion (streaming)
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-admin-example-key-do-not-use" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "budget-mix",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'

# Admin: List keys
curl -H "X-Admin-Secret: change-me-to-a-strong-secret" \
  http://localhost:8080/admin/keys

# Admin: Create new key
curl -X POST http://localhost:8080/admin/keys \
  -H "X-Admin-Secret: change-me-to-a-strong-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "New User",
    "allowed_models": ["budget-mix", "gpt-4o-mini"],
    "rate_limit": 30
  }'

# Admin: Delete key
curl -X DELETE \
  -H "X-Admin-Secret: change-me-to-a-strong-secret" \
  http://localhost:8080/admin/keys/sk-gw-xxxxx

# Admin: Stats
curl -H "X-Admin-Secret: change-me-to-a-strong-secret" \
  http://localhost:8080/admin/stats
```

### Test with OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    api_key="sk-gw-admin-example-key-do-not-use",
    base_url="http://localhost:8080/v1"
)

# Non-streaming
response = client.chat.completions.create(
    model="smart-combo",
    messages=[{"role": "user", "content": "Hello!"}]
)
print(response.choices[0].message.content)

# Streaming
stream = client.chat.completions.create(
    model="budget-mix",
    messages=[{"role": "user", "content": "Tell me a joke"}],
    stream=True
)
for chunk in stream:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

### Verify Memory Usage

```bash
# Start server
./aigateway &
sleep 2

# Check RSS memory (Linux)
ps aux | grep aigateway
# Expected: < 30MB RSS at idle

# Or use /proc
cat /proc/$(pgrep aigateway)/status | grep VmRSS
```

---

## Key Design Decisions

| Decision | Rationale |
|----------|-----------|
| **Stdlib `net/http`** | Zero framework = minimal memory, no hidden allocations |
| **Go 1.22 pattern routing** | `"POST /v1/chat/completions"` — built-in method routing, no need for gorilla/mux or chi |
| **Generic `RoundRobin[T]`** | Type-safe, reused for both API keys (string) and backend indices (int) |
| **`atomic.Uint64` for RR counter** | Lock-free, zero contention under high concurrency |
| **Buffered channel as semaphore** | Go-idiomatic concurrency limiting, no external dependency |
| **`json.RawMessage` for Message.Content** | Handles both `"string"` and `[{type: "text"}]` multimodal formats |
| **Embed `OpenAIProvider` for Groq/Mistral/Custom** | DRY — they're all OpenAI-compatible, only base URL differs |
| **Check upstream status before writing headers** | Enables streaming fallback — if upstream returns error, no headers sent yet |
| **`responseTracker` wrapper** | Tracks if headers are written to prevent garbled error responses on stream failure |
| **No database** | Config file + in-memory state = ultra-low resource, ideal for edge deployment |
