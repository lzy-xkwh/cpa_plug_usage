package main

import (
	"encoding/json"
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
