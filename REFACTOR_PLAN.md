# api-balance 插件重构与优化计划

## 执行摘要

**当前状态**: 
- 单文件 1853 行，功能齐全但架构混乱
- 测试覆盖率 44%，缺乏边界测试
- 已发现并修复多个运行时问题（空body、404等）

**目标**: 
- 模块化架构，提升可维护性
- 完善测试覆盖，防止回归
- 优化代码质量，降低认知负担

## 第一阶段：代码分析与测试增强（1-2天）

### 1.1 完善现有测试覆盖
**优先级**: ⭐⭐⭐⭐⭐

**当前问题**:
- 无性能基准测试
- 边界条件测试不足
- 缺少集成测试

**行动项**:
```
□ 添加基准测试:
  - BenchmarkFetchQuota (常见厂商)
  - BenchmarkDecodeConfig (大配置文件)
  - BenchmarkAutoDetect (多profile场景)

□ 边界测试:
  - 超大数字处理 (float64精度)
  - 畸形JSON响应
  - 并发配置更新
  - HTTP超时/重试

□ 表格驱动测试重构:
  - TestVendorPreset -> 统一测试所有厂商
  - TestExpandEndpoint -> 覆盖所有占位符

□ 测试辅助工具:
  - mockHTTPServer 简化HTTP mock
  - configBuilder 流式构建测试配置
```

**验收标准**: 
- `go test -cover` 达到 75%+
- `go test -race` 无警告
- 新增至少10个边界测试

### 1.2 代码静态分析
**优先级**: ⭐⭐⭐⭐

**工具链**:
```bash
# 安装分析工具
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/tools/cmd/godoc@latest
go install github.com/securego/gosec/v2/cmd/gosec@latest

# 执行分析
staticcheck ./...
gosec ./...
go vet ./...
golangci-lint run
```

**行动项**:
```
□ 修复staticcheck报告的问题
□ 处理未使用的变量/导入
□ 规范错误信息格式
□ 添加缺失的godoc注释
```

## 第二阶段：架构重构（3-5天）

### 2.1 模块拆分设计

**目标结构**:
```
api-balance/
├── main.go                 # CGO入口 + 插件注册 (~200行)
├── config.go               # 配置类型定义 (~150行)
├── config_loader.go        # YAML解析 (~200行)
├── config_resolver.go      # Profile解析 (~150行)
├── quota_fetcher.go        # 余额查询核心 (~300行)
├── vendor_detect.go        # 厂商识别 (~200行)
├── http_client.go          # HTTP封装 (~100行)
├── json_path.go            # JSON路径提取 (~100行)
├── management.go           # 管理API (~200行)
├── management_wizard.go    # 配置向导 (~300行)
├── constants.go            # 常量定义 (~50行)
└── internal/
    ├── testutil/           # 测试辅助
    └── vendor/             # 厂商预设配置
```

**优势**:
- 每个文件职责单一，易于理解
- 便于并行开发和测试
- 降低认知负担

### 2.2 类型系统优化

**问题**: `config` 既是顶层配置又是profile配置

**解决方案**:
```go
// 顶层配置
type PluginConfig struct {
    Enabled    bool
    Priority   int
    
    // 全局HTTP设置
    Headers    map[string]string
    Query      map[string]string
    
    // 全局凭据路径
    CredentialPaths []string
    
    // 管理API
    ManagementKey string
    ManagementURL string
    
    // 档案配置
    Profiles   map[string]ProfileConfig
    
    // 匿名档案（向后兼容）
    ProfileConfig
}

// 档案配置（纯余额查询配置）
type ProfileConfig struct {
    Vendor        VendorType  // 类型化枚举
    BaseURL       string
    Endpoint      string
    // ...其他字段
}

type VendorType string

const (
    VendorDeepSeek   VendorType = "deepseek"
    VendorMoonshot   VendorType = "moonshot"
    VendorOpenRouter VendorType = "openrouter"
    VendorOneAPI     VendorType = "one-api"
    VendorNewAPI     VendorType = "new-api"
    VendorSub2API    VendorType = "sub2api"
    VendorCustom     VendorType = "custom"
)
```

### 2.3 错误处理标准化

**当前问题**: 
- 错误信息不统一
- 缺少错误码
- 调试信息不足

**改进方案**:
```go
// 定义错误类型
type BalanceError struct {
    Code    string  // 机器可读
    Message string  // 人类可读
    Details map[string]any
    Cause   error
}

func (e *BalanceError) Error() string {
    if e.Cause != nil {
        return fmt.Sprintf("%s: %v", e.Message, e.Cause)
    }
    return e.Message
}

// 预定义错误码
const (
    ErrCodeInvalidConfig    = "invalid_config"
    ErrCodeMissingBaseURL   = "missing_base_url"
    ErrCodeNetworkError     = "network_error"
    ErrCodeInvalidResponse  = "invalid_response"
    // ...
)
```

## 第三阶段：功能增强（2-3天）

### 3.1 配置验证增强

**行动项**:
```
□ 启动时配置预检
  - 必填字段检查
  - URL格式验证
  - credential_paths存在性检查

□ 配置迁移助手
  - 检测过时字段并警告
  - 提供自动迁移建议

□ 配置diff工具
  - 热更新时显示变更
```

### 3.2 性能优化

**行动项**:
```
□ HTTP客户端连接池复用
□ 响应缓存策略
  - 短时间内重复查询返回缓存
  - 可配置TTL
□ 并发查询优化
  - 多profile并行查询
□ JSON解析优化
  - 流式解析大响应
```

### 3.3 可观测性增强

**行动项**:
```
□ 结构化日志
  - 统一日志格式
  - 可配置日志级别
  - 请求追踪ID

□ 指标暴露
  - 查询成功/失败计数
  - 响应时间分布
  - 厂商识别命中率

□ 健康检查端点
```

## 第四阶段：文档完善（1-2天）

### 4.1 代码文档

**行动项**:
```
□ 所有导出函数添加godoc
□ 复杂算法添加注释
□ 添加包级别文档
□ 生成godoc站点
```

### 4.2 用户文档

**行动项**:
```
□ 配置项完整参考
  - 每个字段的含义
  - 默认值
  - 示例

□ 故障排查指南
  - 常见错误及解决
  - 调试技巧

□ 最佳实践
  - 多供应商配置模式
  - 性能优化建议

□ 变更日志规范化
  - 使用语义化版本
  - 详细的breaking changes说明
```

## 第五阶段：CI/CD增强（1天）

### 5.1 测试自动化

**行动项**:
```
□ 添加测试覆盖率报告
□ 代码质量门禁
  - coverage >= 75%
  - no critical issues
□ 依赖漏洞扫描
□ 多平台测试矩阵
  - linux/amd64
  - linux/arm64
  - darwin/amd64
  - darwin/arm64
```

### 5.2 发布流程

**行动项**:
```
□ 自动生成release notes
□ 版本号自动递增
□ 构建产物校验和
□ 发布后烟雾测试
```

## 执行时间表

| 阶段 | 时长 | 输出物 |
|------|------|--------|
| 第一阶段 | 1-2天 | 测试覆盖75%+, 静态分析clean |
| 第二阶段 | 3-5天 | 模块化代码, 类型系统v2 |
| 第三阶段 | 2-3天 | 性能优化, 可观测性 |
| 第四阶段 | 1-2天 | 完整文档 |
| 第五阶段 | 1天   | CI/CD管道 |
| **总计** | **8-13天** | 生产级质量插件 |

## 风险评估

### 高风险
- **破坏性变更**: 配置结构重构可能导致不兼容
  - 缓解: 保持向后兼容，提供迁移工具
  - 版本策略: 重大重构发布为 v1.0.0

### 中风险
- **测试不足**: 重构后可能引入新bug
  - 缓解: 先完善测试再重构
  - 每个改动都必须有对应测试

### 低风险
- **性能回退**: 模块化可能影响性能
  - 缓解: 基准测试门禁
  - 持续性能监控

## 成功标准

### 代码质量
- ✅ 测试覆盖率 >= 75%
- ✅ 所有staticcheck问题已修复
- ✅ 无race condition警告
- ✅ Godoc覆盖所有导出符号

### 架构
- ✅ 单文件不超过500行
- ✅ 循环依赖为0
- ✅ 接口定义清晰

### 用户体验
- ✅ 配置向导可用且易用
- ✅ 错误信息清晰可操作
- ✅ 文档完整准确

### 性能
- ✅ 余额查询 < 2s (P95)
- ✅ 配置加载 < 100ms
- ✅ 内存占用 < 50MB

## 下一步行动

**立即开始** (不需要用户确认):
1. 运行静态分析工具
2. 生成当前覆盖率报告
3. 识别最关键的缺失测试

**等待确认**:
1. 重构方案是否认可?
2. 时间预算是否合理?
3. 优先级是否需要调整?

---
*创建时间: 2025-01-09*
*作者: Kiro AI*

---

## 执行状态（2026-09-15 更新）

按「先修正确性、再做结构」的原则执行，避免了为拆文件而拆文件的形式重构。

### 已完成（本轮）

**正确性修复（均有回归测试锁定）：**
1. **profiles 继承全局请求设置** — `applyConfig` 与 `resolveProfile` 统一经
   `inheritGlobalRequestConfig` 把全局 `headers/query/credential_paths`
   注入档案，修复"配置看似生效但请求缺认证"的问题。
2. **JSON 配置 snake_case 解析** — `config` 全字段补齐 `json` 标签，
   修复 `{"base_url":...}` 经管理 API 保存后字段丢失的问题。
3. **向导保存改为严格白名单 + 合并** — 仅允许 `enabled/priority/profiles`
   与无敏感性的余额标量字段（含向导把唯一 default 档案提升到顶层的形态）；
   `management_key/management_url/headers/query/credential_paths/
   credential_*/allow_insecure_http` 一律拒绝；保存前与现有配置合并，
   PUT 不再丢失服务端字段。
4. **厂商域名识别收紧** — `vendorByHost` 只按主机名与域名边界匹配
   （`deepseek.com` 等），不再把 `evil-deepseek.example` 误判为官方。
5. **URL 校验收紧** — endpoint/management_url 拒绝用户信息与 fragment；
   management_url 拒绝携带资源路径。
6. **管理响应类型化** — `managementResponse` 结构体替代匿名 map，
   编译期锁住 `StatusCode/Headers/Body` 契约。
7. **autoDetect 错误传播** — 探测路径中 `normalizeDefaults` 失败不再被吞掉。
8. **探测缓存失效** — `applyConfig` 重载配置时清空 `strategyCache`，
   避免旧站点类型残留。

**测试与验证：**
- 覆盖率 52.6% → **65.6%**；新增 11 个测试：档案继承、snake_case JSON、
  白名单/合并、顶层提升形态、URL 注入拒绝、域名边界、端到端 fetchQuota
  （含认证头断言）、上游错误传播、配置并发交换（-race 通过）、
  模板展开/错误信封、注册元数据。
- `go test ./...`、`go test -race ./...`、`go vet ./...` 全绿；
  Linux arm64 c-shared 构建通过。

**CI：**
- 新增 `.github/workflows/ci.yml`：push/PR 触发，amd64+arm64 矩阵，
  vet + race + 覆盖率 + c-shared 构建验证。

### 未做（与计划的差异）

- **多文件拆分**：未执行。当前单文件 ~2088 行，函数已按区块注释分组；
  拆分属于纯结构改动，建议在下一版本单独做，避免与本轮行为修复混淆。
- **VendorType 类型化枚举 / BalanceError 结构化错误**：涉及对外错误
  文本兼容（CPA UI 直接展示错误字符串），保持现有中文错误文本不变更稳妥。
- **缓存 TTL / 性能基准**：当前无性能问题报告，暂不引入缓存复杂度。

### 剩余风险

- `timeout_seconds` 仅为声明字段，实际超时由宿主 HTTP 桥控制；
  若宿主不支持按请求超时，该配置不会生效（已在 ConfigFields 描述中提示）。
- 管理端点 `PUT /v0/management/plugins/:id/config` 的合并语义依赖宿主
  实现；若宿主未来改为整份替换，合并逻辑需同步复核。
