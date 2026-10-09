# MyToken!!!!! — SPEC v1

轻量、跨平台、原生 UI 的 AI 编程 harness token 统计工具。粒度：**会话 × 供应商 × 模型**。

## 1. 形态与技术栈

| 项 | 决定 |
|---|---|
| 框架 | `github.com/egoist/mygo` v0.3.x，**纯原生 `ui` 包**（无 WebView），Go 1.27+，无 cgo |
| 形态 | 托盘常驻 + 开机自启 + 托盘弹出面板 + 主窗口 |
| 平台 | macOS 优先打磨；Windows / Linux 可运行；GitHub Release 开源分发 |
| 存储 | `modernc.org/sqlite`，只存数字 + 元数据（会话标题 = 首条用户消息前 60 字符），永久保留，可一键重建 |
| 联网 | 仅拉取价格表（models.dev / LiteLLM，磁盘缓存，可离线） |
| 名称 | 显示名 `MyToken!!!!!`，二进制 / bundle `mytoken`；中英双语跟随系统 |
| CLI | `mytoken stats --json [--since 2026-10-01] [--by session|provider|model|project|day]` |
| 数据目录 | macOS `~/Library/Application Support/MyToken`，Linux `$XDG_DATA_HOME/mytoken`，Windows `%APPDATA%\MyToken`；可用 `MYTOKEN_HOME` 覆盖 |

## 2. 目录结构与归属

```
main.go                     # 入口：无参数→GUI；`stats`→CLI          [lead]
internal/model/             # 共享类型（本文件 §3，冻结）              [lead]
internal/store/             # SQLite schema、写入、查询                [astra]
internal/harness/           # Parser 接口 + 注册表                     [lead 定接口]
internal/harness/claude/    # Claude Code                              [flash]
internal/harness/codex/     # Codex CLI                                [flash]
internal/harness/dsh/       # DeepSeek Harness                         [step]
internal/harness/pi/        # pi coding agent                          [step]
internal/harness/{gemini,opencode,crush,cline}/   # M2                 [flash/step]
internal/attrib/            # 供应商归因链                             [astra]
internal/pricing/           # 价格表 + 倍率                            [astra]
internal/scan/              # 扫描调度、fsnotify、增量偏移             [astra]
internal/query/             # UI/CLI 读取 API（§5，冻结）              [astra 实现]
internal/cli/               # stats 子命令                             [astra]
internal/gui/               # 全部原生 UI（避免与 mygo ui 包同名）       [lead]
testdata/<harness>/         # 脱敏后的真实日志 fixture                 [各解析器作者]
```

**规则**：每个子代理只写自己目录；`internal/model` 与 `internal/query` 的接口签名保持兼容，改动需 lead 批准。0.1.x 获准追加费用来源字段（见 §5），不改变已有方法签名或 JSON 字段。

## 3. 核心类型（`internal/model`）

```go
type Harness string // "claude-code","codex","gemini","dsh","opencode","crush","cline","roo","kilo","pi"

type Tokens struct {
    Input, Output, CacheRead, CacheWrite, Reasoning int64
}
func (t Tokens) Total() int64 // Input+Output+CacheRead+CacheWrite+Reasoning（Reasoning 若已含在 Output 中由解析器负责不重复计）

type AttribSource string // "log","cc-switch","config-timeline","inferred","user-rule"

// UsageEvent = 一次模型请求的用量，唯一事实来源。
type UsageEvent struct {
    Harness     Harness
    DedupKey    string    // 必须稳定：优先 harness 的 request/message id；否则 sha1(file+offset)
    SessionID   string    // harness 原生会话 id（子代理用自己的 id）
    ParentID    string    // 子代理/子任务的父会话 id，顶层为空
    ProjectPath string    // cwd / workspace，未知为空
    Timestamp   time.Time // UTC
    Model       string    // 原始模型名
    Provider    string    // 解析器能确定时填写（日志自带），否则空，由 attrib 补
    BaseURL     string    // 日志里能拿到就填
    Tokens      Tokens
    CostUSD     *float64  // 日志自带费用（如有），否则 nil
}

type SessionMeta struct {
    Harness   Harness
    SessionID string
    ParentID  string
    Title     string // 首条用户消息前 60 rune，去换行
    Project   string
    StartedAt, UpdatedAt time.Time
}
```

## 4. 解析器接口（`internal/harness`）

```go
type Source struct {
    Path string // 文件或 sqlite db
    Kind string // "jsonl","json","jsonl.zstd","sqlite"
}

type Cursor struct { // 增量状态，由 store 持久化
    Offset      int64  // 已消费字节
    Size        int64
    ModTime     time.Time
    Fingerprint string // 文件头 4KB sha1，用于检测截断/替换
    Extra       string // 解析器私有（如 sqlite 最大 rowid）
}

type Batch struct {
    Events   []model.UsageEvent
    Sessions []model.SessionMeta
    Next     Cursor
}

type Parser interface {
    Harness() model.Harness
    Roots() []string                          // 需要监听的根目录（已展开 env / home）
    Discover(ctx context.Context) ([]Source, error)
    Parse(ctx context.Context, src Source, cur Cursor) (Batch, error) // 从 cur 增量读到当前末尾
}

func Register(p Parser)
func All() []Parser
```

解析器要求：
- 不读取 / 不返回对话内容，唯一例外是会话标题（≤60 rune）。
- 半行（未写完的 JSONL 尾行）不得消费，`Next.Offset` 停在最后一个完整换行处。
- 指纹变化（截断 / 轮转）→ 从 0 重读，依赖 DedupKey 去重。
- 流式重复记录（同一 message id 多次出现）要取最终值，DedupKey 相同、后写覆盖。
- 每个解析器带 `testdata/` fixture + 表驱动测试，断言 token 合计与会话数。

## 5. 查询 API（`internal/query`，UI 与 CLI 共用）

```go
type Range struct{ From, To time.Time }   // 本地时区，To 不含
type Filter struct {
    Range     Range
    Harnesses []model.Harness // 空 = 全部
    Providers []string
    Models    []string
    Project   string
}

type Totals struct {
    Tokens   model.Tokens
    CostUSD  float64
    CostLogUSD, CostEstimateUSD float64 // 日志金额 / 本地估算
    Requests int64
    Unpriced int64 // 无日志金额且无已知价格的请求，不计入 CostUSD
    Sessions int64
    CacheHit float64 // CacheRead / (Input+CacheRead+CacheWrite)
}

type Point struct { Day time.Time; Tokens model.Tokens; CostUSD float64 }
type Bucket struct {
    Key, Label string
    Tokens model.Tokens
    CostUSD, CostLogUSD, CostEstimateUSD float64
    Requests, Sessions, Unpriced int64
}

type CostSource string // "log", "estimate", "unpriced"；"relay-bill" 保留，尚不产生
type AttributedEvent struct {
    model.UsageEvent
    ResolvedProvider string
    Attrib model.AttribSource
    Cost float64
    Priced bool
    CostSource CostSource
}

type SessionRow struct {
    model.SessionMeta
    Tokens   model.Tokens
    CostUSD  float64
    Requests int64
    Children int            // 子代理数
    Breakdown []ProviderModel
}
type ProviderModel struct {
    Provider string; Model string; Attrib model.AttribSource
    Tokens model.Tokens; CostUSD float64; Requests int64
}

type Service interface {
    Totals(ctx, Filter) (Totals, error)
    Daily(ctx, Filter) ([]Point, error)                // 热力图 / 趋势
    Hourly(ctx, Filter) ([]Point, error)               // 托盘面板“今日”
    ByProvider(ctx, Filter) ([]Bucket, error)
    ByModel(ctx, Filter) ([]Bucket, error)
    ByProject(ctx, Filter) ([]Bucket, error)
    ByHarness(ctx, Filter) ([]Bucket, error)
    Sessions(ctx, Filter, sort string, limit, offset int) ([]SessionRow, int, error)
    Session(ctx, harness model.Harness, id string) (SessionRow, []SessionRow /*children*/, []AttributedEvent, error)
    Subscribe() (<-chan struct{}, func()) // 有新数据时通知（已节流 ≤ 1 次/秒）
}
```

### 5.1 查询语义与 0.1.x 存储

- 所有时间范围为 `[From, To)`；日/小时按当前本地时区切分，显式范围零填充。跨 DST 的一天按日历 `AddDate` 推进，不假定一天总是 24 小时。
- `Sessions` 只列顶层会话；父会话 token、费用、请求数和 Breakdown 递归包含后代。Totals/Bucket 的 Sessions 是匹配事件归属的顶层会话去重数。会话详情仍只返回自身事件，后代另列。
- 模型原名持久化，别名在查询时应用（供应商专用别名优先于全局）；别名/展示设置无需重写事实或预聚合。
- `CostUSD = CostLogUSD + CostEstimateUSD`（允许浮点累计误差）。日志 `log_cost != NULL`（包括 0）是 `log`，本地已知单价×倍率是 `estimate`，其余是 `unpriced`。无价请求仍计 token / 请求数 / 会话数，但费用不混入 0 价已知请求。`relay-bill` 仅预留类型值，不表示已有账单接入。
- 事实存储为 `event_data`：完整原始 dedup_key、稳定 rowid、时间、5 类 token、日志金额、推算费用及 priced。`event_dimensions` 将重复的 harness/session/parent/project/raw_project/model/provider/base_url/resolved_provider/attrib 字符串组成无损字典。`events` 是等价解码视图，保留 INSERT/UPDATE/DELETE 兼容触发器；原 dedup key 不截断、不哈希。
- 索引只保留事实的 timestamp、dimension_id×timestamp、原 dedup 唯一键，以及小型维度/会话索引。旧 model/provider/time、逐事件 raw_project 索引被移除；会话详情通过维度 id 定位事实，避免全库扫描。
- `daily_usage` 是本地日 × harness × session × resolved_provider × **原始** model × project × attrib 的 WITHOUT ROWID 汇总，存 5 类 token、总金额、log/estimate 金额、requests/unpriced。事实表 INSERT/UPDATE/DELETE 触发器在同一事务维护增减，覆盖去重覆盖、归因、重算单价和删除；从日汇总读取完整天，任意范围两端不足一天的部分按时间索引查询事实。
- 日汇总记录 `settings.daily_timezone`（时区名及历史/当前季节偏移指纹）。正常打开数据库时检测系统时区变化并原子重建**仅派生汇总**。查询在读快照内检查时区；CLI `--timezone` 临时展示时区若不同，只读回退到事实 SQL GROUP BY，绝不改写持久汇总。小时查询按时间索引聚合事实，使用 Go 时区规则而非 SQLite 固定 UTC 偏移。
- 旧库在一个事务中无损复制到事实/字典结构，提交后执行一次 VACUUM 回收旧页；取消或失败不会留下半套 schema。首次升级需要额外临时磁盘空间（SQLite 新旧表及 WAL）。连接建立即配置 `busy_timeout=5000`，保留 WAL、单连接快照与单写者。
- “重建索引/全量重扫”只清 cursors、重新解析仍存在的源并 upsert，**不删除 events 或 sessions**；被 Claude Code 等工具轮转清理的历史永久保留。源日志消失不代表用户要求删历史。
- 价格重算按事件 timestamp 选择当时生效的价格快照；日志金额不受本地规则或倍率覆盖。价格 UPDATE 与日汇总费用在同一事务中可见。

## 6. 供应商归因链（`internal/attrib`）

按优先级，首个命中即停，结果记录 `AttribSource`：
1. **log**：解析器填写的 `Provider`。
2. **cc-switch**：只读打开 `~/.cc-switch/cc-switch.db` 的 `proxy_request_logs`（`data_source='proxy'` 或真实 provider_id），按 app_type + model + tokens 精确匹配，时间窗 ±120s；provider_id → `providers.name`。
3. **config-timeline**：常驻监听 `~/.claude/settings.json`（`env.ANTHROPIC_BASE_URL`）、`~/.codex/config.toml`（`model_provider`/`base_url`）等，记录 `(harness, from, base_url)` 时间线；首次运行用 cc-switch `providers.is_current` 作为历史回填。base_url 的 host → 供应商名（内置常见映射，用户可编辑）。
4. **inferred**：模型名推断（claude-* → Anthropic，gpt-*/o* → OpenAI，gemini-* → Google，deepseek-* → DeepSeek …），UI 标“推断”。
5. **user-rule**：用户在设置中定义的规则（harness / model / 时间段 → provider），**覆盖** 1–4 以外的一切自动结果。

## 7. 费用（`internal/pricing`）

- 价格表：models.dev 优先，LiteLLM 兜底；缓存 24h；内置一份离线快照。
- 单价按 Tokens 五类分别计；Reasoning 无单价时按 Output。
- 每个供应商可设倍率或自定义单价；可从 cc-switch `model_pricing` / `providers.cost_multiplier` 导入。
- 日志自带费用优先于计算值。

## 8. UI（lead 负责）

- 主题：Liquid Glass（glass plugin，macOS 26 原生玻璃；其他平台回退半透明模糊）+ MyGO!!!!! 克制彩蛋：五人代表色调色盘（图表系列色）、自绘 SVG 星星/音符/吉他拨片装饰、台词梗空状态文案；不内置任何官方立绘。
- 托盘面板：今日 token / 费用、24h 小时柱图、Top 3 模型、正在进行的会话。
- 主窗口侧边栏：概览（趋势、年度热力图、缓存命中、harness 占比）、会话（虚拟列表 + 详情：供应商×模型拆分、子代理树、请求时间线）、排行（供应商 / 模型）、项目、设置（数据源开关与路径、归因规则、价格与倍率、语言、自启、重建索引）。

## 9. 里程碑

- **M1**：骨架 + store + scan + Claude Code / Codex / DSH / Pi 解析器 + 归因链 + 托盘面板 + 概览 / 会话页；用本机真实数据截图验收。
- **M2**：Gemini / OpenCode / Crush / Cline / Roo / Kilo + 排行 / 项目页 + 定价与倍率设置。
- **M3**：动画打磨、彩蛋、CLI、Win/Linux CI、Release。

## 10. 质量门

- `go vet ./... && go test ./...` 全绿，`-race` 覆盖 scan / store。
- 本机全量首扫 < 10s（以当前 ~/.claude、~/.codex、~/.dsh、~/.pi 为基准），增量更新 < 1s 可见。
- 常驻内存 < 80MB，二进制 < 30MB。
- 0.1.x 查询基准：`go test ./internal/query -run='^$' -bench=BenchmarkService -benchtime=1x -benchmem`，CI 三平台执行。确定性生成 200,000 请求 / 600 会话，覆盖 Totals、Daily、Hourly（24h，含 harness 筛选）、四类排行、Sessions（6 条/全部）及单会话详情。
- 严格本机门槛：`MYTOKEN_STRICT_PERF=1 go test ./internal/query -run TestQueryPerformanceBudget -v -count=1`；每次查询 <100ms、新增 Go 分配 <50MiB。默认测试不在共享 CI 上断言墙钟时间。进程峰值另用编译后的测试二进制测量，避免把 Go 编译器内存算成应用内存。

### 10.1 0.1.x 测量快照（Apple M5，非跨机器保证）

- 206,728 条真实事件的**副本**：库由约 121MB / 587B 每事件降为 52,789,248B / **255.36B 每事件**（含所有表/索引/日汇总，空闲页 0）。迁移 + VACUUM + 本地日构建约 **3.117s**；迁移前后全部 20 个逻辑字段逐行 SHA256 一致。
- GUI 周期切换原 **2.7–8s**，聚合后 today/7d/30d/90d/all 分别约 **73/66/70/81/95ms**，单查询最大 32ms；不依赖 GUI 缓存才能达标。
- 200K 基准：Totals/Daily/By* **2.2–3.3ms**，Hourly24h **14–15ms**，Sessions **3–4ms**，Session **4.2ms**；单方法分配 **0.70–3.22MB**。独立测试进程（含两次 200K 造数、严格测试及基准）峰值 RSS **47,513,600B**，系统 footprint 36.6MB。
- 2,086 个实际源：旧代码空闲增量 459–650ms、全量重建 8.778s；新代码增量 204–209ms、历史保全重扫 9.816s。初版经兼容 view 写入重扫 13.956s，因此正式 CommitMany 改为批量直写事实、批次内字典缓存、跳过完全未变的 upsert；事实触发器仍保证汇总一致。
- 扫描不是查询内存预算：新全量重扫 Go 堆峰值约 **81.8MiB**、RSS **154.1MiB**（旧 Go 堆 77.7MiB / RSS 225MiB）。不要把查询 <50MB 误写成全量解析/扫描进程的承诺。
- 非默认展示时区回退、超长 Hourly 范围、极高维度基数仍按扫描事实/汇总组数量增长；上述性能门槛覆盖默认系统时区与 24h 小时图。字典保留可能不再被引用的少量旧维度，避免昂贵的逐写垃圾回收。
- 真实库探针仅允许 `MYTOKEN_HOME` 位于 `/tmp/mtbench` 副本目录：`MYTOKEN_REAL_PERF=1 go test ./internal/scan -run TestCopiedDatabaseScanPerformance -v -count=1`；旧 schema 子目录可用 `MYTOKEN_MIGRATION_PERF=1 go test ./internal/store -run TestCopiedDatabaseMigrationPerformance -v -count=1` 验证迁移。绝不在正常应用数据目录执行性能重扫。
