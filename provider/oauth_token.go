package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// refreshSkew refreshes the access token this early before expiry.
const refreshSkew = 60 * time.Second

// OAuth2TokenSource holds an OAuth2 access token with automatic
// refresh_token-grant renewal. Safe for concurrent use; concurrent callers
// share a single in-flight refresh.
type OAuth2TokenSource struct {
	mu           sync.Mutex
	tokenURL     string
	clientID     string
	clientSecret string
	refreshToken string
	client       *http.Client

	access string
	expiry time.Time
}

// NewOAuth2TokenSource creates a token source. tokenURL and refreshToken are
// required; clientSecret may be empty for public clients.
func NewOAuth2TokenSource(tokenURL, clientID, clientSecret, refreshToken string, client *http.Client) (*OAuth2TokenSource, error) {
	if strings.TrimSpace(tokenURL) == "" {
		return nil, fmt.Errorf("oauth: token_url is required")
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("oauth: refresh_token is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &OAuth2TokenSource{
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		client:       client,
	}, nil
}

// Token returns a valid bearer token, refreshing first when expired or near expiry.
func (s *OAuth2TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.access != "" && time.Until(s.expiry) > refreshSkew {
		return s.access, nil
	}
	if err := s.refreshLocked(ctx); err != nil {
		return "", err
	}
	return s.access, nil
}

func (s *OAuth2TokenSource) refreshLocked(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", s.refreshToken)
	if s.clientID != "" {
		form.Set("client_id", s.clientID)
	}
	if s.clientSecret != "" {
		form.Set("client_secret", s.clientSecret)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("oauth: create refresh request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("oauth: refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("oauth: read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oauth: refresh HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("oauth: decode refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("oauth: refresh response missing access_token")
	}
	if tok.TokenType != "" && !strings.EqualFold(tok.TokenType, "bearer") {
		return fmt.Errorf("oauth: unsupported token type %q", tok.TokenType)
	}
	s.access = tok.AccessToken
	if tok.RefreshToken != "" {
		s.refreshToken = tok.RefreshToken // rotation
	}
	ttl := tok.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	s.expiry = time.Now().Add(time.Duration(ttl) * time.Second)
	return nil
}
