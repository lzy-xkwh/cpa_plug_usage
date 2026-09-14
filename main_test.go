package main

import (
	"encoding/json"
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
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			ManagementConfigured bool `json:"management_configured"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK || !env.Result.ManagementConfigured {
		t.Fatalf("config-data envelope = %s (%v)", raw, err)
	}
}

func TestSaveRequiresManagementKey(t *testing.T) {
	if err := applyConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}
	result := saveConfigViaManagementAPI(`{"enabled":true}`)
	if result["ok"] != false {
		t.Fatalf("save without management_key should fail, got %v", result)
	}
}
