# 中转站对账（0.2）设计与契约

本文是 0.2 的实现契约：各包之间的类型、表结构、流程和隐私约束。路线图见 [ROADMAP.md](ROADMAP.md) §0.2，已有类型见 [SPEC.md](SPEC.md)。
代码改契约时同步改本文。

## 1. 目标与边界

回答一个问题：**中转站从我这里扣的钱，和我本地记录的用量对得上吗？差在哪？**

- 三层数据，逐站、逐层开启，默认全部关闭：
  - **L1 倍率**：站点的模型倍率、分组倍率、补全倍率、缓存倍率，换算成按时间生效的价格规则。
  - **L2 余额**：剩余额度、已用额度，按时间存快照。
  - **L3 账单**：new-api 的逐条消费日志；sub2api 的按天、按模型用量。
- 对账单位是 **站点 × Key**：`(origin, keyID)`。同一个站点上的两个 Key 是两个账户，分别对账。
- 不做的事：不切换供应商、不代理请求、不改中转站上的任何设置，只发 GET 请求。

## 2. 隐私与网络约束（硬性）

1. 默认不发任何中转站请求。连「识别站点类型」也是网络请求，只有用户对某个站点点「开启」或运行 `mytoken relay enable` 时才会发。
2. 只用用户已经交给该站点的 Key：从 cc-switch 的供应商配置、`~/.claude/settings.json`、`~/.codex/auth.json` / `config.toml` 等处**按需读取**。Key 只在内存里，用完即弃：
   - 不写数据库、不写日志、不进错误信息、不出现在 `--json` 输出里；
   - `source.Secret` 的 `String` / `GoString` / `MarshalJSON` / `Format` 一律输出 `[redacted]`；
   - 只持久化 `keyID`：`hex(sha256("mytoken-key:" + key))[:12]`。
3. 只访问该站点自己的 origin（scheme + host + port）：
   - 跟随重定向时，目标 origin 必须与原 origin 相同，否则报错；
   - 不访问任何第三方，不经过 MyToken 自己的服务器（没有服务器）。
4. 请求统一走 `relay.HTTP`：超时 15 s，响应体最多 8 MiB，`User-Agent: MyToken/<version>`，`Authorization: Bearer <key>`。错误信息里只出现 origin 和路径，不出现查询串里的值。
5. 本地局域网地址和 `127.0.0.1` 当作普通站点处理（例如自建的 new-api）。但 cc-switch 的本地代理 `127.0.0.1:15721` 不是站点，要通过 cc-switch 归因到真正的供应商。

## 3. 站点识别

候选站点**离线**列出：把 `source.Providers` 返回的所有 `BaseURL` 归一化成 origin，去掉 cc-switch 本地代理地址，再按 `(origin, keyID)` 去重。

用户开启某个站点时，`relay.Detect(ctx, origin, cred)` 依次尝试下面几步，命中即停：

| 顺序 | 请求 | 判定 |
|---|---|---|
| 1 | 按 host 匹配官方站点：`openrouter.ai`、`api.deepseek.com`、`api.siliconflow.cn` / `.com`、`api.moonshot.cn` / `.ai`、`open.bigmodel.cn` / `api.z.ai`、`api.minimax.chat` / `api.minimaxi.com` | 对应官方类型，只支持 L2 |
| 2 | `GET /v1/sub2api/billing`（带 Key） | 返回 200 且 JSON 中有 `effective_rate_multiplier` → `sub2api` |
| 3 | `GET /api/status`（不带 Key） | 响应头有 `x-new-api-version`，或 JSON 中有 `data.quota_per_unit` → `newapi`（顺带记下版本、`quota_per_unit`、`quota_display_type`） |
| 4 | 都不命中 | `unknown`：只能手动设置倍率（0.1.x 已支持） |

实测：本机用户的中转站里，4 个是 sub2api（不带 Key 访问 billing 返回 401），4 个是 new-api（rc.25–rc.40）。

## 4. 各类站点的接口

### 4.1 new-api（逐条对账）

| 层 | 接口 | 说明 |
|---|---|---|
| L1 | `GET /api/pricing` | 站长开启 `requireAuth` 后，必须用网页登录的 access token 才能访问，sk- Key 会返回 401。拿不到时，从 L3 日志里每条记录的 `other` 字段反推倍率（见下）。 |
| L2 | `GET /api/usage/token/` | 当前 Key 的额度：总额、已用、剩余、是否无限 |
| L3 | `GET /api/log/token` | 当前 Key 最近的日志，按 id 倒序，**最多 1000 条**（`MaxRecentItems`）。必须定期拉取并保存，否则会被挤掉。 |

日志字段（new-api `model/log.go`）：`id, created_at, type`（2 = 消费，6 = 退款），以及 `model_name, quota, prompt_tokens, completion_tokens, use_time, is_stream, group, request_id, upstream_request_id, other`。

`other` 里的倍率字段（`service/log_info_generate.go`）：`model_ratio, group_ratio, completion_ratio, cache_tokens, cache_ratio, cache_creation_tokens, cache_creation_ratio(_5m/_1h), model_price, user_group_ratio`。

计费公式（`service/text_quota.go` `calculateTextQuotaSummary`）：

- `quota = (base + cache_tokens×cache_ratio + cache_creation_tokens×cache_creation_ratio + completion×completion_ratio) × model_ratio × group_ratio`；
- `base` 是扣掉缓存后的输入 token；
- 按次计价的模型（`model_price > 0`）：`quota = model_price × group_ratio × quota_per_unit`；
- 金额：`USD = quota / quota_per_unit`，`quota_per_unit` 默认 500000。

换算成单价：`model_ratio = 1` 对应 $2 / 1M 输入 token。因此
- 输入单价 = `2 × model_ratio × group_ratio` $/M；
- 输出单价 = 输入单价 × `completion_ratio`；
- 缓存读单价 = 输入单价 × `cache_ratio`；
- 缓存写单价 = 输入单价 × `cache_creation_ratio`。

**token 口径要归一化**：不同上游下，`prompt_tokens` 有时已经扣掉缓存、有时没扣。适配器必须按 `other` 里的 `cache_tokens` / `cache_creation_tokens` 换算成 MyToken 的口径：`Input` 不含缓存。实现时对照 `/tmp/na` 源码，并用测试覆盖。

### 4.2 sub2api（按天、按模型对账）

| 层 | 接口 | 说明 |
|---|---|---|
| L1 | `GET /v1/sub2api/billing` | 分组倍率、用户倍率、实际倍率 `effective_rate_multiplier`，以及高峰时段 `peak_start` / `peak_end` / `peak_rate_multiplier` |
| L2 + L3 | `GET /v1/usage?days=N&timezone=<IANA>` | 模式（`quota_limited` / `unrestricted`）；余额或额度；`usage.today` / `usage.total`；**每日用量**；`model_stats`（各模型的请求数、各类 token、`cost` 按原价、`actual_cost` 按倍率后）。可加 `start_date` / `end_date` 参数 |

sub2api 不提供逐条日志，所以只做按天、按模型的对账：

- 本地估算费用：本地事件按 `(本地日, 模型)` 汇总，用目录价算；
- 站点原价：`cost`；
- 站点实扣：`actual_cost`；
- 实际倍率 = `actual_cost / cost`，可以和 billing 接口给的倍率互相印证。

时区：请求时传用户本地的 IANA 时区，保证「天」的划分和本地一致。

### 4.3 官方 API（只有 L2 余额）

| 站点 | 接口 |
|---|---|
| OpenRouter | `GET /api/v1/key`（当前 Key 的额度上限和已用） |
| DeepSeek | `GET /user/balance` |
| SiliconFlow | `GET /v1/user/info` |
| Moonshot | `GET /v1/users/me/balance` |
| z.ai / 智谱 | 以官方文档为准 |
| MiniMax | 以官方文档为准 |

实现前对照 cc-switch 的 `services/balance.rs`（`/tmp/ccs`）和官方文档逐个确认；拿不准的站点宁可先不做。

## 5. 包与类型

### 5.1 `internal/source`：凭据（step-pricing）

```go
// Secret holds a credential in memory only; every formatting path redacts it.
type Secret struct{ v string }
func (s Secret) Reveal() string
func (Secret) String() string            // "[redacted]"
func (Secret) GoString() string          // "[redacted]"
func (Secret) MarshalJSON() ([]byte, error) // "\"[redacted]\""

// Credential is one key a harness or cc-switch already sends to a site.
type Credential struct {
    Origin   string        // https://host[:port], lowercased, no path
    KeyID    string        // KeyID(secret)
    Provider string        // provider name as attribution sees it ("XLAB")
    Harness  model.Harness // "" when the source is app-wide
    Source   string        // "cc-switch", "harness-config"
    Secret   Secret
}

// Credentials is an optional Source capability. Implementations read keys on
// each call and must not cache them beyond the call.
type Credentials interface{ Credentials(ctx context.Context) ([]Credential, error) }

func KeyID(key string) string
func Origin(rawURL string) (string, bool) // normalize; false for unusable URLs
```

`ProviderInfo` 增加 `Origin`、`KeyID` 两个字段（不含 Key 本身）。

### 5.2 `internal/relay`：识别与适配器（step-pricing）

```go
type Kind string // "newapi" "sub2api" "openrouter" "deepseek" "siliconflow" "moonshot" "zai" "minimax" "unknown"

type Layer uint8
const (
    LayerRatio   Layer = 1 << iota // L1
    LayerBalance                   // L2
    LayerBills                     // L3
)

type Site struct {
    Origin, KeyID string
    Kind          Kind
    Version       string    // e.g. new-api "v1.0.0-rc.39"
    QuotaPerUnit  float64   // new-api only; 500000 by default
    Layers        Layer     // enabled layers; 0 = off
    DetectedAt    time.Time
    Providers     []string  // provider names mapped to this (origin,key)
}

type Ratios struct {
    Model, Completion, Cache, CacheCreate, Group float64
    FixedPrice float64 // per-call model_price; 0 when billed by tokens
}

type Balance struct {
    Origin, KeyID string
    At            time.Time
    RemainingUSD  *float64
    UsedUSD       *float64
    Unlimited     bool
}

// Bill is one per-request charge (new-api).
type Bill struct {
    Origin, KeyID      string
    ID                 int64     // relay log id; unique per origin
    At                 time.Time
    Type               string    // "consume" | "refund"
    Model, Group       string
    RequestID          string
    UpstreamRequestID  string
    Tokens             model.Tokens // normalized: Input excludes cache
    ChargedUSD         float64
    Ratios             Ratios
    Stream             bool
    LatencyMS          int64
}

// Daily is one day × model of usage as the relay reports it (sub2api).
type Daily struct {
    Origin, KeyID string
    Day           string // YYYY-MM-DD in the requested time zone
    Model         string // "" = whole day
    Requests      int64
    Tokens        model.Tokens
    ListUSD       float64 // cost before multipliers
    ChargedUSD    float64 // actual_cost
}

type Snapshot struct {
    Ratios   map[string]Ratios // by model, when L1 is available
    Multiplier *float64        // sub2api effective multiplier
    Peak     *Peak             // sub2api peak window, nil when none
    At       time.Time
}

type Client interface {
    Kind() Kind
    Balance(ctx context.Context) (Balance, error)                       // L2
    Snapshot(ctx context.Context) (Snapshot, error)                     // L1; ErrUnsupported allowed
    Bills(ctx context.Context, afterID int64) ([]Bill, error)           // L3 per-request; ErrUnsupported allowed
    Daily(ctx context.Context, from, to time.Time, loc *time.Location) ([]Daily, error) // L3 daily; ErrUnsupported allowed
}

func Detect(ctx context.Context, h *HTTP, origin string, cred source.Credential) (Site, error)
func New(h *HTTP, site Site, cred source.Credential) (Client, error)
var ErrUnsupported = errors.New("relay: not supported by this site")
```

另外提供 `RulesFromBills` / `RulesFromSnapshot`，把倍率换算成 `pricing.Rule`：
- `Source: "relay:" + origin`；
- `From`：观察时间；
- `Provider`：取 `Site.Providers` 中的每个名字。

### 5.3 `internal/store`：持久化（astra-perf）

新表全部用 `CREATE TABLE IF NOT EXISTS`，挂到现有迁移流程里，老库无损升级：

```sql
relay_sites(origin, key_id, kind, version, quota_per_unit, layers, detected_at, providers_json,
            PRIMARY KEY(origin, key_id))
relay_balances(origin, key_id, at, remaining_usd, used_usd, unlimited,
               PRIMARY KEY(origin, key_id, at))
relay_bills(origin, key_id, id, at, type, model, grp, request_id, upstream_request_id,
            input, output, cache_read, cache_write, charged_usd, ratios_json, stream, latency_ms,
            match_harness, match_dedup_key, match_kind,      -- 对账结果，可重算
            PRIMARY KEY(origin, id)) WITHOUT ROWID
relay_daily(origin, key_id, day, model, requests, input, output, cache_read, cache_write,
            list_usd, charged_usd, fetched_at, PRIMARY KEY(origin, key_id, day, model))
relay_cursors(origin, key_id, last_bill_id, last_sync, last_error, PRIMARY KEY(origin, key_id))
```

以上是逻辑 schema。物理账单表采用无损字典化：`relay_origins(id, origin)`；`relay_bill_dimensions(id, origin_id, key_id, type, model, grp, ratios_json)`；`relay_bill_data(origin_id, id, dimension_id, …)` 以 `(origin_id,id)` 为 WITHOUT ROWID 主键，并有 `(dimension_id,at)` 索引。`relay_bills` 是解码视图，支持写回 `match_*`。五类 token（包括 reasoning）和两种原始 request ID 全量保留，不散列替代，不保存 Key。

- 账单只增不删。重复拉取按主键 upsert；完全相同的账单不清匹配，字段变化才使其重新进入待匹配集合。
- 全量重扫事件时不动 relay 表。关闭站点仅清 layers，历史账单、余额、游标仍保留。
- 体积预算以包含字典、索引、两条 36 字符 ID、匹配结果的 10K 固定样本计：171.21 B/账单，低于 200 B；任意超长 ID/模型和高基数字典不承诺这个上限。
- 迁移以事务创建 relay schema；事件行不改写。`events_time` 原时间单列索引替换为 `(timestamp,output,input,cache_read,cache_write)` 覆盖索引，避免匹配窗口逐行回表；不是新增一份冗余索引。

### 5.4 `internal/reconcile`：对账引擎（astra-perf）

**逐条匹配（new-api）**：对一个 `(origin, keyID)`，候选本地事件是满足下面任一条件的事件：
- `base_url` 的 origin 等于站点 origin；
- `resolved_provider` 属于 `Site.Providers`。

候选事件和账单的匹配规则：
1. 只要有一边的 `request_id` / `upstream_request_id` 等于事件的 `request_id`，就直接匹配。
2. 否则用 `(归一化模型名, output, 输入口径)` 做键，时间窗 ±120 s，在三种口径 `input`、`input + cache_read`、`input + cache_read + cache_write` 的所有候选中取最近的一条，一一对应、不重复使用；距离相同优先较前口径，再按稳定事件键排序。所有精确 ID 匹配先于弱匹配保留事件。增量匹配不得占用以前已经分配的事件。
   - 命中第 2、3 种 → 记为 `token-semantics`：站点把缓存当成普通输入计价。
3. 模型名先过别名表，再去掉日期后缀，再忽略大小写比较。

每对匹配算三种钱：
- **本地估算**：事件现有的 `cost`（目录价 × 本地规则）；
- **站点公式价**：用账单的 tokens 和 ratios 按 §4.1 公式算；
- **实扣**：`ChargedUSD`。

```go
type Category string
const (
    Matched        Category = "matched"         // charged ≈ local (±2% or ±$0.0005)
    PriceDiff      Category = "price-diff"      // tokens agree, price differs → implied multiplier
    TokenSemantics Category = "token-semantics" // cache billed as input, etc.
    BillOnly       Category = "bill-only"       // charge with no local event: shared key, other device, untracked harness, failed request billed
    EventOnly      Category = "event-only"      // local event with no charge: outside the log window, free, other key
    Refund         Category = "refund"
)

type Line struct {
    Category   Category
    Count      int64
    LocalUSD   float64 // our estimate
    FormulaUSD float64 // relay formula on relay tokens
    ChargedUSD float64
    Note       string  // e.g. "implied multiplier 0.31"
}

type Report struct {
    Origin, KeyID   string
    From, To        time.Time
    Coverage        time.Time // oldest bill held; before it, event-only is expected
    LocalUSD, ChargedUSD float64
    ImpliedMultiplier *float64 // Σcharged / Σlocal-at-catalog over matched pairs
    Lines           []Line
    ByModel         []ModelLine // same columns grouped by model
    Balance         *relay.Balance // latest
}
func Reconcile(ctx context.Context, st *store.Store, site relay.Site, from, to time.Time) (Report, error)
```

**按天对账（sub2api）**：对每个 `(天, 模型)` 比较本地估算和 `ChargedUSD`，每天的差额拆成两部分：
- 倍率差 = 本地按目录价 ×（站点实际倍率 − 本地倍率）；
- 用量差 = 站点 token 和本地 token 之差 × 单价。

覆盖范围外的天不出现在报表里。

**匹配与报表分离**：
- `ReconcilePending(ctx, st, site)`（由 `Syncer.Sync` 调用）只匹配新拉到的账单和此前没配上的 `bill-only` 账单，结果写入 `relay_bills.match_*`；已经分配出去的事件保持占用，不重新洗牌。
- `Reconcile(ctx, st, site, from, to)` 做一次全量重匹配（新开启站点时后台调用）。
- `StoredReport(ctx, st, site, from, to)`（`Service.Report` 用它）只读：读取持久化的匹配结果，再用事件**当前**的 `cost` 做 SQL 聚合，不写库。价格规则变化只影响金额，不影响「哪条账单配哪个事件」，所以改价后不需要重匹配，报表自动按新价重算 `price-diff`。

### 5.5 定价接入（step-pricing）

- 中转站规则的优先级：用户规则 > 中转站规则 > cc-switch 导入规则 > 目录价。时间上，仍按 `From` 取不晚于事件时间的最新一条。
- L1 打开后，新的倍率快照或从账单反推出的倍率，会生成一组带 `From` 的规则，写进设置。`Fingerprint` 随之变化，触发重算，所以本地估算会自动靠近实扣。
- **反推得到的规则只在倍率真的变了时才新增**（变化超过 1%），避免规则表无限增长。
- 接口：`query.Settings` 新增 `RelayRules(ctx) ([]pricing.Rule, error)` 和 `AppendRelayRules(ctx, origin string, rules []pricing.Rule) error`。后者负责去重（上一条），并把规则装进 Pricer；规则单独存一个设置键 `relay-rules`，不和用户规则混在一起。`Syncer` 只调用这个接口。

### 5.6 同步调度（astra-perf 实现 `reconcile.Syncer`，lead 接入 app）

包依赖方向：`relay` → `source` / `pricing` / `model`；`store` 可以引用 `relay` 的类型；`reconcile` → `store` / `relay`。`relay` 不得引用 `store`。

`Syncer.Sync(ctx, site)`：调用 `relay.Client` 按开启的层拉 L1 / L2 / L3，写库，再把倍率换成规则交给设置服务，最后对新账单做增量匹配（`ReconcilePending`）。
- `Run`：只遍历库里 `Layers != 0` 的站点，没有开启的站点就不发任何请求；按各站 `LastSync` 和下一次到期时间串行同步，启动时不会对所有站点并发请求；`ctx` 取消后立刻返回（`App.Close` 会等它）。
- 后台：每 5 分钟同步一次所有已开启的站点；new-api 日志只有 1000 条的窗口，所以不能更慢。
- 失败：退避到 30 分钟，并把错误记到 `relay_cursors.last_error`，错误信息不含 Key。
- 用 `mytoken relay sync` 立即同步。

## 6. CLI（flash-cli）

```
mytoken relay list [--json]                 # 离线：候选站点、类型、开启的层、最近同步时间、余额
mytoken relay enable <origin|provider> [--layers ratio,balance,bills] [--key-id ID]
                                            # 识别站点并开启；默认开启全部三层
mytoken relay disable <origin|provider> [--key-id ID]
mytoken relay sync [<origin|provider>]
mytoken reconcile [<origin|provider>] [--since ..] [--until ..] [--last ..] [--by category|model|day] [--format table|json|csv]
```

`relay list` 和 `reconcile` 不联网；`enable` 和 `sync` 才联网。`--offline` 下这两个命令直接报错退出。

## 7. GUI（lead）

- 设置页新增「中转站」一节：
  - 列出候选站点、开关、各层勾选；
  - 显示最近一次同步的时间和错误；
  - 开启前用一句话说明这次开启会访问哪个地址、用哪个 Key（只显示 keyID）。
- 新增「对账」页：
  - 每个站点一张卡片：余额、本期实扣、本地估算、差额、实际倍率；
  - 点开后显示分类明细，以及按模型、按天的表格；
  - 每个数字都标出来源：`relay-bill` / `log` / `local-estimate` / `inferred` / `unpriced`。
- 托盘：已开启的站点显示余额。

## 8. 测试要求

- 适配器：每种站点用 `httptest.Server` 回放录好的响应，响应要脱敏。覆盖：
  - 401 / 403 / 404 / 非 JSON / 超大响应体 / 跨 origin 重定向；
  - 识别顺序；
  - new-api token 口径归一化（至少 Claude 风格、OpenAI 风格两种）；
  - 退款；按次计价；
  - sub2api 高峰时段倍率。
- 隐私测试：用一个哨兵 Key（`sk-SENTINEL-...`）跑完识别、同步、对账、CLI `--json`、错误路径，断言它不出现在数据库文件、日志输出、错误串、JSON 输出的任何字节里。
- 对账引擎：随机生成事件和账单（含时间抖动、缓存口径差、漏单、共享 Key），断言分类正确，金额守恒：Σ各分类 = 总额。
- 性能（1 万条账单 × 20 万事件，单站；`MYTOKEN_STRICT_RECONCILE=1` 时强制）：
  - `StoredReport` 只读报表 ≤ 200 ms（实测约 100–120 ms）；
  - 增量匹配 100 条新账单 ≤ 50 ms（实测约 20–30 ms）；
  - 冷启动全量匹配 ≤ 2 s，后台进行（实测约 230 ms）。
- 存储：每条账单 ≤ 200 B（实测 171 B，含字典和索引）。
