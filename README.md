## 介绍

这是一个用Go语言重写的网站预缓存工具，主要用于网站CDN预热、缓存状态检测和性能优化。

## 项目功能

### 核心功能
- **网站预缓存**: 通过访问sitemap中的所有URL来触发CDN或反向代理的缓存机制
- **缓存状态检测**: 通过检查HTTP响应头来判断页面的缓存状态（HIT/MISS/EXPIRED等）
- **性能统计**: 提供详细的缓存命中率和预缓存效果统计报告
- **并发处理**: 支持可配置的并发请求数量，提高处理效率

### 技术特点
- **高性能**: Go语言实现，支持高并发处理
- **智能解析**: 支持XML解析和正则表达式双重URL提取机制
- **容错机制**: XML解析失败时自动降级到正则表达式提取
- **资源控制**: 内置延迟和并发限制，避免对服务器造成过大压力
- **友好输出**: 彩色文本输出，提供清晰的统计信息和进度反馈

## 实现原理

### 1. Sitemap解析
- 从指定的sitemap URL获取XML内容
- 使用Go标准库的XML解析器提取所有`<loc>`标签中的URL
- 如果XML解析失败，自动fallback到正则表达式提取模式

### 2. 并发HTTP请求处理
- 使用Goroutine + Channel实现高效的并发控制
- 支持可配置的并发数量（默认5个并发）
- 内置请求间延迟机制（默认500ms）防止服务器过载
- 支持单线程顺序模式（并发数为1时）

### 3. 缓存状态智能分析
- 通过指定的缓存标识头（如`x-cache`、`cf-cache-status`等）检测缓存状态
- 自动识别并统计不同类型的页面：
  - **HIT**: 缓存命中的页面
  - **MISS/EXPIRED**: 可预缓存的页面（需要缓存预热）
  - **不可缓存页面**: 其他缓存状态的页面
  - **异常页面**: 请求失败的页面
  - **缺失标识头**: 没有缓存标识头的页面

## 使用场景

### 1. CDN预热
- 网站发布后预先访问所有页面，提高用户首次访问速度
- 新内容发布时触发缓存更新
- 定期预热热门页面

### 2. 缓存配置验证
- 验证CDN或反向代理的缓存规则是否正确配置
- 检查哪些页面可以被缓存，哪些不能
- 排查缓存配置问题

### 3. 性能监控
- 定期检查网站缓存状态
- 监控缓存命中率变化
- 识别缓存性能瓶颈


## 编译和运行

#### Linux (amd64)
```bash
GOOS=linux GOARCH=amd64 go build -o pre-caching main.go
```

### 运行示例
```bash
# 基本使用
./pre-caching -sitemap="https://yoursite.com/sitemap.xml"

# 指定更多参数
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
           -size=5 \
           -timeout=15 \
           -cacheheader="x-cache" \
           -host="127.0.0.1:8080" \
           -debug

# 显示帮助
./pre-caching -h
```

## 参数说明

- `-sitemap`: 网站地图sitemap地址 (必需参数)
- `-size`: 并发请求数量，默认5（建议根据服务器性能调整）
- `-timeout`: 单个请求的超时时间，默认10秒
- `-delay`: 请求间延迟时间，默认500毫秒（防止服务器过载）
- `-host`: 指定真实主机地址，如 127.0.0.1:8080（用于本地测试或负载均衡）
- `-cacheheader`: 缓存标识头名称，如 x-cache、cf-cache-status 等
- `-useragent`: 指定User-Agent标识，默认 Pre-cache/go-http-client/1.0
- `-verify`: 是否校验SSL证书，默认不校验（适合开发环境）
- `-debug`: 显示详细的Debug信息，默认关闭

## 高级用法示例

### 基础CDN预热
```bash
# 预热整站页面
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" -cacheheader="x-cache"
```

### 本地开发测试
```bash
# 测试本地缓存配置
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
           -host="127.0.0.1:8080" \
           -verify=false \
           -debug
```

### 生产环境安全预热
```bash
# 保守的并发设置，避免影响生产服务
./pre-caching -sitemap="https://yoursite.com/sitemap.xml" \
           -size=3 \
           -delay=1000 \
           -timeout=30 \
           -cacheheader="x-cache"
```

## 输出结果说明

工具运行后会显示以下统计信息：

- **页面总数**: 从sitemap中提取的URL总数
- **已被缓存页面数**: 缓存状态为HIT的页面数量
- **可预缓存页面数**: 缓存状态为MISS/EXPIRED的页面数量（这些页面经过此次访问后应该会被缓存）
- **不可缓存页面数**: 无法被缓存的页面数量
- **请求异常页面数**: 访问失败的页面数量
- **缓存标识头缺失页面数**: 没有指定缓存标识头的页面数量
- **总耗时**: 完成所有页面访问的时间

## 常见CDN缓存标识头

| CDN提供商 | 缓存标识头 | 示例值 |
|-----------|------------|--------|
| Cloudflare | cf-cache-status | HIT, MISS, EXPIRED |
| AWS CloudFront | x-cache | Hit from cloudfront, Miss from cloudfront |
| 阿里云CDN | ali-swift-global-savetime | HIT, MISS |
| 腾讯云CDN | x-cache-lookup | HIT, MISS |
| 百度云CDN | x-bce-cache-status | HIT, MISS |
| 自建Nginx | x-cache | HIT, MISS |

## 注意事项

1. **服务器压力**: 建议从较小的并发数开始测试，根据服务器响应情况逐步调整
2. **请求频率**: 适当设置延迟时间，避免被目标服务器识别为攻击行为
3. **SSL证书**: 生产环境建议开启SSL验证，开发环境可以关闭
4. **缓存标识头**: 不同CDN提供商使用的缓存标识头名称不同，需要根据实际情况配置
5. **日志分析**: 开启debug模式可以查看详细的请求过程，便于问题排查

## 性能优化建议

- 对于大型网站（>1000个页面），建议将并发数设置在5-10之间
- 请求延迟建议设置在300-1000ms之间，平衡效率和服务器压力
- 超时时间根据页面复杂度调整，动态页面建议设置更长的超时时间
- 定期运行可以保持缓存的"热度"，提高用户访问体验
