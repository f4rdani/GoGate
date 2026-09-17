package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchUpstreamModelsOpenAIFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("expected /models path, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()

	list, err := FetchUpstreamModels(context.Background(), nil, srv.URL, "k", "openai")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(list) != 2 || list[0] != "m1" || list[1] != "m2" {
		t.Fatalf("unexpected catalog: %v", list)
	}
}

func TestFetchUpstreamModelsArrayFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"a"},{"id":"b"}]`))
	}))
	defer srv.Close()

	list, err := FetchUpstreamModels(context.Background(), nil, srv.URL+"/v1/", "", "custom")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("unexpected catalog: %v", list)
	}
}

func TestFetchUpstreamModelsCloudflare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/search" {
			t.Errorf("expected /models/search path, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"result":[{"name":"@cf/a"},{"name":"@cf/b"}]}`))
	}))
	defer srv.Close()

	list, err := FetchUpstreamModels(context.Background(), nil, srv.URL+"/v1", "k", "cloudflare")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(list) != 2 || list[0] != "@cf/a" {
		t.Fatalf("unexpected catalog: %v", list)
	}
}

func TestFetchUpstreamModelsAnthropicError(t *testing.T) {
	_, err := FetchUpstreamModels(context.Background(), nil, "https://api.anthropic.com", "k", "anthropic")
	if err == nil {
		t.Fatal("expected error for anthropic (no catalog endpoint)")
	}
}

func TestFetchUpstreamModelsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	_, err := FetchUpstreamModels(context.Background(), nil, srv.URL, "bad", "openai")
	if err == nil {
		t.Fatal("expected error on 401")
	}
}
