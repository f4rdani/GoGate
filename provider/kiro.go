package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aigateway/models"
	"github.com/google/uuid"
)

// Kiro (AWS CodeWhisperer) wire protocol constants.
// Mirrors 9router's open-sse kiro implementation: the Q surface is primary,
// CodeWhisperer and the legacy runtime gateway are auth-surface fallbacks.
const (
	kiroSurfaceQ      = "https://q.us-east-1.amazonaws.com/generateAssistantResponse"
	kiroSurfaceCW     = "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse"
	kiroSurfaceRT     = "https://runtime.us-east-1.kiro.dev/generateAssistantResponse"
	kiroCWTarget      = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	kiroRefreshURL    = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	kiroModelsQURL    = "https://q.us-east-1.amazonaws.com/ListAvailableModels?origin=AI_EDITOR"
	kiroModelsCWURL   = "https://codewhisperer.us-east-1.amazonaws.com"
	kiroProfilesTgt   = "AmazonCodeWhispererService.ListAvailableProfiles"
	kiroModelsTgt     = "AmazonCodeWhispererService.ListAvailableModels"
	kiroMaxTokens     = 32000
	kiroRefreshSkew   = 5 * time.Minute
	kiroFrameMaxBytes = 24 * 1024 * 1024
)

// kiroSurfaceFallbackStatuses are retried on the next auth surface (9router parity).
func kiroSurfaceFallbackStatus(code int) bool {
	return code == 401 || code == 403 || code == 404
}

// KiroCredentials selects the Kiro auth method: API keys (TokenType API_KEY)
// or a refresh token (OAuth/social/import, auto-refreshed).
// CatalogURL optionally overrides the model-catalog endpoint (mirrors/tests).
type KiroCredentials struct {
	APIKey       string
	RefreshToken string
	ProfileARN   string
	CatalogURL   string
}

// KiroTokenSource holds a Kiro access token with auto-refresh against the
// Kiro auth service (social/import refresh flow, not standard OAuth2).
type KiroTokenSource struct {
	mu           sync.Mutex
	refreshToken string
	profileARN   string
	refreshURL   string
	client       *http.Client
	// OIDC (Builder ID / IDC) device-flow credentials. When clientID and
	// clientSecret are set, refresh goes to the AWS SSO OIDC endpoint.
	clientID     string
	clientSecret string
	region       string

	access  string
	expiry  time.Time
	profile string // resolved profile ARN cache
}

// NewKiroTokenSource creates a token source from a Kiro refresh token
// (import tokens start with "aorAAAAAG").
func NewKiroTokenSource(refreshToken, profileARN string, client *http.Client) (*KiroTokenSource, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("kiro: refresh_token is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &KiroTokenSource{refreshToken: refreshToken, profileARN: profileARN, refreshURL: kiroRefreshURL, client: client, region: "us-east-1"}, nil
}

// NewKiroOIDCTokenSource creates a token source for AWS SSO OIDC device-flow
// credentials (Builder ID / IDC), refreshed against the OIDC token endpoint.
func NewKiroOIDCTokenSource(refreshToken, profileARN, clientID, clientSecret, region string, client *http.Client) (*KiroTokenSource, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("kiro: refresh_token is required")
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, fmt.Errorf("kiro: OIDC refresh requires client_id and client_secret")
	}
	if region == "" {
		region = "us-east-1"
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &KiroTokenSource{
		refreshToken: refreshToken, profileARN: profileARN,
		clientID: clientID, clientSecret: clientSecret, region: region,
		client: client,
	}, nil
}

// kiroRegion normalizes an AWS region.
func kiroRegion(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		return "us-east-1"
	}
	return region
}

// kiroOIDCBaseOverride redirects OIDC endpoints at a test server.
// Test hook only — production code leaves it empty.
var kiroOIDCBaseOverride = ""

func kiroOIDCEndpoint(region, path string) string {
	if kiroOIDCBaseOverride != "" {
		return kiroOIDCBaseOverride + path
	}
	return fmt.Sprintf("https://oidc.%s.amazonaws.com%s", kiroRegion(region), path)
}

// Token returns a valid access token, refreshing when near expiry.
func (s *KiroTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.access != "" && time.Until(s.expiry) > kiroRefreshSkew {
		return s.access, nil
	}
	return s.access, s.refreshLocked(ctx)
}

func (s *KiroTokenSource) refreshLocked(ctx context.Context) error {
	if s.clientID != "" && s.clientSecret != "" {
		return s.refreshOIDCLocked(ctx)
	}
	url := s.refreshURL
	if url == "" {
		url = kiroRefreshURL
	}
	reqBody, _ := json.Marshal(map[string]string{"refreshToken": s.refreshToken})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("kiro: create refresh request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("kiro: refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("kiro: read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kiro: refresh HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("kiro: decode refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("kiro: refresh response missing accessToken")
	}
	s.access = tok.AccessToken
	if tok.RefreshToken != "" {
		s.refreshToken = tok.RefreshToken
	}
	if tok.ProfileARN != "" {
		s.profile = tok.ProfileARN
	}
	ttl := tok.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	s.expiry = time.Now().Add(time.Duration(ttl) * time.Second)
	return nil
}

// refreshOIDCLocked renews via the AWS SSO OIDC token endpoint.
func (s *KiroTokenSource) refreshOIDCLocked(ctx context.Context) error {
	endpoint := kiroOIDCEndpoint(s.region, "/token")
	reqBody, _ := json.Marshal(map[string]string{
		"clientId":     s.clientID,
		"clientSecret": s.clientSecret,
		"refreshToken": s.refreshToken,
		"grantType":    "refresh_token",
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("kiro: create OIDC refresh request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("kiro: OIDC refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("kiro: read OIDC refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kiro: OIDC refresh HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("kiro: decode OIDC refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("kiro: OIDC refresh response missing accessToken")
	}
	s.access = tok.AccessToken
	if tok.RefreshToken != "" {
		s.refreshToken = tok.RefreshToken
	}
	ttl := tok.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	s.expiry = time.Now().Add(time.Duration(ttl) * time.Second)
	return nil
}

// RefreshNow forces a token renewal regardless of expiry (after a 401).
func (s *KiroTokenSource) RefreshNow(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(ctx); err != nil {
		return "", err
	}
	return s.access, nil
}

// Profile returns the configured or resolved profile ARN ("" when unknown —
// API-key mode must never receive a shared default, per 9router).
func (s *KiroTokenSource) Profile() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.profileARN != "" {
		return s.profileARN
	}
	return s.profile
}

// ==================== Device-code & social login ====================

// Kiro OIDC device-flow constants (mirrors 9router's kiro registry).
const (
	kiroOIDCClientName = "kiro-oauth-client"
	kiroOIDCScopes     = "codewhisperer:completions codewhisperer:analysis codewhisperer:conversations"
	kiroBuilderStartURL = "https://view.awsapps.com/start"
	kiroOIDCIssuerURL   = "https://identitycenter.amazonaws.com/ssoins-722374e8c3c8e6c6"
	kiroSocialAuthBase  = "https://prod.us-east-1.auth.desktop.kiro.dev"
	kiroSocialRedirect  = "kiro://kiro.kiroAgent/authenticate-success"
)

// KiroClientRegistration is an OIDC client registration result.
type KiroClientRegistration struct {
	ClientID     string
	ClientSecret string
}

// KiroDeviceAuth is a pending device authorization for the user to approve.
type KiroDeviceAuth struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int64
	Interval                int64
}

// KiroDevicePoll is one poll attempt result.
type KiroDevicePoll struct {
	Done    bool
	Pending bool
	Tokens  KiroTokens
}

// KiroTokens is a fresh token set from any login flow.
type KiroTokens struct {
	AccessToken  string
	RefreshToken string
	ProfileARN   string
	ExpiresIn    int64
	ClientID     string
	ClientSecret string
}

// KiroRegisterClient registers an OIDC client for the device-code flow.
func KiroRegisterClient(ctx context.Context, client *http.Client, region string) (KiroClientRegistration, error) {
	var out KiroClientRegistration
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	endpoint := kiroOIDCEndpoint(region, "/client/register")
	reqBody, _ := json.Marshal(map[string]interface{}{
		"clientName": kiroOIDCClientName,
		"clientType": "public",
		"scopes":     strings.Split(kiroOIDCScopes, " "),
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  kiroOIDCIssuerURL,
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("kiro: register client: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return out, fmt.Errorf("kiro: register client HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return out, fmt.Errorf("kiro: decode registration: %w", err)
	}
	if data.ClientID == "" || data.ClientSecret == "" {
		return out, fmt.Errorf("kiro: registration missing client credentials")
	}
	out.ClientID, out.ClientSecret = data.ClientID, data.ClientSecret
	return out, nil
}

// KiroStartDeviceAuth starts a device authorization (Builder ID or IDC).
func KiroStartDeviceAuth(ctx context.Context, client *http.Client, region, clientID, clientSecret, startURL string) (KiroDeviceAuth, error) {
	var out KiroDeviceAuth
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if startURL == "" {
		startURL = kiroBuilderStartURL
	}
	endpoint := kiroOIDCEndpoint(region, "/device_authorization")
	reqBody, _ := json.Marshal(map[string]string{
		"clientId": clientID, "clientSecret": clientSecret, "startUrl": startURL,
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("kiro: device authorization: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("kiro: device authorization HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		ExpiresIn               int64  `json:"expiresIn"`
		Interval                int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return out, fmt.Errorf("kiro: decode device authorization: %w", err)
	}
	out = KiroDeviceAuth{data.DeviceCode, data.UserCode, data.VerificationURI, data.VerificationURIComplete, data.ExpiresIn, data.Interval}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return out, nil
}

// KiroPollDeviceToken polls once for device-flow completion.
func KiroPollDeviceToken(ctx context.Context, client *http.Client, region, clientID, clientSecret, deviceCode string) (KiroDevicePoll, error) {
	var out KiroDevicePoll
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	endpoint := kiroOIDCEndpoint(region, "/token")
	reqBody, _ := json.Marshal(map[string]string{
		"clientId": clientID, "clientSecret": clientSecret,
		"deviceCode": deviceCode, "grantType": "urn:ietf:params:oauth:grant-type:device_code",
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("kiro: device token poll: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var data struct {
		Error            string `json:"error"`
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresIn        int64  `json:"expiresIn"`
	}
	_ = json.Unmarshal(body, &data)
	if data.Error == "authorization_pending" || data.Error == "slow_down" {
		out.Pending = true
		return out, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || data.Error != "" || data.AccessToken == "" {
		if data.Error != "" {
			return out, fmt.Errorf("kiro: device flow %s", data.Error)
		}
		return out, fmt.Errorf("kiro: device token HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	out.Done = true
	out.Tokens = KiroTokens{AccessToken: data.AccessToken, RefreshToken: data.RefreshToken, ExpiresIn: data.ExpiresIn, ClientID: clientID, ClientSecret: clientSecret}
	return out, nil
}

// KiroSocialLoginURL builds the Google/GitHub login URL for the manual
// callback flow (Cognito only whitelists the kiro:// redirect).
func KiroSocialLoginURL(idp, codeChallenge, state string) (string, error) {
	var idpName string
	switch strings.ToLower(idp) {
	case "google":
		idpName = "Google"
	case "github":
		idpName = "Github"
	default:
		return "", fmt.Errorf("kiro: social provider must be google or github")
	}
	return fmt.Sprintf("%s/login?idp=%s&redirect_uri=%s&code_challenge=%s&code_challenge_method=S256&state=%s&prompt=select_account",
		kiroSocialAuthBase, idpName, url.QueryEscape(kiroSocialRedirect), url.QueryEscape(codeChallenge), url.QueryEscape(state)), nil
}

// kiroSocialBaseOverride redirects the social exchange at a test server.
// Test hook only — production code leaves it empty.
var kiroSocialBaseOverride = ""

// KiroExchangeSocialCode trades an authorization code (+ PKCE verifier) for tokens.
func KiroExchangeSocialCode(ctx context.Context, client *http.Client, code, codeVerifier string) (KiroTokens, error) {
	var out KiroTokens
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base := kiroSocialAuthBase
	if kiroSocialBaseOverride != "" {
		base = kiroSocialBaseOverride
	}
	reqBody, _ := json.Marshal(map[string]string{
		"code": code, "code_verifier": codeVerifier, "redirect_uri": kiroSocialRedirect,
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/oauth/token", bytes.NewReader(reqBody))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("kiro: social exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("kiro: social exchange HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return out, fmt.Errorf("kiro: decode social exchange: %w", err)
	}
	if data.AccessToken == "" {
		return out, fmt.Errorf("kiro: social exchange missing accessToken")
	}
	out = KiroTokens{AccessToken: data.AccessToken, RefreshToken: data.RefreshToken, ProfileARN: data.ProfileARN, ExpiresIn: data.ExpiresIn}
	return out, nil
}

// ==================== Usage limits ====================

// KiroQuota is one quota bucket (credits, requests, ...).
type KiroQuota struct {
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	ResetAt   string  `json:"reset_at"`
}

// KiroUsage is the parsed quota response.
type KiroUsage struct {
	Plan   string                `json:"plan"`
	Quotas map[string]KiroQuota  `json:"quotas"`
}

// KiroUsageLimits fetches remaining free-tier quota, trying the known usage
// endpoints in order (mirrors 9router). apiKeyMode adds the API_KEY token type.
// kiroUsageEndpointOverride redirects all usage attempts at one test server.
// Test hook only — production code leaves it empty.
var kiroUsageEndpointOverride = ""

func KiroUsageLimits(ctx context.Context, client *http.Client, accessToken string, apiKeyMode bool, profileARN string) (*KiroUsage, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	type attempt struct {
		name   string
		method string
		url    string
		target string
	}
	qs := "isEmailRequired=true&origin=AI_EDITOR&resourceType=AGENTIC_REQUEST"
	cwBase := "https://codewhisperer.us-east-1.amazonaws.com"
	qBase := "https://q.us-east-1.amazonaws.com"
	if kiroUsageEndpointOverride != "" {
		cwBase = kiroUsageEndpointOverride
		qBase = kiroUsageEndpointOverride
	}
	attempts := []attempt{
		{"cw-get", "GET", cwBase + "/getUsageLimits?" + qs, ""},
		{"cw-post", "POST", cwBase, "AmazonCodeWhispererService.GetUsageLimits"},
		{"q-get", "GET", qBase + "/getUsageLimits?" + qs, ""},
	}
	var lastErr error
	for _, a := range attempts {
		var reqBody io.Reader
		if a.method == "POST" {
			payload := map[string]interface{}{"origin": "AI_EDITOR", "resourceType": "AGENTIC_REQUEST"}
			if profileARN != "" {
				payload["profileArn"] = profileARN
			}
			raw, _ := json.Marshal(payload)
			reqBody = bytes.NewReader(raw)
		}
		httpReq, err := http.NewRequestWithContext(ctx, a.method, a.url, reqBody)
		if err != nil {
			lastErr = err
			continue
		}
		httpReq.Header.Set("Authorization", "Bearer "+accessToken)
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.0 KiroIDE")
		if a.target != "" {
			httpReq.Header.Set("Content-Type", "application/x-amz-json-1.0")
			httpReq.Header.Set("x-amz-target", a.target)
		}
		if apiKeyMode {
			httpReq.Header.Set("tokentype", "API_KEY")
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("kiro usage %s: %w", a.name, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("kiro usage %s HTTP %d", a.name, resp.StatusCode)
			continue
		}
		usage, err := parseKiroUsage(body)
		if err != nil {
			lastErr = err
			continue
		}
		return usage, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("kiro usage unreachable")
	}
	return nil, lastErr
}

// FetchKiroUsage resolves credentials and returns remaining quota.
func FetchKiroUsage(ctx context.Context, client *http.Client, cred KiroCredentials) (*KiroUsage, error) {
	if strings.TrimSpace(cred.APIKey) != "" {
		return KiroUsageLimits(ctx, client, strings.TrimSpace(cred.APIKey), true, cred.ProfileARN)
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return nil, fmt.Errorf("kiro: api key or refresh token required")
	}
	src, err := NewKiroTokenSource(cred.RefreshToken, cred.ProfileARN, client)
	if err != nil {
		return nil, err
	}
	tok, err := src.Token(ctx)
	if err != nil {
		return nil, err
	}
	prof := cred.ProfileARN
	if prof == "" {
		prof = src.Profile()
	}
	return KiroUsageLimits(ctx, client, tok, false, prof)
}

// UsageLimits returns remaining quota using this provider's own credentials.
func (k *KiroProvider) UsageLimits(ctx context.Context) (*KiroUsage, error) {
	if k.tokenSource != nil {
		tok, err := k.tokenSource.Token(ctx)
		if err != nil {
			return nil, err
		}
		prof := k.profileARN
		if prof == "" {
			prof = k.tokenSource.Profile()
		}
		return KiroUsageLimits(ctx, k.client, tok, false, prof)
	}
	if len(k.apiKeys) > 0 && strings.TrimSpace(k.apiKeys[0].Key) != "" {
		return KiroUsageLimits(ctx, k.client, k.apiKeys[0].Key, true, k.profileARN)
	}
	return nil, fmt.Errorf("kiro: no credentials for usage lookup")
}

func parseKiroUsage(body []byte) (*KiroUsage, error) {
	var data struct {
		SubscriptionInfo struct {
			SubscriptionTitle string `json:"subscriptionTitle"`
		} `json:"subscriptionInfo"`
		NextDateReset string `json:"nextDateReset"`
		ResetDate     string `json:"resetDate"`
		UsageBreakdownList []struct {
			ResourceType             string  `json:"resourceType"`
			CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
			UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
			FreeTrialInfo *struct {
				CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
				UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
				FreeTrialExpiry           string  `json:"freeTrialExpiry"`
			} `json:"freeTrialInfo"`
		} `json:"usageBreakdownList"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("kiro usage decode: %w", err)
	}
	out := &KiroUsage{Plan: data.SubscriptionInfo.SubscriptionTitle, Quotas: map[string]KiroQuota{}}
	if out.Plan == "" {
		out.Plan = "Kiro"
	}
	reset := data.NextDateReset
	if reset == "" {
		reset = data.ResetDate
	}
	for _, b := range data.UsageBreakdownList {
		key := strings.ToLower(b.ResourceType)
		if key == "" {
			key = "unknown"
		}
		out.Quotas[key] = KiroQuota{Used: b.CurrentUsageWithPrecision, Total: b.UsageLimitWithPrecision, Remaining: b.UsageLimitWithPrecision - b.CurrentUsageWithPrecision, ResetAt: reset}
		if b.FreeTrialInfo != nil {
			out.Quotas[key+"_freetrial"] = KiroQuota{
				Used: b.FreeTrialInfo.CurrentUsageWithPrecision, Total: b.FreeTrialInfo.UsageLimitWithPrecision,
				Remaining: b.FreeTrialInfo.UsageLimitWithPrecision - b.FreeTrialInfo.CurrentUsageWithPrecision,
				ResetAt: func() string {
					if b.FreeTrialInfo.FreeTrialExpiry != "" {
						return b.FreeTrialInfo.FreeTrialExpiry
					}
					return reset
				}(),
			}
		}
	}
	return out, nil
}

// KiroProvider speaks the AWS CodeWhisperer GenerateAssistantResponse API.
type KiroProvider struct {
	*BaseProvider
	tokenSource *KiroTokenSource
	profileARN  string
	surfaces    []string
}

// NewKiroProvider creates a Kiro provider. surfaces defaults to the Q →
// CodeWhisperer → runtime failover chain; tests may override the field.
func NewKiroProvider(base *BaseProvider, tokenSource *KiroTokenSource, profileARN string) *KiroProvider {
	return &KiroProvider{
		BaseProvider: base,
		tokenSource:  tokenSource,
		profileARN:   profileARN,
		surfaces:     []string{kiroSurfaceQ, kiroSurfaceCW, kiroSurfaceRT},
	}
}

// kiroAuthMode resolves credentials for one attempt.
type kiroAuthMode struct {
	apiKey     string // API_KEY mode (rotated per attempt)
	token      string // live oauth access token
	profileARN string
	apiKeyMode bool
}

func kiroHeaders(apiKey string, auth *kiroAuthMode, surface string) map[string]string {
	h := map[string]string{
		"Content-Type":                    "application/json",
		"Accept":                          "application/vnd.amazon.eventstream",
		"User-Agent":                      "AWS-SDK-JS/3.0.0 kiro-ide/1.0.0",
		"X-Amz-User-Agent":                "aws-sdk-js/3.0.0 kiro-ide/1.0.0",
		"x-amzn-kiro-agent-mode":          "spec",
		"x-amzn-codewhisperer-machine-id": "kiro-desktop",
	}
	if strings.Contains(surface, "://codewhisperer.") {
		h["X-Amz-Target"] = kiroCWTarget
	}
	if auth.apiKeyMode {
		h["Authorization"] = "Bearer " + auth.apiKey
		h["TokenType"] = "API_KEY"
	} else {
		h["Authorization"] = "Bearer " + auth.token
		h["x-amz-sso-bearer"] = auth.token
	}
	if auth.profileARN != "" {
		h["x-amzn-codewhisperer-profile-arn"] = auth.profileARN
	}
	return h
}

// stripKiroSuffixes removes 9router-style synthetic suffixes to recover the
// real upstream model id.
func stripKiroSuffixes(model string) string {
	model = strings.TrimSuffix(model, "-agentic")
	model = strings.TrimSuffix(model, "-thinking")
	return model
}

// ==================== OpenAI → Kiro translation ====================

// kiroImage is a Kiro image block.
type kiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

// kiroToolUse is an assistant tool-use block.
type kiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

// kiroToolResult is a user tool-result block.
type kiroToolResult struct {
	ToolUseID string `json:"toolUseId"`
	Status    string `json:"status"`
	Content   []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// sanitizeKiroToolName cleans a tool name to [a-zA-Z0-9_-], deduplicating.
func sanitizeKiroToolName(raw string, index int, used map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = fmt.Sprintf("tool_%d", index+1)
	}
	if len(name) > 64 {
		name = name[:64]
	}
	candidate := name
	for i := 2; used[candidate]; i++ {
		tail := fmt.Sprintf("_%d", i)
		candidate = name
		if len(candidate)+len(tail) > 64 {
			candidate = candidate[:64-len(tail)]
		}
		candidate += tail
	}
	used[candidate] = true
	return candidate
}

// normalizeKiroSchema cleans a JSON schema for Kiro (object root, known props).
func normalizeKiroSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		schema = map[string]interface{}{}
	}
	out := map[string]interface{}{"type": "object"}
	props, _ := schema["properties"].(map[string]interface{})
	if props == nil {
		props = map[string]interface{}{}
	}
	out["properties"] = props
	if req, ok := schema["required"].([]interface{}); ok {
		var kept []string
		for _, r := range req {
			if s, ok := r.(string); ok {
				if _, has := props[s]; has {
					kept = append(kept, s)
				}
			}
		}
		if len(kept) > 0 {
			out["required"] = kept
		}
	}
	return out
}

// kiroToolSpec is one tool specification for the current message context.
type kiroToolSpec struct {
	ToolSpecification struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		InputSchema map[string]interface{} `json:"inputSchema"`
	} `json:"toolSpecification"`
}

// buildKiroToolSpecs normalizes OpenAI/Claude tool definitions to Kiro specs.
func buildKiroToolSpecs(tools json.RawMessage) ([]kiroToolSpec, error) {
	if len(bytes.TrimSpace(tools)) == 0 {
		return nil, nil
	}
	var raw []map[string]interface{}
	if err := json.Unmarshal(tools, &raw); err != nil {
		return nil, fmt.Errorf("invalid tools: %w", err)
	}
	used := map[string]bool{}
	var specs []kiroToolSpec
	for i, t := range raw {
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		params, _ := t["parameters"].(map[string]interface{})
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if n, ok := fn["name"].(string); ok {
				name = n
			}
			if d, ok := fn["description"].(string); ok {
				desc = d
			}
			if p, ok := fn["parameters"].(map[string]interface{}); ok {
				params = p
			}
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		var spec kiroToolSpec
		spec.ToolSpecification.Name = sanitizeKiroToolName(name, i, used)
		spec.ToolSpecification.Description = desc
		if spec.ToolSpecification.Description == "" {
			spec.ToolSpecification.Description = "Tool: " + name
		}
		spec.ToolSpecification.InputSchema = map[string]interface{}{"json": normalizeKiroSchema(params)}
		specs = append(specs, spec)
	}
	return specs, nil
}

// kiroTurn is one normalized conversation turn.
type kiroTurn struct {
	user      map[string]interface{}
	assistant map[string]interface{}
}

// buildKiroBody translates an OpenAI chat request to a Kiro request body.
func buildKiroBody(upstreamModel string, req *models.ChatCompletionRequest) (map[string]interface{}, error) {
	type pending struct {
		texts       []string
		images      []kiroImage
		toolResults []kiroToolResult
	}
	var userTexts []string
	var userImages []kiroImage
	var userResults []kiroToolResult
	var asstTexts []string
	var asstTools []kiroToolUse

	flushUser := func(turns *[]map[string]interface{}) {
		content := strings.TrimSpace(strings.Join(userTexts, "\n\n"))
		if content == "" && len(userResults) == 0 && len(userImages) == 0 {
			return
		}
		if content == "" {
			content = "continue"
		}
		uim := map[string]interface{}{"content": content, "modelId": upstreamModel}
		if len(userImages) > 0 {
			uim["images"] = userImages
		}
		if len(userResults) > 0 {
			uim["userInputMessageContext"] = map[string]interface{}{"toolResults": userResults}
		}
		*turns = append(*turns, map[string]interface{}{"userInputMessage": uim})
		userTexts, userImages, userResults = nil, nil, nil
	}
	flushAsst := func(turns *[]map[string]interface{}) {
		content := strings.TrimSpace(strings.Join(asstTexts, "\n\n"))
		if content == "" && len(asstTools) == 0 {
			return
		}
		if content == "" {
			content = "..."
		}
		am := map[string]interface{}{"content": content}
		if len(asstTools) > 0 {
			am["toolUses"] = asstTools
		}
		*turns = append(*turns, map[string]interface{}{"assistantResponseMessage": am})
		asstTexts, asstTools = nil, nil
	}

	var turns []map[string]interface{}
	lastRole := ""
	// System prompt travels inside the first user turn: a top-level
	// systemPrompt is rejected with 400 by the CodeWhisperer surface.
	pendingSystem := []string{}

	for _, msg := range req.Messages {
		role := msg.Role
		if role == "system" {
			pendingSystem = append(pendingSystem, msg.ContentString())
			continue
		}
		if role == "tool" {
			if lastRole == "assistant" {
				// keep pairing: tool results belong to the pending user turn
			} else if lastRole == "user" {
				// consecutive user content stays merged below
			} else {
				flushAsst(&turns)
			}
			var text string
			_ = json.Unmarshal(msg.Content, &text)
			if text == "" {
				text = strings.TrimSpace(string(msg.Content))
			}
			userResults = append(userResults, kiroToolResult{
				ToolUseID: msg.ToolCallID,
				Status:    "success",
				Content:   []struct {
					Text string `json:"text"`
				}{{Text: text}},
			})
			lastRole = "user"
			continue
		}
		if role == "assistant" {
			if lastRole == "user" {
				flushUser(&turns)
			}
			text, parts := kiroAssistantParts(msg.Content)
			if text != "" {
				asstTexts = append(asstTexts, text)
			}
			asstTexts = append(asstTexts, parts...)
			if len(msg.ToolCalls) > 0 {
				var calls []map[string]interface{}
				if err := json.Unmarshal(msg.ToolCalls, &calls); err == nil {
					for _, c := range calls {
						fn, _ := c["function"].(map[string]interface{})
						name, _ := fn["name"].(string)
						id, _ := c["id"].(string)
						if id == "" {
							id = uuid.NewString()
						}
						input := map[string]interface{}{}
						if args, ok := fn["arguments"].(string); ok && args != "" {
							_ = json.Unmarshal([]byte(args), &input)
						}
						if name != "" {
							asstTools = append(asstTools, kiroToolUse{ToolUseID: id, Name: name, Input: input})
						}
					}
				}
			}
			lastRole = "assistant"
			continue
		}
		// user (and anything else treated as user)
		if lastRole == "assistant" {
			flushAsst(&turns)
		}
		text, images := kiroUserParts(msg.Content)
		if text != "" {
			userTexts = append(userTexts, text)
		}
		userImages = append(userImages, images...)
		lastRole = "user"
	}
	if lastRole == "assistant" {
		flushUser(&turns) // tool results may still arrive; keep turn open via flush below
		flushAsst(&turns)
	} else {
		flushUser(&turns)
	}

	// Guarantee alternating turns starting and ending with user.
	turns = normalizeKiroTurns(turns, upstreamModel)
	if len(turns) == 0 {
		return nil, fmt.Errorf("no messages to send")
	}
	current := turns[len(turns)-1]
	history := turns[:len(turns)-1]

	// Prepend collected system prompt to the first user turn.
	if len(pendingSystem) > 0 {
		sysBlock := "<instructions>\n" + strings.Join(pendingSystem, "\n\n") + "\n</instructions>"
		if first := firstUserTurn(history); first != nil {
			first["content"] = sysBlock + "\n\n" + first["content"].(string)
		} else if cuim, ok := current["userInputMessage"].(map[string]interface{}); ok {
			cuim["content"] = sysBlock + "\n\n" + cuim["content"].(string)
		}
	}

	// Attach tool specs to the current message context.
	if len(req.Tools) > 0 {
		specs, err := buildKiroToolSpecs(req.Tools)
		if err != nil {
			return nil, err
		}
		if len(specs) > 0 {
			if cuim, ok := current["userInputMessage"].(map[string]interface{}); ok {
				ctx, _ := cuim["userInputMessageContext"].(map[string]interface{})
				if ctx == nil {
					ctx = map[string]interface{}{}
				}
				ctx["tools"] = specs
				cuim["userInputMessageContext"] = ctx
			}
		}
	}

	body := map[string]interface{}{
		"conversationState": map[string]interface{}{
			"chatTriggerType": "MANUAL",
			"conversationId":  uuid.NewString(),
			"currentMessage":  current,
			"history":         history,
		},
	}
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens != nil {
		infer := map[string]interface{}{}
		if req.MaxTokens != nil {
			infer["maxTokens"] = *req.MaxTokens
		} else {
			infer["maxTokens"] = kiroMaxTokens
		}
		if req.Temperature != nil {
			infer["temperature"] = *req.Temperature
		}
		if req.TopP != nil {
			infer["topP"] = *req.TopP
		}
		body["inferenceConfig"] = infer
	}
	return body, nil
}

// kiroUserParts extracts text and images from a user message.
func kiroUserParts(content json.RawMessage) (string, []kiroImage) {
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text, nil
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(content, &parts); err != nil {
		return strings.TrimSpace(string(content)), nil
	}
	var texts []string
	var images []kiroImage
	for _, p := range parts {
		t, _ := p["type"].(string)
		switch t {
		case "text", "input_text":
			if s, ok := p["text"].(string); ok {
				texts = append(texts, s)
			}
		case "image_url":
			var u string
			if obj, ok := p["image_url"].(map[string]interface{}); ok {
				u, _ = obj["url"].(string)
			} else {
				u, _ = p["image_url"].(string)
			}
			if strings.HasPrefix(u, "data:") {
				if img, ok := kiroParseDataURI(u); ok {
					images = append(images, img)
				}
			} else if u != "" {
				texts = append(texts, "[Image: "+u+"]")
			}
		case "image":
			if src, ok := p["source"].(map[string]interface{}); ok {
				if data, ok := src["data"].(string); ok && data != "" {
					mt, _ := src["media_type"].(string)
					images = append(images, kiroImageOf(mt, data))
				}
			}
		}
	}
	return strings.Join(texts, "\n"), images
}

// kiroAssistantParts extracts text from an assistant message.
func kiroAssistantParts(content json.RawMessage) (string, []string) {
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text, nil
	}
	return "", nil
}

// kiroParseDataURI parses data:[<mime>];base64,<data>.
func kiroParseDataURI(u string) (kiroImage, bool) {
	if !strings.HasPrefix(u, "data:") {
		return kiroImage{}, false
	}
	comma := strings.Index(u, ",")
	if comma < 0 {
		return kiroImage{}, false
	}
	meta, data := u[5:comma], u[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return kiroImage{}, false
	}
	return kiroImageOf(strings.TrimSuffix(meta, ";base64"), data), data != ""
}

func kiroImageOf(mimeType, data string) kiroImage {
	format := mimeType
	if i := strings.Index(mimeType, "/"); i >= 0 {
		format = mimeType[i+1:]
	}
	if format == "" {
		format = "png"
	}
	var img kiroImage
	img.Format = format
	img.Source.Bytes = data
	return img
}

func firstUserTurn(history []map[string]interface{}) map[string]interface{} {
	for _, t := range history {
		if uim, ok := t["userInputMessage"].(map[string]interface{}); ok {
			return uim
		}
	}
	return nil
}

// normalizeKiroTurns enforces strictly alternating turns starting and ending
// with a user turn (merging consecutive same-role turns).
func normalizeKiroTurns(turns []map[string]interface{}, modelID string) []map[string]interface{} {
	var out []map[string]interface{}
	isUser := func(t map[string]interface{}) bool {
		_, ok := t["userInputMessage"]
		return ok
	}
	mergeInto := func(dst, src map[string]interface{}) {
		if isUser(dst) && isUser(src) {
			d, _ := dst["userInputMessage"].(map[string]interface{})
			s, _ := src["userInputMessage"].(map[string]interface{})
			dc, _ := d["content"].(string)
			sc, _ := s["content"].(string)
			d["content"] = strings.TrimSpace(dc + "\n\n" + sc)
			if sctx, ok := s["userInputMessageContext"]; ok {
				dctx, _ := d["userInputMessageContext"].(map[string]interface{})
				if dctx == nil {
					d["userInputMessageContext"] = sctx
				} else if sm, ok := sctx.(map[string]interface{}); ok {
					if str, ok := sm["toolResults"]; ok {
						dctx["toolResults"] = str
					}
				}
			}
			if imgs, ok := s["images"]; ok {
				d["images"] = imgs
			}
		} else {
			d, _ := dst["assistantResponseMessage"].(map[string]interface{})
			s, _ := src["assistantResponseMessage"].(map[string]interface{})
			dc, _ := d["content"].(string)
			sc, _ := s["content"].(string)
			d["content"] = strings.TrimSpace(dc + "\n\n" + sc)
			if st, ok := s["toolUses"]; ok {
				d["toolUses"] = st
			}
		}
	}
	for _, t := range turns {
		if len(out) > 0 && isUser(out[len(out)-1]) == isUser(t) {
			mergeInto(out[len(out)-1], t)
		} else {
			out = append(out, t)
		}
	}
	mkUser := func(content string) map[string]interface{} {
		return map[string]interface{}{"userInputMessage": map[string]interface{}{"content": content, "modelId": modelID}}
	}
	if len(out) == 0 || !isUser(out[0]) {
		out = append([]map[string]interface{}{mkUser("continue")}, out...)
	}
	if !isUser(out[len(out)-1]) {
		out = append(out, mkUser("continue"))
	}
	return out
}

// ==================== AWS EventStream parsing ====================

// esEvent is one decoded EventStream message.
type esEvent struct {
	messageType string
	eventType   string
	payload     json.RawMessage
}

// esHeaderValueTypes we decode (AWS event stream spec subset).
func esDecodeHeaders(buf []byte) (map[string]string, int, error) {
	out := map[string]string{}
	pos := 0
	for pos < len(buf) {
		if pos+1 > len(buf) {
			return nil, 0, fmt.Errorf("truncated header name length")
		}
		nameLen := int(buf[pos])
		pos++
		if pos+nameLen > len(buf) {
			return nil, 0, fmt.Errorf("truncated header name")
		}
		name := string(buf[pos : pos+nameLen])
		pos += nameLen
		if pos+1 > len(buf) {
			return nil, 0, fmt.Errorf("truncated header value type")
		}
		vtype := buf[pos]
		pos++
		switch vtype {
		case 0:
			out[name] = "true"
		case 1:
			out[name] = "false"
		case 2: // byte
			if pos+1 > len(buf) {
				return nil, 0, fmt.Errorf("truncated byte header")
			}
			out[name] = string(buf[pos : pos+1])
			pos++
		case 3: // short
			if pos+2 > len(buf) {
				return nil, 0, fmt.Errorf("truncated short header")
			}
			out[name] = fmt.Sprint(binary.BigEndian.Uint16(buf[pos:]))
			pos += 2
		case 4: // integer
			if pos+4 > len(buf) {
				return nil, 0, fmt.Errorf("truncated int header")
			}
			out[name] = fmt.Sprint(binary.BigEndian.Uint32(buf[pos:]))
			pos += 4
		case 5: // bytes
			if pos+4 > len(buf) {
				return nil, 0, fmt.Errorf("truncated bytes header length")
			}
			l := int(binary.BigEndian.Uint32(buf[pos:]))
			pos += 4
			if pos+l > len(buf) {
				return nil, 0, fmt.Errorf("truncated bytes header")
			}
			out[name] = string(buf[pos : pos+l])
			pos += l
		case 6: // string
			if pos+2 > len(buf) {
				return nil, 0, fmt.Errorf("truncated string header length")
			}
			l := int(binary.BigEndian.Uint16(buf[pos:]))
			pos += 2
			if pos+l > len(buf) {
				return nil, 0, fmt.Errorf("truncated string header")
			}
			out[name] = string(buf[pos : pos+l])
			pos += l
		case 8: // uuid
			if pos+16 > len(buf) {
				return nil, 0, fmt.Errorf("truncated uuid header")
			}
			pos += 16
			out[name] = "uuid"
		default:
			return nil, 0, fmt.Errorf("unsupported header value type %d", vtype)
		}
	}
	return out, pos, nil
}

// esParseFrame decodes one EventStream frame, returning the event and its total length.
func esParseFrame(data []byte) (esEvent, int, error) {
	var ev esEvent
	if len(data) < 12 {
		return ev, 0, fmt.Errorf("frame too short for prelude")
	}
	totalLen := int(binary.BigEndian.Uint32(data[0:4]))
	headersLen := int(binary.BigEndian.Uint32(data[4:8]))
	if crc32.ChecksumIEEE(data[0:8]) != binary.BigEndian.Uint32(data[8:12]) {
		return ev, 0, fmt.Errorf("eventstream prelude CRC mismatch")
	}
	if totalLen < 16 || headersLen > totalLen-16 || len(data) < totalLen {
		return ev, 0, fmt.Errorf("eventstream frame out of bounds")
	}
	if crc32.ChecksumIEEE(data[0:totalLen-4]) != binary.BigEndian.Uint32(data[totalLen-4:totalLen]) {
		return ev, 0, fmt.Errorf("eventstream message CRC mismatch")
	}
	headers, _, err := esDecodeHeaders(data[12 : 12+headersLen])
	if err != nil {
		return ev, 0, err
	}
	ev.messageType = headers[":message-type"]
	ev.eventType = headers[":event-type"]
	payload := data[12+headersLen : totalLen-4]
	if len(payload) > 0 {
		ev.payload = append([]byte(nil), payload...)
	}
	return ev, totalLen, nil
}

// esParser incrementally parses an EventStream byte flow.
type esParser struct {
	buf []byte
}

// Feed consumes bytes and returns newly completed events.
func (p *esParser) Feed(chunk []byte) ([]esEvent, error) {
	p.buf = append(p.buf, chunk...)
	if len(p.buf) > kiroFrameMaxBytes {
		return nil, fmt.Errorf("eventstream buffer exceeded")
	}
	var out []esEvent
	for len(p.buf) >= 12 {
		totalLen := int(binary.BigEndian.Uint32(p.buf[0:4]))
		if totalLen < 16 || totalLen > kiroFrameMaxBytes {
			return out, fmt.Errorf("eventstream invalid frame length %d", totalLen)
		}
		if len(p.buf) < totalLen {
			break
		}
		ev, _, err := esParseFrame(p.buf[:totalLen])
		if err != nil {
			return out, err
		}
		out = append(out, ev)
		p.buf = append([]byte(nil), p.buf[totalLen:]...)
	}
	return out, nil
}

// ==================== Kiro events → OpenAI ====================

// kiroStreamState accumulates one response turn.
type kiroStreamState struct {
	text         strings.Builder
	toolInputs   map[string]*strings.Builder
	toolNames    map[string]string
	toolOrder    []string
	stopReason   string
	promptTokens int
	completion   int
	sawToolUse   bool
}

func kiroNormalizeStop(reason string, hasTools bool) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	r = strings.ReplaceAll(r, "-", "_")
	switch r {
	case "end_turn", "endturn", "stop", "stop_sequence":
		return "stop"
	case "tool_use", "tooluse":
		return "tool_calls"
	case "max_tokens", "maxtokens", "model_context_window_exceeded":
		return "length"
	case "":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	default:
		return "stop"
	}
}

// kiroApplyEvent folds one EventStream event into streaming state.
// Returns emitted OpenAI SSE payloads (without the "data: " prefix).
func kiroApplyEvent(ev esEvent, st *kiroStreamState) ([]string, error) {
	if ev.messageType == "error" || ev.messageType == "exception" {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(ev.payload, &msg)
		if msg.Message == "" {
			msg.Message = "kiro upstream eventstream error"
		}
		return nil, fmt.Errorf("%s", msg.Message)
	}
	var out []string
	emitText := func(s string) {
		if s == "" {
			return
		}
		st.text.WriteString(s)
		b, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]interface{}{"content": s}}},
		})
		out = append(out, string(b))
	}
	switch ev.eventType {
	case "assistantResponseEvent":
		var p struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(ev.payload, &p) == nil {
			emitText(kiroStripThinking(p.Content))
		}
	case "reasoningContentEvent":
		var p struct {
			Text    string `json:"text"`
			Content string `json:"content"`
		}
		if json.Unmarshal(ev.payload, &p) == nil {
			if p.Text == "" {
				p.Text = p.Content
			}
			if p.Text != "" {
				st.text.WriteString(p.Text)
				b, _ := json.Marshal(map[string]interface{}{
					"choices": []map[string]interface{}{{"delta": map[string]interface{}{"reasoning_content": p.Text}}},
				})
				out = append(out, string(b))
			}
		}
	case "codeEvent":
		var p struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(ev.payload, &p) == nil {
			emitText(p.Content)
		}
	case "toolUseEvent":
		st.sawToolUse = true
		var items []map[string]interface{}
		var single map[string]interface{}
		if json.Unmarshal(ev.payload, &items) == nil && items != nil {
			// array form
		} else if json.Unmarshal(ev.payload, &single) == nil {
			items = []map[string]interface{}{single}
		}
		for _, it := range items {
			name, _ := it["name"].(string)
			id, _ := it["toolUseId"].(string)
			if name == "" || id == "" {
				continue
			}
			if _, ok := st.toolInputs[id]; !ok {
				st.toolInputs[id] = &strings.Builder{}
				st.toolNames[id] = name
				st.toolOrder = append(st.toolOrder, id)
			}
			switch v := it["input"].(type) {
			case string:
				st.toolInputs[id].WriteString(v)
			case map[string]interface{}:
				b, _ := json.Marshal(v)
				st.toolInputs[id].Reset()
				st.toolInputs[id].Write(b)
			}
		}
	case "messageStopEvent":
		var p struct {
			StopReason string `json:"stopReason"`
		}
		_ = json.Unmarshal(ev.payload, &p)
		if p.StopReason != "" {
			st.stopReason = p.StopReason
		}
	case "metadataEvent", "MetadataEvent":
		var p struct {
			StopReason string `json:"stopReason"`
			Metadata   struct {
				StopReason string `json:"stopReason"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(ev.payload, &p)
		if p.StopReason == "" {
			p.StopReason = p.Metadata.StopReason
		}
		if p.StopReason != "" {
			st.stopReason = p.StopReason
		}
	case "metricsEvent":
		var p struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
		}
		if json.Unmarshal(ev.payload, &p) == nil {
			st.promptTokens += p.InputTokens
			st.completion += p.OutputTokens
		}
	}
	return out, nil
}

// kiroStripThinking removes <thinking>...</thinking> segments from text.
func kiroStripThinking(s string) string {
	for {
		start := strings.Index(s, "<thinking>")
		if start < 0 {
			return s
		}
		rest := s[start+len("<thinking>"):]
		if end := strings.Index(rest, "</thinking>"); end >= 0 {
			s = s[:start] + rest[end+len("</thinking>"):]
		} else {
			return s[:start]
		}
	}
}

// kiroToolDeltas emits buffered tool calls as OpenAI tool_calls deltas.
func kiroToolDeltas(st *kiroStreamState) []string {
	var out []string
	for i, id := range st.toolOrder {
		head, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index": i, "id": id, "type": "function",
					"function": map[string]interface{}{"name": st.toolNames[id], "arguments": ""},
				}},
			}}},
		})
		out = append(out, string(head))
		args := st.toolInputs[id].String()
		if args != "" {
			tail, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]interface{}{
						"tool_calls": []map[string]interface{}{
							{"index": i, "function": map[string]interface{}{"arguments": args}},
						},
					}},
				},
			})
			out = append(out, string(tail))
		}
	}
	return out
}

// ==================== Provider ====================

// ChatCompletion sends a non-streaming request to Kiro.
func (k *KiroProvider) ChatCompletion(ctx context.Context, req *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	req.Stream = false
	upstreamModel := stripKiroSuffixes(req.Model)

	body, err := buildKiroBody(upstreamModel, req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal kiro request: %w", err)
	}

	respBody, err := k.doKiro(ctx, req.Model, raw)
	if err != nil {
		return nil, err
	}
	return kiroAssembleResponse(upstreamModel, respBody)
}

// ChatCompletionStream sends a streaming request, translating Kiro EventStream
// frames to OpenAI SSE chunks live.
func (k *KiroProvider) ChatCompletionStream(ctx context.Context, req *models.ChatCompletionRequest, w http.ResponseWriter, flusher http.Flusher) error {
	req.Stream = true
	upstreamModel := stripKiroSuffixes(req.Model)

	body, err := buildKiroBody(upstreamModel, req)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal kiro request: %w", err)
	}

	stream, err := k.openKiroStream(ctx, req.Model, raw)
	if err != nil {
		return err
	}
	defer stream.close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	msgID := "chatcmpl-kiro-" + uuid.NewString()
	created := time.Now().Unix()
	writeChunk := func(delta map[string]interface{}, finish *string, usage *models.Usage) error {
		payload := map[string]interface{}{
			"id": msgID, "object": "chat.completion.chunk", "created": created, "model": upstreamModel,
			"choices": []map[string]interface{}{{
				"index": 0, "delta": delta,
			}},
		}
		if finish != nil {
			payload["choices"].([]map[string]interface{})[0]["finish_reason"] = *finish
		}
		if usage != nil {
			payload["usage"] = map[string]interface{}{
				"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens,
				"total_tokens": usage.TotalTokens,
			}
		}
		b, _ := json.Marshal(payload)
		if _, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n')); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	writeDelta := func(p string) error {
		var decoded map[string]interface{}
		_ = json.Unmarshal([]byte(p), &decoded)
		delta, _ := decoded["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
		return writeChunk(delta, nil, nil)
	}

	st := &kiroStreamState{toolInputs: map[string]*strings.Builder{}, toolNames: map[string]string{}}
	roleSent := false
	buf := make([]byte, 64*1024)
	var readErr error
	for {
		var n int
		n, readErr = stream.body.Read(buf)
		if n > 0 {
			events, err := stream.parser.Feed(buf[:n])
			if err != nil {
				return err
			}
			for _, ev := range events {
				payloads, err := kiroApplyEvent(ev, st)
				if err != nil {
					return &ProviderError{StatusCode: 502, Body: err.Error(), Provider: k.name}
				}
				if len(payloads) > 0 && !roleSent {
					roleSent = true
					if err := writeChunk(map[string]interface{}{"role": "assistant"}, nil, nil); err != nil {
						return nil // client disconnected
					}
				}
				for _, p := range payloads {
					if err := writeDelta(p); err != nil {
						return nil
					}
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if readErr != nil && readErr != io.EOF {
		// Mid-stream transport failure after headers: end gracefully.
		return nil
	}
	for _, d := range kiroToolDeltas(st) {
		if err := writeDelta(d); err != nil {
			return nil
		}
	}
	finish := kiroNormalizeStop(st.stopReason, len(st.toolOrder) > 0)
	usage := &models.Usage{PromptTokens: st.promptTokens, CompletionTokens: st.completion, TotalTokens: st.promptTokens + st.completion}
	if err := writeChunk(map[string]interface{}{}, &finish, usage); err != nil {
		return nil
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
	return nil
}

// Embeddings is not supported by Kiro.
func (k *KiroProvider) Embeddings(ctx context.Context, req *models.EmbeddingsRequest) (*models.EmbeddingsResponse, error) {
	return nil, fmt.Errorf("embeddings not supported by Kiro")
}

// kiroHTTPStream is an open Kiro EventStream response.
type kiroHTTPStream struct {
	body   io.ReadCloser
	parser *esParser
}

func (s *kiroHTTPStream) close() {
	if s.body != nil {
		s.body.Close()
	}
}

func (k *KiroProvider) doKiro(ctx context.Context, routeModel string, raw []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < k.keyAttempts(); attempt++ {
		keyObj, err := k.NextAPIKey()
		if err != nil {
			if lastErr != nil {
				return nil, markKeyExhausted(lastErr)
			}
			return nil, &ProviderError{StatusCode: 503, Body: err.Error(), Provider: k.name}
		}
		var auth *kiroAuthMode
		if k.tokenSource != nil {
			tok, err := k.tokenSource.Token(ctx)
			if err != nil {
				return nil, fmt.Errorf("kiro oauth token: %w", err)
			}
			auth = &kiroAuthMode{token: tok, profileARN: k.profileARN}
			if auth.profileARN == "" {
				auth.profileARN = k.tokenSource.Profile()
			}
		} else {
			auth = &kiroAuthMode{apiKey: keyObj.Key, profileARN: k.profileARN, apiKeyMode: true}
		}

		egressProxy := k.checkoutEgress()
		attemptCtx := ctx
		if egressProxy != "" {
			attemptCtx = WithEgressProxy(ctx, egressProxy)
		}

		surfErr := error(nil)
		for _, surface := range k.surfaces {
			httpReq, err := http.NewRequestWithContext(attemptCtx, "POST", surface, bytes.NewReader(raw))
			if err != nil {
				return nil, fmt.Errorf("create request: %w", err)
			}
			for hk, hv := range kiroHeaders(auth.apiKey, auth, surface) {
				httpReq.Header.Set(hk, hv)
			}
			if tok, err := k.oauthBearer(attemptCtx, ""); err != nil {
				return nil, fmt.Errorf("oauth token: %w", err)
			} else if tok != "" {
				httpReq.Header.Set("Authorization", "Bearer "+tok)
			}

			slog.Debug(fmt.Sprintf("ℹ️ [AUTH] Using %s surface: %s (attempt %d)", k.name, surface, attempt+1))
			slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s | key=%s | surface=%s", k.name, routeModel, k.keyLabel(keyObj), surface))
			resp, err := k.client.Do(httpReq)
			slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", k.name, routeModel))
			if err != nil {
				k.reportEgress(egressProxy, true)
				surfErr = fmt.Errorf("do request: %w", err)
				if egressProxy != "" && ctx.Err() == nil {
					break // next key (likely next proxy)
				}
				return nil, surfErr
			}
			k.reportEgress(egressProxy, false)

			if resp.StatusCode != http.StatusOK {
				bod, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
				resp.Body.Close()
				if kiroSurfaceFallbackStatus(resp.StatusCode) {
					surfErr = &ProviderError{StatusCode: resp.StatusCode, Body: string(bod), Provider: k.name}
					continue // next auth surface
				}
				provErr := &ProviderError{StatusCode: resp.StatusCode, Body: string(bod), Provider: k.name}
				if provErr.IsRetryable() {
					if keyObj != nil {
						keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
					}
					lastErr = provErr
					break // next key
				}
				return nil, provErr
			}

			full, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("read response: %w", err)
			}
			return full, nil
		}
		if surfErr != nil {
			// All surfaces rejected the credential shape — try next key/token.
			lastErr = surfErr
			continue
		}
	}
	if lastErr != nil {
		return nil, markKeyExhausted(lastErr)
	}
	return nil, &ProviderError{StatusCode: 503, Body: "no API keys available for provider " + k.name, Provider: k.name}
}

// openKiroStream performs the same attempt loop but returns the open body.
func (k *KiroProvider) openKiroStream(ctx context.Context, routeModel string, raw []byte) (*kiroHTTPStream, error) {
	var lastErr error
	for attempt := 0; attempt < k.keyAttempts(); attempt++ {
		keyObj, err := k.NextAPIKey()
		if err != nil {
			if lastErr != nil {
				return nil, markKeyExhausted(lastErr)
			}
			return nil, &ProviderError{StatusCode: 503, Body: err.Error(), Provider: k.name}
		}
		var auth *kiroAuthMode
		if k.tokenSource != nil {
			tok, err := k.tokenSource.Token(ctx)
			if err != nil {
				return nil, fmt.Errorf("kiro oauth token: %w", err)
			}
			auth = &kiroAuthMode{token: tok, profileARN: k.profileARN}
			if auth.profileARN == "" {
				auth.profileARN = k.tokenSource.Profile()
			}
		} else {
			auth = &kiroAuthMode{apiKey: keyObj.Key, profileARN: k.profileARN, apiKeyMode: true}
		}

		egressProxy := k.checkoutEgress()
		attemptCtx := ctx
		if egressProxy != "" {
			attemptCtx = WithEgressProxy(ctx, egressProxy)
		}

		var surfErr error
		for _, surface := range k.surfaces {
			httpReq, err := http.NewRequestWithContext(attemptCtx, "POST", surface, bytes.NewReader(raw))
			if err != nil {
				return nil, fmt.Errorf("create request: %w", err)
			}
			for hk, hv := range kiroHeaders(auth.apiKey, auth, surface) {
				httpReq.Header.Set(hk, hv)
			}
			if tok, err := k.oauthBearer(attemptCtx, ""); err != nil {
				return nil, fmt.Errorf("oauth token: %w", err)
			} else if tok != "" {
				httpReq.Header.Set("Authorization", "Bearer "+tok)
			}

			slog.Info(fmt.Sprintf("[PENDING] START | provider=%s | model=%s | key=%s | surface=%s", k.name, routeModel, k.keyLabel(keyObj), surface))
			resp, err := k.client.Do(httpReq)
			slog.Info(fmt.Sprintf("[PENDING] END | provider=%s | model=%s", k.name, routeModel))
			if err != nil {
				k.reportEgress(egressProxy, true)
				surfErr = fmt.Errorf("do request: %w", err)
				if egressProxy != "" && ctx.Err() == nil {
					break
				}
				return nil, surfErr
			}
			k.reportEgress(egressProxy, false)

			if resp.StatusCode != http.StatusOK {
				bod, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
				resp.Body.Close()
				if kiroSurfaceFallbackStatus(resp.StatusCode) {
					surfErr = &ProviderError{StatusCode: resp.StatusCode, Body: string(bod), Provider: k.name}
					continue
				}
				provErr := &ProviderError{StatusCode: resp.StatusCode, Body: string(bod), Provider: k.name}
				if provErr.IsRetryable() {
					if keyObj != nil {
						keyObj.DisabledUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
					}
					lastErr = provErr
					break
				}
				return nil, provErr
			}
			return &kiroHTTPStream{body: resp.Body, parser: &esParser{}}, nil
		}
		if surfErr != nil {
			lastErr = surfErr
			continue
		}
	}
	if lastErr != nil {
		return nil, markKeyExhausted(lastErr)
	}
	return nil, &ProviderError{StatusCode: 503, Body: "no API keys available for provider " + k.name, Provider: k.name}
}

// kiroAssembleResponse parses a full EventStream body into a chat response.
func kiroAssembleResponse(upstreamModel string, data []byte) (*models.ChatCompletionResponse, error) {
	parser := &esParser{}
	events, err := parser.Feed(data)
	if err != nil {
		return nil, &ProviderError{StatusCode: 502, Body: "kiro eventstream parse: " + err.Error(), Provider: "kiro"}
	}
	st := &kiroStreamState{toolInputs: map[string]*strings.Builder{}, toolNames: map[string]string{}}
	for _, ev := range events {
		if _, err := kiroApplyEvent(ev, st); err != nil {
			return nil, &ProviderError{StatusCode: 502, Body: err.Error(), Provider: "kiro"}
		}
	}
	finish := kiroNormalizeStop(st.stopReason, len(st.toolOrder) > 0)
	text := st.text.String()

	var contentJSON json.RawMessage
	if text != "" {
		contentJSON, _ = json.Marshal(text)
	} else if len(st.toolOrder) == 0 {
		contentJSON = json.RawMessage(`""`)
	} else {
		contentJSON = json.RawMessage(`null`)
	}
	var toolCallsJSON json.RawMessage
	if len(st.toolOrder) > 0 {
		calls := make([]map[string]interface{}, 0, len(st.toolOrder))
		for i, id := range st.toolOrder {
			calls = append(calls, map[string]interface{}{
				"index": i, "id": id, "type": "function",
				"function": map[string]interface{}{"name": st.toolNames[id], "arguments": st.toolInputs[id].String()},
			})
		}
		toolCallsJSON, _ = json.Marshal(calls)
	}
	return &models.ChatCompletionResponse{
		ID:      "chatcmpl-kiro-" + uuid.NewString(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   upstreamModel,
		Choices: []models.Choice{{
			Index:        0,
			Message:      &models.Message{Role: "assistant", Content: contentJSON, ToolCalls: toolCallsJSON},
			FinishReason: &finish,
		}},
		Usage: &models.Usage{
			PromptTokens:     st.promptTokens,
			CompletionTokens: st.completion,
			TotalTokens:      st.promptTokens + st.completion,
		},
	}, nil
}

// ==================== Catalog & validation ====================

// KiroListModels returns live model IDs. apiKey mode queries the Amazon Q
// catalog; refresh-token mode refreshes first and queries CodeWhisperer.
func KiroListModels(ctx context.Context, client *http.Client, cred KiroCredentials) ([]string, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(cred.APIKey) != "" {
		catalogURL := strings.TrimSpace(cred.CatalogURL)
		if catalogURL == "" {
			catalogURL = kiroModelsQURL
		}
		httpReq, err := http.NewRequestWithContext(ctx, "GET", catalogURL, nil)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cred.APIKey))
		httpReq.Header.Set("TokenType", "API_KEY")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("User-Agent", "AWS-SDK-JS/3.0.0 kiro-ide/1.0.0")
		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("kiro catalog: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("kiro catalog HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var out struct {
			Models []struct {
				ModelID string `json:"modelId"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("kiro catalog decode: %w", err)
		}
		var ids []string
		for _, m := range out.Models {
			if m.ModelID != "" {
				ids = append(ids, m.ModelID)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("kiro catalog empty")
		}
		return ids, nil
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return nil, fmt.Errorf("kiro: api key or refresh token required")
	}
	src, err := NewKiroTokenSource(cred.RefreshToken, cred.ProfileARN, client)
	if err != nil {
		return nil, err
	}
	tok, err := src.Token(ctx)
	if err != nil {
		return nil, err
	}
	prof := cred.ProfileARN
	if prof == "" {
		prof, _ = kiroResolveProfile(ctx, client, tok)
	}
	payload := map[string]interface{}{"origin": "AI_EDITOR"}
	if prof != "" {
		payload["profileArn"] = prof
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", kiroModelsCWURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-amz-json-1.0")
	httpReq.Header.Set("x-amz-target", kiroModelsTgt)
	httpReq.Header.Set("Authorization", "Bearer "+tok)
	httpReq.Header.Set("Accept", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kiro catalog: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kiro catalog HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Models []struct {
			ModelID string `json:"modelId"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("kiro catalog decode: %w", err)
	}
	var ids []string
	for _, m := range out.Models {
		if m.ModelID != "" {
			ids = append(ids, m.ModelID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("kiro catalog empty")
	}
	return ids, nil
}

// kiroResolveProfile lists CodeWhisperer profiles and picks a region match.
func kiroResolveProfile(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	raw, _ := json.Marshal(map[string]interface{}{"maxResults": 10})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", kiroModelsCWURL, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-amz-json-1.0")
	httpReq.Header.Set("x-amz-target", kiroProfilesTgt)
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("Accept", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kiro profiles HTTP %d", resp.StatusCode)
	}
	var out struct {
		Profiles []struct {
			ARN        string `json:"arn"`
			ProfileARN string `json:"profileArn"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	for _, p := range out.Profiles {
		arn := p.ARN
		if arn == "" {
			arn = p.ProfileARN
		}
		if arn == "" {
			continue
		}
		parts := strings.Split(arn, ":")
		if len(parts) > 3 && parts[3] == "us-east-1" {
			return arn, nil
		}
	}
	if len(out.Profiles) > 0 {
		arn := out.Profiles[0].ARN
		if arn == "" {
			arn = out.Profiles[0].ProfileARN
		}
		return arn, nil
	}
	return "", fmt.Errorf("no kiro profiles available")
}

// KiroTestCompletion runs a minimal completion to validate a model.
// Optional surfaceOverride replaces the default surface chain (tests).
func KiroTestCompletion(ctx context.Context, client *http.Client, cred KiroCredentials, modelID string, surfaceOverride ...string) (string, error) {
	base := &BaseProvider{
		name:         "kiro-test",
		providerType: "kiro",
		client:       client,
		models:       map[string]bool{modelID: true},
	}
	if base.client == nil {
		base.client = &http.Client{Timeout: 60 * time.Second}
	}
	var src *KiroTokenSource
	if strings.TrimSpace(cred.RefreshToken) != "" {
		var err error
		src, err = NewKiroTokenSource(cred.RefreshToken, cred.ProfileARN, base.client)
		if err != nil {
			return "", err
		}
	} else if strings.TrimSpace(cred.APIKey) != "" {
		base.apiKeys = []*UpstreamKey{{Key: strings.TrimSpace(cred.APIKey)}}
	} else {
		return "", fmt.Errorf("kiro: api key or refresh token required")
	}
	kp := NewKiroProvider(base, src, cred.ProfileARN)
	if len(surfaceOverride) > 0 {
		kp.surfaces = surfaceOverride
	}
	resp, err := kp.ChatCompletion(ctx, &models.ChatCompletionRequest{
		Model:    modelID,
		Messages: []models.Message{{Role: "user", Content: json.RawMessage(`"Say OK"`)}},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		return "(empty)", nil
	}
	return resp.Choices[0].Message.ContentString(), nil
}
