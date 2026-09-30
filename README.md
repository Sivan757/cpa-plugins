# cpa-plugins

把 DSH 侧的模型订阅插件改写成 **CLIProxyAPI（CPA）原生插件**，按厂商拆分，统一在 CPA 里做额度查看与模型管理。

每个厂商一个动态库（C ABI），由 CPA 直接加载，**不依赖 Node / DSH 运行时**。

## 目录

| 路径 | 作用 |
|---|---|
| `internal/pluginkit/` | 通用 CPA 插件运行时：C ABI 契约、JSON 信封、host 回调、各能力（quota / auth / model / executor / management）的类型与分发层 |
| `abi/abi.go.tmpl` | cgo C ABI glue 模板；`make sync-abi` 复制到各插件目录 |
| `plugins/<name>/` | 各厂商插件实现 |
| `dist/` | 构建产物（`*.dylib`） |
| `../recon/` | 上游协议契约侦察报告（非本仓库代码） |

## 为什么一个厂商可能产出多个动态库

CPA 把**一个插件的 executor 绑定到一个 provider key**（`internal/pluginhost/adapters_executors.go` 的 `executorProvider`），而多个产品可能共用同一套模型 id。这类产品就**用同一份源码、不同 ldflags 编译出多个库**，各自声明自己的 provider key。WorkBuddy 的国内版与国际版就是这样。

## 构建

需要 Go（本机在 `/opt/homebrew/bin/go`）与 CGO／Xcode 命令行工具。

```sh
make sync-abi      # 把 abi.go 复制进每个插件目录（新建插件后必跑）
make build         # 产出 dist/*.dylib
make test          # 离线单元测试
make test-live     # 打真实上游的测试（消耗额度，见下）
```

## 安装到 CPA

1. 把 `dist/*.dylib` 放到 CPA 的插件目录。目录按 `plugins.dir` 解析，并优先匹配 `<root>/<GOOS>/<GOARCH>/`，其次 `<root>/`：本机是 `~/Library/Application Support/com.cpa.gui/cpa-core/plugins/`（即 `darwin/arm64` 或直接放根）。
2. 在 CPA 的 `config.yaml` 打开插件并逐个启用：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-provider-workbuddy:
      enabled: true
      priority: 10
    cpa-provider-workbuddy-ai:
      enabled: true
      priority: 10
```

3. 重启 CPA 内核（配置变更不会重载插件动态库；插件目录与启用状态在启动时读取）。
4. 为每个要用的 provider 建一个凭据文件放进 `auth-dir`（见各插件小节）。**凭据文件只声明 provider 与要跟随的桌面产品，不放 token**：插件每次都去读桌面 App 当前的登录态，所以在 App 里换账号/退出登录会立刻生效。

## 各插件

### cpa-provider-workbuddy / cpa-provider-workbuddy-ai（腾讯 WorkBuddy）

| 项 | 国内版 | 国际版 |
|---|---|---|
| provider key | `workbuddy` | `workbuddy-ai` |
| 动态库 | `cpa-provider-workbuddy.dylib` | `cpa-provider-workbuddy-ai.dylib` |
| 对话 | `https://copilot.tencent.com/v2/chat/completions` | `https://www.workbuddy.ai/v2/chat/completions` |
| 模型目录 | `GET {base}/v3/config`（CLI 身份 UA） | `GET {base}/v3/config`（App 身份 UA） |
| 额度 | `POST https://www.codebuddy.cn/v2/billing/meter/get-user-resource` | `POST https://www.workbuddy.ai/v2/billing/meter/get-user-resource` |
| 企业额度 | `POST /v2/billing/meter/get-enterprise-user-usage`（CN 且带 enterpriseId 时走这条） | 不走（海外未验证） |
| 凭据来源 | `~/Library/Application Support/CodeBuddyExtension/Data/Public/auth/workbuddy-desktop.info` | 同目录 `workbuddy-desktop-ai.info` |

凭据文件（放进 `auth-dir`）：

```json
{"type":"workbuddy"}
```
```json
{"type":"workbuddy-ai"}
```

**凭据解密**：桌面 App 5.6+ 把 token 字段写成 `{"$wbEncrypted":1,"envelope":"<base64 JSON>"}`，算法是 AES-256-GCM，密钥 = `sha256(atRestSecretKey)`。
`atRestSecretKey` 只能通过 App 自己改过的 Electron 私有 binding 取：

```sh
ELECTRON_RUN_AS_NODE=1 "/Applications/WorkBuddy.app/Contents/MacOS/Electron" \
  -e 'process.stdout.write(String(process._linkedBinding("electron_browser_workbuddy_storage").loggerGet()))'
```

插件用同样的方式取密钥（只读、进程内缓存、不落盘）。可用 `WORKBUDDY_ELECTRON_BIN` / `WORKBUDDY_AI_ELECTRON_BIN` 覆盖 App 路径。

**已在插件里复现的上游怪癖**（不处理就会被上游拒绝）：
- 上游**只接受流式**，非流式请求报 `11101`，所以入站流式/非流式都按流式发，非流式在插件里聚合。
- `role: "developer"` 报 `11128`，改成 `system`。
- 国际版要求首条消息必须是 `system`，否则补一条。
- 国际版拒绝 `reasoning_effort: "off"`，改为删除该字段。
- `tool_choice` 只收字符串，对象形态要摊平；`none`/未知形态要连同 tools 一起删。
- 缺少 `X-User-Id`/`X-Enterprise-Id`/`X-Domain` 时不能省略，要发 `X-No-User-Id: 1` 这类显式"缺失"标记。
- 国际版目录 UA 不能带空格（`12403`）。

**已验证**（2026-09-30，本机）：国内版真实解密凭据 → 16 个模型 → 21 个套餐额度；国际版遗留明文凭据 → 23 个模型 → 额度；两者流式与非流式真实对话均成功。


### cpa-provider-codebuddy（腾讯 CodeBuddy）

与 WorkBuddy 国内版同上游（`copilot.tencent.com`），但凭据自管：

| 项 | 值 |
|---|---|
| 凭据模式 | OAuth 设备流（推荐）/ API Key（`ck_` 前缀，多把轮询） |
| OAuth 令牌落盘 | `~/.dsh-cpa/cpa-provider-codebuddy-auth.json`（0600） |
| API Key 来源 | `plugins.configs.cpa-provider-codebuddy.api_key` 或环境变量 `CODEBUDDY_API_KEY` |
| 对话 | `POST {base}/v2/chat/completions`（仅流式） |
| 目录 | `GET {base}/v3/config` |
| 额度 | `POST {base}/billing/meter/get-user-resource`（OAuth 专属，注意**没有** `/v2` 前缀） |
| 轮询换 Key | 401/403/429/5xx 与网络错误触发，冷却 60s；业务码（11102 等）不换 |

OAuth 流程：管理面板触发 `GET /v0/management/{provider}-auth-url` → `POST /v2/plugin/auth/state?platform=CLI` 拿 `authUrl` → 浏览器授权 → 每秒轮询 `GET /v2/plugin/auth/token?state=`（`11217` = 未完成，pending/伪造/过期三态同码不可区分，只能本地超时 10 分钟）。续期读服务端的 `refreshExpiresIn`（秒）——不是 `refreshExpiresAt`，原始 TS 插件在这里有个潜伏 bug。

### cpa-provider-qoder（阿里 Qoder）

PAT 路线（覆盖 global `qoder.sh` 与 china `qoder.com.cn` 双区）：

```json
{"type":"qoder", "pat":"<Qoder 个人访问令牌>", "region":"global"}
```

或 `plugins.configs.cpa-provider-qoder.pat` / 环境变量 `QODER_PAT`。`/v3/config` 与聊天走 COSY 签名（RSA 包随机 AES key + AES-128-CBC + md5，Go 标准库实现，无 WASM）；额度走裸 Bearer 的 `/api/v2/quota/usage`。

**额度必须读 `addOnQuota`**：`userQuota` 对 `personal_standard` 账号恒为 0，只读它会显示 0 余额。Qoder CN 是 credits + 计费周期重置 + 每日请求数上限，**没有** 5 小时/周滚动窗口。模型路由由 `X-Model-Key` 头决定，不认 `body.model`。



### 本机安装踩过的坑（重要）

**EasyCLIProxyAPI 控制面板会重写 config.yaml**：它用 4 空格缩进并给键加引号（`"pat": "..."`）。插件侧的 YAML 解析器一开始没剥键上的引号，导致 PAT 配了却读不到（状态页显示 unconfigured）。已修复并有回归测试（`internal/pluginkit/config_test.go`）。

**控制面板重写配置时会丢掉手写的配置项**：手写进 `plugins.configs` 的条目在面板保存后可能消失。改完配置后要再检查一遍，或直接从面板 UI 改。

**凭据桩必须建**：插件在 auth-dir 里需要一个 `{"type":"<provider>"}` 的 JSON 文件才会生成凭据记录；缺了它额度查询会报 `auth_index is required`。

## 验证结果（2026-09-30 本机）

`scripts/verify-lab.sh` 在独立 CPA 实例（端口 8318，独立 auth-dir 与插件目录）上的输出：

- 5 个动态库全部加载并注册（`registered=true`）
- 5 个 quota provider 全部注册
- 5 个 `status` 管理路由全部 200
- WorkBuddy CN：16 模型、21 个套餐、剩余 1267 credits；真实对话返回 `OK`（`finish_reason=stop`，含 usage 与 credit）
- WorkBuddy AI：23 模型、Free Plan 剩余 98 credits
- CodeBuddy / Qoder / Trae：本机无凭据，额度返回**带修复指引的错误**而不是假数据

已知限制：CPA 会把重叠的模型 id 归给先注册的插件。WorkBuddy 与 CodeBuddy 同开时，`glm-5.3` 等 id 只会出现在其中一家；要完整用 CodeBuddy 目录就单独启用它。

### 诊断

每个插件注册一个只读管理路由 `GET /v0/management/plugins/<pluginID>/status`，返回凭据状态、解析到的 App 版本、缓存模型数、上游基址等。排查时先看这个。

## 环境变量与配置

| 变量 / 配置项 | 作用 |
|---|---|
| `plugins.configs.<id>.cache_ttl_seconds` | 模型目录缓存时长（默认 300 秒） |
| `WORKBUDDY_ELECTRON_BIN` / `WORKBUDDY_AI_ELECTRON_BIN` | 覆盖桌面 App 的 Electron 可执行文件路径 |
| `CPA_LIVE_TEST=1` | 打开打真实上游的测试（会消耗额度） |

## 测试策略

- **离线单测**（`make test`）：走真实的 JSON 信封路径驱动 RPC，断言能力声明、路由、解析与错误分类；对话侧用 `httptest` 假上游。
- **实机测试**（`make test-live`）：解密真实桌面凭据、拉真实目录、查真实额度。**默认跳过**，因为它读真实账号。
- CPA 侧的端到端验证用 `cpa-lab/`（独立端口 + 独立 auth-dir + 独立插件目录），不触碰用户正在用的 CPA 实例。

## 参考

- 上游协议侦察报告：`../recon/workbuddy.md`、`../recon/codebuddy.md`、`../recon/qoder.md`、`../recon/traework.md`
- CPA 插件 ABI：`sdk/pluginabi/types.go`、`sdk/pluginapi/types.go`（本机对应 v7.3.18 源码）