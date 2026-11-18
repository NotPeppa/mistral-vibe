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
| 资源页 | 「Mistral Vibe」页面：填 key → 一键加到 AI 提供商 + 看余额 |

**这个插件不转发流量。** 它把你的 key 写进 CPA 自己的 `openai-compatibility`
列表，之后调用由 CPA 原生转发到 `api.mistral.ai/v1`——工具调用、流式、多模态
全都是 CPA 和 Mistral 自己处理的，插件不在链路上。

## 安装（下载现成的，不用编译）

从 [Releases](https://github.com/NotPeppa/mistral-vibe/releases) 下载对应平台：

| 系统 | 文件 |
| --- | --- |
| macOS（Apple Silicon） | `cpa-mistral-vibe-darwin-arm64.dylib` |
| macOS（Intel） | `cpa-mistral-vibe-darwin-amd64.dylib` |
| macOS（两种都要） | `cpa-mistral-vibe-darwin-universal.dylib` |
| Linux x86_64 | `cpa-mistral-vibe-linux-amd64.so` |
| Linux arm64 | `cpa-mistral-vibe-linux-arm64.so` |

```bash
# 1. 校验
shasum -a 256 -c cpa-mistral-vibe-darwin-arm64.dylib.sha256

# 2. 放进 CPA 的插件目录（文件名随意，但要以 .dylib/.so 结尾）
cp cpa-mistral-vibe-darwin-arm64.dylib /path/to/cpa/plugins/cpa-mistral-vibe.dylib
```

**3. 在 `config.yaml` 里启用** —— 少了这一段，插件只会被“发现”，永远 `registered: false`：

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  configs:
    cpa-mistral-vibe:
      enabled: true
```

重启 CPA，日志应出现：

```
pluginhost: plugin registered plugin_id=cpa-mistral-vibe plugin_name=Mistral Vibe Token Plan version=v0.1.0
```

**4. 打开页面填 key**：

```
http://127.0.0.1:8317/v0/resource/plugins/cpa-mistral-vibe/page
```

粘贴 key → 点「添加到 AI 提供商」。之后 CPA 的「AI 提供商」列表里就有
`Mistral Vibe` 这一条，客户端可以直接用 `mistral-vibe-cli-latest`。

### 从源码编译（可选）

```bash
git clone https://github.com/NotPeppa/mistral-vibe && cd mistral-vibe
go mod download          # 依赖是 CLIProxyAPI v8.0.77 的公开 sdk，无需 CPA 源码
make build               # 本机架构
make release             # 全部平台（Linux 目标需要 Linux 工具链）
make universal           # macOS 通用包
make verify              # 构建后校验版本戳与导出符号
make test                # 单测 + 真 C ABI 端到端
```

| 平台 | 能在哪编译 |
| --- | --- |
| macOS arm64 / amd64 | 任意 macOS（Go 自带 clang 交叉编译） |
| Linux amd64 / arm64 | 只能在 Linux 上（需要目标平台的 C 工具链） |

CI 用三平台矩阵：ubuntu 出两个 Linux 包，macOS runner 出两个 Mac 包
（clang 从 arm64 主机交叉编 x86_64），再由 `lipo` 合成通用包。

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

插件只在你点「添加到 AI 提供商」时出现一次：写一条渠道，然后退场。

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
