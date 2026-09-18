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
	AccountName string        `json:"account_name,omitempty"`
}

// WebshareAccountView is the API/dashboard view model for a Webshare account.
type WebshareAccountView struct {
	Name           string   `json:"name"`
	IsFreeTier     bool     `json:"is_free_tier"`
	BandwidthLimit int64    `json:"bandwidth_limit"`
	BandwidthUsed  int64    `json:"bandwidth_used"`
	LimitFormatted string   `json:"limit_formatted"`
	UsedFormatted  string   `json:"used_formatted"`
	PercentUsed    float64  `json:"percent_used"`
	CurrentMonth   string   `json:"current_month"`
	ProxiesCount   int      `json:"proxies_count"`
	Proxies        []string `json:"proxies"`
	Status         string   `json:"status"` // "active", "exhausted", "disabled"
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// WebshareAccountState tracks the live in-memory state of a Webshare account.
type WebshareAccountState struct {
	mu             sync.RWMutex
	Name           string       `json:"name"`
	IsFreeTier     bool         `json:"is_free_tier"`
	BandwidthLimit int64        `json:"bandwidth_limit"`
	BandwidthUsed  atomic.Int64 `json:"bandwidth_used"`
	CurrentMonth   string       `json:"current_month"`
	Proxies        []string     `json:"proxies"`
	Status         string       `json:"status"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// ProxyPoolStats holds snapshot metrics of the proxy pool.
type ProxyPoolStats struct {
	Enabled          bool                   `json:"enabled"`
	ActiveCount      int                    `json:"active_count"`
	HealthyCount     int                    `json:"healthy_count"`
	TotalFound       int                    `json:"total_found"`
	TotalScraped     int                    `json:"total_scraped"`
	AverageLatency   string                 `json:"average_latency"`
	LastRefresh      string                 `json:"last_refresh"`
	CheckInterval    string                 `json:"check_interval"`
	Sources          []string               `json:"sources"`
	TopProxies       []*ProxyEntry          `json:"top_proxies,omitempty"`
	Proxies          []*ProxyEntry          `json:"proxies,omitempty"`
	ManualCount      int                    `json:"manual_count"`
	ManualProxies    []*ProxyEntry          `json:"manual_proxies,omitempty"`
	WebshareAccounts []*WebshareAccountView `json:"webshare_accounts,omitempty"`
}

// ProxyPool manages free public proxies, background validation, and round-robin rotation.
type ProxyPool struct {
	mu               sync.RWMutex
	enabled          bool
	sources          []string
	manualProxies    []string
	manualEntries    []*ProxyEntry
	checkInterval    time.Duration
	checkTimeout     time.Duration
	testURL          string
	maxProxies       int
	proxies          []*ProxyEntry
	counter          atomic.Uint64
	lastRefresh      time.Time
	totalFound       int
	isRefreshing     atomic.Bool
	webshareAccounts map[string]*WebshareAccountState
	proxyToAccount   map[string]string
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
		enabled:          cfg.Enabled,
		sources:          sources,
		manualProxies:    cfg.ManualProxies,
		manualEntries:    make([]*ProxyEntry, 0),
		checkInterval:    checkInterval,
		checkTimeout:     checkTimeout,
		testURL:          testURL,
		maxProxies:       maxProxies,
		proxies:          make([]*ProxyEntry, 0),
		webshareAccounts: make(map[string]*WebshareAccountState),
		proxyToAccount:   make(map[string]string),
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

	// Initialize Webshare accounts with monthly bandwidth guard
	nowMonth := time.Now().Format("2006-01")
	for _, wsCfg := range cfg.WebshareAccounts {
		wsCfg.Name = strings.TrimSpace(wsCfg.Name)
		if wsCfg.Name == "" {
			continue
		}
		month := wsCfg.CurrentMonth
		var used int64 = wsCfg.BandwidthUsed
		status := wsCfg.Status
		if month != nowMonth {
			month = nowMonth
			used = 0
			status = "active"
		}
		if status == "" {
			status = "active"
		}
		limit := wsCfg.BandwidthLimit
		if limit <= 0 && wsCfg.IsFreeTier {
			limit = 1_000_000_000 // 1 GB default for free tier
		}

		state := &WebshareAccountState{
			Name:           wsCfg.Name,
			IsFreeTier:     wsCfg.IsFreeTier,
			BandwidthLimit: limit,
			CurrentMonth:   month,
			Status:         status,
			CreatedAt:      wsCfg.CreatedAt,
			UpdatedAt:      time.Now(),
		}
		state.BandwidthUsed.Store(used)

		var cleanProxies []string
		for _, pRaw := range wsCfg.Proxies {
			norm := config.NormalizeProxyURL(pRaw)
			if norm == "" {
				continue
			}
			cleanProxies = append(cleanProxies, norm)
			pool.proxyToAccount[norm] = wsCfg.Name

			// Ensure it exists in manualEntries and proxies with AccountName tagged
			found := false
			for _, me := range pool.manualEntries {
				if me.URL == norm {
					me.AccountName = wsCfg.Name
					found = true
					break
				}
			}
			if !found {
				entry := &ProxyEntry{
					URL:         norm,
					Latency:     100 * time.Millisecond,
					LatencyMs:   100,
					LastChecked: time.Now(),
					IsManual:    true,
					AccountName: wsCfg.Name,
				}
				pool.manualEntries = append(pool.manualEntries, entry)
				pool.proxies = append(pool.proxies, entry)
			}
		}
		state.Proxies = cleanProxies
		pool.webshareAccounts[wsCfg.Name] = state
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

	total = len(p.proxies)
	startIdx := p.counter.Add(1) - 1
	for i := 0; i < total; i++ {
		pe := p.proxies[(startIdx+uint64(i))%uint64(total)]
		if p.isProxyUsableLocked(pe) {
			return pe
		}
	}
	return nil
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
		ManualCount:      len(p.manualEntries),
		ManualProxies:    manCopies,
		WebshareAccounts: p.getWebshareAccountsLocked(),
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

func (p *ProxyPool) isProxyUsableLocked(pe *ProxyEntry) bool {
	if pe == nil {
		return false
	}
	accName := pe.AccountName
	if accName == "" {
		accName = p.proxyToAccount[pe.URL]
	}
	if accName == "" {
		return true
	}

	acc, exists := p.webshareAccounts[accName]
	if !exists || acc == nil {
		return true
	}

	nowMonth := time.Now().Format("2006-01")
	if acc.CurrentMonth != nowMonth {
		acc.CurrentMonth = nowMonth
		acc.BandwidthUsed.Store(0)
		acc.Status = "active"
	}

	if acc.Status == "exhausted" || acc.Status == "disabled" {
		return false
	}

	if acc.BandwidthLimit > 0 && acc.BandwidthUsed.Load() >= acc.BandwidthLimit {
		acc.Status = "exhausted"
		return false
	}

	return true
}

// RecordBandwidth records byte consumption on any Webshare account linked to the proxy.
func (p *ProxyPool) RecordBandwidth(proxyURL string, bytes int64) {
	if bytes <= 0 || proxyURL == "" {
		return
	}
	normURL := config.NormalizeProxyURL(proxyURL)

	p.mu.Lock()
	defer p.mu.Unlock()

	accName, ok := p.proxyToAccount[normURL]
	if !ok {
		accName, ok = p.proxyToAccount[proxyURL]
	}
	if !ok {
		return
	}

	acc, exists := p.webshareAccounts[accName]
	if !exists || acc == nil {
		return
	}

	nowMonth := time.Now().Format("2006-01")
	if acc.CurrentMonth != nowMonth {
		acc.CurrentMonth = nowMonth
		acc.BandwidthUsed.Store(0)
		acc.Status = "active"
	}

	used := acc.BandwidthUsed.Add(bytes)
	acc.UpdatedAt = time.Now()

	if acc.BandwidthLimit > 0 && used >= acc.BandwidthLimit {
		if acc.Status != "exhausted" {
			acc.Status = "exhausted"
			slog.Warn("webshare account monthly bandwidth limit reached; excluding proxies from pool",
				"account", acc.Name,
				"used_bytes", used,
				"limit_bytes", acc.BandwidthLimit,
			)
		}
	}
}

func formatBandwidthBytes(b int64) string {
	const unit = 1000.0
	if b < 1000 {
		return fmt.Sprintf("%d B", b)
	}
	f := float64(b)
	exp := 0
	for f >= unit && exp < 4 {
		f /= unit
		exp++
	}
	suffix := []string{"B", "KB", "MB", "GB", "TB"}[exp]
	return fmt.Sprintf("%.2f %s", f, suffix)
}

func (p *ProxyPool) getWebshareAccountsLocked() []*WebshareAccountView {
	nowMonth := time.Now().Format("2006-01")
	var views []*WebshareAccountView
	for _, acc := range p.webshareAccounts {
		used := acc.BandwidthUsed.Load()
		month := acc.CurrentMonth
		status := acc.Status
		if month != nowMonth {
			month = nowMonth
			used = 0
			status = "active"
		} else if acc.BandwidthLimit > 0 && used >= acc.BandwidthLimit {
			status = "exhausted"
		}

		percent := 0.0
		if acc.BandwidthLimit > 0 {
			percent = (float64(used) / float64(acc.BandwidthLimit)) * 100
			if percent > 100 {
				percent = 100
			}
		}

		limitFmt := "Unlimited"
		if acc.BandwidthLimit > 0 {
			limitFmt = formatBandwidthBytes(acc.BandwidthLimit)
		}

		views = append(views, &WebshareAccountView{
			Name:           acc.Name,
			IsFreeTier:     acc.IsFreeTier,
			BandwidthLimit: acc.BandwidthLimit,
			BandwidthUsed:  used,
			LimitFormatted: limitFmt,
			UsedFormatted:  formatBandwidthBytes(used),
			PercentUsed:    percent,
			CurrentMonth:   month,
			ProxiesCount:   len(acc.Proxies),
			Proxies:        append([]string(nil), acc.Proxies...),
			Status:         status,
			CreatedAt:      acc.CreatedAt,
			UpdatedAt:      acc.UpdatedAt.Format(time.RFC3339),
		})
	}

	sort.Slice(views, func(i, j int) bool {
		return views[i].Name < views[j].Name
	})
	return views
}

// GetWebshareAccounts returns a snapshot list of all tracked Webshare accounts.
func (p *ProxyPool) GetWebshareAccounts() []*WebshareAccountView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.getWebshareAccountsLocked()
}

// GetConfigAccounts returns serialized config representations of Webshare accounts.
func (p *ProxyPool) GetConfigAccounts() []config.WebshareAccountConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var list []config.WebshareAccountConfig
	for _, acc := range p.webshareAccounts {
		list = append(list, config.WebshareAccountConfig{
			Name:           acc.Name,
			IsFreeTier:     acc.IsFreeTier,
			BandwidthLimit: acc.BandwidthLimit,
			BandwidthUsed:  acc.BandwidthUsed.Load(),
			CurrentMonth:   acc.CurrentMonth,
			Proxies:        append([]string(nil), acc.Proxies...),
			Status:         acc.Status,
			CreatedAt:      acc.CreatedAt,
			UpdatedAt:      acc.UpdatedAt.Format(time.RFC3339),
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})
	return list
}

// AddWebshareAccount registers or updates a Webshare account with proxies and monthly bandwidth tracking.
func (p *ProxyPool) AddWebshareAccount(name string, isFreeTier bool, limitBytes int64, rawProxyContent string) (*WebshareAccountView, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("account name cannot be empty")
	}

	if limitBytes <= 0 && isFreeTier {
		limitBytes = 1_000_000_000 // 1 GB default for free tier
	}

	var parsedProxies []string
	scanner := bufio.NewScanner(strings.NewReader(rawProxyContent))
	seen := make(map[string]bool)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		norm := config.NormalizeProxyURL(line)
		if norm != "" && !seen[norm] {
			seen[norm] = true
			parsedProxies = append(parsedProxies, norm)
		}
	}

	if len(parsedProxies) == 0 {
		return nil, fmt.Errorf("no valid proxies found in provided content")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	nowMonth := time.Now().Format("2006-01")
	state, exists := p.webshareAccounts[name]
	if !exists {
		state = &WebshareAccountState{
			Name:           name,
			IsFreeTier:     isFreeTier,
			BandwidthLimit: limitBytes,
			CurrentMonth:   nowMonth,
			Status:         "active",
			CreatedAt:      time.Now().Format(time.RFC3339),
			UpdatedAt:      time.Now(),
		}
		p.webshareAccounts[name] = state
	} else {
		state.IsFreeTier = isFreeTier
		state.BandwidthLimit = limitBytes
		state.Status = "active"
		state.UpdatedAt = time.Now()
	}

	// Remove old proxy mappings for this account if any
	for _, oldP := range state.Proxies {
		delete(p.proxyToAccount, oldP)
	}

	// Add new proxies and map them
	state.Proxies = parsedProxies
	for _, prx := range parsedProxies {
		p.proxyToAccount[prx] = name

		found := false
		for _, me := range p.manualEntries {
			if me.URL == prx {
				me.AccountName = name
				found = true
				break
			}
		}
		if !found {
			entry := &ProxyEntry{
				URL:         prx,
				Latency:     100 * time.Millisecond,
				LatencyMs:   100,
				LastChecked: time.Now(),
				IsManual:    true,
				AccountName: name,
			}
			p.manualEntries = append(p.manualEntries, entry)
			p.proxies = append([]*ProxyEntry{entry}, p.proxies...)
		}
	}

	used := state.BandwidthUsed.Load()
	limitFmt := "Unlimited"
	if limitBytes > 0 {
		limitFmt = formatBandwidthBytes(limitBytes)
	}

	return &WebshareAccountView{
		Name:           state.Name,
		IsFreeTier:     state.IsFreeTier,
		BandwidthLimit: state.BandwidthLimit,
		BandwidthUsed:  used,
		LimitFormatted: limitFmt,
		UsedFormatted:  formatBandwidthBytes(used),
		CurrentMonth:   state.CurrentMonth,
		ProxiesCount:   len(state.Proxies),
		Proxies:        state.Proxies,
		Status:         state.Status,
		CreatedAt:      state.CreatedAt,
		UpdatedAt:      state.UpdatedAt.Format(time.RFC3339),
	}, nil
}

// DeleteWebshareAccount removes a Webshare account and purges its proxies from the pool.
func (p *ProxyPool) DeleteWebshareAccount(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	acc, exists := p.webshareAccounts[name]
	if !exists {
		return false
	}

	for _, prx := range acc.Proxies {
		delete(p.proxyToAccount, prx)
	}

	delete(p.webshareAccounts, name)

	var newManuals []*ProxyEntry
	for _, me := range p.manualEntries {
		if me.AccountName != name {
			newManuals = append(newManuals, me)
		}
	}
	p.manualEntries = newManuals

	var newProxies []*ProxyEntry
	for _, pe := range p.proxies {
		if pe.AccountName != name {
			newProxies = append(newProxies, pe)
		}
	}
	p.proxies = newProxies

	return true
}

// ResetWebshareAccount resets monthly bandwidth usage to zero and re-activates the account.
func (p *ProxyPool) ResetWebshareAccount(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	acc, exists := p.webshareAccounts[name]
	if !exists {
		return false
	}

	acc.BandwidthUsed.Store(0)
	acc.CurrentMonth = time.Now().Format("2006-01")
	acc.Status = "active"
	acc.UpdatedAt = time.Now()
	return true
}

