package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
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

// streamBufPool is for streaming read buffers (32KB initial capacity for high throughput SSE).
var streamBufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 32*1024)
		return &buf
	},
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

// EgressPool abstracts a rotating egress proxy pool (implemented by
// relay.ProxyPool). Providers check out one proxy per upstream attempt and
// report the outcome so dead proxies are evicted quickly. Defined here so
// provider and relay stay decoupled (no import cycle).
type EgressPool interface {
	// Checkout returns a proxy URL for a single upstream attempt ("" = direct).
	Checkout() string
	// Report feeds an attempt outcome back: failed=true only for
	// transport-level failures (the proxy never reached upstream).
	Report(proxyURL string, failed bool)
}

// egressProxyCtxKey carries a checked-out proxy URL through request contexts
// to the http.Transport Proxy func.
type egressProxyCtxKey struct{}

// WithEgressProxy stores a checked-out proxy URL in ctx.
func WithEgressProxy(ctx context.Context, proxyURL string) context.Context {
	if proxyURL == "" {
		return ctx
	}
	return context.WithValue(ctx, egressProxyCtxKey{}, proxyURL)
}

// EgressProxyFromContext returns the checked-out proxy URL, if any.
func EgressProxyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if u, ok := ctx.Value(egressProxyCtxKey{}).(string); ok {
		return u
	}
	return ""
}

// BaseProvider contains common fields shared by all providers.
type BaseProvider struct {
	name         string
	providerType string
	baseURL      string
	relayURL     string
	relaySecret  string
	apiKeys      []*UpstreamKey
	counter      atomic.Uint64
	client       *http.Client
	models       map[string]bool
	healthy      atomic.Bool // current health status
	egress       EgressPool  // optional rotating egress proxy pool (nil = direct)
	tokenSource  *OAuth2TokenSource // optional OAuth2 bearer source (nil = static keys)
}

// SetTokenSource attaches an OAuth2 token source. Safe to call once during
// provider wiring before serving starts.
func (b *BaseProvider) SetTokenSource(src *OAuth2TokenSource) {
	b.tokenSource = src
}

// OAuthToken returns a fresh bearer token from the attached OAuth2 source.
// Returns ("", nil) when no OAuth source is configured.
func (b *BaseProvider) OAuthToken(ctx context.Context) (string, error) {
	if b.tokenSource == nil {
		return "", nil
	}
	return b.tokenSource.Token(ctx)
}

// oauthBearer resolves the Authorization bearer for one attempt: a fresh
// OAuth2 token when a source is attached, otherwise the static key (possibly
// empty for keyless providers).
func (b *BaseProvider) oauthBearer(ctx context.Context, staticKey string) (string, error) {
	if b.tokenSource == nil {
		return staticKey, nil
	}
	return b.tokenSource.Token(ctx)
}

// SetEgressPool attaches a rotating egress proxy pool. Safe to call once
// during provider wiring before serving starts.
func (b *BaseProvider) SetEgressPool(pool EgressPool) {
	b.egress = pool
}

// checkoutEgress checks out one proxy URL for a single upstream attempt.
// Returns "" when no pool is attached (direct connection).
func (b *BaseProvider) checkoutEgress() string {
	if b.egress == nil {
		return ""
	}
	return b.egress.Checkout()
}

// reportEgress feeds a transport-level attempt outcome back to the pool.
// Conventional errors from upstream HTTP responses are NOT proxy failures —
// only pass failed=true when the proxy never reached upstream.
func (b *BaseProvider) reportEgress(proxyURL string, failed bool) {
	if b.egress == nil || proxyURL == "" {
		return
	}
	b.egress.Report(proxyURL, failed)
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

// RelayURL returns the provider's relay URL (if set).
func (b *BaseProvider) RelayURL() string {
	return b.relayURL
}

// RelaySecret returns the provider's relay secret header value (if set).
func (b *BaseProvider) RelaySecret() string {
	return b.relaySecret
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

// keyAttempts returns how many upstream keys a single call may try.
// Keyless providers (e.g. opencode free tier) get exactly one attempt.
func (b *BaseProvider) keyAttempts() int {
	if len(b.apiKeys) == 0 {
		return 1
	}
	return len(b.apiKeys)
}

// markKeyExhausted flags a retryable ProviderError as key-exhausted so the
// router fails over to the next backend instead of sleeping through retries
// against circuit-broken keys.
func markKeyExhausted(err error) error {
	if pe, ok := err.(*ProviderError); ok {
		pe.KeyExhausted = true
	}
	return err
}

// IsSaturationError reports whether err is a per-provider concurrency
// saturation error (local limit, not an upstream failure).
func IsSaturationError(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.StatusCode == 429 && strings.Contains(pe.Body, "concurrency saturated")
	}
	return false
}

// BudgetExceeded builds the 429 error used when a provider exhausted its
// monthly token budget. Like saturation it is retryable (fail over) but must
// never be sleep-retried against the same backend.
func BudgetExceeded(providerName string) *ProviderError {
	return &ProviderError{
		StatusCode: 429,
		Body:       fmt.Sprintf("provider %s monthly budget exceeded", providerName),
		Provider:   providerName,
	}
}

// IsBudgetExceeded reports whether err is a monthly-budget error.
func IsBudgetExceeded(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.StatusCode == 429 && strings.Contains(pe.Body, "monthly budget exceeded")
	}
	return false
}

// IsLocalLimit reports whether err is a client-facing local limit
// (concurrency saturation or budget) that must surface as HTTP 429.
func IsLocalLimit(err error) bool {
	return IsSaturationError(err) || IsBudgetExceeded(err)
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

	if b.relayURL != "" {
		baseURL = b.relayURL
	} else if b.providerType == "cloudflare" && strings.Contains(rawKey, ":") {
		parts := strings.SplitN(rawKey, ":", 2)
		accountID := parts[0]
		apiKey = parts[1]
		baseURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", accountID)
	}

	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, endpointPath) {
		return apiKey, baseURL
	}
	return apiKey, baseURL + endpointPath
}

// ForwardUpstream sends a raw upstream request (binary/multipart friendly,
// e.g. audio endpoints) with full key rotation, egress reporting, and
// circuit breaking. The upstream response is returned for ANY HTTP status so
// the caller can proxy it transparently — the caller MUST close the body.
// Transport errors exhaust the key list, then the last error is returned.
func (b *BaseProvider) ForwardUpstream(ctx context.Context, method, endpointPath string, body []byte, contentType string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < b.keyAttempts(); attempt++ {
		keyObj, err := b.NextAPIKey()
		if err != nil {
			if lastErr != nil {
				return nil, markKeyExhausted(lastErr)
			}
			return nil, &ProviderError{StatusCode: 503, Body: err.Error(), Provider: b.name}
		}

		apiKey, targetURL := b.ResolveKeyAndURL(keyObj.Key, endpointPath)

		egressProxy := b.checkoutEgress()
		attemptCtx := ctx
		if egressProxy != "" {
			attemptCtx = WithEgressProxy(ctx, egressProxy)
		}
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		httpReq, err := http.NewRequestWithContext(attemptCtx, method, targetURL, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if contentType != "" {
			httpReq.Header.Set("Content-Type", contentType)
		}
		if apiKey != "" {
			if b.providerType == "anthropic" {
				httpReq.Header.Set("x-api-key", apiKey)
				httpReq.Header.Set("anthropic-version", "2023-06-01")
			} else {
				httpReq.Header.Set("Authorization", "Bearer "+apiKey)
			}
		}
		if b.relaySecret != "" {
			httpReq.Header.Set("X-Relay-Secret", b.relaySecret)
		}
		if b.relayURL != "" && b.baseURL != "" {
			httpReq.Header.Set("X-Target-URL", b.baseURL)
		}
		if b.providerType == "mimo" {
			httpReq.Header.Set("X-Mimo-Source", "mimocode-cli")
		}
		if tok, err := b.oauthBearer(ctx, ""); err != nil {
			return nil, fmt.Errorf("oauth token: %w", err)
		} else if tok != "" {
			httpReq.Header.Set("Authorization", "Bearer "+tok)
		}

		resp, err := b.client.Do(httpReq)
		if err != nil {
			b.reportEgress(egressProxy, true)
			if egressProxy != "" && ctx.Err() == nil {
				lastErr = fmt.Errorf("do request: %w", err)
				continue
			}
			return nil, fmt.Errorf("do request: %w", err)
		}
		b.reportEgress(egressProxy, false)

		// Any 2xx passes through verbatim (201 Created is standard for
		// audio endpoints).
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		bod, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		resp.Body.Close()
		provErr := &ProviderError{StatusCode: resp.StatusCode, Body: string(bod), Provider: b.name}
		if provErr.IsRetryable() {
			keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
			lastErr = provErr
			continue
		}
		// Non-retryable: re-issue is pointless, but the caller wants to
		// proxy the error — rebuild a lightweight response carrying it.
		return &http.Response{
			Status:        fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
			StatusCode:    resp.StatusCode,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(string(bod))),
			ContentLength: int64(len(bod)),
		}, nil
	}

	if lastErr != nil {
		return nil, markKeyExhausted(lastErr)
	}
	return nil, &ProviderError{StatusCode: 503, Body: "no API keys available for provider " + b.name, Provider: b.name}
}

// ProviderError represents an error from an upstream provider.
type ProviderError struct {
	StatusCode int
	Body       string
	Provider   string
	// KeyExhausted is true when the provider tried every available API key and
	// all of them failed with retryable errors. Callers should NOT sleep-retry
	// the same provider (all keys are circuit-broken); fail over instead.
	KeyExhausted bool
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider %s error (status %d): %s", e.Provider, e.StatusCode, e.Body)
}

// IsRetryable returns true if the error is transient and the request can be retried.
func (e *ProviderError) IsRetryable() bool {
	switch e.StatusCode {
	case 408, 425, 429, 500, 502, 503, 504:
		return true
	}
	return false
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
		MaxIdleConns:          250,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if cfg.ProxyURL != "" && cfg.ProxyURL != "auto" && cfg.ProxyURL != "pool" {
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
		relayURL:     cfg.RelayURL,
		relaySecret:  cfg.RelaySecret,
		apiKeys:      keys,
		client:       &http.Client{Transport: transport}, // no Timeout — streaming-safe
		models:       modelSet,
	}
	// Providers start healthy by default
	base.healthy.Store(true)

	if cfg.Type == "oauth" {
		src, err := NewOAuth2TokenSource(cfg.TokenURL, cfg.ClientID, cfg.ClientSecret, cfg.RefreshToken, nil)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", cfg.Name, err)
		}
		base.SetTokenSource(src)
		return NewOpenAIProvider(&base), nil
	}

	if cfg.Type == "kiro" {
		var src *KiroTokenSource
		if strings.TrimSpace(cfg.RefreshToken) != "" {
			var err error
			if strings.TrimSpace(cfg.ClientID) != "" && strings.TrimSpace(cfg.ClientSecret) != "" {
				src, err = NewKiroOIDCTokenSource(cfg.RefreshToken, cfg.ProfileARN, cfg.ClientID, cfg.ClientSecret, cfg.Region, base.client)
			} else {
				src, err = NewKiroTokenSource(cfg.RefreshToken, cfg.ProfileARN, base.client)
			}
			if err != nil {
				return nil, fmt.Errorf("provider %s: %w", cfg.Name, err)
			}
		}
		kp := NewKiroProvider(&base, src, cfg.ProfileARN)
		// An explicit base_url pins a single (self-hosted/compatible) surface;
		// otherwise the Q → CodeWhisperer → runtime failover chain is used.
		if custom := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"); custom != "" && custom != kiroSurfaceQ {
			kp.surfaces = []string{custom}
		}
		return kp, nil
	}

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

// FetchUpstreamModels performs a live GET against an upstream model catalog
// and returns the model IDs. It speaks the OpenAI `{data:[{id}]}` (or `[{id}]`)
// format and Cloudflare's `/models/search` catalog. Anthropic has no catalog
// endpoint and returns an error. Pass ctx for cancellation; client may be nil
// (a default 15s client is used then). An empty apiKey sends no auth header.
func FetchUpstreamModels(ctx context.Context, client *http.Client, baseURL, apiKey, providerType string) ([]string, error) {
	if providerType == "anthropic" {
		return nil, fmt.Errorf("anthropic has no /models catalog endpoint — configure models manually")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	target := strings.TrimRight(baseURL, "/")
	isCloudflare := providerType == "cloudflare"
	if isCloudflare {
		target = strings.Replace(target, "/v1", "/models/search", 1)
	} else if !strings.HasSuffix(target, "/models") {
		target += "/models"
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, fmt.Errorf("create catalog request: %w", err)
	}
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("catalog HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}

	if isCloudflare {
		var cfResp struct {
			Result []struct {
				Name string `json:"name"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &cfResp); err != nil {
			return nil, fmt.Errorf("decode catalog: %w", err)
		}
		var out []string
		for _, m := range cfResp.Result {
			if m.Name != "" {
				out = append(out, m.Name)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("empty model catalog")
		}
		return out, nil
	}

	var listResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listResp); err == nil && len(listResp.Data) > 0 {
		out := make([]string, 0, len(listResp.Data))
		for _, m := range listResp.Data {
			if m.ID != "" {
				out = append(out, m.ID)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	var arrResp []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &arrResp); err == nil && len(arrResp) > 0 {
		out := make([]string, 0, len(arrResp))
		for _, m := range arrResp {
			if m.ID != "" {
				out = append(out, m.ID)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("unrecognized catalog format")
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
