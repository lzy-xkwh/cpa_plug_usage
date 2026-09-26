# CPA API 余额查询插件（api-balance）

这是一个 CLIProxyAPI（CPA）标准动态库插件，实现 `QuotaProvider` 能力。
它通过 CPA 宿主的 `host.http.do` 桥接访问余额接口，避免插件自行创建
网络客户端，并把响应转换为 CPA 的标准额度窗口。

支持三类余额来源：

- **官方厂商**：内置 `deepseek`（DeepSeek）、`moonshot`（Kimi/Moonshot）、
  `openrouter`（OpenRouter）预设，配置一行 `vendor` 即可读取官方账户余额；
- **one-api 系中转面板**：`vendor: one-api` 自动使用 OpenAI 兼容账单接口，
  支持 one-api / new-api / one-hub / done-hub / Veloera / VoAPI 等衍生版本，
  只需提供站点地址；
- **其它中转站 / 自建网关**：sub2api 等，通过自定义 `endpoint` +
  响应 JSON 路径适配，不需要重新编译插件。

> Z.AI（智谱）目前没有公开的余额查询 API，暂无法内置预设；
> 待官方提供接口后可按同样的方式加入 `vendorPresets`。

与浏览器扩展类工具（如 All API Hub）的差异：All API Hub 依赖登录浏览器的
Cookie 会话读取面板数据；本插件只使用 CPA 存储中的 API Key，在服务端无人
值守运行，适合与 CLIProxyAPI 的额度查询接口集成。若站点只提供面板接口而
没有 API Key 余额端点，则无法用本插件读取。

插件是**只读**的：不会充值、签到、改密钥或重置任何账户。

## 构建

在本目录执行：

```bash
go mod tidy
go build -buildmode=c-shared -o api-balance.so .
rm -f api-balance.h
```

将生成的 `.so` 放入 CPA 的插件目录。macOS 使用 `.dylib`，Windows 使用
`.dll`。插件 ID 来自文件名 `api-balance`。

> 从旧版 `third-party-balance` 升级：插件 ID 已变更，商店不会自动覆盖
> 升级，请先卸载旧插件再安装 `api-balance`。

## 可视化配置（推荐）

较新的 CPA 管理界面会读取插件注册时返回的 `ConfigFields`，因此安装插件后
可以在 **管理后台 → Plugins/插件 → api-balance → Config/配置** 中直接
填写表单并保存，不需要手动编辑 YAML。

可视化表单包含：

- 启用开关、优先级；
- 厂商预设 `vendor`（custom / deepseek / moonshot），选择后自动填充
  官方接口地址与余额路径；
- 余额接口地址、GET/POST 请求方式（自定义站点时使用）；
- Token/API Key 的 JSON 字段路径、认证请求头和前缀；
- 余额、总额度、已用额度、币种、套餐、重置时间等响应 JSON 路径；
- 额度窗口名称和是否允许 HTTP。

`credential_paths` 在可视化界面中填写逗号分隔值，例如：

```text
access_token, api_key, token
```

如果你的 CPA 版本没有插件配置表单，则仍可使用下面的 YAML 方式，或升级到
包含插件配置管理接口的版本。

安装后，CPA 会自动在插件配置中加入 `store` 元数据。该元数据由 CPA 管理，
插件会自动忽略它。

## 从 CPA 插件商店安装

本仓库包含可直接用于 CPA 自定义插件商店的 `registry.json`。当前清单使用
CPA 插件商店 schema v2 的 direct artifact 安装方式，直接下载固定 Release
资产并校验 SHA-256，不需要 CPA 调用 GitHub Releases API，因此可以绕过匿名
GitHub API 的速率限制。在 CPA 的
`config.yaml` 中添加：

```yaml
plugins:
  enabled: true
  dir: "/CLIProxyAPI/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/lzy-xkwh/cpa_plug_usage/main/registry.json"
  # GitHub API 匿名请求有速率限制。Docker 部署建议配置一个环境变量令牌。
  store-auth:
    - match: "https://api.github.com/"
      apply-to:
        - registry
        - artifact
      type: bearer
      token-env: "CLIPROXY_PLUGIN_STORE_TOKEN"
```

Docker Compose 中只传环境变量名，不要把真实令牌提交到 Git：

```yaml
services:
  cpa:
    environment:
      CLIPROXY_PLUGIN_STORE_TOKEN: ${CLIPROXY_PLUGIN_STORE_TOKEN}
```

然后在宿主机 `.env` 或部署系统的 Secret 中设置
`CLIPROXY_PLUGIN_STORE_TOKEN`，再执行：

```bash
docker compose up -d
```

令牌不会写入插件清单、插件状态或日志。CPA 官方示例也将商店认证值从环境
变量读取，并支持分别覆盖 registry 和 artifact 请求。

重启 CPA 后，在插件商店刷新并安装 `api-balance`。后续发布新的
GitHub Release 后，插件商店可以直接更新，不需要再次手动复制动态库。

当前发布工作流提供 Linux `amd64` 和 `arm64` 两种 Docker 常用架构；其他
平台可以按 `.github/workflows/release.yml` 扩展。

## 故障排查：商店安装/更新返回 502

CPA 商店安装端点（`POST /v0/management/plugin-store/:id/install`）在三种
情况下返回 502，可通过响应 body 的 `error` 字段区分：

| error | 含义 | 处理 |
|---|---|---|
| `plugin_store_registry_failed` | CPA 拉取 store-source 的 registry.json 失败 | 检查服务器到 `raw.githubusercontent.com` 的连通性；或换下面的镜像源 |
| `plugin_manifest_invalid` | 清单校验失败 | 保留完整 message 反馈到仓库 issue |
| `plugin_install_failed` | 下载 release zip / sha256 校验 / 解压失败 | 重试一次；反复失败时检查 `objects.githubusercontent.com` 连通性与代理配置 |

日志形如 `502 ... POST "/v0/management/plugin-store/api-balance/install?source=..."`
只记录状态码，真实原因在响应 body 里。可在 CPA 服务器本机执行：

```bash
curl -s -X POST "http://127.0.0.1:8317/v0/management/plugin-store/api-balance/install?source=<source-id>" \
  -H "Authorization: Bearer <管理密钥>"
```

`source-id` 是 store source URL 的哈希前缀（日志里已有，如 `source-ff7d…`）。

如果服务器到 GitHub 的链路不稳定，可以在 `config.yaml` 里把 registry
换成 jsDelivr CDN 镜像（有最长约 12 小时缓存，新版本发布后可能延迟可见）：

```yaml
plugins:
  store-sources:
    - "https://cdn.jsdelivr.net/gh/lzy-xkwh/cpa_plug_usage@main/registry.json"
```

注：镜像只能加速 registry.json 的拉取；release zip 仍从 GitHub 下载。
下载环节持续失败时，可手动安装：从 Releases 页下载对应架构的 zip，
解压出 `api-balance.so` 放入 CPA 插件目录后重启。

## 官方厂商预设

凭据默认会按 `Authorization: Bearer <API Key>` 发送（CPA storage_json 中的
`access_token`/`api_key` 等字段自动查找），因此以下示例无需额外配置请求头。
预设只填补未显式配置的字段，显式配置始终优先。

### DeepSeek

```yaml
plugins:
  configs:
    api-balance:
      enabled: true
      priority: 10
      vendor: deepseek
```

等价于自动填充：`endpoint: https://api.deepseek.com/user/balance`、
`balance_path: balance_infos.0.total_balance`、
`currency_path: balance_infos.0.currency`。余额为人民币。

### Kimi / Moonshot

```yaml
plugins:
  configs:
    api-balance:
      enabled: true
      priority: 10
      vendor: moonshot
```

等价于自动填充：`endpoint: https://api.moonshot.cn/v1/users/me/balance`、
`balance_path: data.available_balance`。余额为人民币（含代金券）。
国际站（platform.kimi.ai）Key 与国内站不通用，如需查询国际站余额，可
显式覆盖 `endpoint: https://api.moonshot.ai/v1/users/me/balance`。

### OpenRouter

```yaml
plugins:
  configs:
    api-balance:
      enabled: true
      priority: 10
      vendor: openrouter
```

使用密钥信息接口 `GET https://openrouter.ai/api/v1/key`，余额按
`data.limit - data.usage`（美元）推导。**注意**：未设置额度上限（unlimited）
的密钥响应中没有 `data.limit`，此时无法读取余额。

### one-api 系面板（one-api / new-api / one-hub / done-hub 等）

```yaml
plugins:
  configs:
    api-balance:
      enabled: true
      priority: 10
      vendor: one-api
      base_url: "https://站点域名"
```

插件会请求 OpenAI 兼容账单接口并自动推导余额：

- 总额度：`GET {base_url}/v1/dashboard/billing/subscription`（`hard_limit_usd`
  = 剩余 + 已用）；
- 已用额度：`GET {base_url}/v1/dashboard/billing/usage`（`total_usage`，
  单位为 0.01，即乘以 `used_scale: 0.01` 还原）；
- 余额 = 总额度 − 已用额度。

该公式在站点开启或关闭"按货币显示"时都成立。适用于 one-api 及其衍生
项目（new-api、one-hub、done-hub、Veloera、VoAPI 旧版等）。

### 多厂商档案（profiles，一个插件实例管所有厂商）

默认情况下整套配置对应一个厂商。如需用同一个插件实例同时查询 DeepSeek、
one-api 系中转站、OpenRouter 等多个来源，使用 `profiles`：**键 = CPA 凭据的
provider 名**（小写），值 = 该凭据使用的余额配置；键 `default` 兜底所有未
匹配的凭据。

```yaml
plugins:
  configs:
    api-balance:
      enabled: true
      priority: 10
      profiles:
        deepseek:
          vendor: deepseek
        openrouter:
          vendor: openrouter
        relay-a:
          vendor: one-api
          base_url: "https://站点A域名"
        relay-b:
          vendor: custom
          endpoint: "https://站点B域名/v1/usage"
          balance_path: remaining
          currency_path: unit
```

说明：

- 档案内支持所有标量配置（`vendor`、`base_url`、`endpoint`、`used_endpoint`、
  `used_scale`、各 JSON 路径、`credential_header`/`credential_prefix` 等），
  厂商预设同样生效；
- `headers`/`query`/`credential_paths` 暂只支持全局配置（对所有档案生效），
  档案内写列表会报错提示；
- CPA 宿主按凭据的 `provider` 字段路由：把凭据的 provider 设为对应档案名
  （如 `relay-a`）；未匹配时先找 `default` 档案，再回退顶层配置，都没有则
  报错；
- 插件会在配置重载时把所有档案名动态声明为可服务的 provider，无需重启。

### 同一 provider 的多个站点/账号

`provider` 只是 CPA 的路由类别，不再代表唯一站点。向导会按以下身份展示和保存账号：

- 宿主凭据优先使用稳定的 `auth_index`；
- 配置文件 API-Key 条目使用 `provider + 规范化 base_url + 条目序号`；
- 每个账号都会得到独立的 `profile_key`，因此 `codex` 的 10 个站点可分别勾选、查询和配置；
- 账号配置优先级为“账号档案 → provider 档案 → `default` → 顶层旧配置”，旧版 provider 配置仍然有效。

没有 `base_url` 的账号会显示“未提供站点地址”，并明确提示需要补充站点地址或账号档案；插件不会把无法识别的余额显示成 0。对于手工 YAML，可直接使用向导展示的 `profile_key`：

```yaml
profiles:
  site-abcdef123456:
    vendor: one-api
    base_url: "https://site-a.example"
  site-7890abcdef12:
    vendor: custom
    endpoint: "https://site-b.example/v1/usage"
    balance_path: data.remaining
```

向导中的“显示”选择会同时保存到 `selected_credentials`，包括显式空数组；因此刷新页面或重启 CPA 后不会丢失勾选。`providers` 仍保留为旧版 provider 级路由兼容字段。

## 面板页面和统计

余额向导提供“账号明细”“总览统计”“每日快照”三个视图：账号明细按站点展示余额、总额度、已用额度和剩余比例；总览只合计同币种且确实返回的数据，未知值、失败请求和不同币种不会被当作 0。每日快照默认保存在当前浏览器本地，用于观察每天看到的余额和已用值；它不是上游完整账单历史，清理浏览器存储会删除这些快照。

若需要跨浏览器、跨实例的长期统计，需要 CPA 或外部数据库提供持久化历史存储；当前插件不会伪造缺失的历史用量。
## 管理后台配置向导（推荐，无需手写 YAML）

插件 v0.9.8 起向导提供账号明细、总览统计和每日快照：**已配置的账号自动显示余额，只有自动搞不定的才需要补配置**。

入口：管理后台 **插件 → api-balance → 「余额」**（或直接访问
`/v0/resource/plugins/api-balance/config-wizard`）。

- **账号/站点列表与「AI 提供商」页签一致，并直接显示余额**：
  - 凭据文件供应商通过宿主回调 `host.auth.list` 零密钥自动读取；
  - config.yaml 里的 API-Key 供应商没有凭据文件，宿主回调会跳过它们，因此插件会在配置了
    `management_key` 时自动调用管理 API 读取 `config` 补齐；现在会动态识别所有以
    `-api-key` 结尾的配置项以及 `openai-compatibility`，不再依赖固定厂商白名单；
  - 列表按独立账号/站点显示，支持按名称、标签、provider、站点搜索和按状态筛选；
  - 每行都有**余额 / 用量**列：点“查询全部余额”或勾选“显示”即由服务端完成实际余额查询，
    页面分开展示余额、总额、已用和剩余比例；
- **自由选择显示账号**：勾选每行的“显示”并“保存显示选择”，选择写入
  `selected_credentials`，刷新页面和重启 CPA 后仍保留；`providers` 仅作为旧版 provider 级路由兼容；
- **便捷管理密钥设置**：
  - CPAMP Full/Manager 模式（例如 CPAMP `192.168.1.30:18317`）下，向导的“当前页面管理密钥”填写 CPAMP 管理员密钥。CPAMP 会在服务端用已保存的 CPA Management Key 访问插件设置路由，插件只保存 CPA Key，不会把 `cpamp_...` 管理员密钥写入 `management_key`；
  - 直接访问 CPA 管理页面时，填写 CPA 配置中 `remote-management.secret-key` 的**原始明文**，不要填写 CPA 启动后写回的 bcrypt 哈希或普通 API Key；
  - 向导同时要求填写 CPA 地址。你的 CPA 如果监听在 `http://192.168.1.2:8137`，这里填写该地址；不要填写 CPAMP 的 `:18317`。CPA 地址必须是插件运行环境或 CPAMP 服务端可访问的地址；
  - 也可在 CPA 后台「插件管理 → api-balance → 配置」表单中直接填写 `management_key` 与 `management_url`；
  - 密钥仅在服务端使用，绝不下发给页面，页面展示的永远只有脱敏凭据与聚合余额数字；
- 可选 `management_url`：CPA 管理 API 服务地址，默认 `http://127.0.0.1:8317`（仅保存
  时使用）。如果浏览器与 CPA 不在同一台机器，需要 CPA 配置
  `remote-management.allow-remote: true`；
- 「仅生成 YAML」按钮保留给喜欢手动维护配置的用户。

## 自动识别（零配置优先）

插件会自动利用 CPA 凭据里已有的信息，尽量不要求手动配置：

1. **API Key**：默认从凭据 `storage_json` 的 `access_token`/`api_key` 等常见
   字段自动查找，按 `Authorization: Bearer <key>` 发送；
2. **站点地址**：依次从凭据属性、metadata、`storage_json` 中查找
   `base_url`/`baseURL`/`api_base` 等常见字段（也可用配置项 `base_url` 显式
   指定，优先级最高）；
3. **厂商识别**：凭据的 `provider` 名就是厂商名（如 `deepseek`、`moonshot`、
   `openrouter`、`new-api`、`sub2api`、`one-api`）时，自动套用对应预设；
4. **域名特征**：站点地址含 `deepseek` / `moonshot` / `openrouter` 时自动
   识别；
5. **接口探测**：其余站点按 New API（`/api/usage/token/`）→ sub2api
   （`/v1/usage`）→ one-api（billing 账单对）的顺序探测，成功策略按站点
   缓存，后续查询不再重复探测。

只有自动识别全部失败时才会报错，错误信息会明确提示需要补什么：在
`profiles` 中为该凭据添加档案，或在凭据 JSON 中补充 `base_url` 字段。

## CPA 配置（高级/自定义站点）

自定义中转站或任何返回 JSON 的余额接口，使用 `vendor: custom`（默认）：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    api-balance:
      enabled: true
      priority: 10
      vendor: custom

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

      # 响应 JSON 中的点路径
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

## 中转站适配示例

凭据默认会按 `Authorization: Bearer <API Key>` 发送（CPA storage_json 中的
`access_token`/`api_key` 等字段自动查找），因此以下示例无需额外配置请求头。
示例中的 `https://站点域名` 请替换为你实际使用的站点地址。

### New API 程序站点

```yaml
plugins:
  configs:
    api-balance:
      enabled: true
      priority: 10
      endpoint: "https://站点域名/api/usage/token/"
      method: GET
      balance_path: data.total_available
      limit_path: data.total_granted
      used_path: data.total_used
      plan_path: data.name
      window_name: 余额
```

说明：

- `GET /api/usage/token/` 是 New API 较新版本提供的“令牌用量”只读接口，
  使用站点的 `sk-` 令牌认证，末尾的 `/` 建议保留（避免 301 重定向）。
- 返回值 `data.total_available` 是**剩余额度**，`data.total_granted` 是总额度，
  `data.total_used` 是已用额度；数值为站点额度原始单位（通常 `500000 = 1 美元`）。
- `plan_path` 指向令牌名称，会作为套餐名展示，可按需删除。
- 若站点版本较老、该接口返回 404，可退回 OpenAI 兼容账单接口
  `https://站点域名/v1/dashboard/billing/subscription`（`balance_path: hard_limit_usd`）。
  注意该接口返回的是"总额度（剩余 + 已用）"而不是剩余余额，仅供参考。

### sub2api 程序站点

```yaml
plugins:
  configs:
    api-balance:
      enabled: true
      priority: 10
      endpoint: "https://站点域名/v1/usage"
      method: GET
      balance_path: remaining
      limit_path: quota.limit
      used_path: quota.used
      currency_path: unit
      plan_path: planName
      window_name: 余额
```

说明：

- `GET /v1/usage` 是 sub2api 的密钥用量接口，使用站点的 `sk-` 密钥认证。
- "钱包余额"模式的密钥返回 `remaining`（剩余美元）与 `balance`；设置了总额度的
  密钥会额外返回 `quota.limit` / `quota.used` / `quota.remaining`，字段缺失时
  插件会自动跳过对应可选路径。
- 纯订阅模式（无钱包、无总额度）的密钥响应中没有 `remaining`，此时无法读取
  余额，属于站点侧限制。

## 认证与安全

- API Key/Token 从 CPA 传入的 `storage_json` 读取，不写入日志，也不会放进
  返回给前端的结果。
- 默认只允许 HTTPS；本地自建服务如确实使用 HTTP，才显式设置
  `allow_insecure_http: true`。
- 请只安装你信任的动态库插件。CPA 文档明确说明动态库插件属于宿主进程内的
 可信代码，插件可影响宿主进程。

## 适配 All API Hub / New API 类站点

All API Hub 的核心思路是保存站点地址和凭据，再按站点类型读取余额。这个插件
把“站点差异”下沉到配置：不同站点既可以选择内置厂商预设，也可以调整
`endpoint`、凭据路径和响应 JSON 路径，不需要重新编译插件。若站点需要
Cookie，可把 `credential_header` 改为 `Cookie`、`credential_prefix` 改为
空字符串，并让 `credential_paths` 指向保存 Cookie 的字段。

## CPA 查询入口

启用后，CPA 会把插件注册为 `api-balance` quota provider。余额查询分两段：

1. CPA 先通过插件协议调用 `quota.fetch`；插件再通过宿主的 `host.http.do` 桥接发起上游 HTTP 请求，因此插件本身不监听余额服务端口。
2. 上游地址由供应商配置决定：官方预设使用固定地址；one-api / New API / sub2api 使用凭据中的 `base_url` 拼接路径；自定义站点使用 `endpoint`。这些地址才是实际余额 API 地址。

管理密钥相关请求访问的是 CPA 管理 API，默认地址为 `http://127.0.0.1:8317`，例如读取配置使用 `GET /v0/management/config`，保存插件配置使用 `PUT /v0/management/plugins/api-balance/config`。`management_url` 只用于这类管理 API 请求，与供应商余额接口地址无关。

可通过 CPA 管理 API 的 quota 查询入口读取：

```text
POST /v0/management/quota/fetch
```

请求中使用对应凭据的 `auth_index`；CPA 会负责选择插件并转发标准化余额结果。
