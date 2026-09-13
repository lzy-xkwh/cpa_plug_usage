# CPA 第三方余额插件

这是一个 CLIProxyAPI（CPA）标准动态库插件，实现 `QuotaProvider` 能力。
它通过 CPA 宿主的 `host.http.do` 桥接访问第三方余额接口，避免插件自行创建
网络客户端，并把响应转换为 CPA 的标准额度窗口。

插件是**只读**的：不会充值、签到、改密钥或重置第三方账户。

## 构建

在本目录执行：

```bash
go mod tidy
go build -buildmode=c-shared -o third-party-balance.so .
rm -f third-party-balance.h
```

将生成的 `.so` 放入 CPA 的插件目录。macOS 使用 `.dylib`，Windows 使用
`.dll`。插件 ID 来自文件名 `third-party-balance`。

## 可视化配置（推荐）

较新的 CPA 管理界面会读取插件注册时返回的 `ConfigFields`，因此安装插件后
可以在 **管理后台 → Plugins/插件 → third-party-balance → Config/配置** 中直接
填写表单并保存，不需要手动编辑 YAML。

可视化表单包含：

- 启用开关、优先级、GET/POST 请求方式；
- 第三方余额接口地址；
- Token/API Key 的 JSON 字段路径、认证请求头和前缀；
- 余额、总额度、已用额度、币种、套餐、重置时间等响应 JSON 路径；
- 额度窗口名称和是否允许 HTTP。

`credential_paths` 在可视化界面中填写逗号分隔值，例如：

```text
access_token, api_key, token
```

如果你的 CPA 版本没有插件配置表单，则仍可使用下面的 YAML 方式，或升级到
包含插件配置管理接口的版本。

## 从 CPA 插件商店安装

本仓库包含可直接用于 CPA 自定义插件商店的 `registry.json`。在 CPA 的
`config.yaml` 中添加：

```yaml
plugins:
  enabled: true
  dir: "/CLIProxyAPI/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/lzy-xkwh/cpa_plug_usage/main/registry.json"
```

重启 CPA 后，在插件商店刷新并安装 `third-party-balance`。后续发布新的
GitHub Release 后，插件商店可以直接更新，不需要再次手动复制动态库。

当前发布工作流提供 Linux `amd64` 和 `arm64` 两种 Docker 常用架构；其他
平台可以按 `.github/workflows/release.yml` 扩展。

## CPA 配置（高级/自动部署）

CPA 配置文件示例：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    third-party-balance:
      enabled: true
      priority: 10

      # 支持 {base_url}、{provider}、{auth_id}、{auth_index}
      endpoint: "https://api.example.com/v1/account/balance"
      method: GET

      # 可选固定请求头/查询参数
      headers:
        X-Client: "cli-proxy-api"
      query:
        detail: "true"

      # 按顺序从 CPA 的 storage_json 中查找凭据。
      credential_paths:
        - access_token
        - api_key
        - token
      credential_header: Authorization
      credential_prefix: "Bearer "

      # 第三方 JSON 响应中的点路径
      balance_path: data.balance
      limit_path: data.limit
      used_path: data.used
      currency_path: data.currency
      plan_path: data.plan
      reset_path: data.reset_at
      window_name: balance
      timeout_seconds: 15
```

如果只有余额而没有总额度，`balance_path` 会被作为 0～1 的剩余比例；
如果同时有 `used_path`，插件会按 `balance / (balance + used)` 推导总额度。
如果有 `limit_path`，则使用 `balance / limit`。

## 认证与安全

- API Key/Token 从 CPA 传入的 `storage_json` 读取，不写入日志，也不会放进
  返回给前端的结果。
- 默认只允许 HTTPS；本地自建服务如确实使用 HTTP，才显式设置
  `allow_insecure_http: true`。
- 请只安装你信任的动态库插件。CPA 文档明确说明动态库插件属于宿主进程内的
 可信代码，插件可影响宿主进程。

## 适配 All API Hub / New API 类站点

All API Hub 的核心思路是保存站点地址和凭据，再按站点类型读取余额。这个插件
把“站点差异”下沉到配置：不同第三方站点只需要调整 `endpoint`、凭据路径和
响应 JSON 路径，不需要重新编译插件。若站点需要 Cookie，可把
`credential_header` 改为 `Cookie`、`credential_prefix` 改为空字符串，并让
`credential_paths` 指向保存 Cookie 的字段。

## CPA 查询入口

启用后，CPA 会把插件注册为 `third-party-balance` quota provider。可通过 CPA
管理 API 的 quota 查询入口读取：

```text
POST /v0/management/quota/fetch
```

请求中使用对应凭据的 `auth_index`；CPA 会负责选择插件并转发标准化余额结果。
