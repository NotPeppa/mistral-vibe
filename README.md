# cpa-mistral-vibe

把 **Mistral Vibe 的 token plan** 接进 **CLIProxyAPI (CPA)** 的原生插件，
同时把 token plan 的**当月预算余额**读出来。

这是 CPA 的 **cgo 动态库插件**（`pluginapi` 契约，schema 10）。
**已在真 CPA 上跑通全链路**：注册 → 写 AI 提供商 → 通过 CPA 调模型（含 tool_calls）。

能力声明：

| 能力 | 作用 |
| --- | --- |
| `AuthProvider` (`mistral-vibe`) | 可选：认领「认证文件」页上传的凭证 |
| `QuotaProvider` (`mistral-vibe`) | plan 与当月预算余额（面板余额卡） |
| `ManagementAPI` | `/mistral-vibe/connect`、`/balance`、`/render` |
| `UsagePlugin` | 每次请求完成后记录 token 用量，持久化到 `usage.json` |
| 资源页 | 用量仪表盘：计划余额 + 额度明细 + Token 用量 |

**这个插件不转发流量。** 它把你的 key 写进 CPA 自己的 `openai-compatibility`
列表，之后调用由 CPA 原生转发到 `api.mistral.ai/v1`——工具调用、流式、多模态
全都是 CPA 和 Mistral 自己处理的，插件不在链路上。

## 安装

### 0. 版本要求

插件声明的 plugin schema 是 **6**。CPA 会拒绝比自己新的插件：

```
plugin.register failed: plugin schema version 10 is not supported
```

所以在 CPA **8.0.4**（schema 6）及以上可用；更老的 CPA 需要改低
`schemaVersion` 重新编译。

### 1. 拿到文件

从 [Releases](https://github.com/NotPeppa/mistral-vibe/releases) 下载：

| 系统 | 文件 |
| --- | --- |
| macOS（Apple Silicon） | `cpa-mistral-vibe-darwin-arm64.dylib` |
| macOS（Intel） | `cpa-mistral-vibe-darwin-amd64.dylib` |
| macOS（两种都要） | `cpa-mistral-vibe-darwin-universal.dylib` |
| Linux x86_64 | `cpa-mistral-vibe-linux-amd64.so` |
| Linux arm64 | `cpa-mistral-vibe-linux-arm64.so` |

```bash
shasum -a 256 -c checksums.txt --ignore-missing
```

### 2. 文件名必须改对

**CPA 从文件名推导插件 ID**（`internal/pluginhost/platform.go`）：

```go
id := name                      // "cpa-mistral-vibe-darwin-arm64"
if i := strings.LastIndex(name, "-v"); i > 0 { id, version = split(...) }
```

所以 `cpa-mistral-vibe-darwin-arm64.dylib` 会被当成 ID
`cpa-mistral-vibe-darwin-arm64`，和配置里的 `cpa-mistral-vibe` **对不上，
插件永远显示“未注册”**。

**必须重命名成 `cpa-mistral-vibe.dylib`**（或 `.so`）：

```bash
cp cpa-mistral-vibe-darwin-arm64.dylib /path/to/cpa/plugins/cpa-mistral-vibe.dylib
```

### 3. key 直接写在 config.yaml

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  configs:
    cpa-mistral-vibe:
      enabled: true
      api_key: "mstrl_..."
      base_url: "https://api.mistral.ai"   # 可省略，默认就是它
```

重启 CPA。插件在注册时读到 `api_key`，会把它写进同一个 `config.yaml` 的
`openai-compatibility` 段（原文件备份为 `config.yaml.cpa-mistral-vibe.bak`），
CPA 的文件监听随即加载：

```
pluginhost: plugin registered plugin_id=cpa-mistral-vibe version=0.1.0
server clients and configuration updated: 1 clients (... + 1 OpenAI-compat)
```

实测（真 CPA + 真 key）：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA 访问密钥>" \
  -d '{"model":"mistral-vibe-cli-latest","messages":[{"role":"user","content":"hi"}]}'
# → content: CFGKEY
```

### 4. 插件页面（只读余额，不需要输入任何东西）

```
http://127.0.0.1:8317/v0/resource/plugins/cpa-mistral-vibe/page
```

打开就直接显示余额 —— **页面读的是你在 `config.yaml` 里配的那个 key**，不用再填一次。
「换一个 key」是折叠项，只在你想临时用别的 key 写进 AI 提供商时才需要，
而且那一步要 CPA 的管理密钥（写配置是 CPA 的权限）。

**这个路径只在插件注册成功后才存在。** 未注册时一定 404，先解决第 2 步。

### 面板上能看到的配置项

| 字段 | 说明 |
| --- | --- |
| `api_key` | Mistral API key；填了就在启动时自动写入 AI 提供商 |
| `base_url` | Mistral 地址，默认 `https://api.mistral.ai` |
| `plan_base` | 读计划的 console 地址，默认 `https://console.mistral.ai` |
| `model` | 余额横幅上显示的模型名 |

### 从源码编译（可选）

```bash
git clone https://github.com/NotPeppa/mistral-vibe && cd mistral-vibe
go mod download          # 依赖 CLIProxyAPI v8.0.77 的公开 sdk
make build               # 本机架构；版本号会自动规整成 CPA 接受的格式
make release             # 全部平台（Linux 目标需要 Linux 工具链）
make verify              # 校验版本戳与导出符号
make test                # 单测 + 真 C ABI 端到端
```

## key 是「一条凭证」，不是一份配置

插件只认一个东西：**你的 Mistral API key**。它会同时用于两件事：

| 用途 | 端点 | 说明 |
| --- | --- | --- |
| **花额度**（调模型） | `api.mistral.ai/v1/chat/completions` | CPA 转发时带的就是这个 key |
| **读余额** | `api.mistral.ai/v1/billing/budget` 等 | 同一个 key，读当月预算 |

一个 key 两用，不需要分别填。历史版本里那套 `plan_key` + `inference_key`
双字段（对应 Vibe CLI 内部把「读计划」和「花钱」拆开的做法）已经废弃，
现在只需要给一个 key。

两种落盘方式，效果一样，**页面/ curl 走的是同一条写入**：

- **AI 提供商条目**（推荐）：`Mistral Vibe` 这一条 `openai-compatibility`，
  key 存在 `api-key-entries` 里。见上一节。
- **认证文件**（可选，用于让面板的「认证文件」页也能看到这条凭证）：
  上传一个 `.json`：

  ```json
  {
    "type": "mistral-vibe",
    "plan_key": "mstrl_...",
    "base_url": "https://api.mistral.ai"
  }
  ```

  插件会认领它（`ParseAuth`），并在 `StorageJSON` 里保存 key——**不写进
  Metadata**，因为 Metadata 会显示在面板上。

两者互不冲突：认证文件让凭证出现在「认证文件」页并可测/可开关，
AI 提供商条目让 CPA 真正能路由请求。只做前者，模型列表里不会出现 Mistral；
只做后者，就不需要认证文件。

## 余额：照着 magpie 的做法拿（已实测）

余额不在 `console` 面板接口上，而在 **inference host 的 billing API** 上。这是
[magpie](https://github.com/yetone/magpie)（`internal/provider/mistral_plan.go`）的做法，
也是 Vibe CLI 自己的做法：

```
GET api.mistral.ai/v1/billing/budget         → 这个月的预算与已用百分比
GET api.mistral.ai/v1/billing/subscription   → 在哪个计划上
GET console.mistral.ai/api/vibe/whoami       → 这个 key 花哪一半预算
```

实测返回（Student 计划）：

```json
{"vibe_budget":{"usage_percentage":5.6684,"initial_budget":150.0,"currency":"USD",
                "reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},
 "api_budget": {"usage_percentage":0.001918,"initial_budget":15.0,
                "currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false}}
```

### 关键：预算属于组织，花哪一半取决于 key 的类型

magpie 踩过这个坑（issue #5741）：**同一组织的每个 key 看到的是同样的两个预算**，
所以必须先用 whoami 的 `plan_type` 判断这个 key 花哪一半：

| key 类型 | whoami `plan_type` | 用哪个预算 | 窗口名 |
| --- | --- | --- | --- |
| Le Chat 计划 key（Pro/Student/Team） | `chat` | `vibe_budget` | `Month` |
| 控制台建的 Studio key | `api` | `api_budget` | `API · Month` |

不看类型直接读 `vibe_budget`，会给 Studio key 显示它根本不会花的 Vibe 月额度。

### 实测输出

```
Mistral Vibe token plan
model   mistral-vibe-cli-latest
plan    Student
tier    CHAT
balance 139.36 USD
usage   Month            ▍      7% used · resets 2026-12-01T00:00:00Z
```

计划名同时保留 whoami 的 id（`EDU`/`FREE`）和 billing 的显示名（`Student`/`Pro`/`Team`），
映射照 magpie 的 `mistralPlanTitles`。

### 兜底：速率额度

billing 读不到时（key 没有身份权限），退回读推理响应头的实时额度：

```
x-ratelimit-limit-tokens-minute / x-ratelimit-remaining-tokens-minute
```

这条路径要花 1 token 发一次探测请求，所以只在 billing 没有结果时才走。

## 链路

```
DSH / Claude Code / Codex
        ↓ /v1/chat/completions
   CPA :8317
        ↓ CPA 原生 openai-compatibility 转发
   https://api.mistral.ai/v1/chat/completions
```

插件只在你点「添加到 AI 提供商」（或启动时读到 `api_key`）出现一次：写一条渠道，然后退场。

**模型列表不是写死的。** 插件用你的 key 请求 `GET /v1/models`，把这个账号真正能用的
模型全部写进去 —— 实测 **62 个**（chat、code、embed、OCR、audio 全都有），而不是只管
那三个 Vibe 模型。读取失败时才退回 `mistral-vibe-cli-latest` 等三个 id。

## Token 用量

CPA 在每次请求完成后回调 `usage.handle`，带上上游报告的 token 计数。插件把它
累加进一个账本（`usage.json`，和配置文件同目录），页面「Token 用量」一块显示
**六个计数器**和按模型的拆分：

| 计数器 | 来源 |
| --- | --- |
| 请求数 | 每次回调 |
| 输入 | `usage.prompt_tokens` |
| 输出 | `usage.completion_tokens` |
| **缓存命中** | `usage.prompt_tokens_details.cached_tokens`，附带命中率 |
| **推理** | `usage.completion_tokens_details.reasoning_tokens` |
| 合计 | `usage.total_tokens` |

缓存为 0 时页面写「本次无缓存命中」而不是 `0%` —— 没说就是没说，不编一个命中率。

```json
GET /v0/management/mistral-vibe/usage
{"all":{"requests":3,"input":51,"output":6,"total":57,...},
 "today":{"requests":3,...},
 "models":[{"Model":"mistral-vibe-cli-latest","Requests":3,"Total":57}],
 "recent":[{"at":"2026-11-18T22:43:01+08:00","model":"...","total":19}]}
```

清空：`POST /v0/management/mistral-vibe/usage/reset`。

记账规则：

- **只记录 CPA 回调的**。绕过 CPA 直接打 Mistral 的请求不会出现在这里。
- **保留 200 条最近请求**，页面显示前 20 条；账本按需写入（最少间隔 3 秒），
  写临时文件再 rename，不会写坏。
- **今日**按本地日期滚动，跨天后自动清零。
- token 数来自响应里的 `usage`，如果上游没报就是 0 —— 不估算、不补数。

实测（真 CPA + 真 key，3 次调用）：

```
今日请求 3 · 今日输入 51 · 今日输出 6 · 今日合计 57
模型 mistral-vibe-cli-latest · 请求 3 · 占比 100%
```

## 面板上为什么看不到「Mistral」这张卡

「AI 提供商」顶部那排（Kimi / Gemini / Codex / Muse…）是 CPA 面板前端**硬编码**
的，插件没有接口往里加卡。CPA 的 management 路由里只有
`GET /v0/management/quota/providers` 与提供商有关，而它返回的是**额度**提供者，
不是卡片。

插件能做、也确实做的事：写一条 `openai-compatibility` 条目，它会作为一条
**列表项**出现在「AI 提供商」里（和「OpenAI 兼容」同类），带名称、Base URL、
key 和模型列表。

## 自测

```bash
./tests/e2e.sh                                   # 43 项，假上游 + 假管理 API
MISTRAL_API_KEY=mstrl_... ./tests/e2e.sh         # 52 项，多出真 key 的余额与发布
```

覆盖：`go vet`、单测（编码 / 预算解析 / 额度 / 提供商 upsert / 页面路由）、
4 个导出符号、以及**通过真实 C ABI**（`dlopen` + `dlsym cliproxy_plugin_init`）
驱动的 connect / balance / 页面 / 未知方法。

## 已知边界

- **不会新增提供商「卡片」**：只能写 `openai-compatibility` 列表项，理由见上。
- **余额是当月预算**：`/v1/billing/budget` 给的是组织月预算，同组织多个 key 数字相同。
- 余额与模型调用在 CPA 里是**两处配置**：AI 提供商那条让 CPA 能调用，
  QuotaProvider 让面板能显示余额。它们读的是同一个 key。
- 写 AI 提供商需要 CPA 的 Management Key；页面会复用面板会话，取不到时明确报错。

## 实现备注（踩过的坑）

- **CPA 的 `Metadata` 没有 json tag**：插件描述符必须用 `Name`/`Version`/`Author`/
  `GitHubRepository` 这些**大写 Go 字段名**，snake_case 会被判为 invalid metadata。
- **`plugin.reconfigure` 也要返回完整描述符**：CPA 会重新校验一次。
- **`ManagementRoute.Handler` 不能出现在响应里**：它是接口，CPA 自己会补；
  传了会让 host 解码失败。
- **资源路由的 `Path` 不能是 `/`**：CPA 会 `TrimRight(path, "/")` 后拒绝空路径。
- **cgo 库拿不到 Go 侧 `os.Setenv`**：插件读的是进程启动时的环境，所以管理面地址
  允许由请求体里的 `management_base` 指定，并会尝试从 `config.yaml` 读 `port`。
- **通过函数指针回调 Go 在 macOS 上会挂死**：必须 C 直接引用 Go 符号。
- **`StorageJSON` 是 `[]byte`**：Go 侧写入是 base64，手写种子却是对象，
  `codec.go` 两种都收并反射回填。
- **map 型字段不能统一 pascal 化**：`http.Header` / `url.Values` 的键是用户数据，
  否则 `X-Vibe-Plan-Key` 会被改写成 `XVibePlanKey`。
- **Go 不读 macOS 系统代理**：读 plan 要走 `VIBE_PLAN_PROXY` / `HTTPS_PROXY`，
  但回环与私网地址必须绕过代理。
- **密钥只进 `StorageJSON`**：Metadata 会显示在面板上并被写进文件。
