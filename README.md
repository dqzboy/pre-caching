## 介绍

用 Go 编写的网站预缓存工具，用于 CDN 预热、缓存状态检测和预热效果验证。

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
- **全局限速**：`-rate` 精确控制整体 QPS（默认 5），比调大/调小并发更可控
- **过载熔断**：连续 N 个请求失败（5xx / 429 / 超时）自动中止，避免把已经扛不住的源站踩死
- **自适应降速**：源站响应变慢时自动收窄并发（最低降到 1），覆盖"站还能访问、只是越来越卡"这种熔断管不到的情况
- **毒页面自动学习**：同一链接连续 3 个自然日超时才记入 `slow-pages.txt`，下次运行自动排除，避免偶发慢请求被误判
- **预热前健康检查**：开工前先探一次源站，已故障或响应过慢就直接退出，不往故障上压请求
- **慢页面报告**：统计里直接列出最慢的页面 Top 5（并写入 JSON），毒页面一眼可见，不用翻日志猜
- **灰度试跑**：`-limit=N` 只跑前 N 个页面，先观察源站负载再放开全量
- **失败重试**：对 429 / 408 / 502 / 503 / 504 及网络错误做指数退避重试，并遵循 `Retry-After`；**超时不重试**——等满整个超时周期仍无响应说明页面极重或源站过载，重试只会白占 PHP 进程
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
- 每个 worker 在两次请求之间保持 `-delay` 间隔，该等待不占用全局并发资源
- 可选 `-rate` 作为全局 QPS 上限，所有 worker 共享同一个令牌间隔

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
# 版本: 2.1.0
#   dist/pre-caching-amd64   6.3M
#   dist/pre-caching-arm64   5.8M
```

`release.sh` 会先跑 `gofmt -l` 和 `go vet`，不通过就中断；版本号从 `main.go` 的 `const version` 读取；编译带 `-trimpath`，同样代码在任何人机器上产出的二进制一致。

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
> `run_precache.sh` 会做版本自检，二进制过旧时直接给出明确提示，不会静默失败。

## 使用示例

```bash
# 最简
./pre-caching -sitemap="https://yoursite.com/sitemap.xml"

# 首次上线：只跑 20 页、单并发，确认源站扛得住再放开
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="x-cache" \
              -limit=20 -size=1 -rate=2

# 生产推荐：预热 + 复检 + JSON 报告
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="cf-cache-status" \
              -size=2 -rate=4 \
              -recheck=2m \
              -json=/var/log/precache/report.json

# 走源站预热（会同时改写 Host 头）
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -host="10.0.0.5:80" -verify=false

# 只预热最近 24 小时变更的页面（增量，压力最小）
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="x-cache" -lastmod=24h

# 精确定速：全局 20 QPS（纯静态站才这么激进，动态站别用）
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
              -cacheheader="x-cache" -size=8 -delay=0 -rate=20

# 查看帮助
./pre-caching -h
```

## 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-sitemap` | 必填 | sitemap 地址，支持 `<urlset>` 与 `<sitemapindex>` |
| `-size` | 5 | 并发 worker 数量（上限 512） |
| `-delay` | 500 | 每个 worker 两次请求之间的间隔（毫秒） |
| `-rate` | 5 | 全局 QPS 上限，**0 表示完全不限制**（不建议） |
| `-max-fail` | 20 | 连续多少个请求失败后熔断中止，0 关闭熔断 |
| `-slow-ratio` | 3 | 平均耗时超过基线的多少倍时自动降并发，0 关闭自适应 |
| `-limit` | 0 | 只处理 sitemap 中的前 N 个页面（灰度试跑），0 全部 |
| `-exclude` | 空 | 排除 URL 包含这些子串的页面，逗号分隔，如 `-exclude=/user,/tags` |
| `-no-learn` | false | 关闭毒页面自动学习（默认连续 3 天超时才自动排除） |
| `-learn-file` | 程序目录/slow-pages.txt | 已确认的排除文件路径；观察记录保存为同路径加 `.history.json` |
| `-timeout` | 10 | 单请求超时（秒） |
| `-retry` | 2 | 429/408/502/503/504 与网络错误的重试次数 |
| `-host` | 空 | 指定真实主机（如 `127.0.0.1:8080`），同时改写 Host 头 |
| `-cacheheader` | 空 | 缓存标识头，如 `x-cache`；留空则只统计状态码 |
| `-useragent` | Chrome 131 UA | 请求 UA，默认模拟浏览器（见下方「关于 UA 与反爬」） |
| `-verify` | false | 是否校验 SSL 证书 |
| `-head` | false | 用 HEAD 预热（省流量，但部分 CDN 只缓存 GET） |
| `-recheck` | 0 | 预热后等待该时长再复检，如 `30s` / `2m`；0 关闭 |
| `-lastmod` | 0 | 只预热最近该时长内变更的页面，如 `24h`；0 全部 |
| `-json` | 空 | JSON 报告输出路径 |
| `-log` | 程序目录/pre-cache.log | 日志路径；传 `none` 关闭文件日志 |
| `-max-error-rate` | 0 | 错误率(%)超过该值时以退出码 3 结束；0 不检查 |
| `-version` | - | 显示版本号后退出 |
| `-debug` | false | 调试输出 |

### 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常完成 |
| 1 | 致命错误（sitemap 拉取/解析失败、无任何页面） |
| 3 | 错误率超过 `-max-error-rate` 阈值（只算 5xx / 超时，404 不算） |
| 4 | 触发熔断：连续失败过多，目标站点疑似过载 |
| 130 | 被信号中断（Ctrl+C / SIGTERM） |

## 怎么定 `-size` 和 `-rate`（最容易踩坑的地方）

预热把站打挂，几乎都是这两个参数配错。先用一句话说清分工：

- **`-size` 决定"同时有几个请求在飞"**，也就是同时占用源站几个 PHP 进程
- **`-rate` 决定"每秒发起几个请求"**，是期望速率

实际在飞请求数 = `min(size, rate × 单页响应时间)`。

关键点：**源站一慢，响应时间变长，在飞请求数会顶到 `size` 这个硬上限**。所以 `size` 才是决定"会不会把源站压垮"的参数，`rate` 只是让请求发得慢一点。

### size 的安全上限

```
CONCURRENCY ≤ CPU 核数
CONCURRENCY ≤ pm.max_children / 3
```

取两者中更小的那个。

实例（2 核 + `pm.max_children = 8` 的 WordPress 站）：
- `8/3 = 2`，核数 = 2 → 上限 2
- **实际用 1**：严格串行，任何时刻只占 1 个 PHP 进程、半个核，其余全留给访客

用 `size=5` 是什么后果：5 个 PHP 进程同时渲染，2 核直接打满，8 个 children 被吃掉 5 个，剩下 3 个还抢不到 CPU，访客访问开始转圈。**这就是最常见的"预热把站搞挂"。**

### rate 取多少

`size` 定好之后，`rate` 只影响"多久跑完"。串行时实际速率 = `1/单页响应时间`，`rate` 设得比它大也没用。

| 场景 | size | rate | 300 页耗时 |
|------|------|------|-----------|
| 2 核小站（最保守） | 1 | 2 | 约 5 分钟 |
| 4 核 + 对象缓存 | 2 | 4 | 约 1.5 分钟 |
| 纯静态站 | 8 | 20 | 约 20 秒 |

**预热不需要快。** 缓存 TTL 是小时/天级的，5 分钟和 20 秒对预热效果没有任何区别，但快 15 倍足够把源站打挂。

### 上线节奏

1. 先查容量：`nproc`、`grep pm.max_children /etc/php-fpm.d/*.conf`
2. 按上面公式算 `size`，用 `-limit=20` 跑一轮，同时刷浏览器看首页卡不卡
3. 不卡 → 去掉 `-limit` 跑全量
4. 还卡 → `size` 继续降（最低 1），**不要靠调 `rate` 解决**

### 自适应降速

即使参数配错，`-slow-ratio`（默认 3）也会兜底：最近 20 个请求的平均耗时超过基线 3 倍时，并发自动减半，最低降到 1。

```
源站变慢：最近 20 个请求平均 420ms（基线 90ms），并发 5 → 2
```

看到这行就说明 `size` 配大了，应该手动调低。设 `-slow-ratio=0` 可关闭。

### 毒页面自动学习与慢页面报告

预热结束时会输出最慢的页面 Top 5（超过 0.5s 才列出）。自动学习以请求是否达到 `-timeout` 超时为准（默认 10 秒），仅耗时较长但未超时的页面只会出现在慢页面报告里。

同一原始 URL **连续 3 个自然日超时**才写入 `slow-pages.txt`，之后的运行才自动排除。观察期间仍然正常请求该链接：

- 按运行机器的本地日期计算，同一天运行多次或出现多次超时，只计 1 天。
- 观察期间只要有一次未超时的 HTTP 响应，就清零连续记录；当天后续的超时也不重新累计。
- 某天没有该链接的超时记录（未运行、未请求或仅发生其他网络错误），连续性就中断，下次超时从第 1 天重新计数。
- 取消和未执行的请求不计为超时，也不作为恢复正常的证据。

前两天的日志示例：

```
最慢的页面 Top 5（预热耗时，>0.5s 才列出）：
       10s  https://www.example.com/user  ← 超时
慢页面观察：https://www.example.com/user（连续超时 1/3 天，暂不排除）
```

第三天仍然超时才会提示：

```
已将连续 3 天超时的 1 个页面记入 /opt/pre-caching/slow-pages.txt，下次运行自动排除
```

观察记录自动保存在排除文件路径加 `.history.json` 的文件中（默认 `slow-pages.txt.history.json`），以便跨运行累计。该文件由程序维护，临时记录不会用于跳过页面。使用 `-learn-file` 时，两个文件的位置会一起改变；`-no-learn` 同时关闭历史排除的加载和观察记录的更新。

已有 `slow-pages.txt` 中的条目仍然有效。升级前因偶发超时被加入的链接，可以从该文件删除，让它重新参与预热和观察；如果还需清空累计记录，可同时删除对应的 `.history.json` 文件。

JSON 报告里同样有 `slowest` 字段（Top 10，含耗时），可直接接入监控。确认连续多天超时的页面可以作为排查慢 SQL、外部调用等性能问题的线索。

### 预热前健康检查

正式预热前会先 GET 一次站点根路径：返回 5xx 或完全无响应时直接中止（退出码 1），避免在故障站点上继续加压。

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
- **复检结果**：等待后重新探测 MISS 页面，得出真实生效比例

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
5 * * * * /bin/bash /opt/pre-caching/run_precache.sh >> /opt/pre-caching/cron.log 2>&1

# 每天全量 + 复检
10 3 * * * RECHECK=1m JSON_OUT=/opt/pre-caching/last-report.json /bin/bash /opt/pre-caching/run_precache.sh >> /opt/pre-caching/cron.log 2>&1
```

脚本还包含：

- Linux / macOS 自动选择可执行文件，并校验二进制版本
- `SITEMAP_URL` 仍是占位地址时直接报错，避免预热到别人的站点
- 防重叠执行锁（`flock`，macOS 降级为 mkdir + PID）
- 输出重定向到 `$SCRIPT_DIR/cron.log` 时自动轮转（超 5MB）

## 注意事项

1. **`-size` 才是决定压力大小的参数**：它等于"同时占用源站几个 PHP 进程"。按 `size ≤ min(CPU 核数, pm.max_children/3)` 取，宁可小（详见上方「怎么定 -size 和 -rate」）
2. **熔断和降速都只是兜底**：熔断要等"整站返回 5xx"，降速要等"连续 20 个请求变慢"，前面的冲击已经发生了。真正的保护是低 `-size` + 灰度 `-limit`
2. **预热走 CDN 而不是源站**：`-host` 是给"绕过 CDN 直连源站测试"用的，用它预热不会产生 CDN 缓存
3. **不可缓存页面**：如果大量页面是 `DYNAMIC` / `BYPASS`，说明缓存规则没覆盖到，改规则比反复预热有效
4. **HTTPS + `-host`**：目标 IP 的证书通常与域名不匹配，需要 `-verify=false`
5. **HEAD 预热**：部分 CDN 只对 GET 建立缓存，用 `-head` 可能全部返回 `BYPASS`（工具会提示），仅建议用于探测
