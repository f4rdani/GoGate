package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aigateway/config"
	"github.com/aigateway/models"
)

// esTestFrame builds one AWS EventStream frame for tests.
func esTestFrame(eventType string, payload []byte) []byte {
	var hdr bytes.Buffer
	writeESHeader := func(name, value string) {
		hdr.WriteByte(byte(len(name)))
		hdr.WriteString(name)
		hdr.WriteByte(6)
		_ = binary.Write(&hdr, binary.BigEndian, uint16(len(value)))
		hdr.WriteString(value)
	}
	writeESHeader(":message-type", "event")
	writeESHeader(":event-type", eventType)
	total := 12 + hdr.Len() + len(payload) + 4
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(total))
	_ = binary.Write(&out, binary.BigEndian, uint32(hdr.Len()))
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(out.Bytes()[:8]))
	out.Write(hdr.Bytes())
	out.Write(payload)
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(out.Bytes()))
	return out.Bytes()
}

func TestESParseFrameRoundTrip(t *testing.T) {
	raw := esTestFrame("assistantResponseEvent", []byte(`{"content":"hello"}`))
	ev, n, err := esParseFrame(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if n != len(raw) || ev.messageType != "event" || ev.eventType != "assistantResponseEvent" {
		t.Fatalf("bad event: %+v n=%d", ev, n)
	}
	if !strings.Contains(string(ev.payload), "hello") {
		t.Fatalf("bad payload: %s", ev.payload)
	}
}

func TestESParseFrameBadCRC(t *testing.T) {
	raw := esTestFrame("assistantResponseEvent", []byte(`{}`))
	raw[10] ^= 0xff
	if _, _, err := esParseFrame(raw); err == nil {
		t.Fatal("expected CRC error")
	}
}

func TestESParserIncremental(t *testing.T) {
	raw := append(esTestFrame("assistantResponseEvent", []byte(`{"content":"a"}`)),
		esTestFrame("messageStopEvent", []byte(`{"stopReason":"end_turn"}`))...)
	p := &esParser{}
	var got []esEvent
	// Feed byte-by-byte to prove incremental parsing.
	for i := 0; i < len(raw); i++ {
		evs, err := p.Feed(raw[i : i+1])
		if err != nil {
			t.Fatalf("feed failed: %v", err)
		}
		got = append(got, evs...)
	}
	if len(got) != 2 || got[0].eventType != "assistantResponseEvent" || got[1].eventType != "messageStopEvent" {
		t.Fatalf("expected 2 events, got %+v", got)
	}
}

func TestBuildKiroBodyMergesSystemAndAlternates(t *testing.T) {
	maxTok := 100
	req := &models.ChatCompletionRequest{
		Model:     "claude-sonnet-4.5",
		MaxTokens: &maxTok,
		Messages: []models.Message{
			{Role: "system", Content: json.RawMessage(`"be nice"`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`"hello"`)},
			{Role: "user", Content: json.RawMessage(`"yo"`)},
			{Role: "user", Content: json.RawMessage(`"again"`)}, // must merge with previous user
		},
		Tools: json.RawMessage(`[{"type":"function","function":{"name":"get_time","description":"d","parameters":{"type":"object"}}}]`),
	}
	body, err := buildKiroBody("claude-sonnet-4.5", req)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	cs, _ := body["conversationState"].(map[string]interface{})
	cur, _ := cs["currentMessage"].(map[string]interface{})
	uim, _ := cur["userInputMessage"].(map[string]interface{})
	content, _ := uim["content"].(string)
	if !strings.Contains(content, "yo") || !strings.Contains(content, "again") {
		t.Fatalf("current message must hold merged user turns: %q", content)
	}
	hist, _ := cs["history"].([]map[string]interface{})
	if len(hist) == 0 {
		t.Fatal("expected history turns")
	}
	first, _ := hist[0]["userInputMessage"].(map[string]interface{})
	fc, _ := first["content"].(string)
	if !strings.Contains(fc, "<instructions>") || !strings.Contains(fc, "be nice") {
		t.Fatalf("system prompt must ride the first user turn: %q", fc)
	}
	ctx, _ := uim["userInputMessageContext"].(map[string]interface{})
	tools, _ := ctx["tools"].([]kiroToolSpec)
	if len(tools) != 1 || tools[0].ToolSpecification.Name != "get_time" {
		t.Fatalf("tool specs must attach to current message: %+v", ctx)
	}
	// Alternation check.
	turns := append(append([]map[string]interface{}{}, hist...), cur)
	for i, turn := range turns {
		_, isUser := turn["userInputMessage"]
		if (i%2 == 0) != isUser {
			t.Fatalf("turns must strictly alternate starting with user (turn %d): %+v", i, turn)
		}
	}
}

func TestStripKiroSuffixes(t *testing.T) {
	if got := stripKiroSuffixes("claude-sonnet-4.5-thinking-agentic"); got != "claude-sonnet-4.5" {
		t.Fatalf("got %q", got)
	}
	if got := stripKiroSuffixes("glm-5"); got != "glm-5" {
		t.Fatalf("got %q", got)
	}
}

func TestKiroTokenSourceRefresh(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			RefreshToken string `json:"refreshToken"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.RefreshToken != "aorAAAAAGtest" && req.RefreshToken != "aorAAAAAGnew" {
			t.Errorf("bad refresh body: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"accessToken":"acc-1","refreshToken":"aorAAAAAGnew","profileArn":"arn:test","expiresIn":3600}`))
	}))
	defer srv.Close()

	src, err := NewKiroTokenSource("aorAAAAAGtest", "", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	src.refreshURL = srv.URL

	ctx := context.Background()
	tok, err := src.Token(ctx)
	if err != nil || tok != "acc-1" {
		t.Fatalf("expected acc-1, got %q %v", tok, err)
	}
	// Cached: no second HTTP hit. Rotation: refresh token updated.
	tok2, err := src.Token(ctx)
	if err != nil || tok2 != "acc-1" || hits.Load() != 1 {
		t.Fatalf("expected cached token (1 hit), hits=%d", hits.Load())
	}
	if src.refreshToken != "aorAAAAAGnew" || src.Profile() != "arn:test" {
		t.Fatalf("rotation/profile must update: %+v", src)
	}
	// Forced refresh hits the endpoint again.
	if _, err := src.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 hits after forced refresh, got %d", hits.Load())
	}
}

// kiroMockSurfaces builds a Kiro provider pointed at httptest surfaces.
func kiroMockSurfaces(t *testing.T, surfaces ...string) (*KiroProvider, *[]string) {
	t.Helper()
	base := &BaseProvider{
		name:         "kiro-test",
		providerType: "kiro",
		client:       &http.Client{},
		models:       map[string]bool{},
		apiKeys:      []*UpstreamKey{{Key: "test-key"}},
	}
	var seen []string
	_ = seen
	kp := NewKiroProvider(base, nil, "")
	kp.surfaces = surfaces
	return kp, nil
}

func TestKiroSurfaceFailover(t *testing.T) {
	var secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"bearer token invalid"}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		if got := r.Header.Get("TokenType"); got != "API_KEY" {
			t.Errorf("api-key mode needs TokenType header, got %q", got)
		}
		w.Write(append(
			esTestFrame("assistantResponseEvent", []byte(`{"content":"hi"}`)),
			esTestFrame("messageStopEvent", []byte(`{"stopReason":"end_turn"}`))...,
		))
	}))
	defer second.Close()

	kp, _ := kiroMockSurfaces(t, first.URL, second.URL)
	resp, err := kp.ChatCompletion(context.Background(), &models.ChatCompletionRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []models.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("failover must succeed: %v", err)
	}
	if secondHits.Load() != 1 {
		t.Fatalf("second surface must serve, hits=%d", secondHits.Load())
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.ContentString() != "hi" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish must map end_turn→stop: %+v", resp.Choices[0].FinishReason)
	}
}

func TestKiroChatToolUseMapping(t *testing.T) {
	toolPayload, _ := json.Marshal(map[string]interface{}{
		"name": "get_time", "toolUseId": "tu-1", "input": map[string]interface{}{},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(append(
			esTestFrame("toolUseEvent", toolPayload),
			esTestFrame("messageStopEvent", []byte(`{"stopReason":"tool_use"}`))...,
		))
	}))
	defer srv.Close()

	kp, _ := kiroMockSurfaces(t, srv.URL)
	resp, err := kp.ChatCompletion(context.Background(), &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: json.RawMessage(`"t?"`)}},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected choices")
	}
	if fr := resp.Choices[0].FinishReason; fr == nil || *fr != "tool_calls" {
		t.Fatalf("finish must be tool_calls: %+v", fr)
	}
	var calls []map[string]interface{}
	if err := json.Unmarshal(resp.Choices[0].Message.ToolCalls, &calls); err != nil || len(calls) != 1 {
		t.Fatalf("expected one tool call: %s %v", resp.Choices[0].Message.ToolCalls, err)
	}
}

func TestKiroListModelsAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("TokenType"); got != "API_KEY" {
			t.Errorf("need TokenType API_KEY, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"modelId":"claude-sonnet-4.5"},{"modelId":"glm-5"}]}`))
	}))
	defer srv.Close()

	list, err := KiroListModels(context.Background(), nil, KiroCredentials{APIKey: "k", CatalogURL: srv.URL})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(list) != 2 || list[0] != "claude-sonnet-4.5" {
		t.Fatalf("unexpected list: %v", list)
	}
}

func TestKiroStreamEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Write(append(
			esTestFrame("assistantResponseEvent", []byte(`{"content":"Hel"}`)),
			esTestFrame("assistantResponseEvent", []byte(`{"content":"lo"}`))...,
		))
		w.Write(esTestFrame("messageStopEvent", []byte(`{"stopReason":"end_turn"}`)))
		w.Write(esTestFrame("metricsEvent", []byte(`{"inputTokens":3,"outputTokens":2}`)))
	}))
	defer srv.Close()

	kp, _ := kiroMockSurfaces(t, srv.URL)
	w := httptest.NewRecorder()
	err := kp.ChatCompletionStream(context.Background(), &models.ChatCompletionRequest{
		Model:    "m",
		Messages: []models.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}, w, w)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo"`) {
		t.Fatalf("text must stream live:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream must terminate properly:\n%s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":3`) {
		t.Fatalf("usage must flow from metricsEvent:\n%s", body)
	}
}

func TestKiroDeviceFlow(t *testing.T) {
	var polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"clientId":"cid","clientSecret":"csec"}`))
	})
	mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"deviceCode":"dev","userCode":"USER-CODE","verificationUri":"https://example.com","verificationUriComplete":"https://example.com/c","expiresIn":600,"interval":1}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) == 1 {
			w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		w.Write([]byte(`{"accessToken":"acc","refreshToken":"aorAAAAAGnew","expiresIn":3600}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	old := kiroOIDCBaseOverride
	kiroOIDCBaseOverride = srv.URL
	defer func() { kiroOIDCBaseOverride = old }()

	ctx := context.Background()
	reg, err := KiroRegisterClient(ctx, srv.Client(), "us-east-1")
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if reg.ClientID != "cid" || reg.ClientSecret != "csec" {
		t.Fatalf("bad registration: %+v", reg)
	}
	dev, err := KiroStartDeviceAuth(ctx, srv.Client(), "us-east-1", reg.ClientID, reg.ClientSecret, "")
	if err != nil {
		t.Fatalf("device auth failed: %v", err)
	}
	if dev.UserCode != "USER-CODE" || dev.DeviceCode != "dev" || dev.Interval != 1 {
		t.Fatalf("bad device auth: %+v", dev)
	}
	poll1, err := KiroPollDeviceToken(ctx, srv.Client(), "us-east-1", reg.ClientID, reg.ClientSecret, dev.DeviceCode)
	if err != nil || !poll1.Pending || poll1.Done {
		t.Fatalf("first poll must be pending: %+v %v", poll1, err)
	}
	poll2, err := KiroPollDeviceToken(ctx, srv.Client(), "us-east-1", reg.ClientID, reg.ClientSecret, dev.DeviceCode)
	if err != nil || !poll2.Done || poll2.Pending {
		t.Fatalf("second poll must complete: %+v %v", poll2, err)
	}
	if poll2.Tokens.AccessToken != "acc" || poll2.Tokens.RefreshToken != "aorAAAAAGnew" {
		t.Fatalf("bad tokens: %+v", poll2.Tokens)
	}
}

func TestKiroPollParsing(t *testing.T) {
	// Pending then success through the real poll function requires fixed OIDC
	// URLs; instead verify the social/device URL builders and exchange shapes.
	u, err := KiroSocialLoginURL("google", "challenge123", "state123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "idp=Google") || !strings.Contains(u, "code_challenge=challenge123") {
		t.Fatalf("bad login URL: %s", u)
	}
	if _, err := KiroSocialLoginURL("twitter", "c", "s"); err == nil {
		t.Fatal("expected error for unknown idp")
	}
}

func TestKiroSocialExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req struct {
			Code         string `json:"code"`
			CodeVerifier string `json:"code_verifier"`
			RedirectURI  string `json:"redirect_uri"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Code != "code-1" || req.CodeVerifier == "" || !strings.HasPrefix(req.RedirectURI, "kiro://") {
			t.Errorf("bad exchange body: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"accessToken":"acc","refreshToken":"aorAAAAAGr","profileArn":"arn:p","expiresIn":3600}`))
	}))
	defer srv.Close()

	old := kiroSocialBaseOverride
	kiroSocialBaseOverride = srv.URL
	defer func() { kiroSocialBaseOverride = old }()

	tokens, err := KiroExchangeSocialCode(context.Background(), srv.Client(), "code-1", "verifier-1")
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if tokens.AccessToken != "acc" || tokens.RefreshToken != "aorAAAAAGr" || tokens.ProfileARN != "arn:p" {
		t.Fatalf("bad tokens: %+v", tokens)
	}
}

func TestKiroOIDCRefreshBranch(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req struct {
			ClientID     string `json:"clientId"`
			ClientSecret string `json:"clientSecret"`
			RefreshToken string `json:"refreshToken"`
			GrantType    string `json:"grantType"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ClientID != "cid" || req.RefreshToken != "rt" || req.GrantType != "refresh_token" {
			t.Errorf("bad OIDC refresh body: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"accessToken":"oidc-acc","expiresIn":1800}`))
	}))
	defer srv.Close()

	old := kiroOIDCBaseOverride
	kiroOIDCBaseOverride = srv.URL
	defer func() { kiroOIDCBaseOverride = old }()

	src, err := NewKiroOIDCTokenSource("rt", "", "cid", "csec", "us-east-1", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token(context.Background())
	if err != nil || tok != "oidc-acc" {
		t.Fatalf("expected oidc-acc, got %q %v", tok, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 refresh hit, got %d", hits.Load())
	}
	if _, err := NewKiroOIDCTokenSource("rt", "", "", "csec", "", nil); err == nil {
		t.Fatal("expected error without client_id")
	}
}

func TestKiroUsageFallbackAndParse(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"nope"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"subscriptionInfo":{"subscriptionTitle":"Kiro Pro"},"nextDateReset":"2026-10-01T00:00:00Z","usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsageWithPrecision":30,"usageLimitWithPrecision":100,"freeTrialInfo":{"currentUsageWithPrecision":5,"usageLimitWithPrecision":50,"freeTrialExpiry":"2026-09-01T00:00:00Z"}}]}`))
	}))
	defer srv.Close()

	old := kiroUsageEndpointOverride
	kiroUsageEndpointOverride = srv.URL
	defer func() { kiroUsageEndpointOverride = old }()

	usage, err := KiroUsageLimits(context.Background(), srv.Client(), "tok", true, "")
	if err != nil {
		t.Fatalf("usage failed: %v", err)
	}
	if usage.Plan != "Kiro Pro" {
		t.Fatalf("bad plan: %+v", usage)
	}
	q, ok := usage.Quotas["agentic_request"]
	if !ok || q.Used != 30 || q.Total != 100 || q.Remaining != 70 {
		t.Fatalf("bad quota: %+v", usage.Quotas)
	}
	ft, ok := usage.Quotas["agentic_request_freetrial"]
	if !ok || ft.Total != 50 || ft.Remaining != 45 {
		t.Fatalf("bad freetrial: %+v", usage.Quotas)
	}
	if hits.Load() != 2 {
		t.Fatalf("must fall through to second endpoint, hits=%d", hits.Load())
	}
}

func TestKiroRegionAndImages(t *testing.T) {
	if kiroRegion("") != "us-east-1" || kiroRegion("eu-west-1") != "eu-west-1" {
		t.Fatal("region normalize broken")
	}
	img, ok := kiroParseDataURI("data:image/png;base64,iVBORw0KGgo=")
	if !ok || img.Format != "png" || img.Source.Bytes == "" {
		t.Fatalf("data uri broken: %+v %v", img, ok)
	}
	if _, ok := kiroParseDataURI("http://x/y.png"); ok {
		t.Fatal("remote URL is not a data URI")
	}
	if _, ok := kiroParseDataURI("data:image/png,iVBOR"); ok {
		t.Fatal("non-base64 must reject")
	}
	if got := kiroImageOf("", "XX"); got.Format != "png" {
		t.Fatalf("empty mime defaults png: %+v", got)
	}
	if got := kiroImageOf("image/jpeg", "XX"); got.Format != "jpeg" {
		t.Fatalf("subtype extract: %+v", got)
	}
}

func TestFetchKiroUsageAndMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"subscriptionInfo":{"subscriptionTitle":"Free"},"usageBreakdownList":[{"resourceType":"CREDITS","currentUsageWithPrecision":10,"usageLimitWithPrecision":50}]}`))
	}))
	defer srv.Close()

	old := kiroUsageEndpointOverride
	kiroUsageEndpointOverride = srv.URL
	defer func() { kiroUsageEndpointOverride = old }()

	u, err := FetchKiroUsage(context.Background(), srv.Client(), KiroCredentials{APIKey: "k"})
	if err != nil {
		t.Fatalf("fetch usage: %v", err)
	}
	if u.Plan != "Free" || u.Quotas["credits"].Remaining != 40 {
		t.Fatalf("bad usage: %+v", u)
	}

	// Provider method delegates with own credentials.
	base := &BaseProvider{name: "kr", providerType: "kiro", client: srv.Client(), apiKeys: []*UpstreamKey{{Key: "k"}}}
	kp := NewKiroProvider(base, nil, "")
	u2, err := kp.UsageLimits(context.Background())
	if err != nil || u2.Plan != "Free" {
		t.Fatalf("method usage: %+v %v", u2, err)
	}

	// No credentials → error, never network.
	kp2 := NewKiroProvider(&BaseProvider{name: "kr", providerType: "kiro", client: srv.Client()}, nil, "")
	if _, err := kp2.UsageLimits(context.Background()); err == nil {
		t.Fatal("credential-less usage must error")
	}
	if _, err := FetchKiroUsage(context.Background(), srv.Client(), KiroCredentials{}); err == nil {
		t.Fatal("credential-less fetch must error")
	}
}

func TestKiroTestCompletionE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(append(
			esTestFrame("assistantResponseEvent", []byte(`{"content":"ok"}`)),
			esTestFrame("messageStopEvent", []byte(`{"stopReason":"end_turn"}`))...,
		))
	}))
	defer srv.Close()

	text, err := KiroTestCompletion(context.Background(), srv.Client(),
		KiroCredentials{APIKey: "k"}, "claude-sonnet-4.5", srv.URL)
	if err != nil {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if text != "ok" {
		t.Fatalf("unexpected text %q", text)
	}
	if _, err := KiroTestCompletion(context.Background(), srv.Client(), KiroCredentials{}, "m"); err == nil {
		t.Fatal("credential-less roundtrip must error")
	}
}

func TestKiroProviderConfigValidation(t *testing.T) {
	p, err := NewProviderFromConfig(config.ProviderConfig{
		Name: "kr", Type: "kiro", BaseURL: "http://x", APIKeys: []string{"k"},
	})
	if err != nil {
		t.Fatalf("api-key kiro must construct: %v", err)
	}
	if _, ok := p.(*KiroProvider); !ok {
		t.Fatalf("expected *KiroProvider, got %T", p)
	}
}
