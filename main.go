package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// 终端 / 日志输出
// ---------------------------------------------------------------------------

// version 由 release.sh 通过 -ldflags "-X main.version=v..." 注入。
// 直接 go build 的开发构建标为 dev；-version 与 JSON 报告共用此值。
var version = "dev"

// 预缓存的目标是"像真实访客那样把页面打进缓存"，所以默认请求头按浏览器构造：
//   - UA 用常见浏览器串：大量 WAF / Bot 管理（Cloudflare SBFM、云厂商 WAF、
//     WordPress 安全插件）会直接拦截或挑战非浏览器 UA
//   - Accept 必须带：不少 nginx 缓存规则用 $http_accept 判断，
//     不带 text/html 会直接 set $skip_cache 1，预热等于白跑
//   - Accept-Language 一并带上，让 Vary 命中与真实访客一致
//
// 如需声明爬虫身份，用 -useragent 覆盖即可。
const (
	defaultUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	defaultAccept         = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
	defaultAcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8"
)

const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiCyan   = "\033[36m"
	ansiGray   = "\033[90m"
)

// Printer 串行化并发输出，同时写控制台与日志文件。
type Printer struct {
	mu               sync.Mutex
	file             *os.File
	color            bool
	logMaxBytes      int64
	logRetentionDays int
	console          io.Writer
	live             string
}

func newPrinter(logPath string, maxBytes int64, retentionDays int) *Printer {
	p := &Printer{color: isTerminal(), console: os.Stdout, logMaxBytes: maxBytes, logRetentionDays: retentionDays}
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			// 日志不可写不应导致整个任务失败
			fmt.Fprintf(os.Stderr, "警告：无法写入日志文件 %s：%v（仅输出到控制台）\n", logPath, err)
		} else {
			p.file = f
			if err := maintainLog(f, maxBytes, retentionDays, 0, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "警告：日志轮转失败 %s：%v\n", logPath, err)
			}
		}
	}
	return p
}

// maintainLog 每天或达到大小阈值时轮转，仅保留 .1；过期文件按修改时间清理。
// 复制后截断保留 inode，兼容外部重定向到相同路径；失败时不截断原日志。
func maintainLog(f *os.File, maxBytes int64, retentionDays int, incoming int64, now time.Time) error {
	path := f.Name()
	backup := path + ".1"
	retention := time.Duration(retentionDays) * 24 * time.Hour
	if info, err := os.Stat(backup); err == nil {
		if now.Sub(info.ModTime()) >= retention {
			if err := os.Remove(backup); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	if now.Sub(info.ModTime()) >= retention {
		return f.Truncate(0)
	}
	sameDay := info.ModTime().In(now.Location()).Format("2006-01-02") == now.Format("2006-01-02")
	if sameDay && info.Size() < maxBytes && incoming <= maxBytes-info.Size() {
		return nil
	}
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".precache-log-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, copyErr := io.Copy(tmp, source)
	if copyErr == nil {
		copyErr = tmp.Chmod(info.Mode().Perm())
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chtimes(tmp.Name(), info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), backup); err != nil {
		return err
	}
	return f.Truncate(0)
}

func isTerminal() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (p *Printer) Printf(color, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.live != "" {
		fmt.Fprint(p.consoleWriter(), "\r\033[2K")
	}
	if p.color && color != "" {
		fmt.Fprintf(p.consoleWriter(), "%s%s%s\n", color, msg, ansiReset)
	} else {
		fmt.Fprintln(p.consoleWriter(), msg)
	}
	if p.live != "" {
		fmt.Fprint(p.consoleWriter(), p.live)
	}
	p.writeLog(msg)
}

func (p *Printer) consoleWriter() io.Writer {
	if p.console != nil {
		return p.console
	}
	return os.Stdout
}

// writeLog 的调用方持有 mu；文件只接收纯文本，不写入动画和光标控制符。
func (p *Printer) writeLog(msg string) {
	if p.file != nil {
		if p.logMaxBytes > 0 && p.logRetentionDays > 0 {
			if err := maintainLog(p.file, p.logMaxBytes, p.logRetentionDays, int64(len(msg)+1), time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "警告：日志轮转失败 %s：%v\n", p.file.Name(), err)
			}
		}
		_, _ = p.file.WriteString(msg + "\n")
	}
}

func (p *Printer) animate(msg string) {
	if !p.color {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = msg
	fmt.Fprint(p.consoleWriter(), "\r\033[2K", msg)
}

func (p *Printer) clearAnimation() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live != "" {
		fmt.Fprint(p.consoleWriter(), "\r\033[2K")
		p.live = ""
	}
}

func (p *Printer) progress(label, content string) {
	if !p.color {
		p.row(ansiGray, label, "%s", content)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeLog(padRight(label, 12) + "  " + content)
}

// finishProgress 固定终端上的最终进度；文件日志已由里程碑输出负责。
func (p *Printer) finishProgress(label, content string) {
	if !p.color {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.consoleWriter(), "%s%s  %s%s\n", ansiGray, padRight(label, 12), content, ansiReset)
}

func (p *Printer) Info(format string, a ...any)  { p.Printf("", format, a...) }
func (p *Printer) Ok(format string, a ...any)    { p.Printf(ansiGreen, format, a...) }
func (p *Printer) Warn(format string, a ...any)  { p.Printf(ansiYellow, format, a...) }
func (p *Printer) Err(format string, a ...any)   { p.Printf(ansiRed, format, a...) }
func (p *Printer) Note(format string, a ...any)  { p.Printf(ansiCyan, format, a...) }
func (p *Printer) Hint(format string, a ...any)  { p.Printf(ansiBlue, format, a...) }
func (p *Printer) Muted(format string, a ...any) { p.Printf(ansiGray, format, a...) }

// displayWidth 使用终端字符单元计算列宽，中文/全角字符占两列，组合符不占列。
// 不使用制表符，保证重定向到文件后仍能按等宽字体阅读。
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Cf, r):
		case r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
			(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) ||
			(r >= 0xfe10 && r <= 0xfe19) || (r >= 0xfe30 && r <= 0xfe6f) ||
			(r >= 0xff01 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) ||
			(r >= 0x20000 && r <= 0x3fffd)):
			width += 2
		default:
			width++
		}
	}
	return width
}

func padRight(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-displayWidth(s)))
}

// row 的内容统一从第 15 列开始，空标签用于表格续行。
func (p *Printer) row(color, label, format string, args ...any) {
	p.Printf(color, "%s  %s", padRight(label, 12), fmt.Sprintf(format, args...))
}

func (p *Printer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file != nil {
		p.file.Close()
		p.file = nil
	}
}

// ---------------------------------------------------------------------------
// 缓存状态分类
// ---------------------------------------------------------------------------

type Class int

// 注意：零值必须是 ClassSkipped，这样未执行的槽位不会被误统计为命中。
const (
	ClassSkipped Class = iota
	ClassHit
	ClassMiss
	ClassUncacheable
	ClassNoHeader
	ClassNotFound
	ClassHTTPError
	ClassException
	ClassUnclassified // 已成功请求，但未配置 -cacheheader，不做缓存分类
	ClassExpired
	ClassStale
	ClassUpdating
	ClassRevalidated
)

func (c Class) String() string {
	switch c {
	case ClassHit:
		return "HIT"
	case ClassMiss:
		return "MISS"
	case ClassExpired:
		return "EXPIRED"
	case ClassStale:
		return "STALE"
	case ClassUpdating:
		return "UPDATING"
	case ClassRevalidated:
		return "REVALIDATED"
	case ClassUncacheable:
		return "不可缓存"
	case ClassNoHeader:
		return "无缓存头"
	case ClassNotFound:
		return "404"
	case ClassHTTPError:
		return "HTTP错误"
	case ClassException:
		return "请求异常"
	case ClassUnclassified:
		return "未分类"
	default:
		return "跳过"
	}
}

// classifyCache 解析缓存标识头的值。
// 保留缓存状态含义；多层缓存响应按最后一个可识别状态分类，原始头另行统计。
func classifyCache(raw string) Class {
	if strings.TrimSpace(raw) == "" {
		return ClassNoHeader
	}
	class := ClassUncacheable
	for _, token := range strings.FieldsFunc(strings.ToUpper(raw), func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
	}) {
		switch token {
		case "HIT":
			class = ClassHit
		case "MISS":
			class = ClassMiss
		case "EXPIRED":
			class = ClassExpired
		case "STALE":
			class = ClassStale
		case "UPDATING":
			class = ClassUpdating
		case "REVALIDATED":
			class = ClassRevalidated
		case "BYPASS", "DYNAMIC", "PASS", "NONE":
			class = ClassUncacheable
		default:
			if strings.HasPrefix(token, "TCP_") {
				if strings.HasSuffix(token, "_HIT") {
					class = ClassHit
				}
				if strings.HasSuffix(token, "_MISS") {
					class = ClassMiss
				}
			}
		}
	}
	return class
}

func (c Class) needsRecheck() bool {
	return c == ClassMiss || c == ClassExpired || c == ClassStale || c == ClassUpdating || c == ClassRevalidated
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

type Config struct {
	Sitemap       string
	Host          string
	CacheHeader   string
	UserAgent     string
	Concurrency   int
	Timeout       time.Duration
	Delay         time.Duration
	Rate          float64
	Retries       int
	UseHead       bool
	Insecure      bool
	Debug         bool
	Recheck       time.Duration
	LastMod       time.Duration
	JSONPath      string
	LogPath       string
	MaxErrRate    float64
	Limit         int           // 只处理前 N 个页面（灰度），0 表示全部
	MaxFail       int           // 连续失败多少个请求后熔断，0 表示关闭熔断
	SlowRatio     float64       // 平均耗时超过基线的多少倍就自动降速，0 表示关闭
	MaxLatency    time.Duration // 单次响应达到此值就休息；0 关闭慢响应/超时保护
	Cooldown      time.Duration // 慢响应后全局冷却时长
	MaxSlow       int           // 连续慢响应达到此数时中止；超时立即中止
	RestRatio     float64       // 非 HIT 请求完成后按耗时倍数全局休息
	SlowThreshold time.Duration // 慢页面榜单阈值，与超时学习、自适应降速无关
	Exclude       []string      // 排除 URL 包含这些子串的页面（毒页面过滤）
	NoLearn       bool          // 关闭毒页面自动学习
	LearnPath     string        // 毒页面学习文件路径（slow-pages.txt）
}

// ---------------------------------------------------------------------------
// 预缓存主体
// ---------------------------------------------------------------------------

type Page struct {
	RawURL  string
	URL     string
	LastMod time.Time
}

type Result struct {
	Page     Page
	Status   int
	CacheRaw string
	Class    Class
	Err      string
	Attempts int
	Elapsed  time.Duration

	Timeout bool // 等满超时周期仍无响应（毒页面的特征）

	retryAfter time.Duration // 仅用于重试退避
}

type PreCache struct {
	cfg        Config
	client     *http.Client
	out        *Printer
	pacer      *pacer
	scheme     string
	domain     string
	transport  *http.Transport
	startTime  time.Time
	discovered int   // 增量过滤前的有效 URL 数，用于区分空 sitemap 和无需更新
	requests   int64 // 实际 HTTP 请求数，包含重试、重定向和 sitemap

	// 熔断相关：目标站点一旦整体不可用，继续跑只会把源站踩得更死。
	// consecFail 记录"连续"失败数，中间只要成功一次就清零。
	consecFail int64
	tripped    int32              // 是否已触发压力保护
	cancel     context.CancelFunc // 由 Start 注入，用于主动中止

	pressureMu sync.Mutex
	pauseUntil time.Time
	slowStreak int
	stopReason string
}

func NewPreCache(cfg Config, out *Printer) (*PreCache, error) {
	parsed, err := url.Parse(cfg.Sitemap)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("sitemap 地址解析失败：%s", cfg.Sitemap)
	}

	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if math.IsNaN(cfg.Rate) || math.IsInf(cfg.Rate, 0) || cfg.Rate < 0 || cfg.Rate > 1e9 {
		return nil, fmt.Errorf("-rate 必须是 0 到 1000000000 之间的有限数字")
	}
	if cfg.Rate > 0 && float64(time.Second)/cfg.Rate >= float64(math.MaxInt64) {
		return nil, fmt.Errorf("-rate 太小，限速间隔超出可表示范围")
	}
	if math.IsNaN(cfg.SlowRatio) || math.IsInf(cfg.SlowRatio, 0) || cfg.SlowRatio < 0 {
		return nil, fmt.Errorf("-slow-ratio 必须是非负有限数字")
	}
	if math.IsNaN(cfg.MaxErrRate) || math.IsInf(cfg.MaxErrRate, 0) || cfg.MaxErrRate < 0 || cfg.MaxErrRate > 100 {
		return nil, fmt.Errorf("-max-error-rate 必须在 0 到 100 之间")
	}
	if cfg.MaxLatency < 0 || cfg.Cooldown < 0 || cfg.MaxSlow < 0 {
		return nil, fmt.Errorf("-max-latency、-cooldown 和 -max-slow 必须非负")
	}
	if math.IsNaN(cfg.RestRatio) || math.IsInf(cfg.RestRatio, 0) || cfg.RestRatio < 0 || cfg.RestRatio > 100 {
		return nil, fmt.Errorf("-rest-ratio 必须是 0 到 100 之间的有限数字")
	}
	if cfg.SlowThreshold < 0 {
		return nil, fmt.Errorf("-slow-threshold 必须大于 0")
	}
	if cfg.SlowThreshold == 0 {
		cfg.SlowThreshold = 2 * time.Second
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Concurrency > 512 {
		cfg.Concurrency = 512
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Delay < 0 {
		cfg.Delay = 0
	}
	if cfg.Limit < 0 {
		cfg.Limit = 0
	}
	if cfg.MaxFail < 0 {
		cfg.MaxFail = 0
	}

	// 自定义 Transport 不会自动启用 HTTP/2，必须显式打开。
	// MaxIdleConnsPerHost 至少要等于并发数，否则连接池会被反复新建。
	maxIdle := cfg.Concurrency + 8
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdle * 2,
		MaxIdleConnsPerHost:   maxIdle,
		MaxConnsPerHost:       cfg.Concurrency + 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: !cfg.Insecure}, // 修复：-verify 此前完全未生效
	}

	pc := &PreCache{
		cfg: cfg,
		client: &http.Client{
			Transport: tr,
			Timeout:   cfg.Timeout,
		},
		out:       out,
		pacer:     newPacer(cfg.Rate),
		scheme:    parsed.Scheme,
		domain:    parsed.Host,
		transport: tr,
		startTime: time.Now(),
	}
	pc.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 2 {
			return fmt.Errorf("已跟随 1 次重定向，停止")
		}
		// 重定向属于已经发出的尝试，只受 QPS 约束，让在飞请求正常完成。
		// 在这里等待其他请求触发的冷却，会消耗 http.Client.Timeout 并制造假超时。
		pc.pacer.wait(req.Context())
		if err := req.Context().Err(); err != nil {
			return err
		}
		if pc.aborted() {
			return context.Canceled
		}
		atomic.AddInt64(&pc.requests, 1)
		return nil
	}
	return pc, nil
}

// 所有网络请求共用同一限速器；重定向由 CheckRedirect 单独领取配额。
func (pc *PreCache) do(req *http.Request) (*http.Response, error) {
	if err := pc.waitRequest(req.Context()); err != nil {
		return nil, err
	}
	atomic.AddInt64(&pc.requests, 1)
	return pc.client.Do(req)
}

// ---------------------------------------------------------------------------
// sitemap 抓取与解析（支持 sitemapindex 递归、.gz 压缩包）
// ---------------------------------------------------------------------------

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

type sitemapRef struct {
	Loc string `xml:"loc"`
}

type sitemapDoc struct {
	XMLName  xml.Name
	URLs     []sitemapURL `xml:"url"`
	Sitemaps []sitemapRef `xml:"sitemap"`
}

// setBrowserHeaders 按真实浏览器的方式补齐请求头，降低被 WAF 拦截、
// 以及被 nginx 的 $http_accept 规则跳过缓存的概率。
// 不手动设置 Accept-Encoding，交给 Transport 自动加 gzip 并透明解压。
func (pc *PreCache) setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", pc.cfg.UserAgent)
	req.Header.Set("Accept", defaultAccept)
	req.Header.Set("Accept-Language", defaultAcceptLanguage)
}

func (pc *PreCache) getBytes(ctx context.Context, target string) ([]byte, error) {
	// 单个 sitemap 的大小上限，防止恶意/异常的 sitemap 把内存吃爆
	const maxSitemapBytes = 64 << 20

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	pc.setBrowserHeaders(req)

	resp, err := pc.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 按实际魔数解压，兼容 .gz?query 与 Transport 已透明解压的响应。
	buffered := bufio.NewReader(resp.Body)
	var reader io.Reader = buffered
	magic, _ := buffered.Peek(2)
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, gzErr := gzip.NewReader(buffered)
		if gzErr != nil {
			return nil, fmt.Errorf("解压 gzip 失败：%w", gzErr)
		}
		defer gz.Close()
		reader = gz
	}

	data, err := io.ReadAll(io.LimitReader(reader, maxSitemapBytes+1))
	if err != nil {
		return nil, err
	}

	// LimitReader 读满 64MB 基本就是被截断了：XML 会解析失败然后降级到正则
	// 提取，静默漏掉后面的 URL，用户还以为预热完整。显式报错比静默缺页强。
	if int64(len(data)) > maxSitemapBytes {
		return nil, fmt.Errorf("sitemap 超过 %dMB 上限，请拆分站点地图", maxSitemapBytes>>(10*2))
	}

	return data, nil
}

var locPattern = regexp.MustCompile(`<loc>\s*([^<\s]+)\s*</loc>`)

// loadPages 递归展开 sitemap / sitemapindex，返回去重后的页面列表。
func (pc *PreCache) loadPages(ctx context.Context) ([]Page, error) {
	seenSitemap := map[string]bool{}
	seenPage := map[string]bool{}
	var pages []Page

	var walk func(target string, depth int) error
	walk = func(target string, depth int) error {
		if depth > 3 {
			return fmt.Errorf("sitemap 嵌套超过 3 层，已停止：%s", target)
		}
		if seenSitemap[target] {
			return nil
		}
		seenSitemap[target] = true

		if err := ctx.Err(); err != nil {
			return err
		}

		data, err := pc.getBytes(ctx, target)
		if err != nil {
			return fmt.Errorf("拉取 %s 失败：%w", target, err)
		}

		var doc sitemapDoc
		if xmlErr := xml.Unmarshal(data, &doc); xmlErr != nil {
			// 兜底：用正则识别是 sitemapindex 还是 urlset，避免把子 sitemap 当成页面去预热
			text := string(data)
			pc.out.Muted("  XML 解析失败（%v），改用正则提取：%s", xmlErr, target)
			if strings.Contains(text, "<sitemapindex") {
				var childErrors []error
				for _, m := range locPattern.FindAllStringSubmatch(text, -1) {
					if err := walk(m[1], depth+1); err != nil {
						childErrors = append(childErrors, err)
					}
				}
				return errors.Join(childErrors...)
			}
			for _, m := range locPattern.FindAllStringSubmatch(text, -1) {
				pc.appendPage(&pages, seenPage, m[1], time.Time{})
			}
			return nil
		}

		switch doc.XMLName.Local {
		case "sitemapindex":
			var childErrors []error
			pc.debugf("sitemap 索引：%d 个子 sitemap · %s", len(doc.Sitemaps), target)
			for _, s := range doc.Sitemaps {
				if err := walk(strings.TrimSpace(s.Loc), depth+1); err != nil {
					childErrors = append(childErrors, err)
				}
			}
			return errors.Join(childErrors...)
		default: // urlset 或未声明根节点
			for _, u := range doc.URLs {
				pc.appendPage(&pages, seenPage, u.Loc, parseLastMod(u.LastMod))
			}
		}
		return nil
	}

	if err := walk(pc.cfg.Sitemap, 0); err != nil {
		return nil, err
	}

	pc.discovered = len(pages)
	// 增量模式：只保留最近变更的页面
	if pc.cfg.LastMod > 0 {
		cutoff := time.Now().Add(-pc.cfg.LastMod)
		kept := pages[:0]
		unknown := 0
		for _, p := range pages {
			if p.LastMod.IsZero() {
				unknown++
			}
			if p.LastMod.IsZero() || p.LastMod.After(cutoff) {
				kept = append(kept, p)
			}
		}
		pages = kept
		pc.out.Info("增量筛选：%d 个页面保留 %d 个（缺少有效 lastmod：%d）", pc.discovered, len(pages), unknown)
		if unknown > 0 {
			pc.out.Warn("  缺少有效 lastmod 的页面仍会预热；增量可能退化为全量，请检查 sitemap 的修改时间字段")
		}
	}

	return pages, nil
}

func (pc *PreCache) appendPage(pages *[]Page, seen map[string]bool, raw string, lastMod time.Time) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	// 去掉锚点，避免同一页面重复预热
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	if seen[raw] {
		return
	}
	seen[raw] = true
	*pages = append(*pages, Page{RawURL: raw, URL: pc.rewrite(raw), LastMod: lastMod})
}

// rewrite 按 -host 改写请求地址（保留原路径与查询串）。
func (pc *PreCache) rewrite(raw string) string {
	if pc.cfg.Host == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = pc.scheme
	u.Host = pc.cfg.Host
	return u.String()
}

func parseLastMod(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 单次探测（含重试）
// ---------------------------------------------------------------------------

func (pc *PreCache) probe(ctx context.Context, p Page) Result {
	method := http.MethodGet
	if pc.cfg.UseHead {
		method = http.MethodHead
	}

	var last Result
	for attempt := 0; attempt <= pc.cfg.Retries; attempt++ {
		if attempt > 0 {
			sleep(ctx, backoff(attempt, last.retryAfter))
			if ctx.Err() != nil {
				break
			}
		}
		res, retryable := pc.once(ctx, p, method)
		res.Attempts = attempt + 1
		last = res
		if ctx.Err() == nil {
			pc.noteResult(res)
		}
		if pc.aborted() {
			return res
		}
		if !retryable {
			return res
		}
		if ctx.Err() != nil {
			return res
		}
	}
	return last
}

func (pc *PreCache) once(ctx context.Context, p Page, method string) (Result, bool) {
	res := Result{Page: p}

	req, err := http.NewRequestWithContext(ctx, method, p.URL, nil)
	if err != nil {
		res.Class = ClassException
		res.Err = err.Error()
		return res, false
	}
	pc.setBrowserHeaders(req)
	// 关键修复：Go 会忽略 Header 里的 Host，必须设置 req.Host 字段
	if pc.cfg.Host != "" {
		req.Host = pc.domain
	}

	// 限速等待不属于页面响应耗时。
	if pc.waitRequest(ctx) != nil {
		return res, false
	}
	start := time.Now()
	atomic.AddInt64(&pc.requests, 1)
	resp, err := pc.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return res, false
		}
		res.Class = ClassException
		res.Err = simplifyErr(err)
		res.Elapsed = time.Since(start)
		// 客户端超时不代表 PHP 已停止；不重试，并交由压力保护中止后续任务。
		if isTimeoutErr(err) {
			res.Timeout = true
			return res, false
		}
		return res, true // 其他网络错误（连接重置等瞬态错误）仍可重试
	}
	defer resp.Body.Close()

	// 关键修复：必须把 body 读完，连接才能归还连接池被复用
	// 流式读完响应体才能确认预热完成、复用连接；内存不随页面大小增长。
	_, readErr := io.Copy(io.Discard, resp.Body)

	res.Status = resp.StatusCode
	res.Elapsed = time.Since(start)
	if readErr != nil {
		if ctx.Err() != nil {
			res.Status = 0
			res.Class = ClassSkipped
			return res, false
		}
		res.Class = ClassException
		res.Err = simplifyErr(readErr)
		res.Timeout = isTimeoutErr(readErr)
		return res, !res.Timeout
	}
	if pc.cfg.CacheHeader != "" {
		res.CacheRaw = resp.Header.Get(pc.cfg.CacheHeader)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		res.Class = ClassNotFound
	case resp.StatusCode >= 400:
		res.Class = ClassHTTPError
		res.Err = resp.Status
	case pc.cfg.CacheHeader == "":
		res.Class = ClassUnclassified
	default:
		res.Class = classifyCache(res.CacheRaw)
	}

	retryable := resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusRequestTimeout ||
		resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusGatewayTimeout
	if retryable {
		res.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return res, retryable
}

func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := 10 * time.Second
	if attempt > 0 && attempt <= 6 {
		d = (300 * time.Millisecond) << uint(attempt-1)
	}
	// 加抖动，避免重试风暴
	return d + time.Duration(rand.Int63n(int64(d/2+1)))
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		if uint64(secs) > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func simplifyErr(err error) string {
	msg := err.Error()
	// 去掉 "Get \"https://...\": " 这类冗余前缀，便于日志聚合
	if i := strings.LastIndex(msg, "\": "); i > 0 && i < len(msg)-3 {
		msg = msg[i+3:]
	}
	return msg
}

// isTimeoutErr 判断是否为"等满超时时间仍无响应"。
// 区别于连接被拒、连接重置这类可以立刻失败、重试有意义的错误：
// 超时意味着源站已经用完整个超时周期还没处理完，再等一轮大概率还是超时。
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if os.IsTimeout(err) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "Client.Timeout") ||
		strings.Contains(err.Error(), "context deadline exceeded")
}

// healthCheck 在正式预热前打一个请求，确认源站可用。
// 避免"站点已经挂了还往上面压请求"——把故障压得更死，预热结果也是无效的。
// 判定：5xx 或网络异常即中止；403 之类交给后续的 blocked 提示，不拦。
func (pc *PreCache) healthCheck(ctx context.Context) error {
	u := pc.scheme + "://" + pc.domain + "/"
	pc.debugf("健康检查：GET %s", u)
	res, _ := pc.once(ctx, Page{RawURL: u, URL: pc.rewrite(u)}, http.MethodGet)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case res.Status == http.StatusTooManyRequests:
		return fmt.Errorf("健康检查失败：%s 返回 HTTP 429，目标站点要求限流，中止预热", u)
	case res.Status >= 500:
		return fmt.Errorf("健康检查失败：%s 返回 HTTP %d，源站异常，中止预热", u, res.Status)
	case res.Class == ClassException:
		return fmt.Errorf("健康检查失败：%s（%s），源站不可达或响应过慢，中止预热", u, res.Err)
	}
	pc.out.row("", "健康检查", "HTTP %d · %s", res.Status, formatDuration(res.Elapsed))
	return nil
}

// loadLearned 读取毒页面学习文件，每行一个 URL 子串。
func (pc *PreCache) loadLearned() []string {
	if pc.cfg.LearnPath == "" || pc.cfg.NoLearn {
		return nil
	}
	data, err := os.ReadFile(pc.cfg.LearnPath)
	if err != nil {
		return nil
	}
	var pats []string
	for _, line := range strings.Split(string(data), "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
			pats = append(pats, s)
		}
	}
	return pats
}

const slowPageDays = 3

// slowPageHistory 按本机自然日观察同一个 RawURL；Days=0 表示当天已正常响应，
// 后续同一天的偶发超时不能再把它算成持续超时的一天。
type slowPageHistory struct {
	LastDate string `json:"last_date"`
	Days     int    `json:"days"`
}

// saveLearned 只有连续 slowPageDays 天超时才加入排除文件。
// 观察记录独立持久化，跨进程保留，同一天运行多次也只计一天。
func (pc *PreCache) saveLearned(results []Result) {
	pc.saveLearnedAt(results, time.Now())
}

func (pc *PreCache) saveLearnedAt(results []Result, now time.Time) {
	if pc.cfg.LearnPath == "" || pc.cfg.NoLearn {
		return
	}
	historyPath := pc.cfg.LearnPath + ".history.json"
	history := map[string]slowPageHistory{}
	data, err := os.ReadFile(historyPath)
	if err == nil {
		if err := json.Unmarshal(data, &history); err != nil || history == nil {
			pc.out.Warn("无法解析慢页面观察记录 %s，本次不更新学习文件", historyPath)
			return
		}
	} else if !os.IsNotExist(err) {
		pc.out.Warn("无法读取慢页面观察记录 %s：%v", historyPath, err)
		return
	}

	today := now.Format(time.DateOnly)
	yesterday := now.AddDate(0, 0, -1).Format(time.DateOnly)
	for u, h := range history {
		if h.LastDate != today && h.LastDate != yesterday {
			delete(history, u)
		}
	}

	// 同一批结果若有重复 URL，以正常响应为准；取消、未执行和其他网络错误
	// 都不是页面恢复正常的证据，也不能累计超时天数。
	observed := map[string]bool{} // true：超时；false：当天有正常响应
	for _, r := range results {
		if r.Page.RawURL == "" || r.Class == ClassSkipped || (!r.Timeout && (r.Status == 0 || r.Class == ClassException)) {
			continue
		}
		previous, seen := observed[r.Page.RawURL]
		observed[r.Page.RawURL] = r.Timeout && (!seen || previous)
	}
	for u, timeout := range observed {
		h := history[u]
		if !timeout {
			history[u] = slowPageHistory{LastDate: today}
			continue
		}
		if h.LastDate != today {
			if h.LastDate != yesterday {
				h.Days = 0
			}
			h.LastDate = today
			h.Days++
			history[u] = h
		}
	}
	// 没有历史也没有有效观察时，不创建文件。
	if len(history) == 0 && os.IsNotExist(err) {
		return
	}
	if err := writeSlowPageHistory(historyPath, history); err != nil {
		pc.out.Warn("无法保存慢页面观察记录 %s：%v", historyPath, err)
		return
	}

	existing := map[string]bool{}
	data, err = os.ReadFile(pc.cfg.LearnPath)
	if err != nil && !os.IsNotExist(err) {
		pc.out.Warn("无法读取毒页面学习文件 %s：%v", pc.cfg.LearnPath, err)
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		existing[strings.TrimSpace(line)] = true
	}
	var urls []string
	for u, timeout := range observed {
		if !timeout || existing[u] {
			continue
		}
		h := history[u]
		if h.Days >= slowPageDays {
			urls = append(urls, u)
		} else if h.Days > 0 {
			pc.out.Muted("慢页面观察：%s（连续超时 %d/%d 天，暂不排除）", u, h.Days, slowPageDays)
		}
	}
	if len(urls) == 0 {
		return
	}
	sort.Strings(urls)
	// 兼容已有文件末行没有换行的情况，避免拼接成一个无效 URL。
	prefix := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	f, err := os.OpenFile(pc.cfg.LearnPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		pc.out.Warn("无法写入毒页面学习文件 %s：%v", pc.cfg.LearnPath, err)
		return
	}
	_, writeErr := f.WriteString(prefix + strings.Join(urls, "\n") + "\n")
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		pc.out.Warn("无法写入毒页面学习文件 %s：%v", pc.cfg.LearnPath, err)
		return
	}
	pc.out.Muted("已将连续 %d 天超时的 %d 个页面记入 %s，下次运行自动排除", slowPageDays, len(urls), pc.cfg.LearnPath)
}

// 先写临时文件再替换，避免中断写入导致观察记录被截断。
func writeSlowPageHistory(path string, history map[string]slowPageHistory) error {
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".slow-pages-history-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ---------------------------------------------------------------------------
// 并发执行（固定 worker 池 + 全局限速）
// ---------------------------------------------------------------------------

type pacer struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func newPacer(qps float64) *pacer {
	if qps <= 0 {
		return nil
	}
	return &pacer{interval: time.Duration(float64(time.Second) / qps)}
}

// wait 按全局 QPS 上限排队，不占用工作协程的"延迟预算"。
func (p *pacer) wait(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	wait := p.next.Sub(now)
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()

	sleep(ctx, wait)
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// ---------------------------------------------------------------------------
// 自适应降速
// ---------------------------------------------------------------------------

// throttle 同时干两件事：限住"同时有几个请求在飞"，并在源站变慢时把上限调低。
//
// 为什么需要它：熔断只在"整站返回 5xx"时才触发，但更常见的情况是站还能访问、
// 只是变得很慢（PHP-FPM 进程被占满、CPU 打满）。这时既不报错也不熔断，工具
// 会一边把源站拖得越来越慢，一边坚持跑完所有页面。实测 2 核 / 8 个 FPM 子进程
// 的站点，-size=5 就足以让访客访问转圈。
//
// 判定方式：
//   - 前 baselineN 个样本（跳过前 2 个冷启动）的平均耗时作为基线
//   - 最近 windowN 个样本的平均耗时超过基线 ratio 倍时，并发减半，最低到 1
//   - 只降不升：预热期间源站压力只会越来越大，恢复没有意义
type throttle struct {
	mu     sync.Mutex
	limit  int           // 当前并发上限
	active int           // 当前在飞请求数
	notify chan struct{} // 释放名额时广播，唤醒等待者重新判断

	floor int
	ratio float64
	out   *Printer

	baseN    int
	baseCnt  int
	baseSum  time.Duration
	baseline time.Duration

	win    []time.Duration
	winN   int
	winSum time.Duration

	reduced int // 已降速次数
}

func newThrottle(limit int, ratio float64, out *Printer) *throttle {
	if limit < 1 {
		limit = 1
	}
	return &throttle{
		limit:  limit,
		notify: make(chan struct{}),
		floor:  1,
		ratio:  ratio,
		out:    out,
		baseN:  10,
		winN:   20,
	}
}

// acquire 拿到一个并发名额；ctx 取消时返回 false。
func (t *throttle) acquire(ctx context.Context) bool {
	for {
		t.mu.Lock()
		if t.active < t.limit {
			t.active++
			t.mu.Unlock()
			return true
		}
		wait := t.notify
		t.mu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-wait:
		}
	}
}

func (t *throttle) release() {
	t.mu.Lock()
	t.active--
	// 有等待者时才广播，避免每请求都 close 一个 channel
	if t.active < t.limit {
		close(t.notify)
		t.notify = make(chan struct{})
	}
	t.mu.Unlock()
}

// observe 记录一次请求耗时，必要时降速。
func (t *throttle) observe(d time.Duration) {
	if t.ratio <= 0 || d <= 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// 建立基线。前两个样本丢掉：首请求要建立连接 + TLS + 可能的冷启动，
	// 明显偏慢，拿它当基线会让后面的降速永远不触发。
	if t.baseline == 0 {
		t.baseCnt++
		if t.baseCnt > 2 {
			t.baseSum += d
		}
		if t.baseCnt >= t.baseN {
			n := t.baseN - 2
			t.baseline = t.baseSum / time.Duration(n)
			if t.baseline < 50*time.Millisecond {
				t.baseline = 50 * time.Millisecond
			}
		}
		return
	}

	t.win = append(t.win, d)
	t.winSum += d
	if len(t.win) > t.winN {
		t.winSum -= t.win[0]
		t.win = t.win[1:]
	}
	if len(t.win) < t.winN {
		return
	}

	avg := t.winSum / time.Duration(len(t.win))
	if float64(avg) <= float64(t.baseline)*t.ratio || t.limit <= t.floor {
		return
	}

	old := t.limit
	t.limit /= 2
	if t.limit < t.floor {
		t.limit = t.floor
	}
	t.reduced++
	// 清空窗口：降速后需要重新累积样本，否则会立刻二次触发
	t.win = t.win[:0]
	t.winSum = 0

	t.out.Warn("  源站变慢：最近 %d 个请求平均 %v（基线 %v），并发 %d → %d",
		t.winN, avg.Round(time.Millisecond), t.baseline.Round(time.Millisecond), old, t.limit)
}

func (t *throttle) limitNow() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.limit
}

func (t *throttle) reductions() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reduced
}

// noteResult 维护"连续失败"计数，达到阈值就熔断。
//
// 为什么需要它：单条 URL 的重试只能保证"这一页重试了"，但当整个站点已经
// 过载时，工具会一边收到 503 一边继续把剩下的页面全打一遍——实测 300 个
// 页面在源站返回 503 后仍产生了 823 个请求、多跑 60 秒。熔断是为了在确认
// "站点整体不可用"时立刻停手。
//
// 判定只覆盖"整体不可用"的信号：
//   - 5xx：源站过载
//   - 429：明确要求降速
//   - 网络错误/超时：连不上
//
// 404 不算（sitemap 里有死链是常态），403 不算（可能只是部分页面被 WAF 拦）。
func (pc *PreCache) noteResult(r Result) {
	pc.notePressure(r)
	if pc.cfg.MaxFail <= 0 {
		return
	}

	serverDown := r.Status == http.StatusTooManyRequests ||
		r.Status >= 500 ||
		r.Class == ClassException
	if !serverDown {
		atomic.StoreInt64(&pc.consecFail, 0)
		return
	}

	n := atomic.AddInt64(&pc.consecFail, 1)
	if n < int64(pc.cfg.MaxFail) {
		return
	}
	pc.stopForPressure(fmt.Sprintf("连续 %d 个请求失败（5xx / 429 / 超时）", n))
}

// waitRequest 在新尝试发起前检查全局休息时间，重试和复检也不能绕过。
// QPS 等待完成后再检查：其他在飞请求可能刚刚延长了冷却时间。
func (pc *PreCache) waitRequest(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pc.aborted() {
			return context.Canceled
		}
		pc.pressureMu.Lock()
		remaining := time.Until(pc.pauseUntil)
		pc.pressureMu.Unlock()
		if remaining > 0 {
			sleep(ctx, remaining)
			continue
		}
		pc.pacer.wait(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		pc.pressureMu.Lock()
		ready := !time.Now().Before(pc.pauseUntil)
		pc.pressureMu.Unlock()
		if ready && !pc.aborted() {
			return nil
		}
		// 等待期间有其他请求触发冷却时，重新领取 QPS 配额，避免冷却结束后突发。
	}
}

// notePressure 对每次完成的尝试立即反馈，即使只有一个 worker 或响应为 200。
func (pc *PreCache) notePressure(r Result) {
	if r.Class == ClassSkipped {
		return
	}
	rest := pc.cfg.Delay
	if r.Class != ClassHit && pc.cfg.RestRatio > 0 {
		ns := float64(r.Elapsed) * pc.cfg.RestRatio
		d := time.Duration(math.MaxInt64)
		if ns < float64(math.MaxInt64) {
			d = time.Duration(ns)
		}
		if d > rest {
			rest = d
		}
	}
	slow := pc.cfg.MaxLatency > 0 && (r.Timeout || r.Elapsed >= pc.cfg.MaxLatency)
	if slow && pc.cfg.Cooldown > rest {
		rest = pc.cfg.Cooldown
	}

	pc.pressureMu.Lock()
	if until := time.Now().Add(rest); until.After(pc.pauseUntil) {
		pc.pauseUntil = until
	}
	if slow {
		pc.slowStreak++
	} else {
		pc.slowStreak = 0
	}
	n := pc.slowStreak
	pc.pressureMu.Unlock()

	if !slow {
		return
	}
	if r.Timeout {
		pc.stopForPressure(fmt.Sprintf("请求超时：%s；服务端任务可能仍在执行", r.Page.RawURL))
	} else if pc.cfg.MaxSlow > 0 && n >= pc.cfg.MaxSlow {
		pc.stopForPressure(fmt.Sprintf("连续 %d 个慢响应（阈值 %v），最近页面 %s", n, pc.cfg.MaxLatency, r.Page.RawURL))
	} else {
		pc.out.Warn("  慢响应：%v %s；全局休息 %v（连续 %d 次）", r.Elapsed.Round(time.Millisecond), r.Page.RawURL, rest, n)
	}
}

func (pc *PreCache) stopForPressure(reason string) {
	pc.pressureMu.Lock()
	if pc.aborted() {
		pc.pressureMu.Unlock()
		return
	}
	pc.stopReason = reason
	atomic.StoreInt32(&pc.tripped, 1)
	pc.pressureMu.Unlock()
	pc.out.Err("压力保护：%s，主动中止预热", reason)
	pc.out.Warn("先确认网站和 PHP 请求恢复，再考虑小批量试跑；客户端取消不能保证服务端任务终止")
	if pc.cancel != nil {
		pc.cancel()
	}
}

func (pc *PreCache) pressureReason() string {
	pc.pressureMu.Lock()
	defer pc.pressureMu.Unlock()
	return pc.stopReason
}

func (pc *PreCache) aborted() bool {
	return atomic.LoadInt32(&pc.tripped) == 1
}

// run 用固定数量的 worker 消费任务队列：
// 不会为每个 URL 起一个 goroutine，结果按索引落位，顺序稳定。
func (pc *PreCache) run(ctx context.Context, pages []Page, phase string) []Result {
	results := make([]Result, len(pages))
	if len(pages) == 0 {
		return results
	}

	// 熔断要能打断所有 worker，所以在传入的 ctx 上再套一层可取消的
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	var wg sync.WaitGroup

	workers := pc.cfg.Concurrency
	if workers > len(pages) {
		workers = len(pages)
	}

	pc.out.row("", phase+"开始", "共 %d 页", len(pages))
	progress := newProgressDisplay(pc.out, phase, len(pages), 100*time.Millisecond)
	defer progress.finish()

	// 自适应降速：源站变慢时自动收窄并发，避免把访客挤掉
	th := newThrottle(workers, pc.cfg.SlowRatio, pc.out)
	defer func() {
		if n := th.reductions(); n > 0 {
			pc.out.Warn("  本轮共触发 %d 次自动降速，最终并发 %d（源站响应变慢，建议降低 -size）",
				n, th.limitNow())
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if runCtx.Err() != nil {
					return
				}

				// 闸门包住整个 probe（含重试），保证"在飞请求数"始终受控
				if !th.acquire(runCtx) {
					return
				}
				res := pc.probe(runCtx, pages[idx])
				th.release()

				results[idx] = res
				th.observe(res.Elapsed)

				if res.Class != ClassSkipped {
					progress.advance()
				}
			}
		}()
	}

	for i := range pages {
		select {
		case jobs <- i:
		case <-runCtx.Done():
			close(jobs)
			wg.Wait()
			return results
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

// progressDisplay 在终端持续刷新动画，日志只在开始、每完成 25% 及结束时记录。
// 每轮独立计数；finish 等待输出协程退出，避免进度穿插到结果摘要中。
type progressDisplay struct {
	mu          sync.Mutex
	out         *Printer
	phase       string
	total       int
	done        int
	lastShown   int
	nextPercent int
	started     time.Time
	frame       int
	stop        chan struct{}
	exited      chan struct{}
}

func newProgressDisplay(out *Printer, phase string, total int, interval time.Duration) *progressDisplay {
	p := &progressDisplay{out: out, phase: phase, total: total, nextPercent: 25, started: time.Now(), stop: make(chan struct{}), exited: make(chan struct{})}
	p.print()
	p.animate()
	go func() {
		defer close(p.exited)
		if !out.color {
			<-p.stop
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.mu.Lock()
				p.animate()
				p.mu.Unlock()
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

// 调用方持有 mu，或尚未启动协程。
func (p *progressDisplay) print() {
	p.out.progress(p.phase+"进度", p.content())
	p.lastShown = p.done
}

func (p *progressDisplay) content() string {
	percent := 0
	if p.total > 0 {
		percent = p.done * 100 / p.total
	}
	return fmt.Sprintf("%3d%% · %*d/%d 页 · 已用 %s", percent, len(strconv.Itoa(p.total)), p.done, p.total, time.Since(p.started).Round(time.Second))
}

func (p *progressDisplay) animate() {
	if !p.out.color {
		return
	}
	const barWidth = 16
	filled := 0
	if p.total > 0 {
		filled = p.done * barWidth / p.total
	}
	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	p.out.animate(fmt.Sprintf("%s  %s [%s%s] %s", padRight(p.phase+"进度", 12), spinner[p.frame%len(spinner)], strings.Repeat("=", filled), strings.Repeat("-", barWidth-filled), p.content()))
	p.frame++
}

func (p *progressDisplay) advance() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	percent := p.done * 100 / p.total
	if percent >= p.nextPercent && p.done < p.total {
		p.print()
		// 小任务可能一次跨过多个阈值，只记录实际进度，不重复打印同一页数。
		for p.nextPercent <= percent {
			p.nextPercent += 25
		}
	}
	if p.done == 1 {
		p.animate()
	}
}

func (p *progressDisplay) finish() {
	close(p.stop)
	<-p.exited
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out.clearAnimation()
	if p.done != p.lastShown {
		p.print()
	}
	p.out.finishProgress(p.phase+"进度", p.content())
}

// ---------------------------------------------------------------------------
// 统计与报告
// ---------------------------------------------------------------------------

type Stats struct {
	Total         int
	Hit           int
	Miss          int
	Expired       int
	Stale         int
	Updating      int
	Revalidated   int
	Uncacheable   int
	NoHeader      int
	NotFound      int
	HTTPError     int
	Exception     int
	Unclassified  int
	Skipped       int
	Retried       int
	ServerErrors  int // 5xx、429 和网络异常；用于源站不可用率
	StatusCodes   map[int]int
	CacheStatuses map[string]int
	Failures      []Result
}

func summarize(results []Result) Stats {
	s := Stats{StatusCodes: map[int]int{}, CacheStatuses: map[string]int{}}
	s.Total = len(results)
	for _, r := range results {
		switch r.Class {
		case ClassHit:
			s.Hit++
		case ClassMiss:
			s.Miss++
		case ClassExpired:
			s.Expired++
		case ClassStale:
			s.Stale++
		case ClassUpdating:
			s.Updating++
		case ClassRevalidated:
			s.Revalidated++
		case ClassUncacheable:
			s.Uncacheable++
		case ClassNoHeader:
			s.NoHeader++
		case ClassNotFound:
			s.NotFound++
		case ClassHTTPError:
			s.HTTPError++
		case ClassException:
			s.Exception++
		case ClassUnclassified:
			s.Unclassified++
		default:
			s.Skipped++
		}
		if r.Status > 0 {
			s.StatusCodes[r.Status]++
		}
		if r.CacheRaw != "" {
			s.CacheStatuses[r.CacheRaw]++
		}
		if r.Attempts > 1 {
			s.Retried++
		}
		if r.Class == ClassNotFound || r.Class == ClassHTTPError || r.Class == ClassException {
			s.Failures = append(s.Failures, r)
		}
		if r.Class != ClassSkipped && (r.Status >= 500 || r.Status == http.StatusTooManyRequests || r.Class == ClassException) {
			s.ServerErrors++
		}
	}
	return s
}

func (s Stats) recheckCount() int {
	return s.Miss + s.Expired + s.Stale + s.Updating + s.Revalidated
}

// hitRate 表示本轮直接 HIT 比例；STALE / REVALIDATED 等单独计数，不等同于 MISS。
func (s Stats) hitRate() (float64, int) {
	cacheable := s.Hit + s.recheckCount()
	if cacheable == 0 {
		return 0, 0
	}
	return float64(s.Hit) / float64(cacheable) * 100, cacheable
}

func (s Stats) counts() map[string]int {
	return map[string]int{
		"hit": s.Hit, "miss": s.Miss, "expired": s.Expired, "stale": s.Stale,
		"updating": s.Updating, "revalidated": s.Revalidated, "uncacheable": s.Uncacheable,
		"no_header": s.NoHeader, "not_found": s.NotFound, "http_error": s.HTTPError,
		"exception": s.Exception, "unclassified": s.Unclassified, "skipped": s.Skipped, "retried": s.Retried,
	}
}

// blocked 统计典型的"被拦截"状态码：403 拒绝、429 限流、503 挑战/过载
func (s Stats) blocked() int {
	return s.StatusCodes[http.StatusForbidden] +
		s.StatusCodes[http.StatusTooManyRequests] +
		s.StatusCodes[http.StatusServiceUnavailable]
}

// errRate 只统计已执行页面的服务不可用比例（5xx / 429 / 网络异常）。
// 404 不计入：sitemap 里存在死链是常态，把它算成错误会让 -max-error-rate
// 每天误报警，最后没人再信这个指标。
func (s Stats) errRate() float64 {
	completed := s.Total - s.Skipped
	if completed == 0 {
		return 0
	}
	return float64(s.ServerErrors) / float64(completed) * 100
}

// formatDuration 简化人读耗时，JSON 保留精确秒数。
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// section 保持终端和文件日志使用相同的分组，不依赖终端宽度或光标控制。
func (p *Printer) section(title string) {
	p.Info("")
	p.Note("── %s ──", title)
}

func (pc *PreCache) debugf(format string, args ...any) {
	if pc.cfg.Debug {
		pc.out.row(ansiGray, "[debug]", format, args...)
	}
}

func (pc *PreCache) printOverview() {
	pc.out.Info("")
	pc.out.row(ansiCyan, "开始执行时间", "%s", pc.startTime.Format("2006-01-02 15:04:05"))
	pc.out.row("", "站点地图", "%s", pc.cfg.Sitemap)
	method := "GET"
	if pc.cfg.UseHead {
		method = "HEAD"
	}
	rate := fmt.Sprintf("%.1f req/s", pc.cfg.Rate)
	if pc.cfg.Rate == 0 {
		rate = "不限速"
	}
	pc.out.row("", "请求设置", "%s · 并发 %d · 上限 %s · 超时 %v · 重试 %d 次", method, pc.cfg.Concurrency, rate, pc.cfg.Timeout, pc.cfg.Retries)
	pc.out.row("", "休息策略", "最少 %v · 非 HIT 耗时 × %.1f（取较大值）", pc.cfg.Delay, pc.cfg.RestRatio)
	cache := pc.cfg.CacheHeader
	if cache == "" {
		cache = "未配置"
	}
	recheck := "关闭"
	if pc.cfg.Recheck > 0 {
		recheck = fmt.Sprintf("首轮结束后等待 %v", pc.cfg.Recheck)
	}
	pc.out.row("", "缓存检测", "%s · 复检 %s", cache, recheck)
	if pc.cfg.Host != "" {
		pc.out.row("", "访问主机", "%s · Host %s", pc.cfg.Host, pc.domain)
	}
	if pc.cfg.Rate == 0 {
		pc.out.row(ansiYellow, "注意", "全局限速已关闭")
	}
	if pc.cfg.MaxLatency == 0 {
		pc.out.row(ansiYellow, "注意", "慢响应及首次超时保护已关闭")
	}
	if pc.cfg.MaxFail == 0 {
		pc.out.row(ansiYellow, "注意", "连续失败熔断已关闭")
	}
	pc.debugf("保护：慢响应阈值 %v · 冷却 %v · 连续慢响应 %d 次 · 连续失败 %d 次", pc.cfg.MaxLatency, pc.cfg.Cooldown, pc.cfg.MaxSlow, pc.cfg.MaxFail)
	pc.debugf("自适应降速：基线 × %.1f（0 关闭）；首次超时中止由慢响应保护控制", pc.cfg.SlowRatio)
	pc.debugf("SSL 校验：%v · UA：%s", pc.cfg.Insecure, pc.cfg.UserAgent)
	if !pc.cfg.NoLearn && pc.cfg.LearnPath != "" {
		pc.debugf("自动排除：连续 %d 天超时 · %s", slowPageDays, pc.cfg.LearnPath)
	}
	pc.out.section("准备")
}

func (pc *PreCache) printStats(s Stats, elapsed time.Duration, title string, includeRate bool) {
	pc.out.section(title)
	completed := s.Total - s.Skipped
	speed := 0.0
	if elapsed > 0 {
		speed = float64(completed) / elapsed.Seconds()
	}
	pc.out.row("", "页面统计", "%d/%d 页 · 耗时 %s · %.1f 页/s", completed, s.Total, formatDuration(elapsed), speed)
	if pc.cfg.CacheHeader != "" {
		if hitRate, total := s.hitRate(); total > 0 && includeRate {
			pc.out.row(ansiGreen, "直接命中", "%.1f%% (%d/%d，仅 HIT)", hitRate, s.Hit, total)
		}
		type cacheCount struct {
			label string
			n     int
		}
		items := []cacheCount{{"HIT", s.Hit}}
		for _, item := range []cacheCount{
			{"MISS", s.Miss}, {"EXPIRED", s.Expired}, {"STALE", s.Stale},
			{"UPDATING", s.Updating}, {"REVALIDATED", s.Revalidated},
		} {
			if item.n > 0 {
				items = append(items, item)
			}
		}
		// 每行三个状态；同列的名称左对齐、数量右对齐，续行沿用内容缩进。
		var widths [3]int
		numberWidth := 1
		for i, item := range items {
			widths[i%3] = max(widths[i%3], displayWidth(item.label))
			numberWidth = max(numberWidth, len(strconv.Itoa(item.n)))
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = fmt.Sprintf("%s %*d", padRight(item.label, widths[i%3]), numberWidth, item.n)
		}
		for i := 0; i < len(parts); i += 3 {
			end := i + 3
			if end > len(parts) {
				end = len(parts)
			}
			label := "缓存状态"
			if i > 0 {
				label = ""
			}
			pc.out.row("", label, "%s", strings.Join(parts[i:end], " · "))
		}
		if s.Uncacheable > 0 || s.NoHeader > 0 {
			pc.out.row(ansiYellow, "其他响应", "不可缓存 %d · 无缓存头 %d", s.Uncacheable, s.NoHeader)
		}
		if pc.cfg.UseHead && s.Uncacheable > 0 && s.Hit == 0 && s.recheckCount() == 0 {
			pc.out.row(ansiYellow, "提示", "HEAD 可能无法建立缓存，建议使用 GET")
		}
		if pc.cfg.Debug {
			if s.Stale+s.Updating > 0 {
				pc.debugf("STALE / UPDATING：返回旧缓存，可能正在后台更新")
			}
			if s.Revalidated > 0 {
				pc.debugf("REVALIDATED：缓存经源站验证仍有效")
			}
			var keys []string
			for key := range s.CacheStatuses {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				pc.debugf("原始缓存头 %s：%q × %d", pc.cfg.CacheHeader, key, s.CacheStatuses[key])
			}
		}
	} else {
		pc.out.row(ansiGray, "缓存检测", "未配置缓存头，仅统计 HTTP 状态")
	}
	pc.printStatusCodes(s)
	if s.NotFound+s.HTTPError+s.Exception+s.Skipped > 0 {
		pc.out.row(ansiYellow, "异常统计", "404/410 %d · HTTP 错误 %d · 网络异常 %d · 未执行 %d", s.NotFound, s.HTTPError, s.Exception, s.Skipped)
	}
	if s.Retried > 0 {
		pc.out.row(ansiGray, "重试页面", "%d", s.Retried)
	}
	if blocked := s.blocked(); blocked > 0 && blocked*4 >= s.Total {
		pc.out.row(ansiYellow, "访问受限", "%d 页返回 403/429/503；检查 WAF、限流和源站负载", blocked)
	}
}

// printCompletion 的正常结束只表示任务执行完毕，缓存效果以各轮统计为准。
func (pc *PreCache) printCompletion(code int, err error) {
	pc.out.section("任务结束")
	status := "已完成"
	switch code {
	case 3:
		status = "错误率超限"
	case 4:
		status = "压力保护中止"
	case 130:
		status = "已中断"
	default:
		if code != 0 {
			status = "执行失败"
		}
	}
	line := fmt.Sprintf("%s · %s · 总耗时 %s · 退出码 %d", status, time.Now().Format("15:04:05"), formatDuration(time.Since(pc.startTime)), code)
	if code == 0 {
		pc.out.row(ansiGreen, "执行结果", "%s", line)
	} else {
		pc.out.row(ansiYellow, "执行结果", "%s", line)
	}
	if err != nil {
		pc.out.row(ansiRed, "原因", "%v", err)
	}
}

func (pc *PreCache) printFailures(s Stats) {
	if len(s.Failures) == 0 {
		return
	}
	limit := 5
	if pc.cfg.Debug {
		limit = 20
	}
	pc.out.section("失败明细")
	pc.out.row(ansiYellow, "失败页面", "共 %d 页，最多显示 %d 页", len(s.Failures), limit)
	for i, r := range s.Failures {
		if i >= limit {
			pc.out.row(ansiGray, "", "其余 %d 页可通过 -json 导出完整明细", len(s.Failures)-limit)
			break
		}
		reason := r.Err
		if reason == "" && r.Status > 0 {
			reason = fmt.Sprintf("HTTP %d", r.Status)
		}
		pc.out.row(ansiGray, "["+r.Class.String()+"]", "%s", r.Page.RawURL)
		if reason != "" {
			pc.out.row(ansiGray, "", "%s", reason)
		}
	}
}

// printSlowest 列出最慢的几个页面。
// 这是定位"毒页面"最直接的输出——超时的重页面问题，
// 靠的就是这个信息；没有它，用户只能去翻 access log 猜。
// 只保留固定数量的候选，不复制、排序整批结果。
func (pc *PreCache) slowest(results []Result, limit int) []Result {
	rs := make([]Result, 0, limit)
	for _, r := range results {
		if r.Class == ClassSkipped || (!r.Timeout && r.Elapsed <= pc.cfg.SlowThreshold) {
			continue
		}
		pos := sort.Search(len(rs), func(i int) bool { return rs[i].Elapsed < r.Elapsed })
		if pos >= limit {
			continue
		}
		rs = append(rs, r)
		copy(rs[pos+1:], rs[pos:len(rs)-1])
		rs[pos] = r
		if len(rs) > limit {
			rs = rs[:limit]
		}
	}
	return rs
}

func (pc *PreCache) printSlowest(results []Result) {
	rs := pc.slowest(results, 5)
	if len(rs) == 0 {
		return
	}
	pc.out.section("慢页面")
	pc.out.row("", "慢页面统计", "最慢 %d 页 · 阈值 >%v · 包含所有超时", len(rs), pc.cfg.SlowThreshold)
	for _, r := range rs {
		mark := ""
		if r.Timeout {
			mark = "  ← 超时"
		}
		pc.out.row(ansiYellow, "响应耗时", "%8v · %s%s", r.Elapsed.Round(10*time.Millisecond), r.Page.RawURL, mark)
	}
}

func (pc *PreCache) printStatusCodes(s Stats) {
	if len(s.StatusCodes) == 0 {
		return
	}
	codes := make([]int, 0, len(s.StatusCodes))
	for c := range s.StatusCodes {
		codes = append(codes, c)
	}
	sort.Ints(codes)

	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%d × %d", c, s.StatusCodes[c]))
	}
	pc.out.row("", "HTTP状态", "%s", strings.Join(parts, " · "))
}

// ---------------------------------------------------------------------------
// JSON 报告
// ---------------------------------------------------------------------------

type jsonFailure struct {
	URL      string `json:"url"`
	Class    string `json:"class"`
	Status   int    `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

type jsonSlow struct {
	URL     string  `json:"url"`
	Seconds float64 `json:"seconds"`
	Timeout bool    `json:"timeout,omitempty"`
}

type jsonRecheck struct {
	WaitSeconds   float64        `json:"wait_seconds"`
	Hit           int            `json:"hit"`
	Miss          int            `json:"miss"`
	Expired       int            `json:"expired"`
	Stale         int            `json:"stale"`
	Updating      int            `json:"updating"`
	Revalidated   int            `json:"revalidated"`
	CacheStatuses map[string]int `json:"cache_statuses"`
	NotHit        int            `json:"not_hit"`
	HitRate       float64        `json:"hit_rate"`
	Total         int            `json:"total"`
	HTTPError     int            `json:"http_error"`
	Exception     int            `json:"exception"`
	Skipped       int            `json:"skipped"`
	NotFound      int            `json:"not_found"`
	Uncacheable   int            `json:"uncacheable"`
	NoHeader      int            `json:"no_header"`
	Unclassified  int            `json:"unclassified"`
	ErrorRate     float64        `json:"error_rate"`
	Failures      []jsonFailure  `json:"failures,omitempty"`
}

type jsonReport struct {
	Version          string         `json:"version"`
	StartTime        string         `json:"start_time"`
	EndTime          string         `json:"end_time"`
	DurationSec      float64        `json:"duration_sec"`
	Sitemap          string         `json:"sitemap"`
	Host             string         `json:"host,omitempty"`
	Total            int            `json:"total"`
	Counts           map[string]int `json:"counts"`
	StatusCodes      map[string]int `json:"status_codes"`
	HitRate          float64        `json:"hit_rate"`
	FirstPassHitRate float64        `json:"first_pass_hit_rate"`
	CacheStatuses    map[string]int `json:"cache_statuses"`
	ErrorRate        float64        `json:"error_rate"`
	AvgRPS           float64        `json:"avg_rps"`
	Failures         []jsonFailure  `json:"failures,omitempty"`
	Slowest          []jsonSlow     `json:"slowest,omitempty"`
	Recheck          *jsonRecheck   `json:"recheck,omitempty"`
	Completed        int            `json:"completed"`
	Requests         int64          `json:"requests"`
	ExitCode         int            `json:"exit_code"`
	SlowThresholdSec float64        `json:"slow_threshold_sec"`
	StopReason       string         `json:"stop_reason,omitempty"`
}

func (pc *PreCache) writeJSON(path string, results []Result, s Stats, elapsed time.Duration, rc *jsonRecheck, exitCode int) error {
	hitRate, _ := s.hitRate()
	rep := jsonReport{
		Version:          version,
		StartTime:        pc.startTime.Format(time.RFC3339),
		EndTime:          time.Now().Format(time.RFC3339),
		DurationSec:      elapsed.Seconds(),
		Sitemap:          pc.cfg.Sitemap,
		Host:             pc.cfg.Host,
		Total:            s.Total,
		Counts:           s.counts(),
		StatusCodes:      map[string]int{},
		HitRate:          hitRate,
		FirstPassHitRate: hitRate,
		CacheStatuses:    s.CacheStatuses,
		ErrorRate:        s.errRate(),
		Recheck:          rc,
		Completed:        s.Total - s.Skipped,
		Requests:         atomic.LoadInt64(&pc.requests),
		ExitCode:         exitCode,
		StopReason:       pc.pressureReason(),
		SlowThresholdSec: pc.cfg.SlowThreshold.Seconds(),
	}
	if elapsed > 0 {
		rep.AvgRPS = float64(rep.Requests) / elapsed.Seconds()
	}
	for c, n := range s.StatusCodes {
		rep.StatusCodes[strconv.Itoa(c)] = n
	}
	for _, f := range s.Failures {
		rep.Failures = append(rep.Failures, jsonFailure{
			URL: f.Page.RawURL, Class: f.Class.String(),
			Status: f.Status, Error: f.Err, Attempts: f.Attempts,
		})
	}
	// 慢页面写进报告，方便监控直接盯"源站上哪些页面最重"
	for _, r := range pc.slowest(results, 10) {
		rep.Slowest = append(rep.Slowest, jsonSlow{
			URL:     r.Page.RawURL,
			Seconds: r.Elapsed.Seconds(),
			Timeout: r.Timeout,
		})
	}

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".precache-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

func (pc *PreCache) Start(ctx context.Context) (int, error) {
	// 熔断要能主动打断所有 worker，这里挂一个可取消的 ctx 并交给 PreCache
	ctx, cancel := context.WithCancel(ctx)
	pc.cancel = cancel
	defer cancel()

	pc.printOverview()

	// 手动排除保留子串语义；学习出的完整 URL 使用精确匹配。
	learned := map[string]bool{}
	for _, u := range pc.loadLearned() {
		learned[u] = true
	}
	if len(learned) > 0 {
		pc.out.row(ansiGray, "历史记录", "%d 个历史超时页面（本次自动排除）", len(learned))
	}

	// 预热前健康检查：源站已经故障时直接退出，不往故障上压请求
	if err := pc.healthCheck(ctx); err != nil {
		if ctx.Err() != nil {
			return 130, nil
		}
		return 1, err
	}

	pc.debugf("开始解析站点地图")
	pages, err := pc.loadPages(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 130, nil
		}
		return 1, fmt.Errorf("解析站点地图失败：%w", err)
	}
	if len(pages) == 0 && !(pc.cfg.LastMod > 0 && pc.discovered > 0) {
		return 1, fmt.Errorf("未从 sitemap 中提取到任何页面，请检查 sitemap 是否符合规范或 -lastmod 过滤是否过严")
	}
	pc.out.row("", "页面发现", "%d 页（已去重）", len(pages))
	if len(pages) == 0 {
		pc.out.Ok("增量检查完成：没有最近变更的页面，无需预热")
	}

	// 过滤手动排除和已经确认连续多天超时的页面。
	if len(pc.cfg.Exclude) > 0 || len(learned) > 0 {
		kept := pages[:0]
		excluded := []string{}
		for _, p := range pages {
			drop := learned[p.RawURL]
			for _, pat := range pc.cfg.Exclude {
				if strings.Contains(p.RawURL, pat) {
					drop = true
					break
				}
			}
			if drop {
				if len(excluded) < 10 {
					excluded = append(excluded, p.RawURL)
				}
				continue
			}
			kept = append(kept, p)
		}
		if n := len(pages) - len(kept); n > 0 {
			pages = kept
			pc.out.row(ansiYellow, "排除页面", "共 %d 页（手动规则 / 学习记录），例如：", n)
			for _, u := range excluded {
				pc.out.row(ansiGray, "", "%s", u)
			}
		}
	}

	// 灰度：先只跑前 N 个页面观察源站负载，确认没问题再放开全量
	if pc.cfg.Limit > 0 && len(pages) > pc.cfg.Limit {
		pc.out.row(ansiYellow, "数量限制", "-limit=%d：本次只处理前 %d 个页面，其余 %d 个跳过",
			pc.cfg.Limit, pc.cfg.Limit, len(pages)-pc.cfg.Limit)
		pages = pages[:pc.cfg.Limit]
	}

	pc.out.section("预热")
	results := pc.run(ctx, pages, "预热")
	elapsed := time.Since(pc.startTime)
	stats := summarize(results)

	if pc.aborted() {
		pc.out.Err("已触发熔断，以下为已执行部分的结果")
	} else if ctx.Err() != nil {
		pc.out.Warn("收到中断信号，已提前结束，以下为部分结果")
	}

	pc.printStats(stats, elapsed, "首轮结果", true)
	pc.printFailures(stats)
	pc.printSlowest(results)

	// 预热后复检：自动验证缓存是否真的生效
	var rc *jsonRecheck
	if pc.cfg.Recheck > 0 && stats.recheckCount() > 0 && ctx.Err() == nil && !pc.aborted() {
		pc.out.section("复检")
		pc.out.row("", "复检准备", "%d 页待确认 · 等待 %v", stats.recheckCount(), pc.cfg.Recheck)
		sleep(ctx, pc.cfg.Recheck)

		var recheckPages []Page
		for _, r := range results {
			if r.Class.needsRecheck() {
				recheckPages = append(recheckPages, r.Page)
			}
		}
		reStart := time.Now()
		reResults := pc.run(ctx, recheckPages, "复检")
		reStats := summarize(reResults)
		pc.printStats(reStats, time.Since(reStart), "复检结果", false)
		rate := float64(reStats.Hit) / float64(len(recheckPages)) * 100
		pc.out.row("", "复检命中", "%.1f%% (%d/%d，仅 HIT)", rate, reStats.Hit, len(recheckPages))
		pc.out.row(ansiGray, "复检范围", "仅首轮待确认页面")
		if reStats.Hit < len(recheckPages) {
			pc.out.row(ansiYellow, "待确认", "%d 页未直接 HIT（含错误及未执行），见上方状态",
				len(recheckPages)-reStats.Hit)
		}
		rc = &jsonRecheck{
			WaitSeconds:   pc.cfg.Recheck.Seconds(),
			Hit:           reStats.Hit,
			Miss:          reStats.Miss,
			Expired:       reStats.Expired,
			Stale:         reStats.Stale,
			Updating:      reStats.Updating,
			Revalidated:   reStats.Revalidated,
			CacheStatuses: reStats.CacheStatuses,
			NotHit:        len(recheckPages) - reStats.Hit,
			HitRate:       rate,
			Total:         len(recheckPages),
			HTTPError:     reStats.HTTPError,
			Exception:     reStats.Exception,
			Skipped:       reStats.Skipped,
			NotFound:      reStats.NotFound,
			Uncacheable:   reStats.Uncacheable,
			NoHeader:      reStats.NoHeader,
			Unclassified:  reStats.Unclassified,
			ErrorRate:     reStats.errRate(),
		}
		for _, f := range reStats.Failures {
			rc.Failures = append(rc.Failures, jsonFailure{URL: f.Page.RawURL, Class: f.Class.String(), Status: f.Status, Error: f.Err, Attempts: f.Attempts})
		}
		pc.printFailures(reStats)
	} else if pc.cfg.Recheck == 0 && stats.recheckCount() > 0 {
		pc.out.row(ansiGray, "复检状态", "未启用 · %d 页待确认，可用 -recheck=5s 验证", stats.recheckCount())
	} else if pc.cfg.Recheck > 0 && stats.recheckCount() == 0 && ctx.Err() == nil && !pc.aborted() {
		pc.out.row(ansiGray, "复检状态", "无待确认页面，已跳过")
	}

	// 更新每日观察记录，连续多天超时的 URL 才记入 slow-pages.txt。
	pc.saveLearned(results)

	code := 0
	if pc.aborted() {
		code = 4
	} else if ctx.Err() != nil {
		code = 130
	} else if pc.cfg.MaxErrRate > 0 && (stats.errRate() > pc.cfg.MaxErrRate || (rc != nil && rc.ErrorRate > pc.cfg.MaxErrRate)) {
		pc.out.Err("首轮或复检的服务不可用率超过阈值 %.1f%%", pc.cfg.MaxErrRate)
		code = 3
	}
	if pc.cfg.JSONPath != "" {
		if err := pc.writeJSON(pc.cfg.JSONPath, results, stats, time.Since(pc.startTime), rc, code); err != nil {
			if code != 0 {
				return code, fmt.Errorf("写入 JSON 报告失败：%w", err)
			}
			return 1, fmt.Errorf("写入 JSON 报告失败：%w", err)
		}
		pc.out.row("", "JSON报告", "%s", pc.cfg.JSONPath)
	}
	return code, nil
}

func main() {
	var (
		sitemap          = flag.String("sitemap", "", "网站地图地址 (必需)")
		size             = flag.Int("size", 1, "并发数量，默认 1；动态站优先保持单并发")
		timeout          = flag.Int("timeout", 10, "单请求超时（秒），默认 10")
		delay            = flag.Int("delay", 1000, "请求完成后全局最小休息（毫秒），默认 1000；与非 HIT 按耗时休息取较大值")
		rate             = flag.Float64("rate", 1, "全局 QPS 上限，默认 1；0 表示完全不限制（不建议，容易压垮源站）")
		retries          = flag.Int("retry", 1, "失败重试次数（429/408/502/503/504 及网络错误），默认 1；超时不重试")
		maxFail          = flag.Int("max-fail", 3, "连续多少个请求失败后熔断中止，默认 3；0 关闭熔断")
		slowRatio        = flag.Float64("slow-ratio", 3.0, "平均耗时超过基线的多少倍就自动降并发，默认 3；0 关闭自适应")
		maxLatency       = flag.Duration("max-latency", 3*time.Second, "慢响应保护阈值，默认 3s；0 关闭慢响应与首次超时保护")
		cooldown         = flag.Duration("cooldown", 10*time.Second, "每次慢响应后的全局冷却，默认 10s")
		maxSlow          = flag.Int("max-slow", 3, "连续慢响应中止阈值，默认 3；0 不按次数中止；超时仍立即中止")
		restRatio        = flag.Float64("rest-ratio", 1, "非 HIT 请求结束后的全局休息/耗时倍数，默认 1；0 关闭按耗时休息")
		slowThreshold    = flag.Duration("slow-threshold", 2*time.Second, "慢页面榜单的耗时阈值，默认 2s；与超时排除无关")
		limit            = flag.Int("limit", 0, "只处理 sitemap 中的前 N 个页面，用于灰度试跑；0 表示全部")
		exclude          = flag.String("exclude", "", "排除 URL 包含这些子串的页面，逗号分隔，例如 -exclude=/user,/tags")
		noLearn          = flag.Bool("no-learn", false, "关闭毒页面自动学习（默认开启：连续 3 天超时才记入 slow-pages.txt 并在下次运行时排除）")
		learnFile        = flag.String("learn-file", "", "毒页面学习文件路径，默认 <程序目录>/slow-pages.txt")
		host             = flag.String("host", "", "指定真实主机，例如 127.0.0.1:8080（会同时改写 Host 头）")
		cacheHeader      = flag.String("cacheheader", "", "缓存标识头，例如 x-cache / cf-cache-status")
		userAgent        = flag.String("useragent", "", "指定 UA，默认模拟 Chrome 浏览器")
		verify           = flag.Bool("verify", false, "是否校验 SSL 证书，默认 false（不校验）")
		head             = flag.Bool("head", false, "使用 HEAD 请求预热（省流量，但部分 CDN 只缓存 GET）")
		recheck          = flag.Duration("recheck", 0, "预热后等待该时长再复检，例如 30s / 2m；0 表示不复检")
		lastMod          = flag.Duration("lastmod", 0, "只预热 sitemap 中最近该时长内变更的页面，例如 24h；0 表示全部")
		jsonPath         = flag.String("json", "", "将结果写入 JSON 文件，便于接入监控")
		logPath          = flag.String("log", "", "日志文件路径，默认 <程序目录>/pre-cache.log；传 none 关闭文件日志")
		logMaxBytes      = flag.Int64("log-max-bytes", 5242880, "程序日志轮转大小（字节），默认 5MiB；每天或达到此大小轮转，最多保留 1 份备份")
		logRetentionDays = flag.Int("log-retention-days", 7, "日志文件保留天数，默认 7；启动和写入时清理过期文件")
		maxErrRate       = flag.Float64("max-error-rate", 0, "错误率超过该百分比时以退出码 3 结束，0 表示不检查")
		showVersion      = flag.Bool("version", false, "显示版本号后退出")
		debug            = flag.Bool("debug", false, "输出调试信息")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("pre-caching %s\n", version)
		return
	}

	if *sitemap == "" {
		fmt.Fprintln(os.Stderr, "错误：必须指定 -sitemap 参数")
		flag.Usage()
		os.Exit(1)
	}

	// 日志路径：默认放在程序目录，且不可写时降级而不是直接失败
	logTarget := *logPath
	if logTarget == "" {
		if execPath, err := os.Executable(); err == nil {
			logTarget = filepath.Join(filepath.Dir(execPath), "pre-cache.log")
		}
	} else if strings.EqualFold(logTarget, "none") {
		logTarget = ""
	}

	if *logMaxBytes <= 0 || *logRetentionDays < 1 || *logRetentionDays > 36500 {
		fmt.Fprintln(os.Stderr, "错误：-log-max-bytes 必须大于 0，-log-retention-days 必须在 1 到 36500 之间")
		os.Exit(1)
	}
	out := newPrinter(logTarget, *logMaxBytes, *logRetentionDays)
	defer out.Close()

	var excl []string
	if *exclude != "" {
		for _, s := range strings.Split(*exclude, ",") {
			if s = strings.TrimSpace(s); s != "" {
				excl = append(excl, s)
			}
		}
	}

	// 学习文件默认放程序目录，与日志路径的定位逻辑一致
	learnPath := *learnFile
	if learnPath == "" && !*noLearn {
		if execPath, err := os.Executable(); err == nil {
			learnPath = filepath.Join(filepath.Dir(execPath), "slow-pages.txt")
		} else {
			learnPath = "slow-pages.txt"
		}
	}

	cfg := Config{
		Sitemap:       *sitemap,
		Host:          *host,
		CacheHeader:   *cacheHeader,
		UserAgent:     *userAgent,
		Concurrency:   *size,
		Timeout:       time.Duration(*timeout) * time.Second,
		Delay:         time.Duration(*delay) * time.Millisecond,
		Rate:          *rate,
		Retries:       *retries,
		UseHead:       *head,
		Insecure:      *verify,
		Debug:         *debug,
		Recheck:       *recheck,
		LastMod:       *lastMod,
		JSONPath:      *jsonPath,
		LogPath:       logTarget,
		MaxErrRate:    *maxErrRate,
		Limit:         *limit,
		MaxFail:       *maxFail,
		SlowRatio:     *slowRatio,
		MaxLatency:    *maxLatency,
		Cooldown:      *cooldown,
		MaxSlow:       *maxSlow,
		RestRatio:     *restRatio,
		SlowThreshold: *slowThreshold,
		Exclude:       excl,
		NoLearn:       *noLearn,
		LearnPath:     learnPath,
	}

	pc, err := NewPreCache(cfg, out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败：%v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := pc.Start(ctx)
	pc.transport.CloseIdleConnections()
	pc.printCompletion(code, err)
	out.Close() // os.Exit 会跳过 defer，日志文件必须显式关闭
	os.Exit(code)
}
