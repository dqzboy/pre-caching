#!/usr/bin/env bash

# 预缓存执行脚本
# 用于定时执行预缓存任务

# 获取脚本所在目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 切换到脚本目录
cd "$SCRIPT_DIR"

# 配置参数 - 请根据实际情况修改
SITEMAP_URL="https://yoursite.com/sitemap.xml"    # 请修改为您的sitemap地址
CACHE_HEADER="X-Cache"                            # 缓存标识头
CONCURRENT_SIZE=5                                 # 并发数
TIMEOUT=10                                        # 超时时间
DELAY=500                                         # 请求延迟

# 检查可执行文件
if [[ "$OSTYPE" == "linux-gnu"* ]]; then
    if [[ $(uname -m) == "x86_64" ]]; then
        EXECUTABLE="./pre-caching-amd64"
    elif [[ $(uname -m) == "aarch64" ]] || [[ $(uname -m) == "arm64" ]]; then
        EXECUTABLE="./pre-caching-arm64"
    else
        echo "不支持的 Linux 架构: $(uname -m)"
        exit 1
    fi
else
    echo "不支持的操作系统: $OSTYPE"
    exit 1
fi

# 检查执行文件是否存在
if [ ! -f "$EXECUTABLE" ]; then
    echo "错误: 找不到可执行文件 $EXECUTABLE"
    echo "请确保预缓存程序在当前目录下"
    exit 1
fi

# 确保执行文件有执行权限
chmod +x "$EXECUTABLE"

# 执行预缓存
echo "开始执行预缓存任务..."
echo "========================================="

"$EXECUTABLE" \
    --sitemap="$SITEMAP_URL" \
    --cacheheader="$CACHE_HEADER" \
    --size="$CONCURRENT_SIZE" \
    --timeout="$TIMEOUT" \
    --delay="$DELAY"

EXIT_CODE=$?

echo "========================================="
echo "预缓存任务完成"
echo "退出码: $EXIT_CODE"

# 如果有日志文件，显示日志文件位置
if [ -f "pre-cache.log" ]; then
    echo "日志文件: $SCRIPT_DIR/pre-cache.log"
    echo "最后10行日志:"
    tail -10 pre-cache.log
fi

exit $EXIT_CODE
