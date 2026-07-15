package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

// bufPool is a shared buffer pool to reduce GC pressure from io.ReadAll calls.
var bufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 32*1024) // 32KB initial capacity
		return &buf
	},
}

// streamBufPool is for streaming read buffers.
var streamBufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 4096)
		return &buf
	},
}

// ReadAllPooled reads all bytes from r using a pooled buffer, reducing allocations.
func ReadAllPooled(r io.Reader) ([]byte, error) {
	bufPtr := bufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]
	defer func() {
		*bufPtr = buf[:0]
		bufPool.Put(bufPtr)
	}()
	result, err := io.ReadAll(io.LimitReader(r, 10<<20)) // 10MB limit
	return result, err
}

// Provider is the interface that all AI providers must implement.
type Provider interface {
	// Name returns the provider's name.
	Name() string
	// ChatCompletion sends a non-streaming chat completion request.
	ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error)
	// ChatCompletionStream sends a streaming chat completion request.
	// It writes SSE data directly to the ResponseWriter in OpenAI format.
	ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error
	// Embeddings sends a non-streaming embeddings request.
	Embeddings(ctx context.Context, req *models.EmbeddingsRequest) (*models.EmbeddingsResponse, error)
	// IsHealthy returns true if the provider is currently healthy.
	IsHealthy() bool
	// SetHealthy sets the health status of the provider.
	SetHealthy(healthy bool)
}

// UpstreamConfigProvider detects if a provider exposes upstream details.
type UpstreamConfigProvider interface {
	ProviderType() string
	BaseURL() string
	Client() *http.Client
	APIKeys() []*UpstreamKey
}

// UpstreamKey wraps an API key with circuit-breaking state.
type UpstreamKey struct {
	Key           string
	DisabledUntil atomic.Int64 // UnixNano timestamp
}

// BaseProvider contains common fields shared by all providers.
type BaseProvider struct {
	name         string
	providerType string
	baseURL      string
	apiKeys      []*UpstreamKey
	counter      atomic.Uint64
	client       *http.Client
	models       map[string]bool
	healthy      atomic.Bool // current health status
}

// Name returns the provider's name.
func (b *BaseProvider) Name() string {
	return b.name
}

// ProviderType returns the provider's type.
func (b *BaseProvider) ProviderType() string {
	return b.providerType
}

// BaseURL returns the provider's base URL.
func (b *BaseProvider) BaseURL() string {
	return b.baseURL
}

// Client returns the provider's HTTP client.
func (b *BaseProvider) Client() *http.Client {
	return b.client
}

// APIKeys returns the list of upstream API keys.
func (b *BaseProvider) APIKeys() []*UpstreamKey {
	return b.apiKeys
}

// NextAPIKey returns the next active API key in round-robin order.
// Returns error if all keys are currently circuit-broken.
func (b *BaseProvider) NextAPIKey() (*UpstreamKey, error) {
	total := uint64(len(b.apiKeys))
	if total == 0 {
		return &UpstreamKey{Key: ""}, nil
	}

	startIdx := b.counter.Add(1) - 1
	now := time.Now().UnixNano()

	for i := uint64(0); i < total; i++ {
		idx := (startIdx + i) % total
		key := b.apiKeys[idx]
		if now >= key.DisabledUntil.Load() {
			return key, nil
		}
	}

	return nil, fmt.Errorf("all %d API keys for provider %s are currently circuit-broken", total, b.name)
}

// SupportsModel checks if the provider supports a given model.
func (b *BaseProvider) SupportsModel(model string) bool {
	return b.models[model]
}

// IsHealthy returns true if the provider is currently healthy.
func (b *BaseProvider) IsHealthy() bool {
	return b.healthy.Load()
}

// SetHealthy sets the health status of the provider.
func (b *BaseProvider) SetHealthy(healthy bool) {
	b.healthy.Store(healthy)
}

// ResolveKeyAndURL parses the key (splitting by colon if cloudflare type)
// and returns the actual API key and target URL to use for the request.
func (b *BaseProvider) ResolveKeyAndURL(rawKey, endpointPath string) (string, string) {
	apiKey := rawKey
	baseURL := b.baseURL

	if b.providerType == "cloudflare" && strings.Contains(rawKey, ":") {
		parts := strings.SplitN(rawKey, ":", 2)
		accountID := parts[0]
		apiKey = parts[1]
		baseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", accountID)
	}

	url := strings.TrimRight(baseURL, "/") + endpointPath
	return apiKey, url
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

	// Transport-level timeouts: connection/TLS/header have limits,
	// but NO overall client timeout so streaming can run indefinitely.
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,  // max time to establish TCP connection
			KeepAlive: 60 * time.Second,  // keepalive probe interval
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,  // max time for TLS handshake
		ResponseHeaderTimeout: 60 * time.Second,  // max time to wait for response headers
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       5 * time.Minute,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if cfg.ProxyURL != "" {
		proxyURL, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("provider %s: invalid proxy_url: %w", cfg.Name, err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	keys := make([]*UpstreamKey, len(cfg.APIKeys))
	for i, k := range cfg.APIKeys {
		keys[i] = &UpstreamKey{Key: k}
	}

	if cfg.Type == "cloudflare" {
		if cfg.AccountID == "" {
			return nil, fmt.Errorf("provider %s: cloudflare requires account_id", cfg.Name)
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", cfg.AccountID)
		}
	}

	base := BaseProvider{
		name:         cfg.Name,
		providerType: cfg.Type,
		baseURL:      cfg.BaseURL,
		apiKeys:      keys,
		client:       &http.Client{Transport: transport}, // no Timeout — streaming-safe
		models:       modelSet,
	}
	// Providers start healthy by default
	base.healthy.Store(true)

	switch cfg.Type {
	case "openai":
		return NewOpenAIProvider(&base), nil
	case "cloudflare":
		return NewOpenAIProvider(&base), nil
	case "cohere":
		return NewOpenAIProvider(&base), nil
	case "opencode":
		return NewOpenAIProvider(&base), nil
	case "cerebras":
		return NewOpenAIProvider(&base), nil
	case "mimo":
		return NewOpenAIProvider(&base), nil
	case "anthropic":
		return NewAnthropicProvider(&base), nil
	case "groq":
		return NewGroqProvider(&base), nil
	case "mistral":
		return NewMistralProvider(&base), nil
	case "custom":
		return NewCustomProvider(&base), nil
	default:
		return nil, fmt.Errorf("unknown provider type: %s", cfg.Type)
	}
}

// Global dynamic models cache for fallback combo routing
var (
	dynamicModelsCache   = make(map[string][]string)
	dynamicModelsCacheMu sync.RWMutex
)

// SetCachedDynamicModels updates the package-level cache for dynamic models of a provider.
func SetCachedDynamicModels(providerName string, models []string) {
	dynamicModelsCacheMu.Lock()
	defer dynamicModelsCacheMu.Unlock()
	dynamicModelsCache[providerName] = models
}

// GetCachedDynamicModels retrieves the cached dynamic models list for a provider.
func GetCachedDynamicModels(providerName string) []string {
	dynamicModelsCacheMu.RLock()
	defer dynamicModelsCacheMu.RUnlock()
	return dynamicModelsCache[providerName]
}
