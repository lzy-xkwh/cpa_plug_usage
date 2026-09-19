package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDecodeConfigYAMLSubset(t *testing.T) {
	var got config
	err := decodeConfig([]byte(`
enabled: true
priority: 10
endpoint: "https://api.example.test/balance"
headers:
  X-Client: "cpa"
credential_paths:
  - access_token
  - api_key
balance_path: data.balance
limit_path: data.limit
`), &got)
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if !got.Enabled || got.Priority != 10 || got.Endpoint != "https://api.example.test/balance" {
		t.Fatalf("decoded scalar config = %#v", got)
	}
	if got.Headers["X-Client"] != "cpa" {
		t.Fatalf("decoded headers = %#v", got.Headers)
	}
	if len(got.CredentialPaths) != 2 || got.CredentialPaths[1] != "api_key" {
		t.Fatalf("decoded credential paths = %#v", got.CredentialPaths)
	}
}

func TestDisabledConfigDoesNotRequireEndpoint(t *testing.T) {
	if err := applyConfig([]byte("enabled: false\npriority: 0\n")); err != nil {
		t.Fatalf("applyConfig() error for disabled plugin = %v", err)
	}
}

func TestStoreMetadataIsIgnored(t *testing.T) {
	if err := applyConfig([]byte(`
enabled: true
priority: 10
store:
  source: source-example
  version: 0.1.0
  artifacts:
    - linux-amd64.zip
    - linux-arm64.zip
`)); err != nil {
		t.Fatalf("applyConfig() should ignore CPA store metadata: %v", err)
	}
}

func TestVendorPresetFillsMissingFields(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nvendor: deepseek\n")); err != nil {
		t.Fatalf("applyConfig() error for deepseek preset = %v", err)
	}
	got := currentConfig()
	if got.Endpoint != "https://api.deepseek.com/user/balance" {
		t.Fatalf("endpoint = %q, want deepseek balance endpoint", got.Endpoint)
	}
	if got.BalancePath != "balance_infos.0.total_balance" || got.CurrencyPath != "balance_infos.0.currency" {
		t.Fatalf("paths = %q / %q", got.BalancePath, got.CurrencyPath)
	}
	if got.WindowName != "余额" {
		t.Fatalf("window name = %q", got.WindowName)
	}
}

func TestVendorExplicitConfigWins(t *testing.T) {
	if err := applyConfig([]byte("vendor: moonshot\nbalance_path: data.cash_balance\n")); err != nil {
		t.Fatalf("applyConfig() error for moonshot preset = %v", err)
	}
	got := currentConfig()
	if got.Endpoint != "https://api.moonshot.cn/v1/users/me/balance" {
		t.Fatalf("endpoint = %q, want moonshot balance endpoint", got.Endpoint)
	}
	if got.BalancePath != "data.cash_balance" {
		t.Fatalf("balance path = %q, want explicit override kept", got.BalancePath)
	}
}

func TestUnknownVendorRejected(t *testing.T) {
	if err := applyConfig([]byte("vendor: no-such-vendor\n")); err == nil {
		t.Fatalf("applyConfig() should reject unknown vendor")
	}
}

func TestOneApiPresetAndDerivedBalance(t *testing.T) {
	if err := applyConfig([]byte("vendor: one-api\nbase_url: https://relay.example.com\n")); err != nil {
		t.Fatalf("applyConfig() error for one-api preset = %v", err)
	}
	got := currentConfig()
	if got.Endpoint != "{base_url}/v1/dashboard/billing/subscription" || got.UsedEndpoint != "{base_url}/v1/dashboard/billing/usage" {
		t.Fatalf("endpoints = %q / %q", got.Endpoint, got.UsedEndpoint)
	}
	if got.UsedScale != 0.01 || got.LimitPath != "hard_limit_usd" || got.UsedPath != "total_usage" {
		t.Fatalf("preset fields = scale %v, limit %q, used %q", got.UsedScale, got.LimitPath, got.UsedPath)
	}
	document := map[string]any{"hard_limit_usd": 10.0}
	usedDocument := map[string]any{"total_usage": 250.0}
	resp, err := normalizeQuota(document, usedDocument, true, got)
	if err != nil {
		t.Fatalf("normalizeQuota() error = %v", err)
	}
	bucket := resp.Groups[0].Buckets[0]
	if bucket.RemainingFraction != 0.75 {
		t.Fatalf("remaining fraction = %v, want 0.75", bucket.RemainingFraction)
	}
}

func TestOpenRouterPreset(t *testing.T) {
	if err := applyConfig([]byte("vendor: openrouter\n")); err != nil {
		t.Fatalf("applyConfig() error for openrouter preset = %v", err)
	}
	got := currentConfig()
	if got.Endpoint != "https://openrouter.ai/api/v1/key" || got.LimitPath != "data.limit" || got.UsedPath != "data.usage" {
		t.Fatalf("preset fields = %q / %q / %q", got.Endpoint, got.LimitPath, got.UsedPath)
	}
}

func TestExpandEndpointBaseURLPrecedence(t *testing.T) {
	req := quotaFetchRequest{Attributes: map[string]string{"base_url": "https://from-auth.example.com/"}}
	got := expandEndpoint("{base_url}/v1/x", "https://from-config.example.com/", req)
	if got != "https://from-config.example.com/v1/x" {
		t.Fatalf("expandEndpoint() = %q", got)
	}
	got = expandEndpoint("{base_url}/v1/x", "", req)
	if got != "https://from-auth.example.com/v1/x" {
		t.Fatalf("expandEndpoint() fallback = %q", got)
	}
}

func TestDecodeConfigVisualCredentialPaths(t *testing.T) {
	var got config
	err := decodeConfig([]byte(`
enabled: true
endpoint: https://api.example.test/balance
credential_paths: access_token, api_key
balance_path: data.balance
`), &got)
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if len(got.CredentialPaths) != 2 || got.CredentialPaths[0] != "access_token" || got.CredentialPaths[1] != "api_key" {
		t.Fatalf("decoded visual credential paths = %#v", got.CredentialPaths)
	}
}

func TestNormalizeQuota(t *testing.T) {
	document := map[string]any{
		"data": map[string]any{
			"balance":  25.0,
			"limit":    100.0,
			"used":     75.0,
			"currency": "USD",
			"plan":     "pro",
		},
	}
	resp, err := normalizeQuota(document, document, false, config{
		BalancePath:  "data.balance",
		LimitPath:    "data.limit",
		UsedPath:     "data.used",
		CurrencyPath: "data.currency",
		PlanPath:     "data.plan",
		WindowName:   "monthly",
	})
	if err != nil {
		t.Fatalf("normalizeQuota() error = %v", err)
	}
	bucket := resp.Groups[0].Buckets[0]
	if bucket.RemainingFraction != 0.25 {
		t.Fatalf("remaining fraction = %v, want 0.25", bucket.RemainingFraction)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "pro" {
		t.Fatalf("subscription = %#v", resp.Subscription)
	}
}

func TestFindCredentialDoesNotReturnMalformedJSON(t *testing.T) {
	if got := findCredential([]byte("not-json"), []string{"token"}); got != "" {
		t.Fatalf("findCredential() = %q, want empty", got)
	}
	var document = map[string]any{"access_token": "secret"}
	raw, _ := json.Marshal(document)
	if got := findCredential(raw, []string{"access_token"}); got != "secret" {
		t.Fatalf("findCredential() = %q, want secret", got)
	}
}

func TestDecodeConfigProfiles(t *testing.T) {
	var got config
	err := decodeConfig([]byte(`
enabled: true
profiles:
  deepseek:
    vendor: deepseek
  relay-a:
    vendor: one-api
    base_url: https://relay.example.com
`), &got)
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if len(got.Profiles) != 2 {
		t.Fatalf("profiles = %#v", got.Profiles)
	}
	if got.Profiles["deepseek"].Vendor != "deepseek" {
		t.Fatalf("deepseek profile vendor = %q", got.Profiles["deepseek"].Vendor)
	}
	if got.Profiles["relay-a"].BaseURL != "https://relay.example.com" {
		t.Fatalf("relay-a base_url = %q", got.Profiles["relay-a"].BaseURL)
	}
}

func TestApplyConfigResolvesProfiles(t *testing.T) {
	if err := applyConfig([]byte(`
enabled: true
profiles:
  deepseek:
    vendor: deepseek
  relay-a:
    vendor: one-api
    base_url: https://relay.example.com
`)); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	got := currentConfig()
	if got.Profiles["deepseek"].Endpoint != "https://api.deepseek.com/user/balance" {
		t.Fatalf("deepseek endpoint = %q", got.Profiles["deepseek"].Endpoint)
	}
	if got.Profiles["relay-a"].UsedScale != 0.01 || got.Profiles["relay-a"].UsedEndpoint == "" {
		t.Fatalf("relay-a preset fields = scale %v used %q", got.Profiles["relay-a"].UsedScale, got.Profiles["relay-a"].UsedEndpoint)
	}
}

func TestResolveProfileRouting(t *testing.T) {
	cfg := config{
		Profiles: map[string]config{
			"deepseek": {Vendor: "deepseek", Endpoint: "https://api.deepseek.com/user/balance"},
			"default":  {Vendor: "custom", Endpoint: "https://fallback.example.com/x"},
		},
	}
	if p, err := resolveProfile(cfg, "DeepSeek"); err != nil || p.Endpoint != "https://api.deepseek.com/user/balance" {
		t.Fatalf("exact match = %#v, %v", p, err)
	}
	if p, err := resolveProfile(cfg, "unknown-relay"); err != nil || p.Endpoint != "https://fallback.example.com/x" {
		t.Fatalf("default fallback = %#v, %v", p, err)
	}
	legacy := config{Vendor: "one-api", Endpoint: "{base_url}/v1/dashboard/billing/subscription"}
	if p, err := resolveProfile(legacy, "anything"); err != nil || p.Endpoint != legacy.Endpoint {
		t.Fatalf("legacy fallback = %#v, %v", p, err)
	}
	empty := config{Profiles: map[string]config{"deepseek": {Vendor: "deepseek"}}}
	if _, err := resolveProfile(empty, "nope"); err == nil {
		t.Fatalf("resolveProfile() should error without match/default/legacy source")
	}
}

func TestDescribeIncludesProfileNames(t *testing.T) {
	if err := applyConfig([]byte(`
enabled: true
profiles:
  relay-a:
    vendor: one-api
    base_url: https://relay.example.com
`)); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	raw, err := handleMethod("quota.describe", nil)
	if err != nil {
		t.Fatalf("handleMethod() error = %v", err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			SupportedProviders []string `json:"supported_providers"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("describe envelope = %s (%v)", raw, err)
	}
	joined := strings.Join(env.Result.SupportedProviders, ",")
	for _, want := range []string{"api-balance", "deepseek", "moonshot", "one-api", "openrouter", "relay-a"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("supported providers %q missing %q", joined, want)
		}
	}
}

func TestDiscoverBaseURL(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"base_url": "https://relay.example.com/"})
	req := quotaFetchRequest{StorageJSON: raw}
	cfg := config{}
	if got := discoverBaseURL(&cfg, req); got != "https://relay.example.com" {
		t.Fatalf("storage_json discovery = %q", got)
	}
	req2 := quotaFetchRequest{Metadata: map[string]any{"api_base": "https://meta.example.com"}}
	if got := discoverBaseURL(&cfg, req2); got != "https://meta.example.com" {
		t.Fatalf("metadata discovery = %q", got)
	}
	req3 := quotaFetchRequest{Attributes: map[string]string{"baseURL": "https://attr.example.com"}}
	if got := discoverBaseURL(&cfg, req3); got != "https://attr.example.com" {
		t.Fatalf("attributes discovery = %q", got)
	}
	if got := discoverBaseURL(&cfg, quotaFetchRequest{}); got != "" {
		t.Fatalf("empty discovery = %q", got)
	}
	if got := discoverBaseURL(&config{BaseURL: "https://config.example.com"}, quotaFetchRequest{}); got != "https://config.example.com" {
		t.Fatalf("config base_url precedence = %q", got)
	}
}

func TestVendorByHost(t *testing.T) {
	cases := map[string]string{
		"https://api.deepseek.com":     "deepseek",
		"https://api.moonshot.cn/v1":   "moonshot",
		"https://openrouter.ai/api/v1": "openrouter",
		"https://relay.example.com":    "",
	}
	for input, want := range cases {
		if got := vendorByHost(input); got != want {
			t.Fatalf("vendorByHost(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAutoDetectByProviderName(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	cfg := currentConfig()
	profile, result, err := autoDetectProfile(cfg, quotaFetchRequest{Provider: "deepseek"})
	if err != nil || result != nil {
		t.Fatalf("autoDetectProfile(deepseek) err=%v result=%v", err, result)
	}
	if profile.Endpoint != "https://api.deepseek.com/user/balance" || profile.BalancePath != "balance_infos.0.total_balance" {
		t.Fatalf("deepseek auto profile = %#v", profile)
	}
	profile, result, err = autoDetectProfile(cfg, quotaFetchRequest{
		Provider: "sub2api",
		Metadata: map[string]any{"base_url": "https://relay.example.com"},
	})
	if err != nil || result != nil {
		t.Fatalf("autoDetectProfile(sub2api) err=%v result=%v", err, result)
	}
	if profile.BaseURL != "https://relay.example.com" || profile.Endpoint != "{base_url}/v1/usage" {
		t.Fatalf("sub2api auto profile = %#v", profile)
	}
}

func TestAutoDetectMissingBaseURLGuidance(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	cfg := currentConfig()
	_, _, err := autoDetectProfile(cfg, quotaFetchRequest{Provider: "some-relay"})
	if err == nil || !strings.Contains(err.Error(), "profiles") {
		t.Fatalf("autoDetectProfile() error = %v, want guidance mentioning profiles", err)
	}
}

func TestUnknownConfigKeyIgnored(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nfuture_option: 1\n")); err != nil {
		t.Fatalf("applyConfig() with unknown key should be ignored, got %v", err)
	}
	if !currentConfig().Enabled {
		t.Fatalf("enabled should still be parsed")
	}
}

func TestManagementRegisterAndHandle(t *testing.T) {
	raw, err := handleMethod("management.register", nil)
	if err != nil {
		t.Fatalf("management.register error = %v", err)
	}
	if !strings.Contains(string(raw), "config-wizard") || !strings.Contains(string(raw), "config-data") || !strings.Contains(string(raw), "\"余额\"") {
		t.Fatalf("register response missing wizard/data routes: %s", raw)
	}
	raw, err = handleMethod("management.handle", []byte(`{"method":"GET","path":"/v0/resource/plugins/api-balance/config-wizard"}`))
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("handle envelope = %s (%v)", raw, err)
	}
	var resp struct {
		StatusCode int                 `json:"StatusCode"`
		Headers    map[string][]string `json:"Headers"`
		Body       []byte              `json:"Body"`
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode response = %v", err)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "余额配置向导") {
		t.Fatalf("wizard response status=%d body len=%d", resp.StatusCode, len(resp.Body))
	}
	raw, err = handleMethod("management.handle", []byte(`{"method":"POST"}`))
	if err != nil {
		t.Fatalf("management.handle POST error = %v", err)
	}
	if !strings.Contains(string(raw), "405") {
		t.Fatalf("POST should return 405, got %s", raw)
	}
}

func TestManagementKeyScalarAccepted(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nmanagement_key: sk-test-123\nmanagement_url: http://127.0.0.1:9999\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	got := currentConfig()
	if got.ManagementKey != "sk-test-123" || got.ManagementURL != "http://127.0.0.1:9999" {
		t.Fatalf("management fields = %q / %q", got.ManagementKey, got.ManagementURL)
	}
}

func TestConfigDataSanitizesManagementKey(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nmanagement_key: secret-do-not-leak\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	raw, err := handleMethod("management.handle", []byte(`{"method":"GET","path":"/v0/resource/plugins/api-balance/config-data"}`))
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	if strings.Contains(string(raw), "secret-do-not-leak") {
		t.Fatalf("config-data leaked management_key: %s", raw)
	}
	resp := decodeManagementResponse(t, raw)
	if resp.StatusCode != 200 || len(resp.Body) == 0 {
		t.Fatalf("config-data status=%d bodyLen=%d", resp.StatusCode, len(resp.Body))
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("config-data body is not JSON: %v (%s)", err, resp.Body)
	}
	if payload["management_configured"] != true {
		t.Fatalf("management_configured = %v (%s)", payload["management_configured"], resp.Body)
	}
}

// 回归：/config-data 必须返回非空 JSON body（此前漏包 ManagementResponse
// 导致宿主回 200 空响应，页面报 Unexpected end of JSON input）。
func TestConfigDataReturnsJSONBody(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	raw, err := handleMethod("management.handle", []byte(`{"method":"GET","path":"/v0/resource/plugins/api-balance/config-data"}`))
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	resp := decodeManagementResponse(t, raw)
	if resp.StatusCode != 200 || len(resp.Body) == 0 {
		t.Fatalf("config-data status=%d bodyLen=%d", resp.StatusCode, len(resp.Body))
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("config-data body is not JSON: %v (%s)", err, resp.Body)
	}
	if _, ok := payload["providers"]; !ok {
		t.Fatalf("config-data payload missing providers: %s", resp.Body)
	}
}

// 回归：保存接口同样必须返回非空 JSON body。
func TestSaveReturnsJSONBody(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	raw, err := handleMethod("management.handle", []byte(`{"method":"GET","path":"/v0/resource/plugins/api-balance/config-wizard","query":{"save":["{\"enabled\":true}"]}}`))
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	resp := decodeManagementResponse(t, raw)
	if resp.StatusCode != 200 || len(resp.Body) == 0 {
		t.Fatalf("save status=%d bodyLen=%d", resp.StatusCode, len(resp.Body))
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("save body is not JSON: %v (%s)", err, resp.Body)
	}
	if ok, _ := payload["ok"].(bool); ok {
		t.Fatalf("save without management_key should fail: %s", resp.Body)
	}
}

func decodeManagementResponse(t *testing.T, raw []byte) struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
} {
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope = %s (%v)", raw, err)
	}
	var resp struct {
		StatusCode int                 `json:"StatusCode"`
		Headers    map[string][]string `json:"Headers"`
		Body       []byte              `json:"Body"`
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode ManagementResponse = %v (%s)", err, env.Result)
	}
	return resp
}

func TestProfilesInheritGlobalRequestSettings(t *testing.T) {
	if err := applyConfig([]byte(`
enabled: true
headers:
  X-Tenant: tenant-a
query:
  region: cn
credential_paths: access_token, nested.token
profiles:
  relay-a:
    vendor: custom
    endpoint: https://relay.example.com/balance
    balance_path: data.balance
`)); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	profile, err := resolveProfile(currentConfig(), "RELAY-A")
	if err != nil {
		t.Fatalf("resolveProfile() error = %v", err)
	}
	if profile.Headers["X-Tenant"] != "tenant-a" || profile.Query["region"] != "cn" {
		t.Fatalf("global request settings not inherited: %#v %#v", profile.Headers, profile.Query)
	}
	if len(profile.CredentialPaths) != 2 || profile.CredentialPaths[1] != "nested.token" {
		t.Fatalf("credential paths not inherited: %#v", profile.CredentialPaths)
	}
}

func TestJSONConfigUsesSnakeCaseFields(t *testing.T) {
	var got config
	if err := decodeConfig([]byte(`{"base_url":"https://relay.example.com","used_endpoint":"https://relay.example.com/usage","balance_path":"data.balance","used_scale":0.01}`), &got); err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if got.BaseURL != "https://relay.example.com" || got.UsedEndpoint == "" || got.BalancePath != "data.balance" || got.UsedScale != 0.01 {
		t.Fatalf("snake_case JSON fields decoded incorrectly: %#v", got)
	}
}

func TestWizardConfigWhitelistAndMerge(t *testing.T) {
	cfg := config{
		Enabled:         true,
		ManagementKey:   "server-only",
		ManagementURL:   "http://127.0.0.1:8317",
		Headers:         map[string]string{"X-Tenant": "tenant-a"},
		CredentialPaths: []string{"access_token"},
	}
	clean, err := validateWizardConfig(`{"enabled":false,"profiles":{"Relay-A":{"vendor":"custom","endpoint":"https://relay.example.com/balance","balance_path":"data.balance"}}}`)
	if err != nil {
		t.Fatalf("validateWizardConfig() error = %v", err)
	}
	merged, err := mergeWizardConfig(cfg, clean)
	if err != nil {
		t.Fatalf("mergeWizardConfig() error = %v", err)
	}
	if merged["management_key"] != "server-only" || merged["management_url"] != "http://127.0.0.1:8317" {
		t.Fatalf("server-only fields were not preserved: %#v", merged)
	}
	profiles, ok := merged["profiles"].(map[string]any)
	if !ok || profiles["relay-a"] == nil {
		t.Fatalf("profiles were not merged: %#v", merged["profiles"])
	}
	for _, input := range []string{
		`{"management_key":"leak"}`,
		`{"headers":{"Authorization":"secret"}}`,
		`{"profiles":{"x":{"credential_prefix":"Bearer "}}}`,
		`{"profiles":{"x":{"used_scale":null}}}`,
	} {
		if _, err := validateWizardConfig(input); err == nil {
			t.Fatalf("validateWizardConfig(%s) should reject unsafe input", input)
		}
	}
}

// 向导在仅配置 default 档案时会把档案字段提升到顶层提交，服务端必须接受。
func TestWizardAcceptsHoistedDefaultProfile(t *testing.T) {
	clean, err := validateWizardConfig(`{"enabled":true,"base_url":"https://relay.example.com","vendor":"one-api","used_scale":0.01}`)
	if err != nil {
		t.Fatalf("validateWizardConfig() error = %v", err)
	}
	if clean["base_url"] != "https://relay.example.com" || clean["used_scale"] != 0.01 {
		t.Fatalf("hoisted fields missing: %#v", clean)
	}
	for _, input := range []string{
		`{"management_url":"http://127.0.0.1:9999"}`,
		`{"allow_insecure_http":true}`,
		`{"credential_header":"X-Key"}`,
		`{"query":{"token":"x"}}`,
		`{"method":"DELETE"}`,
	} {
		if _, err := validateWizardConfig(input); err == nil {
			t.Fatalf("validateWizardConfig(%s) should reject unsafe input", input)
		}
	}
}

// fakeHost 用测试接缝模拟宿主回调，返回一个成功携带 body 的 HTTP 结果。
func fakeHost(t *testing.T, status int, body string, seen *httpRequest) {
	t.Helper()
	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		if hostMethod != "host.http.do" {
			return nil, fmt.Errorf("unexpected host method %q", hostMethod)
		}
		var request httpRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Fatalf("decode host request: %v", err)
		}
		if seen != nil {
			*seen = request
		}
		result, err := json.Marshal(httpResponse{StatusCode: status, Body: []byte(body)})
		if err != nil {
			t.Fatalf("marshal fake response: %v", err)
		}
		return result, nil
	}
	t.Cleanup(func() { hostCallMethod = previous })
}

func TestFetchQuotaEndToEnd(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nvendor: deepseek\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	var seen httpRequest
	fakeHost(t, 200, `{"balance_infos":[{"total_balance":"12.5","currency":"CNY"}]}`, &seen)
	storage, _ := json.Marshal(map[string]any{"api_key": "sk-test"})
	resp, err := fetchQuota(quotaFetchRequest{Provider: "deepseek", StorageJSON: storage})
	if err != nil {
		t.Fatalf("fetchQuota() error = %v", err)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if !strings.Contains(resp.Groups[0].Buckets[0].Description, "balance=12.5") {
		t.Fatalf("balance missing from description: %q", resp.Groups[0].Buckets[0].Description)
	}
	if got := seen.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer sk-test" {
		t.Fatalf("credential header = %#v", seen.Headers["Authorization"])
	}
	if seen.URL != "https://api.deepseek.com/user/balance" {
		t.Fatalf("request URL = %q", seen.URL)
	}
}

func TestFetchQuotaReportsUpstreamErrors(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nvendor: deepseek\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	fakeHost(t, 500, `{"error":"boom"}`, nil)
	_, err := fetchQuota(quotaFetchRequest{Provider: "deepseek"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("fetchQuota() error = %v, want HTTP 500 mention", err)
	}
	fakeHost(t, 200, `not-json`, nil)
	_, err = fetchQuota(quotaFetchRequest{Provider: "deepseek"})
	if err == nil || !strings.Contains(err.Error(), "有效 JSON") {
		t.Fatalf("fetchQuota() error = %v, want invalid JSON mention", err)
	}
}

func TestConfigSwapIsRaceSafe(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = applyConfig([]byte(fmt.Sprintf("enabled: true\npriority: %d\n", i)))
		}
	}()
	for i := 0; i < 50; i++ {
		_ = currentConfig().Priority
		_ = supportedProviders()
	}
	<-done
	if err := applyConfig([]byte("enabled: false\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
}

func TestExpandTemplateAndErrorEnvelope(t *testing.T) {
	req := quotaFetchRequest{Provider: "p", AuthID: "a", AuthIndex: "1"}
	got := expandTemplate("{provider}/{auth_id}/{auth_index}", req)
	if got != "p/a/1" {
		t.Fatalf("expandTemplate() = %q", got)
	}
	raw := errorEnvelope("bad", "出错了")
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.OK || env.Error == nil || env.Error.Code != "bad" {
		t.Fatalf("errorEnvelope() = %s (%v)", raw, err)
	}
}

func TestRegistrationMetadata(t *testing.T) {
	registration := pluginRegistrationResponse()
	if registration.SchemaVersion != schemaVersion {
		t.Fatalf("schema version = %d", registration.SchemaVersion)
	}
	if registration.Metadata.Version == "" || registration.Metadata.Name != pluginID {
		t.Fatalf("metadata = %#v", registration.Metadata)
	}
	if !registration.Capabilities["quota_provider"] {
		t.Fatalf("capabilities = %#v", registration.Capabilities)
	}
}

// 管理密钥填错后必须始终能重填：向导页要有重设入口，
// 且已配置密钥但被 CPA 拒绝（401/403）时前端能自动展开重填卡片。
// v0.9.0 起向导不再展示「保存配置用的 CPA 管理密钥（可选）」卡片：
// 管理密钥改为页面从同源 localStorage 自动恢复或按需弹窗输入，
// 并仅通过请求头（X-Management-Key）随单次请求带给服务端。
func TestWizardHasNoManagementKeyCard(t *testing.T) {
	page := configWizardPage()
	if strings.Contains(page, "保存配置用的 CPA 管理密钥") {
		t.Fatal("wizard page must not show the management key setup card")
	}
	for _, banned := range []string{"saveKey", "showKeySetup", "recoverManagementKey", "localStorage", "prompt("} {
		if strings.Contains(page, banned) {
			t.Fatalf("wizard page must not keep key handling %q", banned)
		}
	}
	for _, want := range []string{"fetchBalance", "toggleDisplay", "fetchSelectedBalances", "providers"} {
		if !strings.Contains(page, want) {
			t.Fatalf("wizard page missing %q", want)
		}
	}
}

// 配置文件供应商列表走服务端保存的 management_key（插件配置内），
// 页面不参与任何密钥交互，且返回数据不得包含密钥原文。
func TestListAllProvidersMergesConfigProviders(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nmanagement_key: stored-key\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		switch hostMethod {
		case "host.auth.list":
			result, _ := json.Marshal(map[string]any{
				"files": []map[string]any{
					{"provider": "deepseek", "label": "DeepSeek 官方"},
				},
			})
			return result, nil
		case "host.http.do":
			var request httpRequest
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatalf("decode host.http.do payload: %v", err)
			}
			if !strings.HasSuffix(request.URL, "/v0/management/config") {
				t.Fatalf("unexpected management URL %q", request.URL)
			}
			if got := request.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer stored-key" {
				t.Fatalf("management call missing stored key: %#v", request.Headers)
			}
			body, _ := json.Marshal(map[string]any{
				"gemini-api-key": []map[string]any{
					{"api-key": "g-key", "base-url": "https://gemini.relay.example.com"},
				},
				"claude-api-key": []map[string]any{
					{"api-key": "c-key"},
				},
				"openai-compatibility": []map[string]any{
					{
						"name":     "nvidia",
						"base-url": "https://integrate.api.nvidia.com/v1",
						"api-key-entries": []map[string]any{
							{"api-key": "nv-1"},
							{"api-key": "nv-2"},
						},
					},
				},
			})
			raw, _ := json.Marshal(httpResponse{StatusCode: 200, Body: body})
			return raw, nil
		default:
			t.Fatalf("unexpected host method %s", hostMethod)
			return nil, nil
		}
	}
	t.Cleanup(func() { hostCallMethod = previous })
	providers, credentials, note := listAllProviders(currentConfig().ManagementKey)
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	byName := map[string]providerStatus{}
	for _, p := range providers {
		byName[p.Provider] = p
	}
	if _, ok := byName["deepseek"]; !ok {
		t.Fatalf("file provider deepseek missing: %#v", providers)
	}
	if byName["gemini"].Status != "configurable" {
		t.Fatalf("gemini status = %#v, want configurable (relay base-url)", byName["gemini"])
	}
	if byName["claude"].Status != "unsupported" {
		t.Fatalf("claude status = %#v, want unsupported (official)", byName["claude"])
	}
	if byName["openai-compatible-nvidia"].Status != "configurable" {
		t.Fatalf("openai-compatible-nvidia = %#v, want configurable", byName["openai-compatible-nvidia"])
	}
	if len(credentials) != 5 {
		t.Fatalf("credentials len = %d, want 5 (1 file + 4 config)", len(credentials))
	}
	for _, cred := range credentials {
		want := "file"
		if cred.Provider == "gemini" || cred.Provider == "claude" || cred.Provider == "openai-compatible-nvidia" {
			want = "config"
		}
		if cred.Source != want {
			t.Fatalf("credential %s source = %q, want %q", cred.Provider, cred.Source, want)
		}
	}
	// 页面数据不得包含任何密钥原文。
	raw, err := handleMethod("management.handle", []byte(`{"method":"GET","path":"/v0/resource/plugins/api-balance/config-data"}`))
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	resp := decodeManagementResponse(t, raw)
	for _, secret := range []string{"g-key", "c-key", "nv-1", "nv-2", "stored-key"} {
		if strings.Contains(string(resp.Body), secret) {
			t.Fatalf("config-data leaked secret %q: %s", secret, resp.Body)
		}
	}
}

// 管理密钥无效（401）时，供应商列表仍需展示凭据文件部分，
// 并在 note 中解释原因，而不是让整页加载失败。
func TestListAllProvidersSurfacesManagementAuthErrors(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nmanagement_key: wrong-key\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		switch hostMethod {
		case "host.auth.list":
			result, _ := json.Marshal(map[string]any{"files": []map[string]any{
				{"provider": "deepseek", "label": "DeepSeek 官方"},
			}})
			return result, nil
		case "host.http.do":
			raw, _ := json.Marshal(httpResponse{StatusCode: 401, Body: []byte("invalid management key")})
			return raw, nil
		default:
			t.Fatalf("unexpected host method %s", hostMethod)
			return nil, nil
		}
	}
	t.Cleanup(func() { hostCallMethod = previous })
	providers, _, note := listAllProviders(currentConfig().ManagementKey)
	if len(providers) != 1 {
		t.Fatalf("providers len = %d, want 1 (file-based only)", len(providers))
	}
	if !strings.Contains(note, "401") {
		t.Fatalf("note = %q, want 401 explanation", note)
	}
}

// 保存走插件配置里已保存的 management_key（存在 CPA config.yaml 中），
// 页面不参与密钥交互。
func TestSaveUsesStoredManagementKey(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nmanagement_key: stored-key\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		if hostMethod != "host.http.do" {
			t.Fatalf("unexpected host method %s", hostMethod)
		}
		var request httpRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Fatalf("decode host.http.do payload: %v", err)
		}
		if !strings.HasSuffix(request.URL, "/v0/management/plugins/api-balance/config") {
			t.Fatalf("unexpected management URL %q", request.URL)
		}
		if got := request.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer stored-key" {
			t.Fatalf("save call missing stored key: %#v", request.Headers)
		}
		raw, _ := json.Marshal(httpResponse{StatusCode: 200, Body: []byte("ok")})
		return raw, nil
	}
	t.Cleanup(func() { hostCallMethod = previous })
	raw, err := handleMethod("management.handle", []byte(`{"method":"GET","path":"/v0/resource/plugins/api-balance/config-wizard","query":{"save":["{\"enabled\":true,\"providers\":[\"openai-compatible-nvidia\"]}"]}}`))
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	resp := decodeManagementResponse(t, raw)
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("save body is not JSON: %v (%s)", err, resp.Body)
	}
	if ok, _ := payload["ok"].(bool); !ok {
		t.Fatalf("save with stored key should succeed: %s", resp.Body)
	}
}

// 向导勾选的「显示余额」名单保存后：
// 1. supportedProviders 只声明名单内的供应商；
// 2. 未勾选供应商的 quota.fetch 被拒绝；
// 3. 勾选供应商即使没有档案也能经自动识别完成余额查询。
func TestProvidersSelectionGatesQuota(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nproviders:\n  - openai-compatible-nvidia\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	supported := supportedProviders()
	found := false
	for _, name := range supported {
		if name == "openai-compatible-nvidia" {
			found = true
		}
		if name == "deepseek" {
			t.Fatalf("unselected vendor %q must not be declared", name)
		}
	}
	if !found {
		t.Fatalf("selected provider missing from supported providers: %v", supported)
	}

	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		if hostMethod != "host.http.do" {
			t.Fatalf("unexpected host method %s", hostMethod)
		}
		var request httpRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Fatalf("decode host.http.do payload: %v", err)
		}
		if !strings.Contains(request.URL, "https://relay.example.com") {
			t.Fatalf("unexpected balance endpoint %q", request.URL)
		}
		body, _ := json.Marshal(map[string]any{
			"data": map[string]any{"total_available": "12.5", "total_granted": "100", "total_used": "87.5"},
		})
		raw, _ := json.Marshal(httpResponse{StatusCode: 200, Body: body})
		return raw, nil
	}
	t.Cleanup(func() { hostCallMethod = previous })

	_, err := fetchQuota(quotaFetchRequest{Provider: "deepseek"})
	if err == nil || !strings.Contains(err.Error(), "未选择显示余额") {
		t.Fatalf("unselected provider quota.fetch error = %v, want selection gate", err)
	}

	// 勾选的供应商：无档案也能通过 base_url 自动识别（new-api 站点）。
	resp, err := fetchQuotaWithSelectionGate(quotaFetchRequest{
		Provider:    "openai-compatible-nvidia",
		StorageJSON: []byte(`{"api_key":"nv-1","base_url":"https://relay.example.com/v1"}`),
		Attributes:  map[string]string{"base_url": "https://relay.example.com/v1"},
	}, false)
	if err != nil {
		t.Fatalf("selected provider quota.fetch error = %v", err)
	}
	if !strings.Contains(resp.Groups[0].Buckets[0].Description, "12.5") {
		t.Fatalf("balance description = %q, want 12.5", resp.Groups[0].Buckets[0].Description)
	}
}

// 向导的 ?balance= 端点在服务端完成取数：凭据文件优先，
// 找不到时回退配置文件 API-Key 供应商；结果不含任何密钥。
func TestFetchProviderBalanceUsesCredentials(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\nmanagement_key: stored-key\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		switch hostMethod {
		case "host.auth.list":
			// 该供应商只有配置文件凭据，凭据文件列表为空。
			result, _ := json.Marshal(map[string]any{"files": []map[string]any{}})
			return result, nil
		case "host.http.do":
			var request httpRequest
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatalf("decode host.http.do payload: %v", err)
			}
			if strings.HasSuffix(request.URL, "/v0/management/config") {
				if got := request.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer stored-key" {
					t.Fatalf("management call missing stored key: %#v", request.Headers)
				}
				body, _ := json.Marshal(map[string]any{
					"openai-compatibility": []map[string]any{
						{
							"name":            "nvidia",
							"base-url":        "https://relay.example.com/v1",
							"api-key-entries": []map[string]any{{"api-key": "nv-1"}},
						},
					},
				})
				raw, _ := json.Marshal(httpResponse{StatusCode: 200, Body: body})
				return raw, nil
			}
			if !strings.Contains(request.URL, "https://relay.example.com") {
				t.Fatalf("unexpected balance endpoint %q", request.URL)
			}
			if got := request.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer nv-1" {
				t.Fatalf("balance call missing api key from config credential: %#v", request.Headers)
			}
			body, _ := json.Marshal(map[string]any{
				"data": map[string]any{"total_available": "66.6", "total_granted": "100", "total_used": "33.4"},
			})
			raw, _ := json.Marshal(httpResponse{StatusCode: 200, Body: body})
			return raw, nil
		default:
			t.Fatalf("unexpected host method %s", hostMethod)
			return nil, nil
		}
	}
	t.Cleanup(func() { hostCallMethod = previous })
	result := fetchProviderBalance("openai-compatible-nvidia")
	if !result.OK || !strings.Contains(result.Description, "66.6") {
		t.Fatalf("fetchProviderBalance() = %#v, want balance 66.6", result)
	}
	raw, _ := json.Marshal(result)
	for _, secret := range []string{"nv-1", "stored-key"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("balance result leaked secret %q: %s", secret, raw)
		}
	}
}

// 服务端发现供应商遇到 403 封禁时，note 必须解释封禁机制。
func TestListCredentialsClassification(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		if hostMethod != "host.auth.list" {
			t.Fatalf("expected host.auth.list, got %s", hostMethod)
		}
		result, _ := json.Marshal(map[string]any{
			"files": []map[string]any{
				{"provider": "deepseek", "label": "DeepSeek 官方"},
				{"provider": "openai", "label": "OpenAI 官方"},
				{"provider": "openai", "label": "第三方中转", "base_url": "https://relay.example.com/v1"},
				{"provider": "my-relay", "label": "中转站", "disabled": true},
			},
		})
		return result, nil
	}
	t.Cleanup(func() { hostCallMethod = previous })
	providers, credentials, note := listCredentials()
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if len(credentials) != 4 {
		t.Fatalf("credentials len = %d, want 4", len(credentials))
	}
	byName := map[string]providerStatus{}
	for _, p := range providers {
		byName[p.Provider] = p
	}
	if byName["deepseek"].Status != "ok" {
		t.Fatalf("deepseek status = %q, want ok", byName["deepseek"].Status)
	}
	if byName["openai"].Status != "configurable" {
		t.Fatalf("openai with relay base_url status = %q, want configurable", byName["openai"].Status)
	}
	if byName["my-relay"].Status != "configurable" {
		t.Fatalf("my-relay status = %q, want configurable", byName["my-relay"].Status)
	}
	if !strings.Contains(byName["my-relay"].Note, "停用") {
		t.Fatalf("my-relay note = %q, want disabled hint", byName["my-relay"].Note)
	}
}

func TestListCredentialsHostErrorIsActionable(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	previous := hostCallMethod
	hostCallMethod = func(hostMethod string, payload []byte) ([]byte, error) {
		return nil, errors.New("host bridge unavailable")
	}
	t.Cleanup(func() { hostCallMethod = previous })
	_, _, note := listCredentials()
	if !strings.Contains(note, "host.auth.list") {
		t.Fatalf("note = %q, want host.auth.list mention", note)
	}
}

func TestURLValidationRejectsCredentialInjection(t *testing.T) {
	for _, endpoint := range []string{
		"https://user:password@example.com/balance",
		"https://example.com/balance#secret",
		"ftp://example.com/balance",
	} {
		if _, err := validateEndpoint(endpoint, false); err == nil {
			t.Fatalf("validateEndpoint(%q) should reject endpoint", endpoint)
		}
	}
	if _, err := validateManagementURL("https://example.com/v0/management"); err == nil {
		t.Fatal("validateManagementURL should reject a resource path")
	}
	if got, err := validateManagementURL("http://127.0.0.1:8317/"); err != nil || got.String() != "http://127.0.0.1:8317/" {
		t.Fatalf("local management URL validation = %v, %v", got, err)
	}
}

func TestVendorByHostUsesDomainBoundaries(t *testing.T) {
	cases := map[string]string{
		"https://api.deepseek.com":        "deepseek",
		"https://sub.api.deepseek.com/v1": "deepseek",
		"https://evil-deepseek.example":   "",
		"https://openrouter.ai.evil.test": "",
		"not a url containing moonshot":   "",
	}
	for input, want := range cases {
		if got := vendorByHost(input); got != want {
			t.Fatalf("vendorByHost(%q) = %q, want %q", input, got, want)
		}
	}
}
