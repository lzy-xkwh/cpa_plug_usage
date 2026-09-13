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
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

const (
	abiVersion    = 1
	schemaVersion = 1
	pluginID      = "third-party-balance"
	providerID    = "third-party-balance"
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
	Endpoint          string            `yaml:"endpoint"`
	Method            string            `yaml:"method"`
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
}

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
			SupportedProviders: []string{providerID},
			DisplayName:        "第三方余额读取",
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
			"message": "第三方余额插件为只读，不支持重置",
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
			Version:          "0.1.3",
			Author:           "community",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []configField{
				{Name: "enabled", Type: "boolean", Description: "是否启用第三方余额读取。"},
				{Name: "priority", Type: "integer", Description: "CPA 选择额度提供方时使用的优先级。"},
				{Name: "endpoint", Type: "string", Description: "第三方余额接口地址，支持 {base_url}、{provider}、{auth_id}、{auth_index} 占位符。"},
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
	if next.WindowName == "" {
		next.WindowName = "balance"
	}
	if next.Method != http.MethodGet && next.Method != http.MethodPost {
		return fmt.Errorf("method 仅支持 GET 或 POST，当前为 %q", next.Method)
	}
	if next.Endpoint != "" {
		if _, err := validateEndpoint(next.Endpoint, next.AllowInsecureHTTP); err != nil {
			return err
		}
	}
	configMu.Lock()
	runtimeConfig = next
	configMu.Unlock()
	return nil
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
			if value == "" {
				section = key
				continue
			}
			if err := setConfigScalar(out, key, parseScalar(value)); err != nil {
				return fmt.Errorf("第 %d 行: %w", lineNumber, err)
			}
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
	if cfg.Endpoint == "" {
		return quotaFetchResponse{}, errors.New("未配置 endpoint（余额接口地址）")
	}
	if cfg.BalancePath == "" {
		return quotaFetchResponse{}, errors.New("未配置 balance_path（余额字段路径）")
	}
	endpoint := expandEndpoint(cfg.Endpoint, req)
	if _, err := validateEndpoint(endpoint, cfg.AllowInsecureHTTP); err != nil {
		return quotaFetchResponse{}, err
	}
	headers := make(map[string][]string, len(cfg.Headers)+1)
	for name, value := range cfg.Headers {
		headers[name] = []string{expandTemplate(value, req)}
	}
	credential := findCredential(req.StorageJSON, cfg.CredentialPaths)
	if credential != "" && cfg.CredentialHeader != "" {
		headers[cfg.CredentialHeader] = []string{cfg.CredentialPrefix + credential}
	}
	if len(cfg.Query) > 0 {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return quotaFetchResponse{}, fmt.Errorf("解析 endpoint 失败: %w", err)
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
		return quotaFetchResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return quotaFetchResponse{}, fmt.Errorf("余额接口返回 HTTP %d", response.StatusCode)
	}
	var document any
	if err := json.Unmarshal(response.Body, &document); err != nil {
		return quotaFetchResponse{}, fmt.Errorf("余额接口返回的不是有效 JSON: %w", err)
	}
	return normalizeQuota(document, cfg)
}

func normalizeQuota(document any, cfg config) (quotaFetchResponse, error) {
	balance, ok := numberAt(document, cfg.BalancePath)
	if !ok {
		return quotaFetchResponse{}, fmt.Errorf("balance_path %q 未解析到数字", cfg.BalancePath)
	}
	used, hasUsed := numberAt(document, cfg.UsedPath)
	limit, hasLimit := numberAt(document, cfg.LimitPath)
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
		Groups: []quotaGroup{{DisplayName: "第三方余额", Buckets: []quotaBucket{bucket}}},
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

func expandEndpoint(endpoint string, req quotaFetchRequest) string {
	baseURL := req.Attributes["base_url"]
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
