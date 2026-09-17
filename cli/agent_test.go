package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aigateway/config"
)

func writeAgentCfg(t *testing.T) string {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Providers: []config.ProviderConfig{
			{Name: "p1", Type: "openai", BaseURL: "http://x", APIKeys: []string{"k"}, Models: []string{"m1"}},
		},
		Models: []config.ModelConfig{
			{Name: "m1", Provider: "p1", Model: "m1"},
			{Name: "combo", Strategy: "round-robin", Backends: []config.BackendConfig{{Provider: "p1", Model: "m1"}, {Provider: "p1", Model: "m1"}}},
		},
		APIKeys: []config.APIKeyConfig{{Key: "sk-1", Name: "n"}},
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.SaveConfig(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCmdVersion(t *testing.T) {
	old := Version
	Version = "v9.9.9-test"
	defer func() { Version = old }()
	if code := CmdVersion([]string{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := CmdVersion([]string{"--bogus"}); code != 2 {
		t.Fatalf("bad flag must exit 2, got %d", code)
	}
}

func TestCmdModelsProviders(t *testing.T) {
	path := writeAgentCfg(t)
	if code := CmdModels([]string{"--config", path}); code != 0 {
		t.Fatalf("models exit %d", code)
	}
	if code := CmdModels([]string{"--config", path, "--json"}); code != 0 {
		t.Fatalf("models json exit %d", code)
	}
	if code := CmdProviders([]string{"--config", path}); code != 0 {
		t.Fatalf("providers exit %d", code)
	}
	if code := CmdProviders([]string{"--config", path, "--json"}); code != 0 {
		t.Fatalf("providers json exit %d", code)
	}
	if code := CmdModels([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml")}); code != 1 {
		t.Fatalf("missing config must exit 1, got %d", code)
	}
	if code := CmdProviders([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml"), "--json"}); code != 1 {
		t.Fatalf("missing config json must exit 1, got %d", code)
	}
}

func TestCollectShapes(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "b", Type: "openai"}, {Name: "a", Type: "groq", APIKeys: []string{"k"}}},
		Models:    []config.ModelConfig{{Name: "z"}, {Name: "a"}},
	}
	models := collectModels(cfg)
	if len(models) != 2 || models[0].Name != "a" {
		t.Fatalf("models must sort: %+v", models)
	}
	provs := collectProviders(cfg)
	if len(provs) != 2 || provs[0].Name != "a" || !provs[0].HasCreds || provs[1].HasCreds {
		t.Fatalf("providers must sort + flag creds: %+v", provs)
	}
}

func TestCmdDoctor(t *testing.T) {
	path := writeAgentCfg(t)

	// Healthy gateway mock.
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer healthy.Close()

	if code := CmdDoctor([]string{"--config", path, "--gateway", healthy.URL, "--json"}); code != 0 {
		t.Fatalf("healthy doctor must exit 0, got %d", code)
	}
	// Dead gateway → exit 1.
	if code := CmdDoctor([]string{"--config", path, "--gateway", "http://127.0.0.1:1"}); code != 1 {
		t.Fatalf("dead gateway must exit 1, got %d", code)
	}
	// Missing config → exit 1.
	if code := CmdDoctor([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml")}); code != 1 {
		t.Fatalf("missing config must exit 1, got %d", code)
	}
}

func TestCmdChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"content":"hello agent"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			return
		}
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"output":[{"type":"message","content":[{"text":"hi resp"}]}],"usage":{"input_tokens":2,"output_tokens":2}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	base := []string{"--gateway", srv.URL, "--api-key", "sk-x", "--model", "m", "--prompt", "hi"}
	if code := CmdChat(append(append([]string{}, base...), "--json")); code != 0 {
		t.Fatalf("chat json exit %d", code)
	}
	if code := CmdChat(append(base, "--api", "bogus")); code != 2 {
		t.Fatalf("bad api must exit 2, got %d", code)
	}
	if code := CmdChat([]string{"--model", "m"}); code != 2 {
		t.Fatalf("missing prompt must exit 2, got %d", code)
	}
	oldEnv, hadEnv := os.LookupEnv("GATEWAY_API_KEY")
	_ = os.Unsetenv("GATEWAY_API_KEY")
	defer func() {
		if hadEnv {
			_ = os.Setenv("GATEWAY_API_KEY", oldEnv)
		}
	}()
	if code := CmdChat([]string{"--gateway", srv.URL, "--model", "m", "--prompt", "hi"}); code != 2 {
		t.Fatalf("missing key must exit 2, got %d", code)
	}
	// Env key is honored.
	_ = os.Setenv("GATEWAY_API_KEY", "sk-env")
	if code := CmdChat([]string{"--gateway", srv.URL, "--model", "m", "--prompt", "hi"}); code != 0 {
		t.Fatalf("env key must work, got %d", code)
	}
	_ = os.Unsetenv("GATEWAY_API_KEY")
	respArgs := []string{"--gateway", srv.URL, "--api-key", "sk-x", "--model", "m", "--prompt", "hi", "--api", "responses"}
	if code := CmdChat(respArgs); code != 0 {
		t.Fatalf("responses exit %d", code)
	}

	// Upstream error surfaces as exit 1.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("boom"))
	}))
	defer bad.Close()
	if code := CmdChat([]string{"--gateway", bad.URL, "--api-key", "k", "--model", "m", "--prompt", "hi"}); code != 1 {
		t.Fatalf("upstream 500 must exit 1, got %d", code)
	}
}

func TestExtractChatText(t *testing.T) {
	text, usage := extractChatText("chat", []byte(`{"choices":[{"message":{"content":"yo"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	if text != "yo" || !strings.Contains(usage, "in=3") {
		t.Fatalf("chat extract: %q %q", text, usage)
	}
	text, _ = extractChatText("responses", []byte(`{"output":[{"type":"message","content":[{"text":"r2"}]}]}`))
	if text != "r2" {
		t.Fatalf("responses extract: %q", text)
	}
	text, _ = extractChatText("chat", []byte(`not json`))
	if text != "not json" {
		t.Fatalf("garbage must pass through: %q", text)
	}
}

func TestDefaultGateway(t *testing.T) {
	if got := defaultGateway(&config.Config{}); got != "http://localhost:8080" {
		t.Fatalf("default: %q", got)
	}
	cfg := &config.Config{}
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.Port = 9000
	if got := defaultGateway(cfg); got != "http://localhost:9000" {
		t.Fatalf("unspecified bind: %q", got)
	}
}

func TestAgentResultJSON(t *testing.T) {
	printAgentJSON(true, map[string]string{"a": "b"}, "")
	if code := agentFailJSON("x %d", 1); code != 1 {
		t.Fatalf("fail exit: %d", code)
	}
}
