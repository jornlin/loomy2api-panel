<p align="center">
  <h1>Loomy2API Panel</h1>
</p>

<p align="center">
  <b>把讯飞 Loomy 账号变成 OpenAI / Anthropic 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  账号池轮转 · 熔断与冷却 · 会话粘性 · 流式 / 非流式 · 零第三方依赖 · 单二进制
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26.10-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

> 上游服务为讯飞 Loomy（`loomyad.xunfei.cn`），**非官方、未获授权**；仓库不含任何账号、token 或密钥。
> 上游随时可能收紧导致通道不可用；因使用非官方客户端导致的账号风控 / 封禁，由使用者自行承担。

## 本仓库（fork）

这是一个**自用 fork**，在上游基础上做了读图链路与面板的修复，日常在本机 Docker 里跑。

| 项 | 值 |
|---|---|
| 上游仓库（`origin`） | `git@github.com:misakano7545/loomy2api-panel.git` |
| Fork（`fork`） | `git@github.com:jornlin/loomy2api-panel.git` |
| 分叉基线 | `609d437`（上游 `main`，2026-11 初） |
| 默认模型 | `DeepSeek-V4.1-Flash`（上游 `deepseek-v4-flash-0731` 已 404） |

### 上游参数与同步

本 fork 只读跟随上游，**所有改动都以独立分支 + PR 的形式提交**（`jornlin` 对上游仓库没有写权限）：

```bash
# 首次：如果本地只有一个 remote
git remote add upstream git@github.com:misakano7545/loomy2api-panel.git

# 跟随上游
git fetch upstream
git checkout main && git merge upstream/main
git push fork main
```

> 上游「血统」说明见本文件末尾；上游 README 里关于维护者个人状况的段落与本 fork 无关，已移除。

## 本仓相对上游的改动

5 个提交，分为三块：**读图链路修复**（两处）、**面板分页与排序**、**面板性能**。
每项后面是当时的实测数据。

### 1. 修复：图片标记改写从「按名字通配」收紧为显式白名单

上游换成 `DeepSeek-V4.1-Flash` 之后，`internal/server/imgtag.go` 里
「模型名含 `deepseek` 就把 data URI 图片改写成 `<image_base64>` 标记」的通配规则失效了——
新模型不认这个标记，图被当成纯文本送进模型，而**全链路不报错**。

| | prompt_tokens | 结果 |
|---|---|---|
| 修复前 | 109,418 | 图被当纯文本内联，模型答「无法解码或识别画面」 |
| 修复后 | 361 | 正常视觉编码，描述与真图逐项吻合 |

改成配置项 `image_tag_models`（子串匹配，**默认空 = 一律透传标准 `image_url`**），
旧行为仍可按模型开启。

### 2. 修复：三条协议的图片都不再被静默降级成文本

两条转换桥都在丢图，且不报错：

- `internal/bridge/responses.go` 的 `ContentToText` 把 `input_image` 压成字面量 `"[image]"`
- `internal/bridge/anthropic.go` 的逐块循环**没有 `image` 分支**，Anthropic 的图块整块丢弃

新增 `internal/bridge/chatcontent.go` 统一拼 content：**无图仍返回纯字符串（旧行为逐字不变）**，
有图才返回 content 数组，图片统一转成 `{"type":"image_url",…}`，data URI 与 http(s) URL 都透传；
`tool_result` 里的图（截图类工具）也一并保住。顺带让 `ChatToResponse` 兼容数组形态正文，
并删掉已无人调用、却留着 `"[image]"` 地雷的 `ContentToText`。

### 3. 面板：11 个长列表统一分页

账号池 136 个账号时页面长到没法用，而所有列表都没有分页。

- 共用分页组件：每页 10/20/50/100/200（记在 `localStorage`），事件委托换页，筛选变化回第 1 页
- 分页条挂在**横向滚动容器之外**：账号表实测 1236px 而容器只有 927px，放 `<tfoot>` 会被滚出右边界
- 覆盖：账号池 / 积分到期 / 模型锁池 / 已下架模型 / 模型目录 / 按小时用量 / 用量维度 /
  积分扣除维度 / 请求记录 / 错误汇总 / 积分流水

同时修掉两个真问题：

- `loadLogs` 原本是 `Promise.all` + **空 catch**：一条慢请求会把请求记录 / 错误汇总 / 积分流水
  三张表一起清空，控制台还没有任何线索 → 改逐块渲染（谁回来先画谁）+ 单请求超时 + 可见报错
- `/panel/api/credit_history` 串行遍历全池账号逐个打上游（136 账号实测 **57.9 秒**）
  → 并发 8 + 单账号 8s 超时 + 60s 缓存：**冷启 5.0s / 命中 0.03s**

### 4. 面板：账号池与请求记录加表头排序；「积分到期提醒」并入账号池

到期表的 133 行全部来自 `/overview` 的 accounts，与账号池是同一批数据；而每日赠送按自然日
滚动，几乎所有账号的到期日都是「明天」，独立成表信息密度极低却多占一整屏。

- 账号池新增「到期」列（每日余额 + 剩余天数，今天到期标黄）并支持排序
- 表头加状态筛选：全部 / 可用 / 冷却中 / 已暂停选号 / 已禁用 / 有到期积分
- 顶部保留一行汇总（多少个账号有每日余额 · 合计 · 1 天内到期数）
- 表头排序只给**账号池（8 列）**与**请求记录（5 列）**加；用量与模型目录本来就有排序下拉，
  错误汇总由后端按严重度聚合，积分流水/按小时天然是时间序，再加是噪音

### 5. 面板：积分构成页 15s → 毫秒级

`/panel/api/packages` 对每个账号打 3 个上游接口（Quota + Points + FirstLogin），并发只有 3，
实测 **15.0s / 次**；前端又一次性渲染 **136 张卡片 + 135 个「每账号一个 box + 一张表」**。

| 指标 | 优化前 | 优化后 |
|---|---|---|
| 接口（冷启 / 缓存命中） | 15.0s | **8.5s / 0.047s** |
| 页面首屏 | 16.0s | **1.7s**（缓存命中） |
| 页面元素 | 8,620 | **2,046**（该视图 7,184 → 610） |
| 页面高度 | 40,599px（28 屏） | **3,262px（2 屏）** |

改法：并发 3→8、**120 秒结果缓存**（`?force=1` 绕过，面板「刷新」走这条）、凭证落盘移出并发段；
卡片与明细都分页，明细的 135 个 box 合并成一张扁平表。

> 以上改动均有对应的回归测试；`go build ./...` / `go vet ./...` / `go test ./...` 全绿（237 用例）。

## 项目简介

Loomy2API 是一个自托管的 **反向代理网关**，把讯飞 Loomy（`loomyad.xunfei.cn`）的账号
包装成统一的、兼容 OpenAI 与 Anthropic 双协议的 API 服务。

- 官方不提供 OpenAI 形态的开放 API。本项目通过**手机号 + 密码服务端登录**拿到登录态
  （`session`），并用它作为上游 Bearer token；
- 面向**个人多账号**场景：多号共享、单号故障自动换号、冷却 / 熔断防雪崩、会话粘性保证多轮不跳号；
- 对客户端暴露 `chat_completions`、`codex_responses` 与 `anthropic_messages`，
  模型名原样交给上游。

> ⚠️ 本项目是**非官方**网关，仅限本人授权账号、本机 / 私有环境自用。
> 使用者需自行遵守上游服务条款。详见[安全与合规](#安全与合规)。

协议细节见 **[docs/PROTOCOL.md](docs/PROTOCOL.md)**。

## 核心能力

| 能力 | 说明 |
|---|---|
| 🔑 **密码自动登录** | 存手机号 + 密码，网关自己走 RSA + HMAC-SHA1 签名登录，14 天登录态到期自动重登 |
| 📅 **模拟签到领积分** | 复刻官方客户端的 `points/first-login`（幂等领每日 5000）：面板单号/全量签到、登录成功自动签到、定时刷新顺带跨日续领 |
| 🎁 **新手任务直领** | 上游 8 个新手任务合计 **10000 积分**，服务端不校验前置行为，网关纯 API 直领、零 token 消耗 |
| 🚫 **失效模型台账** | 上游对已下架模型回 404（与账号无关）→ 记成模型级台账、从目录隐藏、请求直接 404，**不罚账号**，30 天 TTL 自愈 + 面板可手动解除 |
| 🔒 **严格模型校验** | 上游对认不出的模型名不报错，而是静默用默认模型执行（`no-such-model-xyz` → 实际跑 `DeepSeek-V4.1-Flash`）。网关在目录层面直接 404，避免客户端拿到「成功」的错答案 |
| 🆓 **模型限免识别** | 上游没有结构化倍率字段，倍率与「限时免费」都写在展示名里 → 网关解析出限免标记，面板出「限免」标签与排序 |
| 🖼️ **图像模型分道** | `type=image` 的模型只能走 `/v1/images/generations`，网关在对话端点直接 400 指路，不让请求白烧上游、连带把账号打进 404 短冷却 |
| 🔄 **多账号池** | 余额加权 + 成本分层 + 加权随机选号，Top-5 短名单 + 防惊群；每日赠送额度优先花，长期积分垫后 |
| 🛡️ **熔断与冷却** | 429 软冷却指数退避、404 短冷却、额度耗尽硬冷却到次日 04:00、连败熔断、单号在途租约 |
| 🧲 **会话粘性** | 同一会话尽量绑定同一账号，TTL 滚动续期，失败自动解绑 |
| ⚡ **双协议** | OpenAI `chat_completions` + `responses`，Anthropic `messages`（流式、工具调用、**图片输入**） |
| 🧠 **思考链透传** | 上游 `reasoning_content` → Anthropic `thinking` 块 / Responses `reasoning` 项，不静默丢弃 |
| 🕐 **长任务不掐断** | 非流式长请求内部转流式取数再聚合，绕开上游 ~60s 的 504 |
| 📊 **可观测** | 每请求一行表格日志（TTFB / token 速率）、`/status`、`/healthz`、`/metrics`、请求台账落盘 |
| 💾 **状态持久化** | 池状态原子落盘（含额度快照）；用量台账 JSONL 追加，重启不清零 |
| 🖥️ **Web 管理面板** | 内嵌单页（明暗主题），六个视图：账号池（签到/余额/暂停选号/解冻/模型锁池）、用量、积分构成、模型档位、配置、日志；长列表统一分页 |
| 📦 **零依赖** | 全部标准库，`go build` 即可，无 cgo、无 npm、无外部构建步骤 |

## 快速开始

### 方式一：直接跑二进制

```bash
go build -o loo2api ./cmd/server
./loo2api -config config.json     # 首次自动生成 config.json，并在日志里打印随机 api_key
```

打开 `http://127.0.0.1:17890/panel/`，用日志里的 `api_key` 进入面板，
在「账号池 → 添加账号」填手机号与密码，保存即自动登录。

### 方式二：Docker

```bash
docker compose up -d
docker compose logs -f    # 看首启生成的 api_key
```

> 国内机器拉不到 `docker.io` 时，可用仓库里的 `Dockerfile.mirror`：它把基础镜像换成
> 镜像站并把 `# syntax` 行去掉，构建命令 `docker build -f Dockerfile.mirror -t loomy2api-panel:latest .`

### 客户端接入

```bash
# OpenAI 兼容客户端
export OPENAI_BASE_URL=http://127.0.0.1:17890/v1
export OPENAI_API_KEY=sk-loomy-xxxxxxxx

curl $OPENAI_BASE_URL/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"DeepSeek-V4.1-Flash","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

```bash
# Claude Code 等 Anthropic 客户端
export ANTHROPIC_BASE_URL=http://127.0.0.1:17890
export ANTHROPIC_AUTH_TOKEN=sk-loomy-xxxxxxxx

curl $ANTHROPIC_BASE_URL/v1/messages \
  -H "x-api-key: $ANTHROPIC_AUTH_TOKEN" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"DeepSeek-V4.1-Flash","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

```bash
# Codex / Responses API
curl http://127.0.0.1:17890/v1/responses \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"DeepSeek-V4.1-Flash","input":"你好"}'
```

### 带图请求

三条协议都接受图片（data URI 或 http(s) URL），网关按协议翻译成上游的 `image_url`：

```bash
# OpenAI 形态
-d '{"model":"DeepSeek-V4.1-Flash","messages":[{"role":"user","content":[
      {"type":"text","text":"这张图里有什么"},
      {"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}]}]}'

# Anthropic 形态
-d '{"model":"DeepSeek-V4.1-Flash","max_tokens":400,"messages":[{"role":"user","content":[
      {"type":"text","text":"这张图里有什么"},
      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}}]}]}'
```

## 端点一览

| 方法 | 路径 | 协议 | 说明 |
|---|---|---|---|
| POST | `/v1/chat/completions` | OpenAI | 主对话端点（也接受无 `/v1` 前缀） |
| POST | `/v1/responses` | OpenAI Responses | Codex 等客户端 |
| POST | `/v1/messages` | Anthropic | Claude Code 等（也接受 `/messages`） |
| GET | `/v1/models` | OpenAI | 模型目录（上游优先，失败回落内置表）；图像模型额外带 `endpoint` / `supports_chat=false` |
| POST | `/v1/embeddings` | OpenAI | 向量 |
| POST | `/v1/images/generations` | OpenAI | 生图 |
| POST | `/v1/search` | — | 内置联网检索通道 |
| GET | `/v1/points` | — | 全池额度 |
| GET | `/status` | — | 账号池状态（需鉴权） |
| GET | `/healthz` | — | 健康检查（有可用账号 200，否则 503） |
| GET | `/metrics` | Prometheus | 指标文本 |
| GET | `/panel/` | — | Web 管理面板 |
| POST | `/panel/api/accounts/{name}/checkin` | — | 单号签到（幂等） |
| POST | `/panel/api/checkin_all` | — | 全量签到（跳过禁用/登录态失效） |
| POST | `/panel/api/onboarding_all` · `/accounts/{name}/onboarding` | — | 领新手任务（幂等） |
| GET | `/panel/api/dead_models` · `/dead_models/{model}/clear` | — | 失效模型台账与手动解除 |
| POST | `/panel/api/accounts/{name}/remark` | — | 账号备注（落盘 `auths/<账号>.json`） |
| POST | `/panel/api/accounts/{name}/pause` · `/resume` | — | 暂停选号（照常签到/刷额度）与恢复 |
| GET | `/panel/api/packages` | — | 积分构成（缓存 120s，`?force=1` 刷新） |
| GET | `/panel/api/credit_history` | — | 积分流水（跨账号按时间，缓存 60s） |
| POST | `/panel/api/sms/send` · `/panel/api/sms/login` | — | 短信验证码登录 |

鉴权同时接受 `Authorization: Bearer <key>` 与 `x-api-key: <key>`。

## 配置

配置是单个 `config.json`，首次启动自动生成（含随机 `api_key`）。
所有键也可用环境变量覆盖（`LOOMY_` 前缀，如 `LOOMY_LISTEN`、`LOOMY_PANEL_PASSWORD`）。
完整注释版见 **[config.example.json](config.example.json)**。

| 键 | 默认 | 说明 |
|---|---|---|
| `listen` | `:17890` | 监听地址（改动需重启） |
| `api_keys` | `[]` | 网关密钥，空 = 不鉴权（**勿暴露公网**） |
| `auth_dir` | `./auths` | 账号凭证目录（改动需重启） |
| `state_file` | `./data/state.json` | 池状态文件（改动需重启） |
| `upstream` | `https://loomyad.xunfei.cn/api/v1` | 上游模型网关（改动需重启） |
| `default_model` | `DeepSeek-V4.1-Flash` | 未指定模型时用（旧值 `deepseek-v4-flash-0731` 上游已 404） |
| `image_tag_models` | `[]` | 需要把 data URI 图改写成 `<image_base64>` 标记的模型（**默认空 = 透传标准 image_url**，见上文改动 1） |
| `strategy` | `weighted` | `weighted` / `round_robin` / `balance` / `lru` |
| `pick_top_n` | `5` | 加权选号短名单大小 |
| `max_inflight_per_account` | `0` | 单号在途上限（0 = 不限） |
| `balance_floor` | `0` | 积分保底：低于此值停止接单（0 = 关闭） |
| `sticky_ttl_seconds` | `1800` | 会话粘性 TTL（0 = 关闭） |
| `soft_rate_base_seconds` | `600` | 429 软冷却基数 |
| `soft_rate_max_seconds` | `7200` | 软冷却封顶 |
| `breaker_threshold` | `5` | 连败多少次熔断 |
| `keepalive_min_tokens` | `1500` | 非流式长任务阈值（超此值内部转流式） |
| `max_retries` | `2` | 换号重试次数 |
| `proxy` | `""` | 全局出口代理，账号可单独覆盖 |

面板里改配置会区分**热生效**与**需重启**：`listen` / `auth_dir` / `state_file` /
`upstream` / `account_base` / `log_dir` 改动后需重启，其余立即生效。

## 架构

```
cmd/server/           入口：配置 → 账号池 → 上游客户端 → 网关 + 面板 → HTTP
internal/
  account/            账号服务客户端：HMAC-SHA1 签名、RSA 加密、登录
  upstream/           模型网关客户端（含必需头与 camelCase 坑）
  pool/               账号池：选号 / 冷却 / 熔断 / 粘性 / 在途 / 持久化
  bridge/             Anthropic ↔ OpenAI、Responses ↔ OpenAI 翻译（图片保真见 chatcontent.go）
  server/             HTTP 网关：入站解析、换号重试、SSE 转发/聚合
  panel/              内嵌管理面板（go:embed）
  usage/              请求台账（环形缓冲 + JSONL）
  httpauth/           常量时间 Bearer 校验
  config/             配置加载 / 保存 / 脱敏
```

### 选号是怎么算的

```
weight = 余额占比 ×10  +  闲置补偿 ×1  +  成功率 ×3
```

取权重 Top-5 组成短名单，再在名单内做加权随机抽签。
只取最大值会让单号被打爆，纯随机又会让低权重号白拿流量，Top-N + 抽签兼顾两者。

**会话粘性用「最后一条 user 消息」派生会话键**——常见客户端每轮重发完整历史，
若用首条 user 派生，键永远不变，粘性会永久锁死同一账号，多账号池退化成单账号
（参考实现里实测请求分布 779:13）。取最后一条后，多轮对话每轮键都变，
粘性只对「同一条消息的重试」生效。

## 开发

```bash
go build ./...        # 构建
go vet ./...          # 静态检查
go test ./...         # 237 个单测
gofmt -l .            # 格式检查（应无输出）
```

测试覆盖：签名与 RSA 往返、选号分布与排他、冷却退避与封顶、熔断、模型级冷却、
积分保底、粘性 TTL 与容量、错误分类、SSE 解析与聚合、Anthropic/Responses 双向翻译
（含工具调用、思考链与**图片块**）、图像模型在对话端点被本地 400 拦住且不打上游、
未知模型在目录层面 404 且不打上游、账号备注落盘与额度刷新后不丢、404 记模型台账不罚账号、
每日赠送额度优先分档、额度冷却终点为自然日 00:00、新手任务幂等、配置读写与脱敏、
鉴权、面板渲染与安全头、台账落盘与重启恢复。

## 安全与合规

- **凭证存储**：`auths/*.json` 以 0600 权限保存手机号与密码（**明文**）。这是
  「登录态 14 天到期后无人值守自动重登」的必要代价——上游没有 refresh token。
  请确保该目录不在备份 / 同步盘的共享范围内。
- **鉴权**：`api_keys` 为空时 `/v1/*` 与 `/panel/api/*` **完全不鉴权**，仅适合
  `127.0.0.1` 本机或可信内网。放到公网前必须设置 `api_keys`（面板登录与 `/v1/*` 用同一把）。
- **面板**：内嵌页带严格 CSP（`default-src 'none'`、无内联脚本）、`X-Frame-Options: DENY`、
  `Referrer-Policy: no-referrer`；配置视图与账号视图都不回显密钥或完整 session。
- **合规边界**：个人自用的协议互操作实现，**不得用于贩卖、共享他人账号、
  批量薅取免费额度或任何违反上游条款的用途**。

## 免责声明

本项目仅供学习和研究使用。使用者需遵守上游服务条款，自行承担使用风险
（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## 血统与致谢

- 上游仓库：[misakano7545/loomy2api-panel](https://github.com/misakano7545/loomy2api-panel)
  （面板骨架与配置口径与同族的 [workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel) 对齐）。
- 上游服务为讯飞 Loomy（`loomyad.xunfei.cn`），非官方、未获授权，**不是本项目的上游**。
- 本 fork 的改动见上文「本仓相对上游的改动」，以 PR 形式提交给上游。

## License

[MIT](LICENSE)
