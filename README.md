## 介绍

这是一个用Go语言重写的网站预缓存工具

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

- `-sitemap`: 网站地图sitemap地址 (必需)
- `-size`: 并发请求数量，默认10
- `-timeout`: 单个请求的超时时间，默认10秒
- `-host`: 指定真实主机，比如 127.0.0.1:8080
- `-cacheheader`: 缓存标识头，比如: x-cache
- `-useragent`: 指定UA标识，默认 Pre-cache/go-http-client/1.0
- `-verify`: 是否校验SSL，默认不校验
- `-debug`: 显示Debug信息，默认关闭
