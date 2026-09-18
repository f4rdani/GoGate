package relay

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aigateway/config"
	"github.com/aigateway/provider"
)

// ProxyEntry represents a verified public or manual proxy in the pool.
type ProxyEntry struct {
	URL         string        `json:"url"`
	Latency     time.Duration `json:"latency"`
	LatencyMs   int64         `json:"latency_ms"`
	LastChecked time.Time     `json:"last_checked"`
	Failures    atomic.Int32  `json:"failures"`
	IsManual    bool          `json:"is_manual,omitempty"`
}

// ProxyPoolStats holds snapshot metrics of the proxy pool.
type ProxyPoolStats struct {
	Enabled        bool          `json:"enabled"`
	ActiveCount    int           `json:"active_count"`
	HealthyCount   int           `json:"healthy_count"`
	TotalFound     int           `json:"total_found"`
	TotalScraped   int           `json:"total_scraped"`
	AverageLatency string        `json:"average_latency"`
	LastRefresh    string        `json:"last_refresh"`
	CheckInterval  string        `json:"check_interval"`
	Sources        []string      `json:"sources"`
	TopProxies     []*ProxyEntry `json:"top_proxies,omitempty"`
	Proxies        []*ProxyEntry `json:"proxies,omitempty"`
	ManualCount    int           `json:"manual_count"`
	ManualProxies  []*ProxyEntry `json:"manual_proxies,omitempty"`
}

// ProxyPool manages free public proxies, background validation, and round-robin rotation.
type ProxyPool struct {
	mu            sync.RWMutex
	enabled       bool
	sources       []string
	manualProxies []string
	manualEntries []*ProxyEntry
	checkInterval time.Duration
	checkTimeout  time.Duration
	testURL       string
	maxProxies    int
	proxies       []*ProxyEntry
	counter       atomic.Uint64
	lastRefresh   time.Time
	totalFound    int
	isRefreshing  atomic.Bool
}

// NewProxyPool creates a new proxy pool instance from configuration.
func NewProxyPool(cfg config.ProxyPoolConfig) *ProxyPool {
	sources := cfg.Sources
	if len(sources) == 0 {
		sources = []string{
			"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt",
			"https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/http.txt",
			"https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt",
			"https://raw.githubusercontent.com/clarketm/proxy-list/master/proxy-list-raw.txt",
			"https://raw.githubusercontent.com/sunny9577/proxy-scraper/master/generated/http_proxies.txt",
			"https://raw.githubusercontent.com/roosterkid/openproxylist/main/HTTPS_RAW.txt",
		}
	}

	checkInterval := cfg.CheckInterval
	if checkInterval <= 0 {
		checkInterval = 5 * time.Minute
	}

	checkTimeout := cfg.CheckTimeout
	if checkTimeout <= 0 {
		checkTimeout = 4 * time.Second
	}

	testURL := cfg.TestURL
	if testURL == "" {
		testURL = "https://cloudflare.com/cdn-cgi/trace"
	}

	maxProxies := cfg.MaxProxies
	if maxProxies <= 0 {
		maxProxies = 50
	}

	pool := &ProxyPool{
		enabled:       cfg.Enabled,
		sources:       sources,
		manualProxies: cfg.ManualProxies,
		manualEntries: make([]*ProxyEntry, 0),
		checkInterval: checkInterval,
		checkTimeout:  checkTimeout,
		testURL:       testURL,
		maxProxies:    maxProxies,
		proxies:       make([]*ProxyEntry, 0),
	}

	// Initialize manual proxies from config
	for _, raw := range cfg.ManualProxies {
		norm := config.NormalizeProxyURL(raw)
		if norm == "" {
			continue
		}
		entry := &ProxyEntry{
			URL:         norm,
			Latency:     100 * time.Millisecond,
			LatencyMs:   100,
			LastChecked: time.Now(),
			IsManual:    true,
		}
		pool.manualEntries = append(pool.manualEntries, entry)
		pool.proxies = append(pool.proxies, entry)
	}

	return pool
}

// IsEnabled returns true if the proxy pool is enabled.
func (p *ProxyPool) IsEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enabled
}

// SetEnabled toggles the proxy pool enabled state.
func (p *ProxyPool) SetEnabled(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = enabled
}

// Start launches the background updater and health checker goroutine.
func (p *ProxyPool) Start(ctx context.Context) {
	if !p.enabled {
		return
	}

	slog.Info("free proxy pool rotator enabled",
		"sources", len(p.sources),
		"interval", p.checkInterval.String(),
		"max_proxies", p.maxProxies,
	)

	// Run initial refresh in background
	go func() {
		if err := p.Refresh(ctx); err != nil {
			slog.Warn("initial proxy pool refresh failed", "error", err)
		}
	}()

	// Periodic refresh loop
	go func() {
		ticker := time.NewTicker(p.checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.Refresh(ctx); err != nil {
					slog.Warn("periodic proxy pool refresh failed", "error", err)
				}
			}
		}
	}()
}

// Refresh scrapes public sources and checks health concurrently.
func (p *ProxyPool) Refresh(ctx context.Context) error {
	if !p.isRefreshing.CompareAndSwap(false, true) {
		return fmt.Errorf("refresh already in progress")
	}
	defer p.isRefreshing.Store(false)

	slog.Info("refreshing free proxy pool from public sources...")

	rawProxies := p.fetchRawProxies(ctx)
	if len(rawProxies) == 0 {
		return fmt.Errorf("no proxies could be scraped from sources")
	}

	p.mu.Lock()
	p.totalFound = len(rawProxies)
	p.mu.Unlock()

	// Limit validation candidates to at most 800 to conserve CPU and network
	if len(rawProxies) > 800 {
		rawProxies = rawProxies[:800]
	}

	// Concurrently test proxies using worker pool
	alive := p.checkProxies(ctx, rawProxies)

	// Sort by lowest latency
	sort.Slice(alive, func(i, j int) bool {
		return alive[i].Latency < alive[j].Latency
	})

	// Retain up to maxProxies
	if len(alive) > p.maxProxies {
		alive = alive[:p.maxProxies]
	}

	p.mu.Lock()
	var combined []*ProxyEntry
	// Always prioritize manual proxies at top of pool
	combined = append(combined, p.manualEntries...)
	for _, a := range alive {
		isMan := false
		for _, m := range p.manualEntries {
			if m.URL == a.URL {
				isMan = true
				break
			}
		}
		if !isMan {
			combined = append(combined, a)
		}
	}
	p.proxies = combined
	p.lastRefresh = time.Now()
	p.mu.Unlock()

	slog.Info("proxy pool refresh complete",
		"alive_count", len(combined),
		"scraped_alive", len(alive),
		"manual_count", len(p.manualEntries),
		"total_scraped", p.totalFound,
	)
	return nil
}

// fetchRawProxies fetches and parses IP:Port lines from all configured sources.
func (p *ProxyPool) fetchRawProxies(ctx context.Context) []string {
	var mu sync.Mutex
	var all []string
	seen := make(map[string]bool)

	var wg sync.WaitGroup
	client := &http.Client{Timeout: 10 * time.Second}

	for _, src := range p.sources {
		wg.Add(1)
		go func(sourceURL string) {
			defer wg.Done()

			req, err := http.NewRequestWithContext(ctx, "GET", sourceURL, nil)
			if err != nil {
				return
			}

			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return
			}

			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}

				proxyURL := config.NormalizeProxyURL(line)
				if proxyURL == "" {
					continue
				}

				mu.Lock()
				if !seen[proxyURL] {
					seen[proxyURL] = true
					all = append(all, proxyURL)
				}
				mu.Unlock()
			}
		}(src)
	}

	wg.Wait()
	return all
}

// checkProxies validates a list of raw proxies concurrently.
func (p *ProxyPool) checkProxies(ctx context.Context, candidateURLs []string) []*ProxyEntry {
	var alive []*ProxyEntry
	var mu sync.Mutex

	p.mu.Lock()
	p.proxies = nil
	p.mu.Unlock()

	concurrency := 100
	jobs := make(chan string, len(candidateURLs))
	for _, u := range candidateURLs {
		jobs <- u
	}
	close(jobs)

	ctxCancel, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxyStr := range jobs {
				select {
				case <-ctxCancel.Done():
					return
				default:
				}

				entry, ok := p.testProxy(ctxCancel, proxyStr)
				if ok && entry != nil {
					mu.Lock()
					alive = append(alive, entry)
					p.mu.Lock()
					p.proxies = append(p.proxies, entry)
					p.mu.Unlock()
					if len(alive) >= p.maxProxies {
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	return alive
}

// testProxy probes a single proxy against testURL.
func (p *ProxyPool) testProxy(ctx context.Context, proxyStr string) (*ProxyEntry, bool) {
	proxyURL, err := url.Parse(proxyStr)
	if err != nil {
		return nil, false
	}

	dialer := &net.Dialer{
		Timeout:   p.checkTimeout,
		KeepAlive: 10 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyURL(proxyURL),
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   p.checkTimeout,
		ResponseHeaderTimeout: p.checkTimeout,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   p.checkTimeout,
	}

	start := time.Now()
	targetURL := p.testURL
	if targetURL == "" {
		targetURL = "https://cloudflare.com/cdn-cgi/trace"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return nil, false
	}

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 400 {
		fallbackURL := "https://www.google.com/generate_204"
		if targetURL == fallbackURL {
			fallbackURL = "https://cloudflare.com/cdn-cgi/trace"
		}
		req2, err2 := http.NewRequestWithContext(ctx, "GET", fallbackURL, nil)
		if err2 != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return nil, false
		}
		resp2, err2 := client.Do(req2)
		if err2 != nil || resp2.StatusCode < 200 || resp2.StatusCode >= 400 {
			if resp != nil {
				resp.Body.Close()
			}
			if resp2 != nil {
				resp2.Body.Close()
			}
			return nil, false
		}
		resp2.Body.Close()
	} else {
		resp.Body.Close()
	}

	latency := time.Since(start)
	return &ProxyEntry{
		URL:         proxyStr,
		Latency:     latency,
		LatencyMs:   latency.Milliseconds(),
		LastChecked: time.Now(),
	}, true
}

// Next returns the next active proxy in round-robin order. Returns nil if pool is empty.
func (p *ProxyPool) Next() *ProxyEntry {
	p.mu.RLock()
	total := len(p.proxies)
	p.mu.RUnlock()

	if total == 0 {
		if p.enabled && !p.isRefreshing.Load() {
			go func() {
				_ = p.Refresh(context.Background())
			}()
		}
		return nil
	}

	// Auto-heal if proxies are running low (fewer than 5)
	if total < 5 && p.enabled && !p.isRefreshing.Load() {
		go func() {
			_ = p.Refresh(context.Background())
		}()
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.proxies) == 0 {
		return nil
	}

	idx := p.counter.Add(1) - 1
	return p.proxies[idx%uint64(len(p.proxies))]
}

// Checkout returns a proxy URL for a single upstream attempt, or "" when the
// pool is disabled or empty (caller must connect directly).
func (p *ProxyPool) Checkout() string {
	if !p.IsEnabled() {
		return ""
	}
	if pe := p.Next(); pe != nil {
		return pe.URL
	}
	return ""
}

// Report feeds a per-attempt outcome back to the pool. failed must be true
// only for transport-level failures (the proxy never reached upstream); any
// completed HTTP exchange — even an upstream 5xx — proves the proxy works
// and resets its consecutive-failure counter.
func (p *ProxyPool) Report(proxyURL string, failed bool) {
	if proxyURL == "" {
		return
	}
	if !failed {
		p.mu.RLock()
		var target *ProxyEntry
		for _, pe := range p.proxies {
			if pe.URL == proxyURL {
				target = pe
				break
			}
		}
		p.mu.RUnlock()
		if target != nil {
			target.Failures.Store(0)
		}
		return
	}
	p.MarkFailure(proxyURL)
}

// MarkFailure records an upstream request failure for a proxy. Evicts proxy after 3 consecutive failures.
func (p *ProxyPool) MarkFailure(proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, pe := range p.proxies {
		if pe.URL == proxyURL {
			if pe.IsManual {
				// Record failure on manual proxy, but never evict from pool
				pe.Failures.Add(1)
				return
			}
			if pe.Failures.Add(1) >= 3 {
				// Evict dead proxy
				slog.Info("evicting failing proxy from pool", "proxy", proxyURL)
				p.proxies = append(p.proxies[:i], p.proxies[i+1:]...)
			}
			return
		}
	}
}

// TestProxyDirect tests a proxy URL directly and returns the ProxyEntry with latency or an error.
func (p *ProxyPool) TestProxyDirect(ctx context.Context, proxyStr string) (*ProxyEntry, error) {
	proxyStr = config.NormalizeProxyURL(proxyStr)
	if proxyStr == "" {
		return nil, fmt.Errorf("proxy URL cannot be empty")
	}
	entry, ok := p.testProxy(ctx, proxyStr)
	if !ok {
		return nil, fmt.Errorf("proxy is unreachable or failed health check")
	}
	return entry, nil
}

// AddManualProxy adds a manual proxy to the pool, testing it first.
func (p *ProxyPool) AddManualProxy(ctx context.Context, rawURL string) (*ProxyEntry, error) {
	rawURL = config.NormalizeProxyURL(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("proxy URL cannot be empty")
	}
	if _, err := url.Parse(rawURL); err != nil {
		return nil, fmt.Errorf("invalid proxy URL format: %w", err)
	}

	entry, err := p.TestProxyDirect(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("proxy test failed: %w", err)
	}
	entry.IsManual = true

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if already in manualProxies
	exists := false
	for _, mp := range p.manualProxies {
		if mp == rawURL {
			exists = true
			break
		}
	}
	if !exists {
		p.manualProxies = append(p.manualProxies, rawURL)
	}

	// Update manualEntries
	foundEntry := false
	for i, me := range p.manualEntries {
		if me.URL == rawURL {
			p.manualEntries[i] = entry
			foundEntry = true
			break
		}
	}
	if !foundEntry {
		p.manualEntries = append(p.manualEntries, entry)
	}

	// Add to active pool at the front
	inPool := false
	for i, pe := range p.proxies {
		if pe.URL == rawURL {
			p.proxies[i] = entry
			inPool = true
			break
		}
	}
	if !inPool {
		p.proxies = append([]*ProxyEntry{entry}, p.proxies...)
	}

	return entry, nil
}

// RemoveManualProxy removes a manual proxy from the pool.
func (p *ProxyPool) RemoveManualProxy(rawURL string) bool {
	rawURL = config.NormalizeProxyURL(rawURL)

	p.mu.Lock()
	defer p.mu.Unlock()

	removed := false
	var newManuals []string
	for _, mp := range p.manualProxies {
		if mp == rawURL {
			removed = true
		} else {
			newManuals = append(newManuals, mp)
		}
	}
	p.manualProxies = newManuals

	var newEntries []*ProxyEntry
	for _, me := range p.manualEntries {
		if me.URL != rawURL {
			newEntries = append(newEntries, me)
		}
	}
	p.manualEntries = newEntries

	var newPool []*ProxyEntry
	for _, pe := range p.proxies {
		if pe.URL != rawURL || !pe.IsManual {
			newPool = append(newPool, pe)
		}
	}
	p.proxies = newPool

	return removed
}

// GetManualProxies returns the list of configured manual proxy URLs.
func (p *ProxyPool) GetManualProxies() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	res := make([]string, len(p.manualProxies))
	copy(res, p.manualProxies)
	return res
}

// SetManualProxies initializes the manual proxies list.
func (p *ProxyPool) SetManualProxies(proxies []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manualProxies = proxies
	p.manualEntries = nil
	for _, mp := range proxies {
		entry := &ProxyEntry{
			URL:         mp,
			Latency:     100 * time.Millisecond,
			LatencyMs:   100,
			LastChecked: time.Now(),
			IsManual:    true,
		}
		p.manualEntries = append(p.manualEntries, entry)
	}
}

// Stats returns a snapshot of proxy pool health metrics.
func (p *ProxyPool) Stats() ProxyPoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	totalLatency := time.Duration(0)
	for _, pe := range p.proxies {
		totalLatency += pe.Latency
	}

	avgLatency := "0ms"
	if len(p.proxies) > 0 {
		avgLatency = fmt.Sprintf("%dms", (totalLatency / time.Duration(len(p.proxies))).Milliseconds())
	}

	lastRef := "Never"
	if !p.lastRefresh.IsZero() {
		lastRef = time.Since(p.lastRefresh).Round(time.Second).String() + " ago"
	}

	var top []*ProxyEntry
	for i, pe := range p.proxies {
		if i >= 15 {
			break
		}
		top = append(top, pe)
	}

	var manCopies []*ProxyEntry
	for _, me := range p.manualEntries {
		manCopies = append(manCopies, me)
	}

	return ProxyPoolStats{
		Enabled:        p.enabled,
		ActiveCount:    len(p.proxies),
		HealthyCount:   len(p.proxies),
		TotalFound:     p.totalFound,
		TotalScraped:   p.totalFound,
		AverageLatency: avgLatency,
		LastRefresh:    lastRef,
		CheckInterval:  p.checkInterval.String(),
		Sources:        p.sources,
		TopProxies:     top,
		Proxies:        top,
		ManualCount:    len(p.manualEntries),
		ManualProxies:  manCopies,
	}
}

// DynamicProxyFunc returns a Proxy function suitable for http.Transport.Proxy
// that automatically rotates through healthy proxies on each outgoing connection.
func (p *ProxyPool) DynamicProxyFunc() func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if !p.IsEnabled() {
			return nil, nil // pool disabled, direct connection
		}
		pe := p.Next()
		if pe == nil {
			return nil, nil // fall back to direct connection
		}
		return url.Parse(pe.URL)
	}
}

// ContextProxyFunc returns a Proxy function that prefers the proxy checked
// out for the specific attempt (carried in the request context by the
// provider) so failures can be attributed to the right proxy entry.
// Falls back to round-robin rotation when no checkout is present.
func (p *ProxyPool) ContextProxyFunc() func(*http.Request) (*url.URL, error) {
	fallback := p.DynamicProxyFunc()
	return func(req *http.Request) (*url.URL, error) {
		if u := provider.EgressProxyFromContext(req.Context()); u != "" {
			return url.Parse(u)
		}
		return fallback(req)
	}
}
