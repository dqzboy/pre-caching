package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Colors 用于输出彩色文本
type Colors struct {
	logFile *os.File
}

func (c *Colors) log(msg string) {
	if c.logFile != nil {
		c.logFile.WriteString(msg + "\n")
	}
}

func (c *Colors) Normal(msg string) {
	fmt.Println(msg)
	c.log(msg)
}

func (c *Colors) Green(msg string) {
	colorMsg := fmt.Sprintf("\033[32m%s\033[0m", msg)
	fmt.Println(colorMsg)
	c.log(msg)
}

func (c *Colors) Yellow(msg string) {
	colorMsg := fmt.Sprintf("\033[33m%s\033[0m", msg)
	fmt.Println(colorMsg)
	c.log(msg)
}

func (c *Colors) Red(msg string) {
	colorMsg := fmt.Sprintf("\033[31m%s\033[0m", msg)
	fmt.Println(colorMsg)
	c.log(msg)
}

func (c *Colors) Blue(msg string) {
	colorMsg := fmt.Sprintf("\033[34m%s\033[0m", msg)
	fmt.Println(colorMsg)
	c.log(msg)
}

func (c *Colors) Cyan(msg string) {
	colorMsg := fmt.Sprintf("\033[36m%s\033[0m", msg)
	fmt.Println(colorMsg)
	c.log(msg)
}

func (c *Colors) Debug(msg string, debug bool) {
	if debug {
		fmt.Println(msg)
		c.log(msg)
	}
}

// Sitemap XML结构
type Sitemap struct {
	XMLName xml.Name `xml:"urlset"`
	URLs    []URL    `xml:"url"`
}

type URL struct {
	Loc string `xml:"loc"`
}

// RequestResult 请求结果
type RequestResult struct {
	URL        string
	StatusCode int
	Headers    http.Header
	Error      error
}

// PreCache 预缓存结构体
type PreCache struct {
	sitemapURL  string
	host        string
	cacheHeader string
	userAgent   string
	size        int
	timeout     time.Duration
	delay       time.Duration // 请求间延迟
	verify      bool
	debug       bool
	client      *http.Client
	report      *Colors
	scheme      string
	domain      string
	startTime   time.Time
	logFile     *os.File

	// 统计信息
	hitCount       int
	missCount      int
	noneCount      int
	noHeaderCount  int
	exceptionCount int
}

// NewPreCache 创建新的PreCache实例
func NewPreCache(sitemap, host, cacheHeader, userAgent string, size int, timeout int, delay int, verify, debug bool) (*PreCache, error) {
	parsedURL, err := url.Parse(sitemap)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return nil, fmt.Errorf("网站地图URL解析失败：%s，请检查！", sitemap)
	}

	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}

	// 创建日志文件
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("获取执行文件路径失败：%v", err)
	}
	execDir := filepath.Dir(execPath)
	logPath := filepath.Join(execDir, "pre-cache.log")

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("创建日志文件失败：%v", err)
	}

	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	report := &Colors{logFile: logFile}

	return &PreCache{
		sitemapURL:  sitemap,
		host:        host,
		cacheHeader: cacheHeader,
		userAgent:   userAgent,
		size:        size,
		timeout:     time.Duration(timeout) * time.Second,
		delay:       time.Duration(delay) * time.Millisecond,
		verify:      verify,
		debug:       debug,
		client:      client,
		report:      report,
		scheme:      parsedURL.Scheme,
		domain:      parsedURL.Host,
		startTime:   time.Now(),
		logFile:     logFile,
	}, nil
}

// getSitemap 获取sitemap内容
func (pc *PreCache) getSitemap() (string, error) {
	pc.report.Normal(fmt.Sprintf("开始拉取站点地图文件：%s", pc.sitemapURL))

	req, err := http.NewRequest("GET", pc.sitemapURL, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", pc.userAgent)

	resp, err := pc.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("拉取站点地图失败：HTTP状态码：%d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	pc.report.Normal("拉取站点地图文件成功！\n")
	pc.report.Normal("开始解析站点地图文件...")

	return string(body), nil
}

// getURLs 通过XML解析提取URL
func (pc *PreCache) getURLs() ([]string, error) {
	sitemapContent, err := pc.getSitemap()
	if err != nil {
		return nil, err
	}

	var sitemap Sitemap
	err = xml.Unmarshal([]byte(sitemapContent), &sitemap)
	if err != nil {
		// 如果XML解析失败，尝试正则表达式
		pc.report.Yellow(fmt.Sprintf("通过XML解析器提取网址失败: %v，尝试改为正则提取...", err))
		return pc.getURLsRegex(sitemapContent)
	}

	var urls []string
	for _, u := range sitemap.URLs {
		processedURL := pc.processURL(u.Loc)
		pc.report.Debug(fmt.Sprintf("[DEBUG]成功提取网址：%s", processedURL), pc.debug)
		urls = append(urls, processedURL)
	}

	return urls, nil
}

// getURLsRegex 通过正则表达式提取URL
func (pc *PreCache) getURLsRegex(sitemapContent string) ([]string, error) {
	pattern := `<loc>https?://[^<]+</loc>`
	re := regexp.MustCompile(pattern)
	matches := re.FindAllString(sitemapContent, -1)

	var urls []string
	urlPattern := regexp.MustCompile(`https?://[^<]+`)

	for _, match := range matches {
		urlMatch := urlPattern.FindString(match)
		if urlMatch == "" {
			pc.report.Debug(fmt.Sprintf("[DEBUG]提取网址失败：%s", match), pc.debug)
			continue
		}

		processedURL := pc.processURL(urlMatch)
		pc.report.Debug(fmt.Sprintf("[DEBUG]成功提取网址：%s", processedURL), pc.debug)
		urls = append(urls, processedURL)
	}

	return urls, nil
}

// processURL 处理URL（替换host等）
func (pc *PreCache) processURL(rawURL string) string {
	if pc.host == "" {
		return rawURL
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	// 替换host
	newURL := fmt.Sprintf("%s://%s%s", pc.scheme, pc.host, parsedURL.Path)
	if parsedURL.RawQuery != "" {
		newURL += "?" + parsedURL.RawQuery
	}

	return newURL
}

// makeRequest 发起HTTP请求
func (pc *PreCache) makeRequest(targetURL string) RequestResult {
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return RequestResult{URL: targetURL, Error: err}
	}

	req.Header.Set("User-Agent", pc.userAgent)
	if pc.host != "" {
		req.Header.Set("Host", pc.domain)
	}

	resp, err := pc.client.Do(req)
	if err != nil {
		return RequestResult{URL: targetURL, Error: err}
	}
	defer resp.Body.Close()

	return RequestResult{
		URL:        targetURL,
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Error:      nil,
	}
}

// processResults 处理请求结果并统计缓存信息
func (pc *PreCache) processResults(results []RequestResult) {
	pc.hitCount = 0
	pc.missCount = 0
	pc.noneCount = 0
	pc.noHeaderCount = 0
	pc.exceptionCount = 0

	if pc.cacheHeader == "" {
		return
	}

	for _, result := range results {
		if result.Error != nil {
			pc.exceptionCount++
			continue
		}

		displayURL := result.URL
		if pc.host != "" {
			displayURL = strings.Replace(displayURL, pc.host, pc.domain, 1)
		}

		found := false
		for header, values := range result.Headers {
			if strings.EqualFold(header, pc.cacheHeader) {
				found = true
				value := strings.Join(values, ", ")
				upperValue := strings.ToUpper(value)

				if strings.Contains(upperValue, "HIT") {
					pc.hitCount++
				} else if strings.Contains(upperValue, "MISS") || strings.Contains(upperValue, "EXPIRED") {
					pc.missCount++
					pc.report.Green(fmt.Sprintf("可预缓存页面：%s 缓存标识头：%s: %s", displayURL, header, value))
				} else {
					pc.noneCount++
					pc.report.Red(fmt.Sprintf("不可缓存页面：%s 缓存标识头：%s: %s", displayURL, header, value))
				}
				break
			}
		}

		if !found {
			pc.report.Yellow(fmt.Sprintf("缓存标识头缺失页面：%s", displayURL))
			pc.noHeaderCount++
		}
	}
}

// Start 启动预缓存
func (pc *PreCache) Start() error {
	pc.report.Cyan(fmt.Sprintf("执行开始时间：%s", pc.startTime.Format("2006-01-02 15:04:05")))
	pc.report.Normal(fmt.Sprintf("站点地图：%s", pc.sitemapURL))

	if pc.host != "" {
		pc.report.Normal(fmt.Sprintf("指定主机：%s", pc.host))
	}

	pc.report.Normal(fmt.Sprintf("并发数量：%d", pc.size))
	pc.report.Normal(fmt.Sprintf("超时时间：%v", pc.timeout))
	pc.report.Normal(fmt.Sprintf("缓存标识：%s", pc.cacheHeader))
	pc.report.Normal(fmt.Sprintf("UA  标识：%s", pc.userAgent))
	pc.report.Blue("预缓存开始:")
	pc.report.Normal("---------------------------------------------------------")

	// 获取所有URL
	urls, err := pc.getURLs()
	if err != nil {
		return fmt.Errorf("提取网址失败：%v", err)
	}

	if len(urls) == 0 {
		return fmt.Errorf("提取网址失败，请检查sitemap文件是否符合规范")
	}

	pc.report.Green(fmt.Sprintf("提取网址成功，共 %d 条记录", len(urls)))

	// 并发请求处理
	results := pc.processURLsConcurrently(urls)

	// 统计结果
	pc.processResults(results)

	// 输出统计信息
	pc.printStatistics(len(results))

	return nil
}

// processURLsConcurrently 并发处理URL请求
func (pc *PreCache) processURLsConcurrently(urls []string) []RequestResult {
	results := make([]RequestResult, 0, len(urls))

	// 如果并发数为1，使用顺序处理
	if pc.size == 1 {
		return pc.processURLsSequentially(urls)
	}

	semaphore := make(chan struct{}, pc.size) // 限制并发数
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, targetURL := range urls {
		wg.Add(1)
		go func(url string, index int) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 根据索引添加额外延迟，避免突发请求
			baseDelay := time.Duration(index%10) * 50 * time.Millisecond
			time.Sleep(pc.delay + baseDelay)

			result := pc.makeRequest(url)

			mu.Lock()
			results = append(results, result)
			if result.Error != nil {
				pc.report.Debug(fmt.Sprintf("请求异常：%s, %v", url, result.Error), true)
			}
			mu.Unlock()
		}(targetURL, i)
	}

	wg.Wait()
	return results
}

// processURLsSequentially 顺序处理URL请求（单线程模式）
func (pc *PreCache) processURLsSequentially(urls []string) []RequestResult {
	results := make([]RequestResult, 0, len(urls))

	for i, targetURL := range urls {
		// 在顺序模式下也添加延迟
		if i > 0 {
			time.Sleep(pc.delay * 2) // 顺序模式下使用更长的延迟
		}

		result := pc.makeRequest(targetURL)
		results = append(results, result)

		if result.Error != nil {
			pc.report.Debug(fmt.Sprintf("请求异常：%s, %v", targetURL, result.Error), true)
		}

		// 每10个请求后额外暂停
		if (i+1)%10 == 0 {
			pc.report.Debug(fmt.Sprintf("已处理 %d/%d 个URL，暂停1秒...", i+1, len(urls)), pc.debug)
			time.Sleep(1 * time.Second)
		}
	}

	return results
} // printStatistics 打印统计信息
func (pc *PreCache) printStatistics(totalCount int) {
	elapsed := time.Since(pc.startTime)
	endTime := time.Now()

	pc.report.Normal("---------------------------------------------------------")
	pc.report.Blue(fmt.Sprintf("预缓存完成，页面总数：%d，耗时%d秒", totalCount, int(elapsed.Seconds())))
	pc.report.Cyan(fmt.Sprintf("执行结束时间：%s", endTime.Format("2006-01-02 15:04:05")))

	if pc.hitCount > 0 {
		pc.report.Green(fmt.Sprintf("已被缓存页面数：%d", pc.hitCount))
	}

	if pc.missCount > 0 {
		pc.report.Blue(fmt.Sprintf("可预缓存页面数：%d", pc.missCount))
	}

	if pc.noneCount > 0 {
		pc.report.Red(fmt.Sprintf("不可缓存页面数：%d", pc.noneCount))
	}

	if pc.exceptionCount > 0 {
		pc.report.Red(fmt.Sprintf("请求异常页面数：%d", pc.exceptionCount))
	}

	if pc.noHeaderCount > 0 {
		pc.report.Yellow(fmt.Sprintf("缓存标识头缺失页面数：%d", pc.noHeaderCount))
	}

	// 计算缓存命中率和预缓存效果
	if pc.cacheHeader != "" && (pc.hitCount+pc.missCount) > 0 {
		totalCacheable := pc.hitCount + pc.missCount
		hitRate := float64(pc.hitCount) / float64(totalCacheable) * 100
		pc.report.Normal("---------------------------------------------------------")
		pc.report.Green(fmt.Sprintf("缓存命中率：%.1f%% (%d/%d)", hitRate, pc.hitCount, totalCacheable))

		if pc.missCount > 0 {
			pc.report.Blue(fmt.Sprintf("预缓存效果：本次访问触发了 %d 个页面的缓存生成", pc.missCount))
			pc.report.Yellow("建议：等待几分钟后再次运行此工具，验证缓存是否生效")
		}
	}

	if pc.hitCount+pc.missCount == 0 {
		if pc.cacheHeader != "" {
			pc.report.Yellow(fmt.Sprintf("指定的缓存标识头 %s 可能不对，未能找到这个头信息.", pc.cacheHeader))
		} else {
			pc.report.Normal("Ps：如果指定了缓存命中的头信息，将会显示更多统计信息，比如加上：--cacheheader=x-cache")
		}
	}
}

func main() {
	var (
		sitemap     = flag.String("sitemap", "", "网站地图sitemap地址 (必需)")
		size        = flag.Int("size", 5, "并发请求数量,默认5 (进一步降低以减少服务器压力)")
		timeout     = flag.Int("timeout", 10, "单个请求的超时时间,默认10s")
		delay       = flag.Int("delay", 500, "请求间延迟(毫秒),默认500ms")
		host        = flag.String("host", "", "指定真实主机，比如 127.0.0.1:8080")
		cacheHeader = flag.String("cacheheader", "", "缓存标识，比如: x-cache")
		userAgent   = flag.String("useragent", "", "指定UA标识，默认 Chrome 120 UA")
		verify      = flag.Bool("verify", false, "是否校验SSL，默认不校验")
		debug       = flag.Bool("debug", false, "显示Debug信息, 默认关闭")
	)
	flag.Parse()

	if *sitemap == "" {
		fmt.Println("错误: 必须指定sitemap参数")
		flag.Usage()
		return
	}

	preCache, err := NewPreCache(*sitemap, *host, *cacheHeader, *userAgent, *size, *timeout, *delay, *verify, *debug)
	if err != nil {
		fmt.Printf("初始化失败: %v\n", err)
		return
	}

	// 确保日志文件关闭
	defer func() {
		if preCache.logFile != nil {
			preCache.logFile.Close()
		}
	}()

	if err := preCache.Start(); err != nil {
		fmt.Printf("执行失败: %v\n", err)
		return
	}
}
