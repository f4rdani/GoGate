package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aigateway/config"
)

// Version is the gateway release version, set by main (ldflags-compatible).
var Version = "dev"

// AgentResult is the JSON envelope for machine-readable agent commands.
type AgentResult struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
}

func printAgentJSON(ok bool, data interface{}, errMsg string) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(AgentResult{OK: ok, Data: data, Error: errMsg})
}

func agentFailJSON(format string, args ...interface{}) int {
	printAgentJSON(false, nil, fmt.Sprintf(format, args...))
	return 1
}

// CmdVersion prints the gateway version: aigateway version [--json].
func CmdVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *asJSON {
		printAgentJSON(true, map[string]string{"version": Version}, "")
		return 0
	}
	fmt.Println(Version)
	return 0
}

type modelRow struct {
	Name      string `json:"name"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Strategy  string `json:"strategy,omitempty"`
	Backends  int    `json:"backends,omitempty"`
	Reasoning bool   `json:"reasoning,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

func collectModels(cfg *config.Config) []modelRow {
	rows := make([]modelRow, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		rows = append(rows, modelRow{
			Name: m.Name, Provider: m.Provider, Model: m.Model,
			Strategy: m.Strategy, Backends: len(m.Backends),
			Reasoning: m.Reasoning, Disabled: m.Disabled,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// CmdModels lists model routes: aigateway models [--config path] [--json].
func CmdModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "config file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		if *asJSON {
			return agentFailJSON("load config: %v", err)
		}
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	rows := collectModels(cfg)
	if *asJSON {
		printAgentJSON(true, rows, "")
		return 0
	}
	for _, r := range rows {
		target := r.Provider + "/" + r.Model
		if r.Strategy != "" {
			target = fmt.Sprintf("%s(%d backends)", r.Strategy, r.Backends)
		}
		disabled := ""
		if r.Disabled {
			disabled = " [disabled]"
		}
		fmt.Printf("%-40s -> %s%s\n", r.Name, target, disabled)
	}
	return 0
}

type providerRow struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Tier      int    `json:"tier"`
	BaseURL   string `json:"base_url"`
	KeyCount  int    `json:"key_count"`
	HasCreds  bool   `json:"has_credentials"`
	Models    int    `json:"models"`
	Disabled  bool   `json:"disabled"`
}

func collectProviders(cfg *config.Config) []providerRow {
	rows := make([]providerRow, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		rows = append(rows, providerRow{
			Name: p.Name, Type: p.Type, Tier: p.Tier, BaseURL: p.BaseURL,
			KeyCount: len(p.APIKeys), HasCreds: p.HasCredentials(),
			Models: len(p.Models), Disabled: p.Disabled,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// CmdProviders lists upstream providers (keys never printed):
// aigateway providers [--config path] [--json].
func CmdProviders(args []string) int {
	fs := flag.NewFlagSet("providers", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "config file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		if *asJSON {
			return agentFailJSON("load config: %v", err)
		}
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	rows := collectProviders(cfg)
	if *asJSON {
		printAgentJSON(true, rows, "")
		return 0
	}
	for _, r := range rows {
		disabled := ""
		if r.Disabled {
			disabled = " [disabled]"
		}
		fmt.Printf("%-16s %-10s tier=%d keys=%d models=%d%s\n  %s\n",
			r.Name, r.Type, r.Tier, r.KeyCount, r.Models, disabled, r.BaseURL)
	}
	return 0
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func runDoctorChecks(cfg *config.Config, gateway string) []doctorCheck {
	checks := []doctorCheck{}

	withCreds := 0
	for _, p := range cfg.Providers {
		if p.HasCredentials() && !p.Disabled {
			withCreds++
		}
	}
	checks = append(checks, doctorCheck{
		Name: "config", OK: true,
		Detail: fmt.Sprintf("%d providers (%d with credentials), %d models, %d api keys",
			len(cfg.Providers), withCreds, len(cfg.Models), len(cfg.APIKeys)),
	})
	if len(cfg.Providers) == 0 {
		checks = append(checks, doctorCheck{Name: "providers", OK: false, Detail: "no providers configured"})
	} else if withCreds == 0 {
		checks = append(checks, doctorCheck{Name: "providers", OK: false, Detail: "no provider has credentials"})
	} else {
		checks = append(checks, doctorCheck{Name: "providers", OK: true, Detail: fmt.Sprintf("%d usable", withCreds)})
	}
	if len(cfg.Models) == 0 {
		checks = append(checks, doctorCheck{Name: "models", OK: false, Detail: "no model routes configured"})
	} else {
		checks = append(checks, doctorCheck{Name: "models", OK: true, Detail: fmt.Sprintf("%d routes", len(cfg.Models))})
	}

	if gateway != "" {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(strings.TrimRight(gateway, "/") + "/health")
		if err != nil {
			checks = append(checks, doctorCheck{Name: "gateway", OK: false, Detail: err.Error()})
		} else {
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
			checks = append(checks, doctorCheck{
				Name: "gateway", OK: resp.StatusCode == 200,
				Detail: fmt.Sprintf("GET %s/health -> %d %s", strings.TrimRight(gateway, "/"), resp.StatusCode, strings.TrimSpace(string(body))),
			})
		}
	}
	return checks
}

// CmdDoctor validates config and probes the gateway:
// aigateway doctor [--config path] [--gateway URL] [--json].
// Exit 0 when every check passes, 1 otherwise.
func CmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "config file")
	gateway := fs.String("gateway", "", "running gateway base URL to probe (e.g. http://localhost:8080)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		if *asJSON {
			return agentFailJSON("load config: %v", err)
		}
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	checks := runDoctorChecks(cfg, *gateway)
	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	if *asJSON {
		printAgentJSON(ok, checks, "")
	} else {
		for _, c := range checks {
			mark := "PASS"
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Printf("[%s] %-10s %s\n", mark, c.Name, c.Detail)
		}
	}
	if !ok {
		return 1
	}
	return 0
}

// CmdChat sends a one-shot completion through a running gateway:
// aigateway chat --model X --prompt "..." [--gateway URL] [--api-key KEY]
// [--system ...] [--max-tokens N] [--api chat|responses] [--json]
// [--timeout 60s]. Prints the assistant text (or full JSON with --json).
func CmdChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	gateway := fs.String("gateway", "http://localhost:8080", "running gateway base URL")
	apiKey := fs.String("api-key", "", "gateway API key (or $GATEWAY_API_KEY)")
	model := fs.String("model", "", "model route name")
	prompt := fs.String("prompt", "", "user prompt")
	system := fs.String("system", "", "system prompt (optional)")
	maxTokens := fs.Int("max-tokens", 0, "max output tokens (0 = default)")
	api := fs.String("api", "chat", "endpoint: chat|responses")
	asJSON := fs.Bool("json", false, "print full response JSON")
	timeout := fs.Duration("timeout", 90*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *model == "" || *prompt == "" {
		fmt.Fprintln(os.Stderr, "chat: --model and --prompt are required")
		fs.Usage()
		return 2
	}
	key := *apiKey
	if key == "" {
		key = os.Getenv("GATEWAY_API_KEY")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "chat: --api-key or $GATEWAY_API_KEY is required")
		return 2
	}

	var path string
	var payload interface{}
	switch *api {
	case "responses":
		path = "/v1/responses"
		req := map[string]interface{}{"model": *model, "input": *prompt}
		if *system != "" {
			req["instructions"] = *system
		}
		if *maxTokens > 0 {
			req["max_output_tokens"] = *maxTokens
		}
		payload = req
	case "chat":
		msgs := []map[string]string{}
		if *system != "" {
			msgs = append(msgs, map[string]string{"role": "system", "content": *system})
		}
		msgs = append(msgs, map[string]string{"role": "user", "content": *prompt})
		req := map[string]interface{}{"model": *model, "messages": msgs}
		if *maxTokens > 0 {
			req["max_tokens"] = *maxTokens
		}
		path = "/v1/chat/completions"
		payload = req
	default:
		fmt.Fprintln(os.Stderr, "chat: --api must be chat|responses")
		return 2
	}

	raw, _ := json.Marshal(payload)
	client := &http.Client{Timeout: *timeout}
	httpReq, err := http.NewRequest("POST", strings.TrimRight(*gateway, "/")+path, bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "chat: HTTP %d: %s\n", resp.StatusCode, compactErr(body))
		return 1
	}
	if *asJSON {
		fmt.Println(string(body))
		return 0
	}
	text, usage := extractChatText(*api, body)
	fmt.Println(text)
	if usage != "" {
		fmt.Fprintf(os.Stderr, "%s\n", usage)
	}
	return 0
}

func compactErr(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// extractChatText pulls assistant text (+ a usage footer) from a chat or
// responses payload. Best-effort: unknown shapes print raw JSON.
func extractChatText(api string, body []byte) (string, string) {
	if api == "responses" {
		var out struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return string(body), ""
		}
		var sb strings.Builder
		for _, item := range out.Output {
			if item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				sb.WriteString(part.Text)
			}
		}
		usage := ""
		if out.Usage != nil {
			usage = fmt.Sprintf("[tokens in=%d out=%d]", out.Usage.InputTokens, out.Usage.OutputTokens)
		}
		return sb.String(), usage
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 {
		return string(body), ""
	}
	var text string
	_ = json.Unmarshal(out.Choices[0].Message.Content, &text)
	if text == "" {
		text = string(out.Choices[0].Message.Content)
	}
	usage := ""
	if out.Usage != nil {
		usage = fmt.Sprintf("[tokens in=%d out=%d]", out.Usage.PromptTokens, out.Usage.CompletionTokens)
	}
	return text, usage
}
