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
	"math"
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
	schemaVersion = 6
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
	Enabled           bool              `yaml:"enabled" json:"enabled"`
	Priority          int               `yaml:"priority" json:"priority"`
	Vendor            string            `yaml:"vendor" json:"vendor"`
	BaseURL           string            `yaml:"base_url" json:"base_url"`
	Endpoint          string            `yaml:"endpoint" json:"endpoint"`
	UsedEndpoint      string            `yaml:"used_endpoint" json:"used_endpoint"`
	Method            string            `yaml:"method" json:"method"`
	UsedScale         float64           `yaml:"used_scale" json:"used_scale"`
	Headers           map[string]string `yaml:"headers" json:"headers"`
	Query             map[string]string `yaml:"query" json:"query"`
	CredentialPaths   []string          `yaml:"credential_paths" json:"credential_paths"`
	CredentialHeader  string            `yaml:"credential_header" json:"credential_header"`
	CredentialPrefix  string            `yaml:"credential_prefix" json:"credential_prefix"`
	AllowInsecureHTTP bool              `yaml:"allow_insecure_http" json:"allow_insecure_http"`
	TimeoutSeconds    int               `yaml:"timeout_seconds" json:"timeout_seconds"`
	// ManagementKey / ManagementURL 供配置向导在服务端调用 CPA 管理 API：
	// 自动列出已配置供应商、保存配置。仅保存在插件配置里，不回传给页面。
	ManagementKey string `yaml:"management_key" json:"management_key"`
	ManagementURL string `yaml:"management_url" json:"management_url"`
	// Providers 是「显示余额」的供应商名单（provider 名，即 profiles 的键）。
	// 非空时 CPA 只会把这些供应商的余额查询路由给本插件；为空时沿用
	// 旧规则（内置厂商名 + 全部档案名）。
	Providers []string `yaml:"providers" json:"providers"`
	// SelectedCredentials 非 nil 时表示用户明确保存了逐账号选择，空数组也有意义。
	SelectedCredentials []string `yaml:"selected_credentials" json:"selected_credentials"`
	BalancePath         string   `yaml:"balance_path" json:"balance_path"`
	UsedPath            string   `yaml:"used_path" json:"used_path"`
	LimitPath           string   `yaml:"limit_path" json:"limit_path"`
	CurrencyPath        string   `yaml:"currency_path" json:"currency_path"`
	PlanPath            string   `yaml:"plan_path" json:"plan_path"`
	ResetPath           string   `yaml:"reset_path" json:"reset_path"`
	WindowName          string   `yaml:"window_name" json:"window_name"`
	// Profiles 多厂商档案：键为 CPA 凭据的 provider 名（小写），
	// 值为该凭据使用的余额配置；特殊键 default 兜底未匹配的凭据。
	// 仅支持标量字段；headers/query/credential_paths 使用全局配置。
	Profiles map[string]config `yaml:"profiles" json:"profiles"`
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
	AuthIndex     string            `json:"auth_index"`
	AuthID        string            `json:"auth_id"`
	CredentialKey string            `json:"credential_key,omitempty"`
	Provider      string            `json:"provider"`
	StorageJSON   []byte            `json:"storage_json"`
	Metadata      map[string]any    `json:"metadata"`
	Attributes    map[string]string `json:"attributes"`
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
	Balance           float64 `json:"-"`
	Limit             float64 `json:"-"`
	Used              float64 `json:"-"`
	HasLimit          bool    `json:"-"`
	HasUsed           bool    `json:"-"`
	Currency          string  `json:"-"`
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
			DisplayName:        "供应商余额查询",
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
	case "management.register":
		return okEnvelope(map[string]any{
			"routes": []map[string]any{
				{
					"method":      http.MethodPost,
					"path":        "/plugins/api-balance/config-wizard/key",
					"description": "通过当前 CPA/CPAMP 管理认证保存插件连接信息",
				},
				{
					"method":      http.MethodPatch,
					"path":        "/plugins/api-balance/config-wizard/key",
					"description": "通过当前 CPA/CPAMP 管理认证保存插件连接信息",
				},
			},
			"resources": []map[string]any{
				{
					"path":        "/config-wizard",
					"menu":        "余额",
					"description": "查看与配置各供应商余额，无需手写 YAML",
				},
				{
					// 数据端点：不带 menu，不出现在管理菜单里，仅供页面调用。
					"path":        "/config-data",
					"description": "向导页数据源：当前配置与供应商余额支持状态",
				},
			},
		}), nil
	case "management.handle":
		return handleManagementRPC(request)
	default:
		return errorEnvelope("unknown_method", "未知方法: "+method), nil
	}
}

func pluginRegistrationResponse() pluginRegistration {
	return pluginRegistration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginID,
			Version:          "0.9.9",
			Author:           "community",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []configField{
				{Name: "enabled", Type: "boolean", Description: "是否启用余额查询。"},
				{Name: "priority", Type: "integer", Description: "CPA 选择额度提供方时使用的优先级。"},
				{Name: "management_key", Type: "string", Description: "CPA 管理密钥：填写 remote-management.secret-key 的原始明文（不是 CPA 启动后写回配置的 bcrypt 哈希）；本插件配置字段名为 management_key，用于读取配置文件中的 API-Key 供应商及保存配置。"},
				{Name: "management_url", Type: "string", Description: "可选，CPA 服务管理接口地址，默认 http://127.0.0.1:8317。"},
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
		Capabilities: map[string]bool{"quota_provider": true, "management_api": true},
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
	if len(next.Providers) > 0 {
		resolved := make([]string, 0, len(next.Providers))
		seen := map[string]struct{}{}
		for _, name := range next.Providers {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			resolved = append(resolved, name)
		}
		next.Providers = resolved
	}
	if len(next.Profiles) > 0 {
		resolved := make(map[string]config, len(next.Profiles))
		for name, profile := range next.Profiles {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				return errors.New("profiles 不能包含空档案名")
			}
			if _, exists := resolved[name]; exists {
				return fmt.Errorf("profiles 包含重复档案名 %q（大小写不应不同）", name)
			}
			// headers/query/credential_paths 是全局请求设置；档案只覆盖
			// 自身的余额字段，避免多供应商配置后认证设置意外丢失。
			profile = inheritGlobalRequestConfig(next, profile)
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
	clearStrategyCache()
	configMu.Lock()
	runtimeConfig = next
	configMu.Unlock()
	return nil
}

func inheritGlobalRequestConfig(base, profile config) config {
	if profile.Headers == nil {
		profile.Headers = cloneStringMap(base.Headers)
	}
	if profile.Query == nil {
		profile.Query = cloneStringMap(base.Query)
	}
	if len(profile.CredentialPaths) == 0 {
		profile.CredentialPaths = append([]string(nil), base.CredentialPaths...)
	}
	return profile
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func clearStrategyCache() {
	strategyCache.Range(func(key, _ any) bool {
		strategyCache.Delete(key)
		return true
	})
}

// applyVendorPreset 依据 vendor 字段填补未显式配置的接口与路径。
func applyVendorPreset(next *config) error {
	vendor := strings.ToLower(strings.TrimSpace(next.Vendor))
	if vendor == "" {
		vendor = "custom"
	}
	next.Vendor = vendor
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
	if next.ManagementURL != "" {
		parsed, err := validateManagementURL(next.ManagementURL)
		if err != nil {
			return err
		}
		next.ManagementURL = strings.TrimRight(parsed.String(), "/")
	}
	// 含 {base_url} 等占位符的地址在运行时展开后再校验。
	if next.Endpoint != "" && !strings.Contains(next.Endpoint, "{") {
		if _, err := validateEndpoint(next.Endpoint, next.AllowInsecureHTTP); err != nil {
			return err
		}
	}
	return nil
}

// resolveProfile 按 credential key、provider 名选择余额档案，兼容旧版 provider 配置。
func resolveProfile(cfg config, provider string, credentialKeys ...string) (config, error) {
	if len(cfg.Profiles) == 0 {
		return cfg, nil
	}
	for _, key := range credentialKeys {
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			if profile, ok := cfg.Profiles[key]; ok {
				return inheritGlobalRequestConfig(cfg, profile), nil
			}
		}
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	if name != "" {
		if profile, ok := cfg.Profiles[name]; ok {
			return inheritGlobalRequestConfig(cfg, profile), nil
		}
	}
	if profile, ok := cfg.Profiles["default"]; ok {
		return inheritGlobalRequestConfig(cfg, profile), nil
	}
	if cfg.Vendor != "" || cfg.Endpoint != "" {
		return cfg, nil
	}
	return config{}, fmt.Errorf("凭据 provider %q 没有匹配的余额档案，请配置账号档案、%q 或 default", provider, name)
}

// supportedProviders 声明本插件可服务的凭据 provider 集合，供 CPA 宿主
// 路由 quota.fetch。用户在向导里勾选的「显示余额」名单（providers）优先：
// 非空时只声明名单内的供应商，未勾选的不再参与余额查询；为空时沿用旧规则
// （内置厂商名 + 全部档案名），保持向后兼容。
func supportedProviders() []string {
	cfg := currentConfig()
	if len(cfg.Providers) > 0 {
		set := map[string]struct{}{
			providerID:            {},
			"third-party-balance": {},
		}
		for _, name := range cfg.Providers {
			set[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
		}
		out := make([]string, 0, len(set))
		for name := range set {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	set := map[string]struct{}{
		providerID:            {},
		"third-party-balance": {},
	}
	for _, vendor := range vendorNames {
		set[vendor] = struct{}{}
	}
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
			if section == "providers" {
				out.Providers = append(out.Providers, parseScalar(strings.TrimSpace(strings.TrimPrefix(content, "- "))))
				continue
			}
			if section == "selected_credentials" {
				out.SelectedCredentials = append(out.SelectedCredentials, parseScalar(strings.TrimSpace(strings.TrimPrefix(content, "- "))))
				continue
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
	case "management_key":
		out.ManagementKey = strings.TrimSpace(value)
	case "management_url":
		out.ManagementURL = strings.TrimSpace(value)
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
	case "selected_credentials":
		if strings.TrimSpace(value) == "[]" {
			out.SelectedCredentials = []string{}
		} else {
			out.SelectedCredentials = splitCredentialPaths(value)
		}
	default:
		// 未知配置项不再致命：旧版本插件加载含新键的配置时忽略并写宿主日志，
		// 避免 reconfigure 失败导致插件被宿主摘除。
		hostLog("warn", fmt.Sprintf(
			"api-balance: 忽略未知配置项 %q（可能是当前插件版本不支持的新选项；如需使用请升级插件）",
			key))
		return nil
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
	return fetchQuotaWithSelectionGate(req, true)
}

// fetchQuotaWithSelectionGate 控制「providers 选择名单」是否生效：
// CPA 路由的 quota.fetch 必须受限，而向导页的手动查询不受限，
// 否则保存前的预览查询会被自己的名单拦住。
func fetchQuotaWithSelectionGate(req quotaFetchRequest, enforceSelection bool) (quotaFetchResponse, error) {
	cfg := currentConfig()
	if !cfg.Enabled {
		return quotaFetchResponse{}, errors.New("插件未启用，请在配置中设置 enabled: true")
	}
	if enforceSelection {
		provider := strings.ToLower(strings.TrimSpace(req.Provider))
		if cfg.SelectedCredentials != nil {
			key := strings.TrimSpace(req.CredentialKey)
			if key == "" {
				key = credentialProfileKey(provider, req.Attributes["base_url"], req.AuthIndex)
			}
			found := false
			for _, name := range cfg.SelectedCredentials {
				if strings.EqualFold(strings.TrimSpace(name), key) || strings.EqualFold(strings.TrimSpace(name), provider) {
					found = true
					break
				}
			}
			if !found {
				return quotaFetchResponse{}, fmt.Errorf("账号 %q 未选择显示余额：请在余额面板中勾选并保存", key)
			}
		} else if len(cfg.Providers) > 0 {
			found := false
			for _, name := range cfg.Providers {
				if name == provider {
					found = true
					break
				}
			}
			if !found {
				return quotaFetchResponse{}, fmt.Errorf("供应商 %q 未选择显示余额：请在余额向导中勾选后保存", req.Provider)
			}
		}
	}
	profileKeys := []string{req.CredentialKey, req.AuthIndex, req.AuthID}
	if req.AuthIndex != "" {
		profileKeys = append([]string{credentialProfileKey(req.Provider, req.Attributes["base_url"], req.AuthIndex)}, profileKeys...)
	}
	profile, profileErr := resolveProfile(cfg, req.Provider, profileKeys...)
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
		if err := normalizeDefaults(&profile); err != nil {
			return config{}, nil, err
		}
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
		if err := normalizeDefaults(&profile); err != nil {
			return config{}, nil, err
		}
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

func discoverBaseURL(cfg *config, req quotaFetchRequest) string {
	// 同一 provider 可能对应多个站点；带凭据身份时先读该凭据的站点字段。
	if req.CredentialKey != "" || req.AuthIndex != "" || req.AuthID != "" {
		if baseURL := discoverRequestBaseURL(req); baseURL != "" {
			return baseURL
		}
	}
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	return discoverRequestBaseURL(req)
}

func discoverRequestBaseURL(req quotaFetchRequest) string {
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

// vendorByHost 依据站点域名特征识别厂商。只匹配主机名，避免把
// evil-deepseek.example 等无关域名误判为官方厂商。
func vendorByHost(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	matches := func(domains ...string) bool {
		for _, domain := range domains {
			if host == domain || strings.HasSuffix(host, "."+domain) {
				return true
			}
		}
		return false
	}
	switch {
	case matches("deepseek.com"):
		return "deepseek"
	case matches("moonshot.cn", "moonshot.ai", "kimi.com"):
		return "moonshot"
	case matches("openrouter.ai"):
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
		Balance:           balance,
		Limit:             limit,
		Used:              used,
		HasLimit:          hasLimit,
		HasUsed:           hasUsed,
		Currency:          currency,
	}
	resp := quotaFetchResponse{
		Groups: []quotaGroup{{DisplayName: "余额", Buckets: []quotaBucket{bucket}}},
	}
	if plan != "" {
		resp.Subscription = &quotaSubscription{Plan: plan}
	}
	return resp, nil
}

// hostCallMethod 是宿主回调的测试接缝：生产环境始终是 callHostMethod，
// 测试可替换以模拟宿主的 HTTP/日志行为。
var hostCallMethod = callHostMethod

// callHostMethod 调用宿主回调 RPC（host.http.do / host.log 等）并解包结果。
func callHostMethod(hostMethod string, payload []byte) ([]byte, error) {
	cMethod := C.CString(hostMethod)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(payload) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(requestPtr))
	}
	if C.call_host_api(cMethod, requestPtr, C.size_t(len(payload)), &response) != 0 {
		return nil, errors.New("宿主回调调用失败")
	}
	if response.ptr == nil || response.len == 0 {
		return nil, errors.New("宿主回调返回空响应")
	}
	raw := C.GoBytes(response.ptr, C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)
	var envelope hostEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("解码宿主回调响应失败: %w", err)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return nil, fmt.Errorf("宿主回调请求失败: %s", envelope.Error.Message)
		}
		return nil, errors.New("宿主回调请求失败")
	}
	return envelope.Result, nil
}

// hostLog 尽力向宿主日志写一条插件日志，失败时静默。
func hostLog(level, message string) {
	payload, err := json.Marshal(map[string]any{"level": level, "message": message})
	if err != nil {
		return
	}
	_, _ = callHostMethod("host.log", payload)
}

func callHostHTTP(request httpRequest) (httpResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return httpResponse{}, fmt.Errorf("编码宿主 HTTP 请求失败: %w", err)
	}
	raw, err := hostCallMethod("host.http.do", payload)
	if err != nil {
		return httpResponse{}, err
	}
	var result httpResponse
	if err := json.Unmarshal(raw, &result); err != nil {
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
	requestBaseURL := req.Attributes["base_url"]
	if requestBaseURL == "" {
		requestBaseURL = req.Attributes["baseURL"]
	}
	if (req.CredentialKey != "" || req.AuthIndex != "" || req.AuthID != "") && requestBaseURL != "" {
		baseURL = requestBaseURL
	}
	if baseURL == "" {
		baseURL = requestBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	// CPA's OpenAI-compatible base URL often ends in /v1. Preset billing
	// routes start at the site root, whereas custom routes may need /v1.
	for _, vendor := range autoProbeStrategies {
		preset := vendorPresets[vendor]
		if endpoint == preset.Endpoint || endpoint == preset.UsedEndpoint {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			break
		}
	}
	return strings.NewReplacer(
		"{base_url}", baseURL,
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
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("endpoint 必须是完整的绝对 URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("endpoint 不得包含用户信息或 fragment")
	}
	if parsed.Scheme != "https" && !(allowInsecure && parsed.Scheme == "http") {
		return nil, errors.New("endpoint 必须使用 https，除非 allow_insecure_http 为 true")
	}
	return parsed, nil
}

func validateManagementURL(raw string) (*url.URL, error) {
	parsed, err := validateEndpoint(raw, true)
	if err != nil {
		return nil, fmt.Errorf("management_url 无效: %w", err)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("management_url 不应包含资源路径")
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

// ---------- 管理后台：配置向导 ----------

type managementRPCRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Query   map[string][]string `json:"query"`
	Body    []byte              `json:"body"`
}

func firstQuery(query map[string][]string, key string) string {
	if values, ok := query[key]; ok && len(values) > 0 {
		return values[0]
	}
	return ""
}

func firstHeader(headers map[string][]string, names ...string) string {
	for _, name := range names {
		for key, values := range headers {
			if strings.EqualFold(key, name) && len(values) > 0 {
				return strings.TrimSpace(values[0])
			}
		}
	}
	return ""
}

func managementKeyFromHeaders(headers map[string][]string) string {
	raw := firstHeader(headers, "Authorization")
	if len(raw) >= len("Bearer ") && strings.EqualFold(raw[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(raw[len("Bearer "):])
	}
	if raw != "" {
		return raw
	}
	return firstHeader(headers, "X-Management-Key")
}

func saveKeyViaManagementRequest(req managementRPCRequest) map[string]any {
	key := managementKeyFromHeaders(req.Headers)
	if key == "" {
		return map[string]any{
			"ok":      false,
			"message": "请求未携带管理认证。CPAMP 页面请输入 CPAMP 管理员密钥；直接访问 CPA 页面请输入 CPA Management Key。",
		}
	}
	var input struct {
		ManagementURL string `json:"management_url"`
	}
	if len(bytes.TrimSpace(req.Body)) > 0 {
		if err := json.Unmarshal(req.Body, &input); err != nil {
			return map[string]any{"ok": false, "message": "解析 CPA 地址失败: " + err.Error()}
		}
	}
	cfg := currentConfig()
	managementURL := strings.TrimSpace(input.ManagementURL)
	if managementURL == "" {
		managementURL = managementBaseURL(cfg)
	}
	parsed, err := validateManagementURL(managementURL)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	cfg.ManagementKey = key
	cfg.ManagementURL = strings.TrimRight(parsed.String(), "/")
	clean := map[string]any{
		"management_key": cfg.ManagementKey,
		"management_url": cfg.ManagementURL,
	}
	merged, err := mergeWizardConfig(currentConfig(), clean)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	payload, err := json.Marshal(merged)
	if err != nil {
		return map[string]any{"ok": false, "message": "配置编码失败: " + err.Error()}
	}
	response, err := callHostHTTP(httpRequest{
		Method:  http.MethodPut,
		URL:     cfg.ManagementURL + "/v0/management/plugins/api-balance/config",
		Headers: managementHeaders(key, "application/json"),
		Body:    payload,
	})
	if err != nil {
		return map[string]any{"ok": false, "message": "调用 CPA 管理 API 失败: " + err.Error()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(response.Body))
		if message == "" {
			message = "管理 API 未返回错误详情"
		}
		return map[string]any{"ok": false, "message": fmt.Sprintf("保存失败（HTTP %d）：%s", response.StatusCode, message)}
	}
	configMu.Lock()
	runtimeConfig = cfg
	configMu.Unlock()
	return map[string]any{"ok": true, "message": "CPA 连接已保存，正在刷新供应商列表。"}
}

// handleManagementRPC 处理宿主转发的插件管理/资源请求：
//
//	GET /v0/resource/plugins/api-balance/config-wizard           向导页面
//	GET /v0/resource/plugins/api-balance/config-wizard?save=JSON  保存配置（服务端直调管理 API）
//	GET /v0/resource/plugins/api-balance/config-wizard?balance=P  查询某供应商的实际余额
//	GET /v0/resource/plugins/api-balance/config-data             当前配置 + 供应商支持状态
func handleManagementRPC(request []byte) ([]byte, error) {
	var req managementRPCRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("解析管理请求失败: %w", err)
	}
	if strings.HasSuffix(strings.TrimRight(req.Path, "/"), "/config-wizard/key") {
		if req.Method != http.MethodPost && req.Method != http.MethodPatch {
			return okEnvelope(managementTextResponse(http.StatusMethodNotAllowed, "text/plain; charset=utf-8", []byte("本接口仅支持 POST 或 PATCH 请求"))), nil
		}
		payload, err := json.Marshal(saveKeyViaManagementRequest(req))
		if err != nil {
			return nil, fmt.Errorf("编码管理认证保存结果失败: %w", err)
		}
		return okEnvelope(managementJSONResponse(payload)), nil
	}
	if provider := firstQuery(req.Query, "balance"); provider != "" {
		payload, err := json.Marshal(fetchProviderBalance(provider, firstQuery(req.Query, "credential_key")))
		if err != nil {
			return nil, fmt.Errorf("编码余额结果失败: %w", err)
		}
		return okEnvelope(managementJSONResponse(payload)), nil
	}
	if saveJSON := firstQuery(req.Query, "save"); saveJSON != "" {
		payload, err := json.Marshal(saveConfigViaManagementAPI(saveJSON))
		if err != nil {
			return nil, fmt.Errorf("编码保存结果失败: %w", err)
		}
		return okEnvelope(managementJSONResponse(payload)), nil
	}
	if strings.HasSuffix(strings.TrimRight(req.Path, "/"), "/config-data") {
		payload, err := json.Marshal(wizardDataResponse())
		if err != nil {
			return nil, fmt.Errorf("编码向导数据失败: %w", err)
		}
		return okEnvelope(managementJSONResponse(payload)), nil
	}
	switch req.Method {
	case http.MethodGet, "":
		return okEnvelope(managementTextResponse(http.StatusOK, "text/html; charset=utf-8", []byte(configWizardPage()))), nil
	default:
		return okEnvelope(managementTextResponse(http.StatusMethodNotAllowed, "text/plain; charset=utf-8", []byte("本页面仅支持 GET 请求"))), nil
	}
}

func configWizardPage() string {
	return wizardHTML
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

func managementJSONResponse(payload []byte) managementResponse {
	return managementResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       payload,
	}
}

func managementTextResponse(status int, contentType string, payload []byte) managementResponse {
	return managementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {contentType}},
		Body:       payload,
	}
}

// knownNoBalanceProviders 官方侧没有公开余额接口的内置供应商，
// 在向导里明确标注“无法查询”，避免用户白配置。
var knownNoBalanceProviders = map[string]string{
	"openai":      "OpenAI 官方未提供可查询余额的公开接口",
	"claude":      "Claude 官方未提供可查询余额的公开接口",
	"claude-code": "Claude 官方未提供可查询余额的公开接口",
	"anthropic":   "Anthropic 官方未提供可查询余额的公开接口",
	"codex":       "OpenAI 官方未提供可查询余额的公开接口",
	"gemini":      "Gemini 官方未提供可查询余额的公开接口",
	"gemini-cli":  "Gemini 官方未提供可查询余额的公开接口",
	"qwen":        "Qwen 官方未提供可查询余额的公开接口",
	"qwen-code":   "Qwen 官方未提供可查询余额的公开接口",
}

// credentialInfo 向导诊断用的单条凭据摘要（不含任何密钥）。
type credentialInfo struct {
	Provider    string `json:"provider"`
	Name        string `json:"name,omitempty"`
	Label       string `json:"label,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	ProfileKey  string `json:"profile_key,omitempty"`
	AuthIndex   string `json:"auth_index,omitempty"`
	Status      string `json:"status,omitempty"`
	Note        string `json:"note,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	RuntimeOnly bool   `json:"runtime_only,omitempty"`
	// Source 区分凭据来源：file = 宿主凭据文件（host.auth.list），
	// config = config.yaml 中的 API-Key 供应商（经管理 API 聚合）。
	Source string `json:"source,omitempty"`
}

type providerStatus struct {
	Provider        string `json:"provider"`
	Label           string `json:"label,omitempty"`
	Status          string `json:"status"` // ok | unsupported | configurable
	Note            string `json:"note,omitempty"`
	CredentialCount int    `json:"credential_count"`
	ActiveCount     int    `json:"active_count"`
	SiteCount       int    `json:"site_count"`
}

// wizardDataResponse 返回向导页所需的全部数据：当前配置（脱敏）与
// 已配置供应商的余额支持状态。management_key 只在服务端使用，不下发。
func wizardDataResponse() map[string]any {
	cfg := currentConfig()
	resp := map[string]any{
		"management_configured": cfg.ManagementKey != "",
		"management_url":        managementBaseURL(cfg),
		"config":                sanitizedPageConfig(cfg),
	}
	providers, credentials, note := listAllProviders(cfg.ManagementKey)
	resp["providers"] = providers
	resp["credentials"] = credentials
	if strings.TrimSpace(cfg.ManagementKey) == "" {
		note = strings.TrimSpace(note + " 插件配置未设置 management_key：config.yaml 中的 API-Key 供应商（AI 提供商页签里的大部分）暂无法列出，设置后刷新即可。")
	}
	if note != "" {
		resp["providers_note"] = note
	}
	return resp
}

// sanitizedPageConfig 把当前配置裁剪成页面可用的形态（不含密钥与请求头）。
func sanitizedPageConfig(cfg config) map[string]any {
	out := map[string]any{
		"enabled":              cfg.Enabled,
		"priority":             cfg.Priority,
		"providers":            cfg.Providers,
		"selected_credentials": cfg.SelectedCredentials,
		"profiles":             map[string]any{},
	}
	profiles := map[string]any{}
	for name, p := range cfg.Profiles {
		profiles[name] = profileToPage(p)
	}
	out["profiles"] = profiles
	if len(cfg.Profiles) == 0 && (cfg.Vendor != "" || cfg.Endpoint != "" || cfg.BaseURL != "" || cfg.BalancePath != "" || cfg.LimitPath != "") {
		out["__top__"] = profileToPage(cfg)
	}
	return out
}

func profileToPage(p config) map[string]any {
	m := map[string]any{}
	set := func(key, value string) {
		if value != "" {
			m[key] = value
		}
	}
	set("vendor", p.Vendor)
	set("base_url", p.BaseURL)
	set("endpoint", p.Endpoint)
	set("used_endpoint", p.UsedEndpoint)
	set("balance_path", p.BalancePath)
	set("used_path", p.UsedPath)
	set("limit_path", p.LimitPath)
	set("currency_path", p.CurrencyPath)
	set("plan_path", p.PlanPath)
	set("window_name", p.WindowName)
	if p.UsedScale != 0 && p.UsedScale != 1 {
		m["used_scale"] = p.UsedScale
	}
	return m
}

func managementHeaders(key string, contentType string) map[string][]string {
	headers := map[string][]string{
		"Authorization":    {"Bearer " + key},
		"X-Management-Key": {key},
	}
	if contentType != "" {
		headers["Content-Type"] = []string{contentType}
	}
	return headers
}

func managementBaseURL(cfg config) string {
	if cfg.ManagementURL != "" {
		return strings.TrimRight(cfg.ManagementURL, "/")
	}
	return "http://127.0.0.1:8317"
}

// wizardAggregate 按供应商名聚合的凭据计数与展示信息。
type wizardAggregate struct {
	label      string
	active     int
	disabled   int
	sampleURL  string
	siteURLs   map[string]struct{}
	credential []credentialInfo
}

// listCredentials 通过宿主回调 host.auth.list 读取凭据文件供应商，
// 按供应商聚合并标注余额状态。带 base_url 的凭据视为第三方中转，
// 即使 provider 名与官方厂商同名（如 openai）也按可配置处理。
func listCredentials() ([]providerStatus, []credentialInfo, string) {
	return listAllProviders("")
}

// listAllProviders 汇总两个来源的供应商：
//  1. 宿主回调 host.auth.list 返回的凭据文件供应商（无需任何密钥）；
//  2. config.yaml 里的 API-Key 供应商（动态读取所有以 -api-key 结尾的配置项，
//     以及 openai-compatibility）。
//
// host.auth.list 只返回有凭据文件的记录，config.yaml 中的 API-Key
// 供应商没有凭据文件，宿主会跳过它们——这正是「AI 提供商」页签
// 能看到它们而本页看不到的原因。因此这里在有管理密钥时（页面随
// 请求头带来，或插件配置里已保存）额外调用管理 API 读取 /config
// 聚合补齐，让两边的供应商数量一致。
func listAllProviders(managementKey string) ([]providerStatus, []credentialInfo, string) {
	order := []string{}
	byProvider := map[string]*wizardAggregate{}
	credentials := []credentialInfo{}
	notes := []string{}
	ensure := func(provider string) *wizardAggregate {
		if entry, ok := byProvider[provider]; ok {
			return entry
		}
		entry := &wizardAggregate{}
		byProvider[provider] = entry
		order = append(order, provider)
		return entry
	}
	addCredential := func(cred credentialInfo, disabled bool) {
		credentials = append(credentials, cred)
		entry := ensure(cred.Provider)
		if disabled {
			entry.disabled++
		} else {
			entry.active++
		}
		if cred.BaseURL != "" {
			if entry.siteURLs == nil {
				entry.siteURLs = map[string]struct{}{}
			}
			entry.siteURLs[cred.BaseURL] = struct{}{}
			if entry.sampleURL == "" {
				entry.sampleURL = cred.BaseURL
			}
		}
		if entry.label == "" {
			entry.label = cred.Label
			if entry.label == "" {
				entry.label = cred.Name
			}
		}
		entry.credential = append(entry.credential, cred)
	}

	// 来源 1：凭据文件（host.auth.list）。失败时保留错误说明，继续读取配置文件供应商。
	raw, err := hostCallMethod("host.auth.list", nil)
	if err != nil {
		notes = append(notes, "无法从 CPA 读取凭据列表（host.auth.list: "+err.Error()+"）")
	} else {
		var payload struct {
			Files []struct {
				Provider    string `json:"provider"`
				AuthIndex   string `json:"auth_index"`
				Name        string `json:"name"`
				Label       string `json:"label"`
				Disabled    bool   `json:"disabled"`
				RuntimeOnly bool   `json:"runtime_only"`
				BaseURL     string `json:"base_url"`
			} `json:"files"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			notes = append(notes, "解析凭据列表失败: "+err.Error())
		} else {
			for _, file := range payload.Files {
				provider := strings.ToLower(strings.TrimSpace(file.Provider))
				baseURL := strings.TrimSpace(file.BaseURL)
				name := strings.TrimSpace(file.Name)
				label := strings.TrimSpace(file.Label)
				if provider == "" {
					provider = "(未命名)"
				}
				addCredential(credentialInfo{
					Provider: provider, Name: name, Label: label,
					BaseURL: baseURL, AuthIndex: file.AuthIndex,
					ProfileKey: credentialProfileKey(provider, baseURL, file.AuthIndex),
					Disabled:   file.Disabled, RuntimeOnly: file.RuntimeOnly,
					Source: "file",
				}, file.Disabled)
			}
		}
	}

	// 来源 2：config.yaml 中的 API-Key 供应商（需要管理密钥）。
	if key := strings.TrimSpace(managementKey); key != "" {
		if configCredentials, configNote := configProviderCredentials(key); len(configCredentials) > 0 || configNote != "" {
			if configNote != "" {
				notes = append(notes, configNote)
			}
			for _, cred := range configCredentials {
				addCredential(cred, cred.Disabled)
			}
		}
	}

	supported := map[string]struct{}{}
	for _, name := range supportedProviders() {
		supported[strings.ToLower(name)] = struct{}{}
	}
	result := make([]providerStatus, 0, len(order))
	for _, provider := range order {
		entry := byProvider[provider]
		status := providerStatus{
			Provider:        provider,
			Label:           entry.label,
			CredentialCount: entry.active + entry.disabled,
			ActiveCount:     entry.active,
			SiteCount:       len(entry.siteURLs),
		}
		isRelay := len(entry.siteURLs) > 0 // 有站点地址 = 第三方中转，不是官方接口
		switch {
		case knownNoBalanceProviders[provider] != "" && !isRelay:
			status.Status = "unsupported"
			status.Note = knownNoBalanceProviders[provider]
		default:
			if _, ok := supported[provider]; ok && (entry.active > 0 || entry.disabled == 0) {
				status.Status = "ok"
				if entry.active == 0 {
					status.Note = "插件支持该供应商；当前凭据均已停用，启用后即可显示余额"
				} else {
					status.Note = "余额已在供应商页签显示，无需任何配置"
				}
			} else {
				status.Status = "configurable"
				if entry.active == 0 {
					status.Note = "全部凭据已停用；启用后勾选并配置余额查询即可"
				} else if isRelay {
					status.Note = fmt.Sprintf("第三方中转（%s 等共 %d 个站点地址）：勾选后选择厂商类型即可", entry.sampleURL, len(entry.siteURLs))
				} else {
					status.Note = "尚未配置余额查询：勾选后选择厂商类型即可"
				}
			}
		}
		result = append(result, status)
	}
	return result, credentials, strings.Join(notes, " ")
}

// configProviderCredentials 通过 CPA 管理 API 读取 config.yaml 中的
// API-Key 供应商，聚合成与凭据文件同构的凭据摘要（不含任何密钥）。
// provider 名与 CPA 合成 auth 时使用的 Provider 字段一致
// （openai-compatibility 的条目为 openai-compatible-<name>），
// 保证这里展示的名字就是余额档案 profiles 需要的键。
// configCredential 记录一条配置文件凭据。apiKey 仅在服务端内部使用，
// 绝不出现在任何返回给页面的数据里。
type configCredential struct {
	Provider   string
	Name       string
	Label      string
	BaseURL    string
	APIKey     string
	ProfileKey string
	Disabled   bool
}

// configProviderCredentialRecords 通过 CPA 管理 API 读取 config.yaml 中的
// API-Key 供应商凭据（含密钥，仅服务端使用）。provider 名与 CPA 合成
// auth 时的 Provider 字段一致（openai-compatibility 条目为
// openai-compatible-<name>），保证与余额档案 profiles 的键相同。
func configProviderCredentialRecords(managementKey string) ([]configCredential, string) {
	cfg := currentConfig()
	request := httpRequest{
		Method:  http.MethodGet,
		URL:     managementBaseURL(cfg) + "/v0/management/config",
		Headers: managementHeaders(managementKey, ""),
	}
	response, err := callHostHTTP(request)
	if err != nil {
		return nil, "读取 CPA 配置文件供应商失败: " + err.Error()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(response.Body))
		if len(message) > 200 {
			message = message[:200]
		}
		if response.StatusCode == http.StatusUnauthorized {
			return nil, "管理密钥无效（HTTP 401），配置文件中的 API-Key 供应商未读取；请在插件配置中更新 management_key"
		}
		if response.StatusCode == http.StatusForbidden && strings.Contains(message, "banned") {
			return nil, "已触发 CPA 防爆破封禁（HTTP 403，30 分钟）；配置文件中的 API-Key 供应商暂未读取"
		}
		return nil, fmt.Sprintf("读取 CPA 配置文件供应商失败（HTTP %d）：%s", response.StatusCode, message)
	}
	var payload struct {
		OpenAICompat []struct {
			Name          string `json:"name"`
			BaseURL       string `json:"base-url"`
			Disabled      bool   `json:"disabled"`
			APIKeyEntries []struct {
				APIKey string `json:"api-key"`
			} `json:"api-key-entries"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return nil, "解析 CPA 配置文件供应商失败: " + err.Error()
	}
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &rawConfig); err != nil {
		return nil, "解析 CPA 配置文件供应商失败: " + err.Error()
	}
	credentials := []configCredential{}
	addSimple := func(provider, label string, entries []struct {
		APIKey  string `json:"api-key"`
		BaseURL string `json:"base-url"`
	}) {
		for i, entry := range entries {
			credentials = append(credentials, configCredential{
				Provider:   provider,
				Name:       fmt.Sprintf("%s-%d", provider, i+1),
				Label:      label,
				ProfileKey: configProfileKey(provider, entry.BaseURL, i+1),
				BaseURL:    strings.TrimSpace(entry.BaseURL),
				APIKey:     strings.TrimSpace(entry.APIKey),
			})
		}
	}
	keys := make([]string, 0, len(rawConfig))
	for key := range rawConfig {
		if strings.HasSuffix(key, "-api-key") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		var entries []struct {
			APIKey  string `json:"api-key"`
			BaseURL string `json:"base-url"`
		}
		if json.Unmarshal(rawConfig[key], &entries) != nil {
			continue
		}
		provider := strings.TrimSuffix(key, "-api-key")
		addSimple(provider, provider+" API-Key（配置文件）", entries)
	}
	for _, compat := range payload.OpenAICompat {
		name := strings.ToLower(strings.TrimSpace(compat.Name))
		if name == "" {
			name = "openai-compatibility"
		}
		provider := "openai-compatible-" + name
		count := len(compat.APIKeyEntries)
		if count == 0 {
			count = 1 // 无密钥条目时 CPA 也会合成一条无密钥 auth，保证可路由
		}
		for i := 0; i < count; i++ {
			var apiKey string
			if i < len(compat.APIKeyEntries) {
				apiKey = strings.TrimSpace(compat.APIKeyEntries[i].APIKey)
			}
			credentials = append(credentials, configCredential{
				Provider:   provider,
				Name:       fmt.Sprintf("%s-%d", provider, i+1),
				Label:      strings.TrimSpace(compat.Name) + "（配置文件）",
				ProfileKey: configProfileKey(provider, compat.BaseURL, i+1),
				BaseURL:    strings.TrimSpace(compat.BaseURL),
				APIKey:     apiKey,
				Disabled:   compat.Disabled,
			})
		}
	}
	return credentials, ""
}

// configProviderCredentials 把配置文件凭据裁剪成向导页可展示的摘要
// （不含任何密钥）。
func configProviderCredentials(managementKey string) ([]credentialInfo, string) {
	records, note := configProviderCredentialRecords(managementKey)
	out := make([]credentialInfo, 0, len(records))
	for _, record := range records {
		out = append(out, credentialInfo{
			Provider:   record.Provider,
			Name:       record.Name,
			Label:      record.Label,
			BaseURL:    record.BaseURL,
			ProfileKey: record.ProfileKey,
			Disabled:   record.Disabled,
			Source:     "config",
		})
	}
	return out, note
}

// providerCredential 查询余额所需的最小凭据信息（服务端内部使用）。
type providerCredential struct {
	storageJSON []byte
	baseURL     string
}

// credentialForProvider 定位指定账号的可用凭据；credentialKey 为空时兼容旧版取第一条。
func credentialForProvider(provider string, credentialKey ...string) (providerCredential, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	wanted := ""
	if len(credentialKey) > 0 {
		wanted = strings.TrimSpace(credentialKey[0])
	}
	raw, err := hostCallMethod("host.auth.list", nil)
	if err == nil {
		var payload struct {
			Files []struct {
				Provider  string `json:"provider"`
				AuthIndex string `json:"auth_index"`
				Disabled  bool   `json:"disabled"`
			} `json:"files"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			for _, file := range payload.Files {
				if strings.ToLower(strings.TrimSpace(file.Provider)) != provider || file.Disabled || file.AuthIndex == "" {
					continue
				}
				profileKey := credentialProfileKey(provider, "", file.AuthIndex)
				if wanted != "" && wanted != file.AuthIndex && wanted != profileKey {
					continue
				}
				getRequest, _ := json.Marshal(map[string]string{"auth_index": file.AuthIndex})
				if getRaw, err := hostCallMethod("host.auth.get", getRequest); err == nil {
					var getResult struct {
						JSON json.RawMessage `json:"json"`
					}
					if json.Unmarshal(getRaw, &getResult) == nil && len(getResult.JSON) > 0 {
						return providerCredential{storageJSON: getResult.JSON}, nil
					}
				}
			}
		}
	}
	// 凭据文件未命中：回退 config.yaml 的 API-Key 供应商。
	if key := strings.TrimSpace(currentConfig().ManagementKey); key != "" {
		if records, _ := configProviderCredentialRecords(key); len(records) > 0 {
			for _, record := range records {
				if record.Provider != provider || record.Disabled || record.APIKey == "" {
					continue
				}
				if wanted != "" && wanted != record.ProfileKey {
					continue
				}
				document, _ := json.Marshal(map[string]string{
					"api_key":  record.APIKey,
					"base_url": record.BaseURL,
				})
				return providerCredential{storageJSON: document, baseURL: record.BaseURL}, nil
			}
		}
	}
	return providerCredential{}, fmt.Errorf("未找到供应商 %q 的可用凭据", provider)
}

// providerBalanceResult 返回给向导页的单个供应商余额（不含任何密钥）。
type providerBalanceResult struct {
	OK          bool    `json:"ok"`
	Description string  `json:"description,omitempty"`
	Fraction    float64 `json:"fraction,omitempty"`
	Balance     float64 `json:"balance"`
	Limit       float64 `json:"limit,omitempty"`
	Used        float64 `json:"used,omitempty"`
	HasLimit    bool    `json:"has_limit,omitempty"`
	HasUsed     bool    `json:"has_used,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	Message     string  `json:"message,omitempty"`
}

// fetchProviderBalance 在服务端为指定供应商执行一次余额查询，
// 复用 quota.fetch 的完整管线（档案解析 → 自动识别 → 响应解析）。
// 凭据（storage_json / api_key）只在本函数内部使用，不下发页面。
func fetchProviderBalance(provider string, credentialKeys ...string) providerBalanceResult {
	credentialKey := ""
	if len(credentialKeys) > 0 {
		credentialKey = strings.TrimSpace(credentialKeys[0])
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return providerBalanceResult{Message: "缺少供应商名"}
	}
	credential, err := credentialForProvider(provider, credentialKey)
	if err != nil {
		return providerBalanceResult{Message: err.Error()}
	}
	req := quotaFetchRequest{
		Provider:      provider,
		CredentialKey: credentialKey,
		StorageJSON:   credential.storageJSON,
		Attributes:    map[string]string{"base_url": credential.baseURL},
	}
	resp, err := fetchQuotaWithSelectionGate(req, false)
	if err != nil {
		return providerBalanceResult{Message: err.Error()}
	}
	result := providerBalanceResult{OK: true}
	if len(resp.Groups) > 0 && len(resp.Groups[0].Buckets) > 0 {
		bucket := resp.Groups[0].Buckets[0]
		result.Description = bucket.Description
		result.Fraction = bucket.RemainingFraction
		result.Balance = bucket.Balance
		result.Limit = bucket.Limit
		result.Used = bucket.Used
		result.HasLimit = bucket.HasLimit
		result.HasUsed = bucket.HasUsed
		result.Currency = bucket.Currency
	}
	if resp.Subscription != nil && strings.TrimSpace(resp.Subscription.Plan) != "" {
		result.Description += " · " + resp.Subscription.Plan
	}
	return result
}

// wizardTopLevelKeys 允许向导提交的顶层字段：控制字段 + 余额标量字段
// （向导在仅配置 default 档案时会把档案字段提升到顶层提交）。
// 密钥、认证头、凭据路径、管理地址与 allow_insecure_http 始终拒绝。
var wizardTopLevelKeys = map[string]struct{}{
	"enabled":              {},
	"priority":             {},
	"profiles":             {},
	"providers":            {},
	"selected_credentials": {},
	"vendor":               {},
	"base_url":             {},
	"endpoint":             {},
	"used_endpoint":        {},
	"used_scale":           {},
	"method":               {},
	"balance_path":         {},
	"used_path":            {},
	"limit_path":           {},
	"currency_path":        {},
	"plan_path":            {},
	"reset_path":           {},
	"window_name":          {},
}

var wizardProfileKeys = map[string]struct{}{
	"vendor":        {},
	"base_url":      {},
	"endpoint":      {},
	"used_endpoint": {},
	"used_scale":    {},
	"balance_path":  {},
	"used_path":     {},
	"limit_path":    {},
	"currency_path": {},
	"plan_path":     {},
	"reset_path":    {},
	"window_name":   {},
}

func validateWizardConfig(saveJSON string) (map[string]any, error) {
	var incoming map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(saveJSON))
	if err := decoder.Decode(&incoming); err != nil {
		return nil, fmt.Errorf("配置数据解析失败: %w", err)
	}
	if incoming == nil {
		return nil, errors.New("配置数据必须是 JSON 对象")
	}
	clean := make(map[string]any, len(incoming))
	for key, raw := range incoming {
		if _, ok := wizardTopLevelKeys[key]; !ok {
			return nil, fmt.Errorf("出于安全考虑，向导不允许提交 %s，请在插件配置 YAML 中手动维护", key)
		}
		switch key {
		case "enabled":
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, errors.New("enabled 必须是布尔值")
			}
			clean[key] = value
		case "priority":
			var value int
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, errors.New("priority 必须是整数")
			}
			clean[key] = value
		case "used_scale":
			var value float64
			if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
				return nil, errors.New("used_scale 必须是正数")
			}
			clean[key] = value
		case "method":
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, errors.New("method 必须是字符串")
			}
			value = strings.ToUpper(strings.TrimSpace(value))
			if value != http.MethodGet && value != http.MethodPost {
				return nil, errors.New("method 仅支持 GET 或 POST")
			}
			clean[key] = value
		case "profiles":
			profiles, err := validateWizardProfiles(raw)
			if err != nil {
				return nil, err
			}
			clean[key] = profiles
		case "providers":
			var providers []string
			if err := json.Unmarshal(raw, &providers); err != nil {
				return nil, errors.New("providers 必须是字符串数组")
			}
			clean[key] = providers
		case "selected_credentials":
			var selected []string
			if err := json.Unmarshal(raw, &selected); err != nil {
				return nil, errors.New("selected_credentials 必须是字符串数组")
			}
			clean[key] = selected
		default:
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, fmt.Errorf("%s 必须是字符串", key)
			}
			clean[key] = value
		}
	}
	return clean, nil
}

func validateWizardProfiles(raw json.RawMessage) (map[string]any, error) {
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(raw, &profiles); err != nil || profiles == nil {
		return nil, errors.New("profiles 必须是 JSON 对象")
	}
	clean := make(map[string]any, len(profiles))
	for name, rawProfile := range profiles {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return nil, errors.New("profiles 不能包含空档案名")
		}
		var profile map[string]json.RawMessage
		if err := json.Unmarshal(rawProfile, &profile); err != nil || profile == nil {
			return nil, fmt.Errorf("档案 %q 必须是 JSON 对象", name)
		}
		cleanProfile := make(map[string]any, len(profile))
		for key, rawValue := range profile {
			if _, ok := wizardProfileKeys[key]; !ok {
				return nil, fmt.Errorf("档案 %q 包含不支持的字段 %s", name, key)
			}
			if key == "used_scale" {
				var value float64
				if err := json.Unmarshal(rawValue, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
					return nil, fmt.Errorf("档案 %q 的 used_scale 必须是正数", name)
				}
				cleanProfile[key] = value
				continue
			}
			var value string
			if err := json.Unmarshal(rawValue, &value); err != nil {
				return nil, fmt.Errorf("档案 %q 的 %s 必须是字符串", name, key)
			}
			cleanProfile[key] = value
		}
		clean[name] = cleanProfile
	}
	return clean, nil
}

// mergeWizardConfig 保留插件现有的认证、管理密钥和全局请求设置，
// 再叠加向导允许修改的字段，避免 PUT 替换配置时丢失关键运行配置。
func mergeWizardConfig(cfg config, clean map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("编码现有配置失败: %w", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		return nil, fmt.Errorf("解析现有配置失败: %w", err)
	}
	for key, value := range clean {
		merged[key] = value
	}
	return merged, nil
}

// saveConfigViaManagementAPI 接收向导页提交的配置（JSON），严格白名单过滤并
// 与现有插件配置合并后，通过 CPA 管理 API PUT 保存并触发热加载。
// 管理密钥优先用页面随请求头带来的（仅本次请求使用），其次用插件
// 配置里已保存的 management_key。
func saveConfigViaManagementAPI(saveJSON string) map[string]any {
	cfg := currentConfig()
	key := strings.TrimSpace(cfg.ManagementKey)
	if key == "" {
		return map[string]any{
			"ok":      false,
			"message": "保存需要 CPA 管理密钥：请在插件配置（CPA config.yaml 的 plugins.configs.api-balance）中设置 management_key 后重试",
		}
	}
	clean, err := validateWizardConfig(saveJSON)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	merged, err := mergeWizardConfig(cfg, clean)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	payload, err := json.Marshal(merged)
	if err != nil {
		return map[string]any{"ok": false, "message": "配置编码失败: " + err.Error()}
	}
	request := httpRequest{
		Method:  http.MethodPut,
		URL:     managementBaseURL(cfg) + "/v0/management/plugins/api-balance/config",
		Headers: managementHeaders(key, "application/json"),
		Body:    payload,
	}
	response, err := callHostHTTP(request)
	if err != nil {
		return map[string]any{"ok": false, "message": "调用 CPA 管理 API 失败: " + err.Error()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(response.Body))
		if message == "" {
			message = "管理 API 未返回错误详情"
		}
		if response.StatusCode == http.StatusForbidden && strings.Contains(message, "banned") {
			message += "（这是 CPA 的防爆破封禁：连续 5 次密钥错误后封禁 30 分钟，期间密钥正确也会被拒；等待解除或重启 CPA。）"
		}
		return map[string]any{
			"ok":      false,
			"message": fmt.Sprintf("保存失败（HTTP %d）：%s", response.StatusCode, message),
		}
	}
	return map[string]any{"ok": true, "message": "已保存！CPA 正在热加载新配置。"}
}
