package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

const (
	abiVersion    = 1
	schemaVersion = 1
	pluginID      = "api-balance"
	providerID    = "api-balance"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type config struct {
	Enabled           bool              `yaml:"enabled"`
	Priority          int               `yaml:"priority"`
	Vendor            string            `yaml:"vendor"`
	BaseURL           string            `yaml:"base_url"`
	Endpoint          string            `yaml:"endpoint"`
	UsedEndpoint      string            `yaml:"used_endpoint"`
	Method            string            `yaml:"method"`
	UsedScale         float64           `yaml:"used_scale"`
	Headers           map[string]string `yaml:"headers"`
	Query             map[string]string `yaml:"query"`
	CredentialPaths   []string          `yaml:"credential_paths"`
	CredentialHeader  string            `yaml:"credential_header"`
	CredentialPrefix  string            `yaml:"credential_prefix"`
	AllowInsecureHTTP bool              `yaml:"allow_insecure_http"`
	TimeoutSeconds    int               `yaml:"timeout_seconds"`
	BalancePath       string            `yaml:"balance_path"`
	UsedPath          string            `yaml:"used_path"`
	LimitPath         string            `yaml:"limit_path"`
	CurrencyPath      string            `yaml:"currency_path"`
	PlanPath          string            `yaml:"plan_path"`
	ResetPath         string            `yaml:"reset_path"`
	WindowName        string            `yaml:"window_name"`
	// Profiles 多厂商档案：键为 CPA 凭据的 provider 名（小写），
	// 值为该凭据使用的余额配置；特殊键 default 兜底未匹配的凭据。
	// 仅支持标量字段；headers/query/credential_paths 使用全局配置。
	Profiles map[string]config `yaml:"profiles"`
}

// vendorPreset 描述一个内置厂商的余额接口与响应字段路径。
// 预设只填补未显式配置的字段，用户配置始终优先。
type vendorPreset struct {
	Endpoint     string
	UsedEndpoint string
	UsedScale    float64
	BalancePath  string
	UsedPath     string
	LimitPath    string
	CurrencyPath string
	PlanPath     string
	WindowName   string
}

var vendorPresets = map[string]vendorPreset{
	"deepseek": {
		Endpoint:     "https://api.deepseek.com/user/balance",
		BalancePath:  "balance_infos.0.total_balance",
		CurrencyPath: "balance_infos.0.currency",
		WindowName:   "余额",
	},
	"moonshot": {
		Endpoint:    "https://api.moonshot.cn/v1/users/me/balance",
		BalancePath: "data.available_balance",
		WindowName:  "余额",
	},
	// one-api 系（one-api / new-api / one-hub / done-hub 等）：
	// hard_limit_usd = 剩余 + 已用，total_usage = 已用 × 100，
	// 余额 = hard_limit_usd - total_usage × 0.01，与站点显示单位配置无关。
	"one-api": {
		Endpoint:     "{base_url}/v1/dashboard/billing/subscription",
		UsedEndpoint: "{base_url}/v1/dashboard/billing/usage",
		UsedScale:    0.01,
		UsedPath:     "total_usage",
		LimitPath:    "hard_limit_usd",
		WindowName:   "余额",
	},
	"openrouter": {
		Endpoint:   "https://openrouter.ai/api/v1/key",
		UsedPath:   "data.usage",
		LimitPath:  "data.limit",
		WindowName: "余额",
	},
	"new-api": {
		Endpoint:    "{base_url}/api/usage/token/",
		BalancePath: "data.total_available",
		LimitPath:   "data.total_granted",
		UsedPath:    "data.total_used",
		PlanPath:    "data.name",
		WindowName:  "余额",
	},
	"sub2api": {
		Endpoint:     "{base_url}/v1/usage",
		BalancePath:  "remaining",
		LimitPath:    "quota.limit",
		UsedPath:     "quota.used",
		CurrencyPath: "unit",
		PlanPath:     "planName",
		WindowName:   "余额",
	},
}

var vendorNames = []string{"custom", "deepseek", "moonshot", "one-api", "openrouter", "new-api", "sub2api"}

// autoProbeStrategies 未匹配厂商时按顺序探测的常见站点类型。
var autoProbeStrategies = []string{"new-api", "sub2api", "one-api"}

// strategyCache 记录 base_url → 已识别成功的站点类型（config），
// 避免每次查询都重复探测。
var strategyCache sync.Map

type quotaFetchRequest struct {
	AuthIndex   string            `json:"auth_index"`
	AuthID      string            `json:"auth_id"`
	Provider    string            `json:"provider"`
	StorageJSON []byte            `json:"storage_json"`
	Metadata    map[string]any    `json:"metadata"`
	Attributes  map[string]string `json:"attributes"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type quotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

type quotaFetchResponse struct {
	Subscription       *quotaSubscription `json:"subscription,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []quotaGroup       `json:"groups,omitempty"`
}

type quotaSubscription struct {
	Plan string `json:"plan,omitempty"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type httpRequest struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

type httpResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

type hostEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type pluginRegistration struct {
	SchemaVersion uint32          `json:"schema_version"`
	Metadata      pluginMetadata  `json:"metadata"`
	Capabilities  map[string]bool `json:"capabilities"`
}

type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

var (
	configMu      sync.RWMutex
	runtimeConfig = config{
		Enabled:          false,
		Method:           http.MethodGet,
		CredentialHeader: "Authorization",
		CredentialPrefix: "Bearer ",
		CredentialPaths:  []string{"access_token", "accessToken", "api_key", "apiKey", "token", "key"},
		TimeoutSeconds:   15,
		WindowName:       "balance",
	}
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "缺少 method 参数"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var lifecycle lifecycleRequest
		if err := json.Unmarshal(request, &lifecycle); err != nil {
			return nil, fmt.Errorf("解析生命周期请求失败: %w", err)
		}
		if err := applyConfig(lifecycle.ConfigYAML); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistrationResponse()), nil
	case "quota.identifier":
		return okEnvelope(map[string]string{"identifier": providerID}), nil
	case "quota.describe":
		return okEnvelope(quotaDescribeResponse{
			SupportedProviders: supportedProviders(),
			DisplayName:        "API 余额查询",
			SupportsReset:      false,
		}), nil
	case "quota.fetch":
		var req quotaFetchRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("解析余额请求失败: %w", err)
		}
		resp, err := fetchQuota(req)
		if err != nil {
			return nil, err
		}
		return okEnvelope(resp), nil
	case "quota.reset":
		return okEnvelope(map[string]any{
			"success": false,
			"message": "余额查询插件为只读，不支持重置",
		}), nil
	default:
		return errorEnvelope("unknown_method", "未知方法: "+method), nil
	}
}

func pluginRegistrationResponse() pluginRegistration {
	return pluginRegistration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginID,
			Version:          "0.5.0",
			Author:           "community",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []configField{
				{Name: "enabled", Type: "boolean", Description: "是否启用余额查询。"},
				{Name: "priority", Type: "integer", Description: "CPA 选择额度提供方时使用的优先级。"},
				{Name: "vendor", Type: "enum", Description: "内置厂商预设，自动填充接口地址与余额路径；显式配置的字段优先。custom 表示完全自定义。", EnumValues: vendorNames},
				{Name: "endpoint", Type: "string", Description: "余额接口地址，支持 {base_url}、{provider}、{auth_id}、{auth_index} 占位符；vendor 预设已含官方地址，仅自定义时填写。"},
				{Name: "used_endpoint", Type: "string", Description: "可选，已用额度的独立查询地址（如 one-api 系的 billing/usage）；留空则从主响应中取已用额度。"},
				{Name: "used_scale", Type: "string", Description: "可选，已用额度的换算倍率，例如 one-api 系 usage 单位为美分时填 0.01；默认 1。"},
				{Name: "base_url", Type: "string", Description: "可选，one-api 系等预设中 {base_url} 占位符使用的站点地址；未填写时使用凭据属性中的 base_url。"},
				{Name: "method", Type: "enum", Description: "余额请求使用的 HTTP 方法。", EnumValues: []string{"GET", "POST"}},
				{Name: "credential_paths", Type: "string", Description: "在 CPA storage_json 中查找令牌/API Key 的 JSON 路径，多个用英文逗号分隔。"},
				{Name: "credential_header", Type: "string", Description: "承载凭据的请求头，例如 Authorization 或 Cookie。"},
				{Name: "credential_prefix", Type: "string", Description: "凭据前的前缀文本，例如 Bearer；留空时默认为 Bearer 。"},
				{Name: "balance_path", Type: "string", Description: "响应 JSON 中当前余额所在路径。"},
				{Name: "limit_path", Type: "string", Description: "可选，响应 JSON 中总额度所在路径。"},
				{Name: "used_path", Type: "string", Description: "可选，响应 JSON 中已用额度所在路径。"},
				{Name: "currency_path", Type: "string", Description: "可选，响应 JSON 中币种代码所在路径。"},
				{Name: "plan_path", Type: "string", Description: "可选，响应 JSON 中套餐名称所在路径。"},
				{Name: "reset_path", Type: "string", Description: "可选，响应 JSON 中重置时间所在路径。"},
				{Name: "window_name", Type: "string", Description: "标准化额度窗口的显示名称。"},
				{Name: "allow_insecure_http", Type: "boolean", Description: "是否允许 HTTP 接口；仅在服务可信且本地内网时开启。"},
				{Name: "profiles", Type: "string", Description: "高级：多厂商档案映射，键为 CPA 凭据的 provider 名，值为一组余额配置；详见 README。仅 YAML 配置可用。"},
			},
		},
		Capabilities: map[string]bool{"quota_provider": true},
	}
}

func applyConfig(raw []byte) error {
	var next config
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := decodeConfig(raw, &next); err != nil {
			return fmt.Errorf("解析插件配置失败: %w", err)
		}
	}
	if err := applyVendorPreset(&next); err != nil {
		return err
	}
	if err := normalizeDefaults(&next); err != nil {
		return err
	}
	if len(next.Profiles) > 0 {
		resolved := make(map[string]config, len(next.Profiles))
		for name, profile := range next.Profiles {
			if err := applyVendorPreset(&profile); err != nil {
				return fmt.Errorf("档案 %q: %w", name, err)
			}
			if err := normalizeDefaults(&profile); err != nil {
				return fmt.Errorf("档案 %q: %w", name, err)
			}
			resolved[name] = profile
		}
		next.Profiles = resolved
	}
	configMu.Lock()
	runtimeConfig = next
	configMu.Unlock()
	return nil
}

// applyVendorPreset 依据 vendor 字段填补未显式配置的接口与路径。
func applyVendorPreset(next *config) error {
	vendor := strings.ToLower(strings.TrimSpace(next.Vendor))
	if vendor == "" {
		vendor = "custom"
	}
	if vendor == "custom" {
		return nil
	}
	preset, ok := vendorPresets[vendor]
	if !ok {
		return fmt.Errorf("不支持的 vendor %q，可选值: %s", next.Vendor, strings.Join(vendorNames, ", "))
	}
	if next.Endpoint == "" {
		next.Endpoint = preset.Endpoint
	}
	if next.UsedEndpoint == "" {
		next.UsedEndpoint = preset.UsedEndpoint
	}
	if preset.UsedScale != 0 && next.UsedScale == 0 {
		next.UsedScale = preset.UsedScale
	}
	if next.BalancePath == "" {
		next.BalancePath = preset.BalancePath
	}
	if next.UsedPath == "" {
		next.UsedPath = preset.UsedPath
	}
	if next.LimitPath == "" {
		next.LimitPath = preset.LimitPath
	}
	if next.CurrencyPath == "" {
		next.CurrencyPath = preset.CurrencyPath
	}
	if next.PlanPath == "" {
		next.PlanPath = preset.PlanPath
	}
	if next.WindowName == "" {
		next.WindowName = preset.WindowName
	}
	return nil
}

// normalizeDefaults 填补通用默认值并做静态校验。
func normalizeDefaults(next *config) error {
	if next.Method == "" {
		next.Method = http.MethodGet
	}
	next.Method = strings.ToUpper(strings.TrimSpace(next.Method))
	if next.CredentialHeader == "" {
		next.CredentialHeader = "Authorization"
	}
	if next.CredentialPrefix == "" {
		next.CredentialPrefix = "Bearer "
	}
	if len(next.CredentialPaths) == 0 {
		next.CredentialPaths = []string{"access_token", "accessToken", "api_key", "apiKey", "token", "key"}
	}
	if next.TimeoutSeconds <= 0 {
		next.TimeoutSeconds = 15
	}
	if next.UsedScale <= 0 {
		next.UsedScale = 1
	}
	if next.WindowName == "" {
		next.WindowName = "余额"
	}
	if next.Method != http.MethodGet && next.Method != http.MethodPost {
		return fmt.Errorf("method 仅支持 GET 或 POST，当前为 %q", next.Method)
	}
	// 含 {base_url} 等占位符的地址在运行时展开后再校验。
	if next.Endpoint != "" && !strings.Contains(next.Endpoint, "{") {
		if _, err := validateEndpoint(next.Endpoint, next.AllowInsecureHTTP); err != nil {
			return err
		}
	}
	return nil
}

// resolveProfile 按 CPA 凭据的 provider 名选择余额档案。
func resolveProfile(cfg config, provider string) (config, error) {
	if len(cfg.Profiles) == 0 {
		return cfg, nil
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	if name != "" {
		if profile, ok := cfg.Profiles[name]; ok {
			return profile, nil
		}
	}
	if profile, ok := cfg.Profiles["default"]; ok {
		return profile, nil
	}
	// 向后兼容：定义了 profiles 但顶层仍有显式 vendor/endpoint 时，
	// 未匹配的凭据回退到顶层配置。
	if cfg.Vendor != "" || cfg.Endpoint != "" {
		return cfg, nil
	}
	return config{}, fmt.Errorf("凭据 provider %q 没有匹配的余额档案，请在 profiles 中添加 %q 或 default", provider, name)
}

// supportedProviders 声明本插件可服务的凭据 provider 集合，
// 包含内置厂商名与用户定义的档案名，供 CPA 宿主路由 quota.fetch。
func supportedProviders() []string {
	set := map[string]struct{}{
		providerID:            {},
		"third-party-balance": {},
	}
	for _, vendor := range vendorNames {
		set[vendor] = struct{}{}
	}
	cfg := currentConfig()
	for name := range cfg.Profiles {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			set[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// decodeConfig accepts JSON and the flat/nested YAML subset emitted by CPA's
// plugin config normalizer. Keeping this parser local makes the plugin
// buildable with only the Go standard library.
func decodeConfig(raw []byte, out *config) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		return json.Unmarshal(trimmed, out)
	}

	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	section := ""
	profileName := ""
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		content := strings.TrimSpace(line)
		if strings.HasPrefix(content, "- ") {
			if section == "profiles" {
				return fmt.Errorf("第 %d 行: 档案内暂不支持列表写法，credential_paths 请使用逗号分隔", lineNumber)
			}
			if section != "credential_paths" {
				// CPA may pass store metadata containing lists such as
				// artifact URLs. Unknown host-managed sections are ignored.
				continue
			}
			out.CredentialPaths = append(out.CredentialPaths, parseScalar(strings.TrimSpace(strings.TrimPrefix(content, "- "))))
			continue
		}
		key, value, ok := strings.Cut(content, ":")
		if !ok {
			return fmt.Errorf("第 %d 行: 应为 key: value 格式", lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(stripComment(value))
		if indent == 0 {
			section = ""
			profileName = ""
			if value == "" {
				section = key
				continue
			}
			if err := setConfigScalar(out, key, parseScalar(value)); err != nil {
				return fmt.Errorf("第 %d 行: %w", lineNumber, err)
			}
			continue
		}
		if section == "profiles" {
			if value == "" {
				if key == "headers" || key == "query" || key == "credential_paths" {
					return fmt.Errorf("第 %d 行: 档案内暂不支持 %s 子表，请使用全局配置", lineNumber, key)
				}
				profileName = strings.ToLower(key)
				if out.Profiles == nil {
					out.Profiles = make(map[string]config)
				}
				out.Profiles[profileName] = config{}
				continue
			}
			if profileName == "" {
				return fmt.Errorf("第 %d 行: 档案字段缺少所属档案名", lineNumber)
			}
			profile := out.Profiles[profileName]
			if err := setConfigScalar(&profile, key, parseScalar(value)); err != nil {
				return fmt.Errorf("第 %d 行: %w", lineNumber, err)
			}
			out.Profiles[profileName] = profile
			continue
		}
		if section != "headers" && section != "query" {
			// CPA adds host-managed sections such as `store` after a
			// store installation. They are not plugin settings.
			continue
		}
		if value == "" {
			return fmt.Errorf("第 %d 行: 嵌套值不能为空", lineNumber)
		}
		if section == "headers" {
			if out.Headers == nil {
				out.Headers = make(map[string]string)
			}
			out.Headers[key] = parseScalar(value)
		} else {
			if out.Query == nil {
				out.Query = make(map[string]string)
			}
			out.Query[key] = parseScalar(value)
		}
	}
	return scanner.Err()
}

func setConfigScalar(out *config, key, value string) error {
	switch key {
	case "enabled":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("enabled 必须是布尔值")
		}
		out.Enabled = parsed
	case "priority":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("priority 必须是整数")
		}
		out.Priority = parsed
	case "endpoint":
		out.Endpoint = value
	case "vendor":
		out.Vendor = value
	case "base_url":
		out.BaseURL = value
	case "used_endpoint":
		out.UsedEndpoint = value
	case "used_scale":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("used_scale 必须是数字")
		}
		out.UsedScale = parsed
	case "method":
		out.Method = value
	case "credential_paths":
		out.CredentialPaths = splitCredentialPaths(value)
	case "credential_header":
		out.CredentialHeader = value
	case "credential_prefix":
		out.CredentialPrefix = value
	case "allow_insecure_http":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("allow_insecure_http 必须是布尔值")
		}
		out.AllowInsecureHTTP = parsed
	case "timeout_seconds":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("timeout_seconds 必须是整数")
		}
		out.TimeoutSeconds = parsed
	case "balance_path":
		out.BalancePath = value
	case "used_path":
		out.UsedPath = value
	case "limit_path":
		out.LimitPath = value
	case "currency_path":
		out.CurrencyPath = value
	case "plan_path":
		out.PlanPath = value
	case "reset_path":
		out.ResetPath = value
	case "window_name":
		out.WindowName = value
	default:
		return fmt.Errorf("不支持的配置项 %q", key)
	}
	return nil
}

func splitCredentialPaths(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func stripComment(value string) string {
	quoted := byte(0)
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\'', '"':
			if quoted == 0 {
				quoted = value[index]
			} else if quoted == value[index] {
				quoted = 0
			}
		case '#':
			if quoted == 0 && (index == 0 || value[index-1] == ' ') {
				return value[:index]
			}
		}
	}
	return value
}

func currentConfig() config {
	configMu.RLock()
	defer configMu.RUnlock()
	return runtimeConfig
}

func fetchQuota(req quotaFetchRequest) (quotaFetchResponse, error) {
	cfg := currentConfig()
	if !cfg.Enabled {
		return quotaFetchResponse{}, errors.New("插件未启用，请在配置中设置 enabled: true")
	}
	profile, profileErr := resolveProfile(cfg, req.Provider)
	if profileErr != nil || profile.Endpoint == "" {
		// 显式配置不可用：尝试凭据自动识别（厂商名 / 域名特征 / 接口探测）。
		auto, result, autoErr := autoDetectProfile(cfg, req)
		if autoErr != nil {
			if profileErr != nil {
				return quotaFetchResponse{}, profileErr
			}
			return quotaFetchResponse{}, autoErr
		}
		if result != nil {
			return *result, nil
		}
		profile = auto
	}
	return fetchQuotaWithProfile(profile, req)
}

// fetchQuotaWithProfile 使用已解析的档案完成一次余额查询。
func fetchQuotaWithProfile(profile config, req quotaFetchRequest) (quotaFetchResponse, error) {
	if profile.BalancePath == "" && (profile.LimitPath == "" || profile.UsedPath == "") {
		return quotaFetchResponse{}, errors.New("未配置 balance_path，且缺少 limit_path + used_path 组合（无法推导余额）")
	}
	document, err := fetchBalanceDocument(profile.Endpoint, profile, req, true)
	if err != nil {
		return quotaFetchResponse{}, err
	}
	usedDocument := document
	hasUsedDocument := false
	if profile.UsedEndpoint != "" {
		if second, err := fetchBalanceDocument(profile.UsedEndpoint, profile, req, false); err == nil {
			usedDocument = second
			hasUsedDocument = true
		}
	}
	return normalizeQuota(document, usedDocument, hasUsedDocument, profile)
}

// autoDetectProfile 在用户未配置（或配置不匹配）时，自动为凭据选择余额策略：
// 1. provider 名即为厂商名；2. base_url 域名特征；3. 已缓存的探测结果；
// 4. 按常见站点类型逐个探测。全部失败时返回指导性的错误信息。
// 探测直接命中时 result 非空（复用探测请求的结果，避免重复查询）。
func autoDetectProfile(cfg config, req quotaFetchRequest) (config, *quotaFetchResponse, error) {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if _, ok := vendorPresets[provider]; ok {
		profile := cfg
		profile.Vendor = provider
		if err := applyVendorPreset(&profile); err != nil {
			return config{}, nil, err
		}
		if strings.Contains(profile.Endpoint, "{base_url}") {
			baseURL := discoverBaseURL(&profile, req)
			if baseURL == "" {
				return config{}, nil, missingBaseURLError(provider)
			}
			profile.BaseURL = baseURL
		}
		normalizeDefaults(&profile)
		return profile, nil, nil
	}

	baseURL := discoverBaseURL(&cfg, req)
	if baseURL == "" {
		return config{}, nil, missingBaseURLError(provider)
	}

	if vendor := vendorByHost(baseURL); vendor != "" {
		profile := cfg
		profile.BaseURL = baseURL
		profile.Vendor = vendor
		if err := applyVendorPreset(&profile); err != nil {
			return config{}, nil, err
		}
		normalizeDefaults(&profile)
		return profile, nil, nil
	}

	if cached, ok := strategyCache.Load(baseURL); ok {
		if profile, ok := cached.(config); ok {
			return profile, nil, nil
		}
	}

	var lastErr error
	for _, vendor := range autoProbeStrategies {
		if _, ok := vendorPresets[vendor]; !ok {
			continue
		}
		profile := cfg
		profile.BaseURL = baseURL
		profile.Vendor = vendor
		if err := applyVendorPreset(&profile); err != nil {
			continue
		}
		normalizeDefaults(&profile)
		result, err := fetchQuotaWithProfile(profile, req)
		if err == nil {
			strategyCache.Store(baseURL, profile)
			// 探测成功即复用真实结果。
			return profile, &result, nil
		}
		lastErr = err
	}
	return config{}, nil, fmt.Errorf(
		"自动识别失败：已依次尝试 %s 的常见余额接口（base_url=%s），最后错误：%v。请在插件配置中为该凭据手动指定 profiles 或 vendor/endpoint/balance_path，并确认 API Key 有效",
		strings.Join(autoProbeStrategies, " → "), baseURL, lastErr)
}

func missingBaseURLError(provider string) error {
	return fmt.Errorf(
		"无法自动识别余额接口：凭据 provider %q 未匹配任何厂商，且凭据中未找到 base_url 等站点地址字段。请在插件配置的 profiles 中为 %q 添加档案（vendor/endpoint），或在凭据 JSON 中补充 base_url 字段",
		provider, provider)
}

// discoverBaseURL 依次从配置、凭据属性、metadata、storage_json 中寻找站点地址。
func discoverBaseURL(cfg *config, req quotaFetchRequest) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	keys := []string{"base_url", "baseURL", "baseUrl", "api_base", "api_base_url", "site_url", "endpoint"}
	for _, key := range keys {
		if v, ok := req.Attributes[key]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
		if v, ok := req.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
	}
	if len(req.StorageJSON) > 0 {
		var document any
		if json.Unmarshal(req.StorageJSON, &document) == nil {
			for _, key := range keys {
				if v, ok := stringAt(document, key); ok && strings.TrimSpace(v) != "" {
					return strings.TrimRight(strings.TrimSpace(v), "/")
				}
			}
		}
	}
	return ""
}

// vendorByHost 依据站点域名特征识别厂商。
func vendorByHost(baseURL string) string {
	host := strings.ToLower(baseURL)
	switch {
	case strings.Contains(host, "deepseek"):
		return "deepseek"
	case strings.Contains(host, "moonshot"):
		return "moonshot"
	case strings.Contains(host, "openrouter"):
		return "openrouter"
	default:
		return ""
	}
}

// fetchBalanceDocument 请求一个余额相关端点并解析为 JSON 文档。
// withQuery 表示是否合并配置中的固定 query 参数（仅主端点使用）。
func fetchBalanceDocument(endpointTemplate string, cfg config, req quotaFetchRequest, withQuery bool) (any, error) {
	endpoint := expandEndpoint(endpointTemplate, cfg.BaseURL, req)
	if _, err := validateEndpoint(endpoint, cfg.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	headers := make(map[string][]string, len(cfg.Headers)+1)
	for name, value := range cfg.Headers {
		headers[name] = []string{expandTemplate(value, req)}
	}
	credential := findCredential(req.StorageJSON, cfg.CredentialPaths)
	if credential != "" && cfg.CredentialHeader != "" {
		headers[cfg.CredentialHeader] = []string{cfg.CredentialPrefix + credential}
	}
	if withQuery && len(cfg.Query) > 0 {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("解析 endpoint 失败: %w", err)
		}
		query := parsed.Query()
		for name, value := range cfg.Query {
			query.Set(name, expandTemplate(value, req))
		}
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
	}
	request := httpRequest{Method: cfg.Method, URL: endpoint, Headers: headers}
	if cfg.Method == http.MethodPost {
		request.Headers["Content-Type"] = []string{"application/json"}
		request.Body = []byte(`{}`)
	}
	response, err := callHostHTTP(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("余额接口返回 HTTP %d", response.StatusCode)
	}
	var document any
	if err := json.Unmarshal(response.Body, &document); err != nil {
		return nil, fmt.Errorf("余额接口返回的不是有效 JSON: %w", err)
	}
	return document, nil
}

func normalizeQuota(document any, usedDocument any, hasUsedDocument bool, cfg config) (quotaFetchResponse, error) {
	balance, hasBalance := numberAt(document, cfg.BalancePath)
	used, hasUsed := numberAt(usedDocument, cfg.UsedPath)
	if !hasUsed && hasUsedDocument {
		// 第二端点解析失败时回退到主文档。
		used, hasUsed = numberAt(document, cfg.UsedPath)
	}
	if hasUsed && cfg.UsedScale != 1 {
		used *= cfg.UsedScale
	}
	limit, hasLimit := numberAt(document, cfg.LimitPath)
	if !hasBalance && hasLimit && hasUsed {
		// 只有总额度与已用额度时，余额 = 总额度 - 已用。
		balance = limit - used
		hasBalance = true
	}
	if !hasBalance {
		return quotaFetchResponse{}, fmt.Errorf("balance_path %q 未解析到数字", cfg.BalancePath)
	}
	if !hasLimit && hasUsed {
		limit = balance + used
		hasLimit = limit > 0
	}
	fraction := balance
	if hasLimit && limit > 0 {
		fraction = balance / limit
	}
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	currency, _ := stringAt(document, cfg.CurrencyPath)
	plan, _ := stringAt(document, cfg.PlanPath)
	reset, _ := stringAt(document, cfg.ResetPath)
	description := "balance=" + formatNumber(balance)
	if hasLimit {
		description += " / limit=" + formatNumber(limit)
	}
	if hasUsed {
		description += " / used=" + formatNumber(used)
	}
	if currency != "" {
		description += " " + currency
	}
	bucket := quotaBucket{
		Window:            cfg.WindowName,
		RemainingFraction: fraction,
		ResetTime:         reset,
		Description:       description,
	}
	resp := quotaFetchResponse{
		Groups: []quotaGroup{{DisplayName: "余额", Buckets: []quotaBucket{bucket}}},
	}
	if plan != "" {
		resp.Subscription = &quotaSubscription{Plan: plan}
	}
	return resp, nil
}

func callHostHTTP(request httpRequest) (httpResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return httpResponse{}, fmt.Errorf("编码宿主 HTTP 请求失败: %w", err)
	}
	cMethod := C.CString("host.http.do")
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(payload) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(requestPtr))
	}
	if C.call_host_api(cMethod, requestPtr, C.size_t(len(payload)), &response) != 0 {
		return httpResponse{}, errors.New("宿主 HTTP 桥接调用失败")
	}
	if response.ptr == nil || response.len == 0 {
		return httpResponse{}, errors.New("宿主 HTTP 桥接返回空响应")
	}
	raw := C.GoBytes(response.ptr, C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)
	var envelope hostEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return httpResponse{}, fmt.Errorf("解码宿主 HTTP 响应失败: %w", err)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return httpResponse{}, fmt.Errorf("宿主 HTTP 请求失败: %s", envelope.Error.Message)
		}
		return httpResponse{}, errors.New("宿主 HTTP 请求失败")
	}
	var result httpResponse
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return httpResponse{}, fmt.Errorf("解码宿主 HTTP 结果失败: %w", err)
	}
	return result, nil
}

func findCredential(raw []byte, paths []string) string {
	if len(raw) == 0 {
		return ""
	}
	var document any
	if json.Unmarshal(raw, &document) != nil {
		return ""
	}
	for _, path := range paths {
		if value, ok := stringAt(document, path); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func numberAt(document any, path string) (float64, bool) {
	value, ok := valueAt(document, path)
	if !ok {
		return 0, false
	}
	switch value := value.(type) {
	case float64:
		return value, true
	case json.Number:
		number, err := value.Float64()
		return number, err == nil
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func stringAt(document any, path string) (string, bool) {
	value, ok := valueAt(document, path)
	if !ok {
		return "", false
	}
	switch value := value.(type) {
	case string:
		return value, true
	case float64:
		return formatNumber(value), true
	case bool:
		return strconv.FormatBool(value), true
	default:
		return "", false
	}
}

func valueAt(document any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	current := document
	for _, part := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		if part == "" {
			continue
		}
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func expandEndpoint(endpoint string, baseURL string, req quotaFetchRequest) string {
	if baseURL == "" {
		baseURL = req.Attributes["base_url"]
	}
	if baseURL == "" {
		baseURL = req.Attributes["baseURL"]
	}
	return strings.NewReplacer(
		"{base_url}", strings.TrimRight(baseURL, "/"),
		"{provider}", req.Provider,
		"{auth_id}", req.AuthID,
		"{auth_index}", req.AuthIndex,
	).Replace(endpoint)
}

func expandTemplate(value string, req quotaFetchRequest) string {
	return strings.NewReplacer(
		"{provider}", req.Provider,
		"{auth_id}", req.AuthID,
		"{auth_index}", req.AuthIndex,
	).Replace(value)
}

func validateEndpoint(endpoint string, allowInsecure bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("endpoint 必须是完整的绝对 URL")
	}
	if parsed.Scheme != "https" && !(allowInsecure && parsed.Scheme == "http") {
		return nil, errors.New("endpoint 必须使用 https，除非 allow_insecure_http 为 true")
	}
	return parsed, nil
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func okEnvelope(value any) []byte {
	raw, _ := json.Marshal(value)
	envelope, _ := json.Marshal(envelope{OK: true, Result: raw})
	return envelope
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
