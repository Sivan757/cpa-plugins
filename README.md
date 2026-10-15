# cpa-plugins

把模型订阅接进 **CLIProxyAPI（CPA）** 的原生插件。每个插件是一个 C ABI 动态库（Linux 为 `.so`，macOS 为 `.dylib`），由 CPA 直接加载，不依赖 Node 与 DSH 运行时。

## 目录

| 路径 | 作用 |
|---|---|
| `internal/cpakit/` | 插件运行时：ABI 信封、host RPC 桥、能力分发、错误分类、流式助手 |
| `abi/main.go.tmpl` | cgo ABI 胶水模板。`make sync-abi` 把它复制到每个插件目录 |
| `plugins/<name>/` | 一个插件对应一个 provider key |
| `tools/export-tencent-credentials/` | 导出一台已登录机器的 WorkBuddy 桌面凭据，供没装该 App 的机器使用 |
| `tools/export-opencode-credentials/` | 导出 OpenCode 控制台会话令牌与工作区，免费层需要 |
| `scripts/install.sh` | 把构建产物安装到本机 CPA |

## 插件

| 插件 | provider key | 凭据 |
|---|---|---|
| `cpa-provider-opencode` | `opencode` | 付费 Go 层用 API key；匿名免费层不需要密钥 |
| `cpa-provider-qoder` | `qoder` | Qoder 个人访问令牌，加一个区域 |
| `cpa-provider-tencent` | `tencent` | WorkBuddy 与 WorkBuddy AI 桌面登录态，或 CodeBuddy OAuth / API key |

上游协议的怪癖（只接受流式、角色与字段限制、缺省头标记、目录 UA 限制等）记录在对应插件的源码注释里，改动请求体前先读那些注释。

## 构建

需要 Go 与 CGO；macOS 还要 Xcode 命令行工具。

```sh
make sync-abi     # 新增插件目录后必跑
make build        # 产出 dist/*.dylib
make build-linux  # 交叉编译 Linux，产出 dist/*.so
make test         # 离线单元测试
```

在 macOS 上跑 `make build-linux` 需要 Linux 的 cgo 工具链。实际部署在服务器的 `golang` 容器里构建。

## 安装到本机 CPA

```sh
make build
scripts/install.sh          # 首次安装
scripts/install.sh 0.4.0    # 替换已加载的库时带上版本号，见「热重载」
```

脚本只复制动态库并打印待办的配置改动，不改 `config.yaml`，也不重启内核。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    cpa-provider-opencode:
      enabled: true
      priority: 10
      api_key: "oc_sk_..."        # 付费 Go 层；多把用逗号分隔
      expose:
        - "*-free"                # 只暴露免费层模型
    cpa-provider-qoder:
      enabled: true
      priority: 10
      pat: "pt-..."
      region: "china"
      expose:
        - "qwen3.8-flash"
    cpa-provider-tencent:
      enabled: true
      priority: 10
      credential_dir: "/CLIProxyAPI/data/dsh-cpa"
      expose:
        - "deepseek-flash"
```

`expose` 是白名单：留空则暴露该 provider 的全部模型，写了就只暴露匹配的 id，支持 `*` 通配，多个模式可以写成 YAML 列表或逗号分隔的字符串。生产环境用它把每个 provider 收到需要的几个模型。

`credential_dir` 指定插件自己的凭据目录。容器里必须指向挂载路径，否则容器重建后凭据丢失。

## provider key 与模型 id 归属

CPA 把一个 executor 绑定到一个 provider key。宿主按优先级排序后逐个声明模型，**同一个模型 id 只归第一个声明它的插件**（`internal/pluginhost/adapters_executors.go` 的 `claimedModels`）。后声明的插件若有重叠 id，那些 id 会被静默丢弃，不在模型列表里出现。

Qoder 的模型 id 与 OpenCode Go 目录完全重叠。两个插件同时启用时，实测 `qwen3.8-max` 归给 `opencode`，Qoder 的对应模型不可用。要让 Qoder 真正可用，给它配不同的模型 id 或 prefix，或者用 `expose` 把两者的 id 集合分开。

## 凭据

### opencode

付费层在 `plugins.configs.cpa-provider-opencode.api_key` 配置 `oc_sk_...`，多把用逗号分隔；插件为每把 key 生成一条 auth 记录。

**免费层需要账号凭据，不再接受匿名访问。** 实测：带 `Authorization: Bearer public` 的请求即使其余字段与官方 CLI 完全一致，也被上游以 `FreeTierError` 拒绝。接受的凭据是控制台签发的会话令牌加上所属工作区，两者都必须有：

| 值 | 前缀 | 位置 |
|---|---|---|
| 会话令牌 | `st_` | `credential` 表，`integration_id='opencode' and active=1`，取 `value.access` |
| 续期令牌 | `rt_` | 同一行，取 `value.refresh` |
| 工作区 | `org_` | 同一行，取 `value.metadata.orgID` |

数据库在 `~/.local/share/opencode/opencode.db`。

**注意不要读错表。** 同一个数据库里的 `account` 与 `account_state` 存着另一套更早的凭据（`wrk_` 工作区），拿它去请求会返回 HTTP 401 或看起来像指纹问题的 403。

用导出工具取凭据：

```sh
node tools/export-opencode-credentials/export.mjs
# 输出 ~/.dsh-cpa/opencode-account.json（权限 0600）
scp ~/.dsh-cpa/opencode-account.json <server>:/opt/cliproxyapi/data/dsh-cpa/
```

**两个请求头不能发。** 实测（令牌、工作区、会话 id、请求体、UA 全部相同，仅改请求头）：

| 请求头 | 结果 |
|---|---|
| 带 `HTTP-Referer` | 403 `FreeTierError` |
| 带 `x-session-affinity` | 403 `FreeTierError` |
| 两者都不带 | 200 |

触发条件是**头名**而不是值：换一个 referer 值同样被拒，而无关的头（如 `X-Foo`）不影响。`X-Session-Id` 被接受，但官方 CLI 不发它，所以也不发。

**工具列表还要满足形状要求。** 列表里必须有名为 `read` 的工具且其 `properties` 非空；如果声明了 `bash`，它的 `properties` 也不能为空。属性名不校验，但工具名要：同样的 schema 换个名字就失效。占位声明因此各带一个真实参数（`bash.command`、`read.filePath`）。

**令牌自动续期。** `refresh_token` 配好后，插件在到期前 1 小时自动续期：

```sh
POST https://opencode.ai/console/auth/device/token
{"grant_type":"refresh_token","refresh_token":"rt_...","client_id":"opencode-cli"}
# -> {"access_token":"st_...","refresh_token":"rt_...","expires_in":2592000,"org_id":"org_..."}
```

**专用账号。** 免费层凭据应当由一个专用账号持有，不要与日常登录共用。线上插件会持续续期并轮换令牌，而轮换会作废被替换的那一对；同一个账号若同时被本机 CLI 和插件使用，先续期的一方会让另一方的凭据失效。

当前专用账号是 lizf@zealwon.com，工作区 org_01M4WW41D9KRDYJAFN674ATAWX（Personal）。

**工作区 id 会自动发现。** device 登录的令牌响应不带 org_id，而控制台的每次调用都要求 x-org-id。插件在缺少工作区时会调 GET /console/api/orgs（带 Bearer 令牌）取第一个工作区并落盘，所以新账号登录后不需要手工填这个值。

上游每次续期都会**轮换两个令牌并作废旧令牌**（旧 refresh token 再用报 `invalid_grant`）。因此同一凭据只能有一个所有者：不要让 CLI 和插件同时续期同一个账号，否则先续期的一方会把另一方的令牌作废。插件把轮换后的凭据写入 `credential_dir/opencode-account.json`，并且该文件优先于配置项，避免配置里的旧令牌在重载时覆盖已轮换的凭据。

配置项 `session_token` / `refresh_token` / `org_id` 只是首次启动的引导值；`credential_dir` 必须指向挂载路径，否则容器重建会丢失轮换后的凭据。

### qoder

用配置项的 `pat`，或 `auth-dir` 里的记录：

```json
{"type":"qoder","pat":"<令牌>","region":"china"}
```

### tencent

桌面版读 App 自己的登录态文件，所以在 App 里换账号或退出登录会立刻生效。服务器上没装 App，改用导出的凭据。

**导出**（在一台已登录的 Mac 上）：

```sh
node tools/export-tencent-credentials/export.mjs all
```

工具向 App 自己的 Electron 进程取 at-rest 密钥，解开 AES-256-GCM 信封，写出 `~/.dsh-cpa/static-<kind>.json`（权限 0600）。把文件放进服务器的 `credential_dir`：

```sh
scp ~/.dsh-cpa/static-workbuddy-ai.json <server>:/opt/cliproxyapi/data/dsh-cpa/
```

导出的记录含明文 token，用完删掉。access token 会过期，到期前重新导出。

## muse-spark 与 Responses API

免费层把 muse-spark 只发布在 Responses API 上。发到 /chat/completions 会被上游以 ModelProtocolUnsupported 拒绝，所以插件在目录里过滤掉了它们，等于完全无法访问。

现在插件在内部做协议转换，并且只对 muse-spark 生效：

- 请求侧：messages 换成 input，max_tokens 换成 max_output_tokens，system 换成 developer，function 工具声明拍平，工具调用与工具结果变成同级 item。
- 响应侧：读 Responses 事件流并重新发出 chat.completion 分块；非流式路径把终止事件折叠成一个完整回复。

其他模型完全走原来的 chat 路径，因此这条改动不影响已经工作的免费层。

**区域限制。** muse-spark 有区域限制，上游按请求来源 IP 判断。实测同一账号从中国大陆被拒（This model is not available in your country），从服务器所在地可以正常调用。

## 上游端点

插件按官方 Console API 文档使用端点（https://opencode.ai/v2/docs/console/api/inference/）：

| 用途 | 端点 |
|---|---|
| 免费层对话 | POST https://opencode.ai/inference/openai/v1/chat/completions |
| 免费层模型列表 | GET https://opencode.ai/inference/v1/models |
| Go 层对话与列表 | https://opencode.ai/inference/go/v1/... |

**免费层的对话根与列表根不是同一个。** 对话在 /inference/openai/v1，列表在 /inference/v1；拿对话根去取列表会 404。配 base_url 时插件把列表按 <base_url>/models 解析，所以本地抓包代理仍然可用。

旧的 /zen/v1 与 /zen/go/v1 目前仍然应答，两个 Go 基址也都能用。插件改用 /inference 是因为那是文档承认的契约，/zen 没有被承诺保留。

**Go 层必须有 x-opencode-session。** 缺了它上游返回 400 MissingSessionID（实测）。免费层同样发送该头。

官方文档还说明：付费模型需要 Authorization 头，免费对话模型可以不带；控制台也读 x-api-key、api-key、x-goog-api-key。服务账号 key（oc_sk_...）绑定单个工作区，不需要工作区头；用户令牌（st_...）则需要 x-org-id。

## 热重载

配置内容变更会触发 CPA 重载（`internal/watcher/config_reload.go`）。宿主先比对文件内容哈希：内容没变就跳过，内容变了才重建插件运行时，并向已加载的插件发 `plugin.reconfigure`。只改文件时间戳不触发重载。

插件**二进制**只在选中的文件路径变化时才替换。同一个路径、同一份内容不会重载，宿主继续用已加载的旧库。要就地替换一个库，用带版本号的文件名：

```sh
cp cpa-provider-tencent.so <plugins>/cpa-provider-tencent-v0.4.0.so
```

文件名按 `-v<版本>` 解析出版本号。宿主在两个候选之间优先选带版本号的那个，走 `plugin hot reloaded` 路径退休旧库；不带版本号的名字解析为「无版本」，优先级最低。

## 测试

```sh
go test ./...
```

测试走真实的 JSON 信封路径驱动 RPC，断言能力声明、路由、解析与错误分类；上游用 `httptest` 假服务，不打真实网络。会写入真实凭据目录的测试用 `t.TempDir()`，不碰用户数据。

## 部署

部署到服务器：把本仓库克隆到构建机，用 `golang` 容器跑 `-buildmode=c-shared`，把产物放进 CPA 的插件目录，再改配置触发重载。替换已加载的库要给产物加版本号后缀。

本地构建产物（`dist/`、`dist-linux/`）不入库。