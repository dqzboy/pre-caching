## 介绍

用 Go 编写的网站预缓存工具，用于 CDN 预热、缓存状态检测和预热效果验证。部署环境为 **Linux amd64 / arm64**。

## 项目功能

### 核心功能
- **网站预缓存**：遍历 sitemap 中的所有 URL 触发 CDN / 反向代理缓存
- **缓存状态检测**：通过缓存标识头（`x-cache`、`cf-cache-status` 等）判定 HIT / MISS / 不可缓存
- **预热后复检**：预热结束等待指定时长后自动二次探测，输出真实生效比例
- **增量预热**：按 sitemap 的 `<lastmod>` 只预热最近变更的页面
- **JSON 报告**：输出机器可读结果，可直接接入监控 / 告警

### 技术特点
- **连接复用**：完整读取响应体，keep-alive 连接会被复用，不再每个页面重新握手
- **固定 worker 池**：并发数固定，不会因 URL 数量膨胀而创建海量 goroutine
- **全局限速**：`-rate` 控制实际 HTTP 请求的整体 QPS（默认 1），健康检查、sitemap、重试和重定向均计入配额
- **过载熔断**：连续 N 次请求尝试失败（5xx / 429 / 网络异常）自动中止，重试也计数，避免把已经扛不住的源站踩死
- **慢响应保护**：响应达到 3s 后全局冷却 10s，连续 3 次慢响应或首次超时立即中止；单并发也生效
- **按耗时休息**：非 HIT 请求完成后，默认休息与请求耗时相同的时间，且每次预热请求后至少休息 1s
- **自适应降速**：源站响应变慢时自动收窄并发（最低降到 1）；单并发的恢复时间由全局休息和慢响应保护提供
- **毒页面自动学习**：同一链接连续 3 个自然日超时才记入 `slow-pages.txt`，下次运行按完整 URL 精确排除，避免偶发慢请求和相似路径被误伤
- **预热前健康检查**：开工前先探一次源站，已故障或响应过慢就直接退出，不往故障上压请求
- **慢页面报告**：统计里直接列出最慢的页面 Top 5（并写入 JSON），毒页面一眼可见，不用翻日志猜
- **灰度试跑**：`-limit=N` 只跑前 N 个页面，先观察源站负载再放开全量
- **失败重试**：对 429 / 408 / 502 / 503 / 504 及网络错误做指数退避重试，并完整遵循 `Retry-After`（不会把服务端要求的长等待截短）；**超时不重试**——等满整个超时周期仍无响应说明页面极重或源站过载，重试只会白占 PHP 进程
- **sitemap 索引**：支持 `<sitemapindex>` 递归展开（最多 3 层）与 `.gz` 压缩包
- **优雅中断**：Ctrl+C / SIGTERM 会停止派发并输出已完成部分的结果
- **可观测**：状态码分布、失败明细、命中率、吞吐、退出码语义

## 实现原理

### 1. Sitemap 解析
- 拉取 sitemap，按根节点类型处理：`<urlset>` 取页面，`<sitemapindex>` 递归展开子 sitemap
- 自动识别 gzip（含 `.gz` 后缀与内容魔数两种方式）
- XML 解析失败时降级为正则提取，并根据内容判断是索引还是页面列表（不会再把子 sitemap 当页面请求）
- URL 去重（去除 `#anchor`）

### 2. 并发请求处理
- 固定 `-size` 个 worker 消费任务队列，结果按索引写回，顺序稳定
- 每次预热尝试完成后，全局休息至少 `-delay`；非 HIT 还按 `-rest-ratio × 请求耗时` 休息，取较大值
- 慢响应后再与 `-cooldown` 取较大值；等待可取消，重试和复检也遵守，冷却结束仍受 QPS 限制；在飞请求及其重定向可完成，重定向仍计入 QPS
- 可选 `-rate` 作为全局 QPS 上限，所有 worker、重试和重定向共享同一个令牌间隔

### 3. 缓存状态判定
按 HTTP 状态码优先分级，再看缓存头：

| 分类 | 判定条件 |
|------|----------|
| 已缓存 HIT | 缓存头值包含 `HIT` |
| 可预缓存 MISS | 包含 `MISS` / `EXPIRED` / `REVALIDATED` / `STALE` / `UPDATING` |
| 不可缓存 | `BYPASS` / `DYNAMIC` / `PASS` / `NONE` 等 |
| 缺少缓存头 | 响应中没有该头 |
| 404 | 状态码 404 / 410，**单独统计，不计入"可预缓存"** |
| HTTP 错误 | 其他 4xx / 5xx |
| 请求异常 | 网络错误 / 超时（重试后仍失败） |

命中率分母只包含 HIT + MISS，避免被 404 和不可缓存页面稀释。

## 编译

```bash
go build -o pre-caching main.go

# Linux amd64
GOOS=linux GOARCH=amd64 go build -o pre-caching-amd64 main.go

# Linux arm64
GOOS=linux GOARCH=arm64 go build -o pre-caching-arm64 main.go
```

## 部署与升级

二进制**不入库**（`.gitignore` 已排除），构建产物统一放在 `dist/`，服务器不需要装 Go 环境。

### 构建

```bash
./release.sh
# 版本: 2.7.0
#   dist/pre-caching-amd64   6.3M
#   dist/pre-caching-arm64   5.8M
```

`release.sh` 会先检查 Go 格式，执行 `go vet`、`go test -race` 和脚本回归测试，不通过就中断；版本号从 `main.go` 的 `const version` 读取；编译带 `-trimpath`，减少本地路径对产物的影响；精确复现产物还需要一致的 Go 版本、目标平台和构建参数。

`run_precache.sh` 会按架构自动在 `./` 和 `./dist/` 两个位置查找二进制，所以本地直接跑也行。

### 首次部署

```bash
# 上传二进制和脚本
scp dist/pre-caching-amd64 run_precache.sh precache.env.example user@server:/opt/pre-caching/

ssh user@server
cd /opt/pre-caching
chmod +x pre-caching-amd64 run_precache.sh

cp precache.env.example precache.env
vi precache.env                   # 填 SITEMAP_URL / CACHE_HEADER

./pre-caching-amd64 -version      # 确认二进制能跑
./run_precache.sh                 # 手动跑一次
```

### 升级

```bash
./release.sh
scp dist/pre-caching-amd64 run_precache.sh user@server:/opt/pre-caching/
```

`precache.env` 不会被覆盖（发布物里只有 `.example`），配置可以放心留在服务器上。

> **二进制和脚本必须一起更新**，否则会出现"脚本传了新参数、二进制不认识"的情况：
> `flag provided but not defined: -retry`。
> `run_precache.sh` 会做版本自检，二进制低于 v2.7.0 时直接给出明确提示，不会等到传入新参数后才失败。

## 使用示例

```bash
# 最简
./pre-caching -sitemap="https://yoursite.com/sitemap.xml"

# 首次上线：只跑 20 页、单并发，确认源站扛得住再放开
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="x-cache" \
              -limit=10 -size=1 -rate=1

# 小批量验证稳定后：预热 + 可选复检 + JSON 报告
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="cf-cache-status" \
              -size=1 -rate=1 \
              -recheck=2m \
              -json=/var/log/precache/report.json

# 走源站预热（会同时改写 Host 头）
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -host="10.0.0.5:80" -verify=false

# 只预热最近 24 小时变更的页面（缺少 lastmod 的页面仍会请求）
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="x-cache" -lastmod=24h

# 精确定速：全局 20 QPS（纯静态站才这么激进，动态站别用）
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="x-cache" -size=8 -delay=0 -rest-ratio=0 -rate=20

# 查看帮助
./pre-caching -h
```

## 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-sitemap` | 必填 | sitemap 地址，支持 `<urlset>` 与 `<sitemapindex>` |
| `-size` | 1 | 并发 worker 数量（上限 512） |
| `-delay` | 1000 | 每次预热请求完成后的全局最小休息（毫秒），重试也遵守 |
| `-rate` | 1 | 全局 QPS 上限，**0 表示完全不限制**（不建议） |
| `-max-fail` | 3 | 连续多少次 HTTP 请求尝试失败后熔断中止，重试计数；0 关闭熔断 |
| `-rest-ratio` | 1 | 非 HIT 请求后的休息/耗时倍数，与 `-delay` 取较大值；0 关闭按耗时休息 |
| `-max-latency` | 3s | 慢响应阈值；0 关闭慢响应和首次超时保护 |
| `-cooldown` | 10s | 每次慢响应后的全局冷却，与常规休息取较大值 |
| `-max-slow` | 3 | 连续慢响应达到此数后停止；0 不按次数停止；首次超时仍立即停止 |
| `-slow-ratio` | 3 | 平均耗时超过基线的多少倍时自动降并发，0 关闭自适应 |
| `-slow-threshold` | 2s | 控制台 Top 5 和 JSON Top 10 的慢页面阈值，例如 `3s` / `500ms`；超时始终列出，不影响自动学习或自适应降速 |
| `-limit` | 0 | 只处理 sitemap 中的前 N 个页面（灰度试跑），0 全部 |
| `-exclude` | 空 | 排除 URL 包含这些子串的页面，逗号分隔，如 `-exclude=/user,/tags` |
| `-no-learn` | false | 关闭毒页面自动学习（默认连续 3 天超时才自动排除） |
| `-learn-file` | 程序目录/slow-pages.txt | 已确认的排除文件路径；观察记录保存为同路径加 `.history.json` |
| `-timeout` | 10 | 单请求超时（秒） |
| `-retry` | 1 | 429/408/502/503/504 与网络错误的重试次数 |
| `-host` | 空 | 指定真实主机（如 `127.0.0.1:8080`），同时改写 Host 头 |
| `-cacheheader` | 空 | 缓存标识头，如 `x-cache`；留空则只统计状态码 |
| `-useragent` | Chrome 131 UA | 请求 UA，默认模拟浏览器（见下方「关于 UA 与反爬」） |
| `-verify` | false | 是否校验 SSL 证书 |
| `-head` | false | 用 HEAD 预热（省流量，但部分 CDN 只缓存 GET） |
| `-recheck` | 0 | 预热后等待该时长再复检，如 `30s` / `2m`；0 关闭 |
| `-lastmod` | 0 | 只预热最近该时长内变更的页面，如 `24h`；0 全部 |
| `-json` | 空 | JSON 报告输出路径 |
| `-log` | 程序目录/pre-cache.log | 日志路径；传 `none` 关闭文件日志 |
| `-max-error-rate` | 0 | 首轮或复检的服务不可用率(%)超过该值时以退出码 3 结束；0 不检查 |
| `-version` | - | 显示版本号后退出 |
| `-debug` | false | 调试输出 |

### 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常完成 |
| 1 | 致命错误（健康检查失败、任一 sitemap 拉取失败、空 sitemap、指定的 JSON 报告写入失败） |
| 3 | 首轮或复检的服务不可用率超过 `-max-error-rate` 阈值（5xx / 429 / 网络异常，其他 4xx 不算） |
| 4 | 压力保护中止：首次超时、连续慢响应或连续失败；JSON `stop_reason` 给出原因 |
| 130 | 被信号中断（Ctrl+C / SIGTERM） |

## 如何控制预热压力

`-size` 限制工具同时等待的请求数，`-rate` 限制发起请求的速度。两者都不代表 PHP 的实际 CPU 占用，也不能保证服务器上只运行同等数量的任务。特别是客户端超时后，服务端脚本可能仍在执行；继续请求其他页面会积累后台工作。

动态站从 `-size=1 -rate=1 -delay=1000 -rest-ratio=1` 开始。默认每次请求完成后至少休息 1 秒；非 HIT 请求如耗时 2 秒，就休息至少 2 秒。HIT 也保留最小休息，无缓存标识头的响应按非 HIT 处理。`-delay` 从 v2.7 起是全局完成后休息，原来的非零配置也会遵守此语义。慢响应的冷却与上述休息取较大值，不重复相加。

### 慢响应与超时保护

默认响应达到 `-max-latency=3s` 后，全局休息 `-cooldown=10s`；连续 `-max-slow=3` 次慢响应即中止。即使响应是 HTTP 200，且并发已经为 1，也会生效。首次请求超时就中止，不再继续下一页面，避免 PHP 可能尚未退出时继续加压。退出码为 4，日志和 JSON `stop_reason` 会说明原因。

这些判断依据客户端观察到的响应，不读取服务器 CPU、FPM 队列或数据库锁，不能保证单个重页面不会引发故障。超时仍按原规则进入多日观察，但本轮保护不需要等待自动学习确认。`-max-latency=0` 会同时关闭慢响应及首次超时保护，不建议在动态站使用；`-max-slow=0` 只关闭连续次数中止，仍会冷却且首次超时仍中止。

`-slow-ratio=3` 继续提供相对基线降并发：建立基线后，最近 20 次平均耗时超出基线 3 倍则将并发减半，最低为 1。它对单并发没有进一步降并发的空间，不能独自承担过载保护。

### 上线节奏

1. 网站已有卡顿时停止预热和相应定时任务，先恢复服务。
2. 检查 PHP-FPM 的有效配置及慢日志。`request_terminate_timeout=0` 代表没有启用 FPM 请求终止时限；客户端超时不能替代它。设置服务端时限前，评估上传、后台和定时任务的正常耗时。
3. 保留原有 `EXCLUDE` 配置，先用 `LIMIT=10 RECHECK=0` 小批量试跑，同时观察网站与 CPU。
4. 有卡顿或触发退出码 4 时先排查日志，避免立即重跑；降低 RATE、增加 DELAY / REST_RATIO 可以进一步减少持续工作比例。
5. 小批量验证稳定后逐步扩大范围，再评估全量和复检。单并发仍可能过载，没有仅凭 CPU 核数或 `pm.max_children` 就能推出的安全并发公式。

升级时同时更新二进制与执行脚本。旧 `precache.env`、cron 或 `EXTRA_ARGS` 中显式设置的 `DELAY=0`、较高 `RATE` / `RETRY` 等值会覆盖新默认，必须检查。不要用示例配置覆盖已有站点地址和排除项。v2.7 默认重试由 2 次降为 1 次，连续失败阈值由 20 降为 3。

### 毒页面自动学习与慢页面报告

预热结束时输出最慢的页面 Top 5，默认只列出单次请求耗时 **超过 2 秒** 的页面，超时页面始终列出。可通过 `-slow-threshold=3s` 或脚本配置 `SLOW_THRESHOLD=3s` 调整；JSON 的 Top 10 使用同一阈值。榜单只用于观察，与自动降并发的 `-slow-ratio` 和请求超时的 `-timeout` 相互独立。自动学习以请求是否达到 `-timeout` 超时为准（默认 10 秒），仅耗时较长但未超时的页面只会出现在慢页面报告里。

同一原始 URL **连续 3 个自然日超时**才写入 `slow-pages.txt`，之后的运行才自动排除。观察期间仍然正常请求该链接：

- 按运行机器的本地日期计算，同一天运行多次或出现多次超时，只计 1 天。
- 观察期间只要有一次未超时的 HTTP 响应，就清零连续记录；当天后续的超时也不重新累计。
- 某天没有该链接的超时记录（未运行、未请求或仅发生其他网络错误），连续性就中断，下次超时从第 1 天重新计数。
- 取消和未执行的请求不计为超时，也不作为恢复正常的证据。

前两天的日志示例：

```
最慢的页面 Top 1（单次请求耗时 >2s，超时页面始终列出）：
       10s  https://www.example.com/user  ← 超时
慢页面观察：https://www.example.com/user（连续超时 1/3 天，暂不排除）
```

第三天仍然超时才会提示：

```
已将连续 3 天超时的 1 个页面记入 /opt/pre-caching/slow-pages.txt，下次运行自动排除
```

观察记录自动保存在排除文件路径加 `.history.json` 的文件中（默认 `slow-pages.txt.history.json`），以便跨运行累计。该文件由程序维护，临时记录不会用于跳过页面。使用 `-learn-file` 时，两个文件的位置会一起改变；`-no-learn` 同时关闭历史排除的加载和观察记录的更新。

已有 `slow-pages.txt` 中的完整 URL 条目仍然有效，并采用精确匹配（`/post/1` 不会误排除 `/post/10`）。手动 `-exclude` 继续使用子串匹配。升级前因偶发超时被加入的链接，可以从该文件删除，让它重新参与预热和观察；如果还需清空累计记录，可同时删除对应的 `.history.json` 文件。

JSON 报告里同样有 `slowest` 字段（Top 10，含耗时），可直接接入监控。确认连续多天超时的页面可以作为排查慢 SQL、外部调用等性能问题的线索。

### 预热前健康检查

正式预热前会先 GET 一次站点根路径：返回 5xx / 429 或请求失败（包括响应体超时）时直接中止（退出码 1），避免在故障站点上继续加压。

```
健康检查：GET https://www.example.com/
健康检查通过：HTTP 200（54ms）
```

注意：如果你的根路径本身就是慢页面，健康检查会拦住整个预热——这通常说明站点真的有故障，先修站再说。

## 关于 UA 与反爬

预热的目标是"像真实访客那样把页面打进缓存"，所以工具默认发送一整套浏览器请求头：

```
User-Agent:      Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 ... Chrome/131.0.0.0 Safari/537.36
Accept:          text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8
Accept-Language: zh-CN,zh;q=0.9,en;q=0.8
Accept-Encoding: gzip（由 Go 自动添加并透明解压）
```

**为什么要这么做：**

1. **WAF / Bot 管理会拦非浏览器 UA**。Cloudflare Super Bot Fight、云厂商 WAF 的"非浏览器 UA"规则、WordPress 安全插件，都会拦截或挑战自定义 UA。被拦后页面返回 403/503，预热直接失败。
2. **有些缓存规则本身依赖 `Accept`**。这类 nginx 配置很常见：

   ```nginx
   if ($http_accept !~* "text/html") { set $skip_cache 1; }
   ```

   请求不带 `Accept` 就会 `BYPASS`，看起来"跑成功了"，实际一条都没缓存。

**如果确实被拦了**，工具会在报告里提示：

```
提示：300 个请求返回 403/429/503，疑似被 WAF / 限流拦截
      可尝试：-useragent 换浏览器 UA、降低 -rate/-size，或把预热机 IP 加白名单
```

按优先级处理：

1. 把预热机 IP 加入 CDN / WAF 白名单（最干净，也最推荐）
2. 降低 `-rate` / `-size`，避开速率限制
3. 用 `-useragent` 换一个更贴近真实访客的 UA
4. 需要声明爬虫身份时，按 RFC 9309 写成 `MyBot/1.0 (+https://your.site/bot)`——但要接受被部分站点拦截

> 注意：UA 只是第一层。Cloudflare 这类还会校验 TLS 指纹（JA3/JA4），伪造 UA 但指纹对不上照样会被挑战。真要绕过，只能上白名单。

## 输出结果说明

- **页面总数**：从 sitemap 提取并去重后的数量
- **已缓存 (HIT)**：缓存已命中的页面
- **可预缓存 (MISS)**：本次访问触发了缓存生成的页面
- **不可缓存**：CDN 规则决定不缓存的页面（需要改缓存规则，预热无效）
- **缺少缓存标识头**：CDN 没有返回该头，通常说明 `-cacheheader` 配错了
- **404 / HTTP 错误 / 请求异常**：失败明细会单独列出，便于清理死链
- **复检结果**：只在首轮存在 MISS 时等待并复检，生效比例 = 复检 HIT 数 / 全部待复检页面数；HTTP 错误、网络异常和中断不会从分母中消失，失败详情写入 `recheck.failures`；`recheck.miss` 只统计真实 MISS，所有未命中项（含错误和中断）使用 `recheck.not_hit`
- **JSON 完整性**：报告使用临时文件加原子替换写入，避免读取到半份报告；指定报告但写入失败会非零退出。`exit_code` 记录任务状态，`requests` 包括健康检查、sitemap、重试、重定向与复检，`avg_rps` 按实际请求数及总耗时计算，`duration_sec` 包含复检等待
- **增量筛选**：输出筛选前后的数量及缺少有效 `lastmod` 的页面数；未知修改时间的页面仍保留，避免漏预热，并明确提示增量可能退化为全量
- **增量无变更**：有效 sitemap 经 `-lastmod` 过滤后无页面时正常退出 0，空 sitemap 仍视为错误
- **sitemap 完整性**：任一子 sitemap 拉取失败都会中止任务，避免只预热部分页面却报告成功

## 常见 CDN 缓存标识头

| CDN | 参数 |
|-----|------|
| Cloudflare | `-cacheheader="cf-cache-status"` |
| AWS CloudFront | `-cacheheader="x-cache"` |
| 阿里云 CDN | `-cacheheader="x-cache"` |
| 腾讯云 CDN | `-cacheheader="x-cache-lookup"` |
| 百度云 CDN | `-cacheheader="x-bce-cache-status"` |
| 自建 Nginx | `-cacheheader="x-cache"` |

> 建议先用 `curl -I` 看真实响应头确认字段名，不同套餐/配置可能不同。

## 定时任务

站点配置放在 `precache.env`（与脚本同目录），**不要写进 `run_precache.sh`**——升级时脚本会被整个覆盖，写死的配置会丢。

```bash
cp precache.env.example precache.env
vi precache.env          # 填 SITEMAP_URL / CACHE_HEADER

# 手动试跑（临时换站点直接在命令前加变量即可，优先级高于 precache.env）
SITEMAP_URL="https://other.com/sitemap.xml" ./run_precache.sh

# 安装定时任务（记得先改 crontab_example.txt 里的路径）
crontab crontab_example.txt
```

配置优先级：**命令行 / 定时任务里的环境变量 > `precache.env` > 脚本内默认值**。

所以 crontab 可以写得很短，只写"这一次执行特有的差异"：

```
SHELL=/bin/bash
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin

# 每小时增量
5 * * * * LASTMOD=2h /bin/bash /opt/pre-caching/run_precache.sh >> /opt/pre-caching/cron.log 2>&1

# 每天全量 + 复检
10 3 * * * LASTMOD=0 RECHECK=1m JSON_OUT=/opt/pre-caching/last-report.json /bin/bash /opt/pre-caching/run_precache.sh >> /opt/pre-caching/cron.log 2>&1
```

脚本还包含：

- 仅支持 Linux，按 amd64 / arm64 自动选择可执行文件，并校验二进制版本
- `SITEMAP_URL` 仍是占位地址时直接报错，避免预热到别人的站点
- 防重叠执行锁（优先 `flock`，未安装时使用 mkdir + PID），中断时转发信号并等待子进程退出
- 输出重定向到 `$SCRIPT_DIR/cron.log` 时自动轮转（超 5MB）

## 注意事项

1. **单并发也要休息**：并发、QPS、完成后休息和慢响应保护共同控制节奏；客户端超时后服务端可能继续工作
2. **压力保护属于事后反馈**：单个重页面也可能影响访客，先小批量验证、检查 PHP 慢日志，再考虑全量预热
2. **预热走 CDN 而不是源站**：`-host` 是给"绕过 CDN 直连源站测试"用的，用它预热不会产生 CDN 缓存
3. **不可缓存页面**：如果大量页面是 `DYNAMIC` / `BYPASS`，说明缓存规则没覆盖到，改规则比反复预热有效
4. **HTTPS + `-host`**：目标 IP 的证书通常与域名不匹配，需要 `-verify=false`
5. **HEAD 预热**：部分 CDN 只对 GET 建立缓存，用 `-head` 可能全部返回 `BYPASS`（工具会提示），仅建议用于探测
