package gui

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// Lang is "zh" or "en".
var (
	langOnce sync.Once
	lang     = ""
)

// SetLang forces the interface language ("zh" or "en"); tests use it.
func SetLang(l string) { langOnce.Do(func() {}); lang = l }

// Lang returns the interface language, following the system's.
func Lang() string {
	langOnce.Do(func() { lang = detectLang() })
	return lang
}

func detectLang() string {
	for _, k := range []string{"MYTOKEN_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.ToLower(os.Getenv(k)); v != "" && v != "c" && v != "posix" {
			if strings.HasPrefix(v, "zh") {
				return "zh"
			}
			if k == "MYTOKEN_LANG" {
				return "en"
			}
			break
		}
	}
	if runtime.GOOS == "darwin" {
		// GUI apps get no LANG; ask the user defaults.
		if out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output(); err == nil {
			s := strings.ToLower(string(out))
			if i := strings.IndexAny(s, "\"abcdefghijklmnopqrstuvwxyz"); i >= 0 {
				s = strings.TrimLeft(s[i:], "\"")
				if strings.HasPrefix(s, "zh") {
					return "zh"
				}
			}
		}
	}
	return "en"
}

var strs = map[string][2]string{ // key: {zh, en}
	"overview":         {"概览", "Overview"},
	"sessions":         {"会话", "Sessions"},
	"ranking":          {"排行", "Ranking"},
	"projects":         {"项目", "Projects"},
	"settings":         {"设置", "Settings"},
	"today":            {"今天", "Today"},
	"7d":               {"7 天", "7 days"},
	"30d":              {"30 天", "30 days"},
	"90d":              {"90 天", "90 days"},
	"all":              {"全部", "All"},
	"tokens":           {"Tokens", "Tokens"},
	"cost":             {"费用", "Cost"},
	"requests":         {"请求", "Requests"},
	"cacheHit":         {"缓存命中", "Cache hit"},
	"input":            {"输入", "Input"},
	"output":           {"输出", "Output"},
	"cacheRead":        {"缓存读", "Cache read"},
	"cacheWrite":       {"缓存写", "Cache write"},
	"reasoning":        {"推理", "Reasoning"},
	"trend":            {"每日用量", "Daily usage"},
	"composition":      {"Token 构成", "Token mix"},
	"activity":         {"活跃热力", "Activity"},
	"topModels":        {"常用模型", "Top models"},
	"byHarness":        {"按工具", "By harness"},
	"byProvider":       {"供应商", "Providers"},
	"byModel":          {"模型", "Models"},
	"recent":           {"最近", "Recent"},
	"mostTokens":       {"最多 Token", "Most tokens"},
	"mostCost":         {"最贵", "Costliest"},
	"failed":           {"出错了", "Failed"},
	"mapped":           {"已把 %s 映射为 %s", "Mapped %s to %s"},
	"badMultiplier":    {"倍率要是一个大于 0 的数字", "The multiplier must be a number above 0"},
	"multiplierSet":    {"%s 的倍率已设为 %s×", "%s now bills at %s×"},
	"unpricedTitle":    {"没找到价格的模型", "Models without a price"},
	"unpricedSub":      {"这些名字在价格表里找不到，多半是中转站或配置里自定义的别名。映射到真实模型后会按它计价，并在排行里合并。", "These names aren't in the price list — usually aliases a relay or your config made up. Map each to the real model to price it and merge it in rankings."},
	"allPriced":        {"所有模型都找到价格了 ✓", "Every model has a price ✓"},
	"importCCSwitch":   {"导入 cc-switch 价格", "Import cc-switch prices"},
	"imported":         {"从 cc-switch 导入了 %d 条价格规则", "Imported %d price rules from cc-switch"},
	"aliasesTitle":     {"模型映射", "Model mappings"},
	"remove":           {"移除", "Remove"},
	"multipliersTitle": {"供应商倍率", "Provider multipliers"},
	"multipliersSub":   {"中转站常按官方价的若干倍计费；按回车生效。日志自带的费用不受影响。", "Relays often bill a multiple of list price. Press Return to apply; costs the log reported stay as they are."},
	"customPrices":     {"另有 %d 条模型自定义价格（来自 cc-switch 或命令行）", "Plus %d custom model prices (from cc-switch or the CLI)"},
	"nReq":             {"%s 次请求", "%s requests"},
	"mapTo":            {"映射为", "Map to"},
	"other":            {"其他…", "Other…"},
	"cancel":           {"取消", "Cancel"},
	"searchCatalog":    {"搜索价格表里的模型", "Search the price list"},
	"noMatch":          {"没有匹配的模型", "No matching model"},
	"unpricedBanner":   {"%d 个模型没找到价格，费用可能偏低", "%d models have no price, so costs may be low"},
	"fixIt":            {"去映射 →", "Map them →"},
	"noModelName":      {"（日志里没有模型名）", "(no model name in the log)"},
	"noModelNameSub":   {"这些请求没有记录模型，无法映射；多半是中转站的探活或报错请求。", "These requests never named a model, so there's nothing to map — usually relay health checks or errors."},
	"search":           {"搜索会话、项目、模型…", "Search sessions, projects, models…"},
	"noSessions":       {"还没有会话呢", "No sessions yet"},
	"noSessionsSub":    {"迷子でもいい、前へ進め——先去写点代码吧", "Go write some code, the tokens will follow"},
	"pickSession":      {"选一个会话看看", "Pick a session"},
	"pickSub":          {"左边的每一行都是一次和 AI 的合奏", "Every row on the left is a jam session with an AI"},
	"subagents":        {"子代理", "Subagents"},
	"breakdown":        {"供应商 × 模型", "Provider × model"},
	"timeline":         {"请求时间线", "Request timeline"},
	"started":          {"开始", "Started"},
	"updated":          {"更新", "Updated"},
	"project":          {"项目", "Project"},
	"inferred":         {"推断", "inferred"},
	"viaCCSwitch":      {"cc-switch", "cc-switch"},
	"viaConfig":        {"配置", "config"},
	"viaRule":          {"规则", "rule"},
	"viaLog":           {"日志", "log"},
	"scanning":         {"正在扫描", "Scanning"},
	"upToDate":         {"已是最新", "Up to date"},
	"openMain":         {"打开主窗口", "Open MyToken"},
	"quit":             {"退出", "Quit"},
	"last24h":          {"最近 24 小时", "Last 24 hours"},
	"activeNow":        {"正在进行", "Active now"},
	"nothingToday":     {"今天还很安静", "Quiet today"},
	"nothingTodaySub":  {"一辈子……的 token，从第一句开始", "Every token starts with a first prompt"},
	"launchAtLogin":    {"开机自动启动", "Open at login"},
	"launchAtLoginSub": {"在菜单栏常驻，随时看一眼", "Live in the menu bar"},
	"rebuild":          {"重建索引", "Rebuild index"},
	"rebuildSub":       {"重新扫描全部日志（历史永久保留在本地数据库中）", "Rescan every log (history stays in the local database)"},
	"dataDir":          {"数据目录", "Data folder"},
	"sources":          {"数据来源", "Sources"},
	"sourcesSub":       {"只读本地日志，从不上传；仅价格表会联网更新", "Reads local logs only; only the price list goes online"},
	"about":            {"关于", "About"},
	"sessionsN":        {"%d 个会话", "%d sessions"},
	"requestsN":        {"%s 次请求", "%s requests"},
	"childrenN":        {"%d 个子代理", "%d subagents"},
	"share":            {"占比", "Share"},
	"lessThanMin":      {"刚刚", "just now"},
	"minAgo":           {"%d 分钟前", "%dm ago"},
	"hourAgo":          {"%d 小时前", "%dh ago"},
	"dayAgo":           {"%d 天前", "%dd ago"},
	"untitled":         {"（无标题）", "(untitled)"},
	"vsPrev":           {"较上期", "vs prev."},
	"perDay":           {"日均", "per day"},
	"less":             {"少", "Less"},
	"more":             {"多", "More"},
	"m2":               {"这一页会在下个里程碑完成", "Coming in the next milestone"},
	"busiestDay":       {"最忙的一天", "Busiest day"},
	"streak":           {"连续活跃", "Streak"},
	"days":             {"%d 天", "%d days"},
	"noData":           {"暂无数据", "No data"},
	"noDataSub":        {"这段时间舞台上很安静", "The stage has been quiet for this span"},
	"noResults":        {"什么都没找到", "Nothing found"},
	"noResultsSub":     {"拨片掉了一地，也没找到它——换个关键词试试", "Picks everywhere, but not that one — try another word"},
	"allHarnesses":     {"全部工具", "All harnesses"},
	"heavy":            {"为什么要烧这么多 token！", "Why burn so many tokens?!"},
}

// tr translates key.
func tr(key string) string {
	s, ok := strs[key]
	if !ok {
		return key
	}
	if Lang() == "zh" {
		return s[0]
	}
	return s[1]
}

func trf(key string, args ...any) string { return fmt.Sprintf(tr(key), args...) }

// fmtTokens shortens a token count: 1.23亿 / 4,567万 in Chinese, 1.23B / 45.6M in English.
func fmtTokens(n int64) string {
	f := float64(n)
	if Lang() == "zh" {
		switch {
		case f >= 1e8:
			return trimNum(f/1e8, 2) + "亿"
		case f >= 1e4:
			return trimNum(f/1e4, 1) + "万"
		}
		return fmtInt(n)
	}
	switch {
	case f >= 1e9:
		return trimNum(f/1e9, 2) + "B"
	case f >= 1e6:
		return trimNum(f/1e6, 1) + "M"
	case f >= 1e3:
		return trimNum(f/1e3, 1) + "K"
	}
	return fmtInt(n)
}

func trimNum(f float64, prec int) string {
	if f >= 100 {
		prec = 0
	} else if f >= 10 && prec > 1 {
		prec = 1
	}
	s := fmt.Sprintf("%.*f", prec, f)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// fmtInt groups thousands.
func fmtInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func fmtCost(usd float64) string {
	switch {
	case usd < 0:
		// an overdrawn relay wallet
		return "−" + fmtCost(-usd)
	case usd == 0:
		return "$0"
	case usd < 0.01:
		return "<$0.01"
	case usd < 100:
		return fmt.Sprintf("$%.2f", usd)
	case usd < 10000:
		return "$" + fmtInt(int64(math.Round(usd)))
	}
	return "$" + trimNum(usd/1000, 1) + "K"
}

// fmtCostOf formats a cost that may lack a price: "—" when nothing in it
// was priced, the priced part with a trailing "+" when only some was.
func fmtCostOf(usd float64, requests, unpriced int64) string {
	switch {
	case requests > 0 && unpriced >= requests:
		return "—"
	case unpriced > 0:
		return fmtCost(usd) + "+"
	}
	return fmtCost(usd)
}

func fmtPct(f float64) string {
	if f <= 0 {
		return "0%"
	}
	if f < 0.001 {
		return "<0.1%"
	}
	return trimNum(f*100, 1) + "%"
}

// fmtAgo is a relative time.
func fmtAgo(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return tr("lessThanMin")
	case d < time.Hour:
		return trf("minAgo", int(d.Minutes()))
	case d < 24*time.Hour:
		return trf("hourAgo", int(d.Hours()))
	case d < 30*24*time.Hour:
		return trf("dayAgo", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

func fmtWhen(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// attribLabel is the short label of an attribution source, "" for log.
func attribLabel(a model.AttribSource) string {
	switch a {
	case model.AttribInferred:
		return tr("inferred")
	case model.AttribCCSwitch:
		return tr("viaCCSwitch")
	case model.AttribConfig:
		return tr("viaConfig")
	case model.AttribUserRule:
		return tr("viaRule")
	}
	return ""
}

// shortPath shows a project path with ~ and at most the last two parts.
func shortPath(p string) string {
	if p == "" {
		return "—"
	}
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		p = "~" + p[len(h):]
	}
	return p
}

// baseName is the last element of a path.
func baseName(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	if p == "" {
		return "—"
	}
	return p
}
