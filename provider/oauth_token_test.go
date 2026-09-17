package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

func TestOAuth2Validation(t *testing.T) {
	if _, err := NewOAuth2TokenSource("", "c", "", "rt", nil); err == nil {
		t.Fatal("expected error without token_url")
	}
	if _, err := NewOAuth2TokenSource("http://x", "c", "", "", nil); err == nil {
		t.Fatal("expected error without refresh_token")
	}
}

func TestOAuth2RefreshAndCache(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-0" {
			t.Errorf("bad refresh form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok-1","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	src, err := NewOAuth2TokenSource(srv.URL, "cid", "sec", "rt-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tok, err := src.Token(ctx)
	if err != nil || tok != "tok-1" {
		t.Fatalf("expected tok-1, got %q %v", tok, err)
	}
	// Second call must use the cache (no extra HTTP hit).
	tok2, err := src.Token(ctx)
	if err != nil || tok2 != "tok-1" || hits.Load() != 1 {
		t.Fatalf("expected cached token, hits=%d", hits.Load())
	}
}

func TestOAuth2ConcurrentSingleRefresh(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok-c","expires_in":3600}`))
	}))
	defer srv.Close()

	src, _ := NewOAuth2TokenSource(srv.URL, "c", "", "rt", nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := src.Token(context.Background()); err != nil {
				t.Errorf("token failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("concurrent callers must share one refresh, hits=%d", hits.Load())
	}
}

func TestOAuth2RefreshFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	src, _ := NewOAuth2TokenSource(srv.URL, "c", "", "bad", nil)
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}
}

// End-to-end: oauth provider injects the refreshed bearer upstream.
func TestOAuthProviderEndToEnd(t *testing.T) {
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"live-tok","expires_in":3600}`))
	}))
	defer tokSrv.Close()

	var gotAuth string
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upSrv.Close()

	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "oa", Type: "oauth", BaseURL: upSrv.URL,
		TokenURL: tokSrv.URL, ClientID: "c", RefreshToken: "rt",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.ChatCompletion(context.Background(), &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: []byte(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
	if gotAuth != "Bearer live-tok" {
		t.Fatalf("upstream must see refreshed bearer, got %q", gotAuth)
	}
}

func TestOAuthProviderConfigValidation(t *testing.T) {
	if _, err := NewProviderFromConfig(config.ProviderConfig{Name: "x", Type: "oauth", BaseURL: "http://u"}); err == nil {
		t.Fatal("expected error without token_url/refresh_token")
	}
}

// Interface compliance: every provider exposes OAuthToken (nil-safe).
func TestOAuthTokenNilSource(t *testing.T) {
	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "p", Type: "openai", BaseURL: "http://u", APIKeys: []string{"k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	src, ok := p.(interface {
		OAuthToken(context.Context) (string, error)
	})
	if !ok {
		t.Fatal("providers must expose OAuthToken")
	}
	tok, err := src.OAuthToken(context.Background())
	if err != nil || tok != "" {
		t.Fatalf("expected empty token without source, got %q %v", tok, err)
	}
}
