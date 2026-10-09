# MyToken!!!!! 路线图

> 状态：已定稿（2026-10-09）。当前版本 0.1.0。
> 本文记录方向、版本划分和做/不做的边界，以及每个决定的理由。实现细节以代码和 [SPEC.md](SPEC.md) 为准。

## 定位

**中转站用户的对账工具。** 核心问题：「我的钱花哪了，和中转站扣的对不对得上」。

开源社区里已有大量 token 统计工具（ccusage、codeburn、tokscale、CodexBar、Claude-Code-Usage-Monitor、token-monitor……），
但截至 2026-10 没有一个会抓中转站倍率并与站点账单对账：CodexBar 只读官方 API 花费，cc-switch 只显示余额、倍率靠手填。
这是 MyToken 的空位。

两根支柱：

1. **对账** —— 自动拉倍率 → 本地按倍率算应扣 → 对照余额 / 扣费日志 → 解释差额。归因可追溯，**每个数字标注来源**。
2. **缓存账本** —— 只用 token 数推导，解释「为什么比预期贵」。

与 cc-switch 的关系：互补。cc-switch 是可选的只读数据源（抽象为 `ProviderSource`）；
MyToken 不做供应商切换、本地路由、MCP、Skills、提示词管理。

## 全局约定

### 隐私

- 默认只发一个网络请求：价格表（models.dev → LiteLLM）。
- 中转站、官方 API 余额、Claude 订阅额度一律**默认关闭、逐站开启**。
- 只用用户已经交给该站点的凭据（harness 配置或 cc-switch 里现成的 `sk-` Key、本机登录态），只访问该站点本身，不经过任何第三方。
- 中转站能力分层、各自可关：L1 倍率 / 价格快照，L2 余额，L3 逐条或按天扣费记录。
- 0.2 发布时，README 的隐私承诺改为：
  「默认只发一个请求（价格表）；中转站对账需逐站开启，只用你已交给该站点的 Key 访问该站点本身」。

### 历史

只增不删。harness 日志被清理（例如 Claude Code 默认 30 天）后，已入库的用量必须保留；全量重扫不得清空历史。

### 数字来源标签

每个金额 / 用量都带来源，界面、CLI、JSON 一致展示，例如：

| 标签 | 含义 |
| --- | --- |
| `relay-bill` | 中转站扣费记录（L3） |
| `log` | harness 日志自带的费用 |
| `local-estimate` | 本地按价格表 × 倍率估算 |
| `inferred` | 供应商由模型名推断 |
| `unpriced` | 找不到价格，不计为 0 |

与已有的归因链标签（`log` / `cc-switch` / `config-timeline` / `inferred` / `user-rule`）并存：前者回答「钱怎么算的」，后者回答「算到谁头上」。

### 前端

全部用 mygo 原生 `ui`（不用 WebView）。可视化组件自研：热力图、桑基图（harness → 中转站 → 模型）、
对账瀑布图、缓存时间轴。设计吸收 codeburn 与 GitHub 动效库的长处；`ui` 缺的能力直接补进 mygo。

### 性能预算（基准数据 20 万条请求）

CI 在三个平台上跑 200K 事件的查询基准（`internal/query/bench_test.go`）；墙钟硬门槛只在设置 `MYTOKEN_STRICT_PERF=1` 时启用，因为 CI 机器的速度波动太大。复现方法见 [SPEC §10](SPEC.md)。

| 指标 | 预算 | 0.1.0 实测（206,615 条 / 627 会话） | 0.1.x 实测（206,728 条） |
| --- | --- | --- | --- |
| 托盘弹窗首帧 | < 50 ms | — | — |
| `stats` / `statusline` | < 100 ms，峰值内存 < 50 MB | `stats` 1.08 s / 173 MB | 单查询 2–15 ms，基准进程 RSS 47.5 MB |
| 概览切换周期 | — | 2.7–8.2 s | 全套 66–95 ms；切换只重查周期相关部分，约 20 ms |
| 无新数据的增量扫描 | < 300 ms | 482 ms（墙钟 2.1 s）/ 115 MB | 204–209 ms |
| 全量重扫 | 只跟踪 | 8.8 s / RSS 225 MB | 9.8 s / RSS 154 MB（保留历史 + 维护汇总表） |
| 托盘常驻空闲 | < 60 MB，CPU ≈ 0 | — | — |
| 数据库体积 | < 300 B / 条，另建按天预聚合表 | ≈ 590 B / 条（121 MB + 12 MB WAL） | 255 B / 条（含汇总表；旧库首次升级约 3 s） |
| 二进制体积 | 只跟踪不卡 | 27 MB | — |

### Harness

覆盖 codeburn 支持的全部工具，外加 WorkBuddy、CodeBuddy（腾讯，Claude Code 分支），分三批随版本推进。
每个新 harness 必须带 fixture 测试并通过性能基准。可参照 codeburn（MIT）的解析逻辑用 Go 移植，并在 `THIRD_PARTY_NOTICES` 中注明。

| 批次 | 跟随版本 | 范围 |
| --- | --- | --- |
| 第 1 批 | 0.2 | 能接中转站的 CLI：Grok Build、Hermes、OpenClaw、Claude Desktop、Qwen、Kimi / Kimi Code、Droid、Goose、OpenClaude、Forge 等；WorkBuddy、CodeBuddy |
| 第 2 批 | 0.3 / 0.4 | 其余可离线读取的：Amp、Kiro、Zed、Antigravity、Codebuff、Mux 等 |
| 第 3 批 | 0.5 | 订阅制或需联网的：Cursor、Copilot、Warp、Devin、Vercel AI Gateway（联网的一律逐个开启） |

订阅制工具（Cursor、Copilot、Warp、Devin）没有中转站，只做用量统计，不参与对账。

## 版本

### 0.1.x —— 打地基

- ✅ **修倍率 bug**：[`internal/pricing/rules.go`](../internal/pricing/rules.go) 的 `effective()` 在 `{provider,model}`、`{"",model}`、`{provider,""}` 中取第一条命中并用它的 `Multiplier`，导致模型价格规则（倍率 1）覆盖供应商倍率（例：网关 0.3 × sonnet $3/M，1M 输入算成 $3 而非 $0.9）。改为**价格按模型优先、倍率按供应商优先，分开查**；用户规则同样受影响。
- ✅ **价格按时间存快照**：倍率会随高峰时段、站长调整而变，`PriceRule` 改为带生效时间的快照，历史请求按当时的价格计算。
- **`ProviderSource` 抽象**：cc-switch 归因 / 价格导入改为可选数据源之一，为中转站数据源铺路。
- ✅ **历史保全**：全量重扫不再执行 [`internal/store/store.go`](../internal/store/store.go) 中的 `DELETE FROM events; DELETE FROM sessions; DELETE FROM cursors;`；源日志消失后数据保留。
- ✅ **数字来源标签**写进数据模型（费用拆成 `log` / `estimate` / `unpriced`，`relay-bill` 预留）。
- ✅ **性能**：按天预聚合表、维度字典化、基准测试进 CI（托盘首帧和空闲内存还没测）。
- ✅ **CLI 对齐**：补 `--until`、`--last`、`--timezone`、`--offline`、`--no-cost`，以及 CSV 导出（目前只有 `--since`）。
- **新增 harness 流程标准化**：Parser 接口补 compact 边界、harness 自带账单两类信号；fixture + 基准模板。

### 0.2 —— 对账 v1

- 自动识别站点类型：先试 `/v1/sub2api/billing`，再试 `/api/status`，都不是则手填倍率。
- **new-api**（逐条对账）：
  - `GET /api/pricing` —— `model_ratio`、`completion_ratio`、`cache_ratio`、`create_cache_ratio`、`model_price`、`group_ratio`
  - `GET /api/usage/token/` —— Key 余额 / 额度
  - `GET /api/log/token` —— 该 Key 最近的请求日志（最多 1000 条，需轮询并本地持久化），按 `request_id` / 时间 + token 与本地事件配对
  - 额度换算：`quota / 500000 = USD`
- **sub2api**（按天对账）：
  - `GET /v1/sub2api/billing` —— 分组 / 用户 / 实际倍率，高峰时段与高峰倍率
  - `GET /v1/usage` —— 余额、今日 token（含缓存）、1–90 天日用量
- **官方 API 余额与花费**：OpenRouter、DeepSeek、SiliconFlow、Moonshot、z.ai、MiniMax，与中转站走同一套对账框架，同样逐个开启。
- **对账视图**：应扣 vs 实扣、差额拆解、来源标签。
- harness 第 1 批。WorkBuddy / CodeBuddy 优先复用 claude parser 读 `projects/**/*.jsonl`；WorkBuddy 的 `workbuddy.db` → `session_usage.credit_json`（每请求积分）作为 harness 自带账单。

### 0.3 —— 缓存账本

- 只用 token 数，所有 harness 通用：本轮应命中前缀 ≈ 上一轮 `input + cache_read + cache_write`；未命中 = 应命中 − `cache_read`。
- 未命中归因四类：
  1. **TTL 过期** —— 两轮间隔超过 5 min / 1 h
  2. **压缩后重付** —— 输入骤降或日志里的 compact 标记（Claude Code、Codex）
  3. **换模型**
  4. **原因不明** —— 多为中转站缓存透传或粘性路由问题 → 计入**中转站缓存健康度**
- 输出「因缓存未命中多花了 ¥xx」，按会话 / 中转站汇总。
- 需要 parser 提供 compact 边界信号，`events` 表增加 1–2 列。
- harness 第 2 批。

### 0.4 —— 日常使用

- **带版本号的快照输出**（`--json --once`）：statusline、托盘、MCP 共用同一份，口径一致。
- statusline：会话花费 + 中转站余额 + 缓存命中率。
- 托盘实时刷新，含 tok/s。
- 预算与告警：只通知，不拦截。
- 报表：日 / 周 / 月 / 会话、环比，按项目与 git 分支统计（worktree 折叠到所属仓库）。
- 额度窗口：Codex 直接读日志里的 `rate_limits`（离线）；Claude 需开启后读 Keychain `Claude Code-credentials` 或 `~/.claude/.credentials.json` 并调用 OAuth 用量接口。不经 cc-switch。
- 额度消耗速度、预计耗尽时间、均匀用完参考线。
- harness 第 2 批。

### 0.5 —— 视觉

- 可视化组件库：热力图、桑基图、对账瀑布图、缓存时间轴。
- 全局动效与视觉改版。
- 只读 MCP server（查询用量、花费、对账结果）。
- harness 第 3 批。

### 以后

上下文内容分析（DSH 式折叠经济学）、多币种与充值汇率、模型对比、任务分类、TUI。

## 明确不做

| 功能 | 理由 |
| --- | --- |
| `optimize --apply` / 自动改配置 | MyToken 只读，不改用户的 harness |
| guard 拦截请求 | 同上；预算只做通知 |
| PR / yield（git 产出关联） | 偏离对账定位 |
| 多设备同步 | 本地优先，不引入服务端 |
| VS Code 扩展、桌面小组件 | 维护成本高，托盘 + statusline 已覆盖 |
| 遥测 | 隐私承诺 |
| 排行榜 / 社交 | 需要上传数据 |
| 用 LLM 给会话做摘要 | 需联网且产生费用 |
| 服务状态页 | 与对账无关的额外网络请求 |

## 参考

| 工具 | 借鉴点 |
| --- | --- |
| [ccusage](https://github.com/ccusage/ccusage) | 报表维度、5 小时块、CLI 参数 |
| [codeburn](https://github.com/getagentseal/codeburn) | harness 覆盖、桌面端与菜单栏、项目 / 分支花费 |
| [Claude-Code-Usage-Monitor](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor) | 来源标签、历史保全、额度预测、版本化快照 |
| [CodexBar](https://github.com/steipete/CodexBar) | 官方 API 花费、额度燃尽曲线 |
| [tokscale](https://github.com/junhoyeo/tokscale) | worktree 折叠、贡献热力图 |
| [token-monitor](https://github.com/Javis603/token-monitor) | tok/s、CSV 导出、保留已删会话 |
| [ccstatusline](https://github.com/sirmalloc/ccstatusline) | statusline 组件 |
| [cc-switch](https://github.com/farion1231/cc-switch) | 只读数据源、余额模板 |
