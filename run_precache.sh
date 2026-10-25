#!/usr/bin/env bash
#
# 预缓存执行脚本（供 cron / systemd timer 调用）
#
# 所有参数都可用环境变量覆盖，例如：
#   SITEMAP_URL="https://blog.example.com/sitemap.xml" CONCURRENCY=8 ./run_precache.sh
#

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR" || exit 1

# ---------------------------------------------------------------------------
# 加载配置文件
# 站点地址这类"跟环境相关"的配置放 precache.env，不要写死在本脚本里：
# 升级时通常是用新脚本整个覆盖本文件，写死的配置会被冲掉，而 precache.env 不受影响。
#
# 优先级：命令行环境变量 > precache.env > 脚本内默认值
# （所以临时想换站点直接 `SITEMAP_URL=... ./run_precache.sh` 就能覆盖）
# ---------------------------------------------------------------------------
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/precache.env}"
if [[ -f "$ENV_FILE" ]]; then
    # 先记下调用方已经显式传入的值，稍后恢复，避免被配置文件盖掉
    _PRESET=()
    for _v in SITEMAP_URL CACHE_HEADER CONCURRENCY TIMEOUT DELAY RATE RETRY RECHECK \
              LASTMOD JSON_OUT MAX_ERROR_RATE VERIFY_SSL EXTRA_ARGS LOG_FILE CRON_LOG \
              MAX_FAIL SLOW_RATIO LIMIT EXCLUDE NO_LEARN; do
        [[ -n "${!_v-}" ]] && _PRESET+=("${_v}=${!_v}")
    done

    set -a
    # shellcheck disable=SC1090
    . "$ENV_FILE"
    set +a

    for _kv in "${_PRESET[@]+"${_PRESET[@]}"}"; do
        export "$_kv"
    done
    unset _v _kv _PRESET
fi

# ---------------------------------------------------------------------------
# 配置（可用环境变量覆盖）
# ---------------------------------------------------------------------------
# 兜底默认值，不需要改：真实地址配在 precache.env（或用环境变量传入）。
# 故意保留占位符——忘记配置时脚本会直接报错退出，而不是去预热别人的站点。
SITEMAP_URL="${SITEMAP_URL:-https://yoursite.com/sitemap.xml}"
CACHE_HEADER="${CACHE_HEADER:-x-cache}"     # CDN 缓存标识头，留空则不统计缓存状态
CONCURRENCY="${CONCURRENCY:-5}"             # 并发数
TIMEOUT="${TIMEOUT:-10}"                    # 单请求超时（秒）
DELAY="${DELAY:-500}"                       # 每个并发槽的请求间隔（毫秒）
RATE="${RATE:-}"                             # 全局 QPS 上限：留空=用程序默认(5)，显式设 0=完全不限速
RETRY="${RETRY:-2}"                         # 失败重试次数
MAX_FAIL="${MAX_FAIL:-}"                    # 连续失败多少个请求后熔断：留空=程序默认(20)，0=关闭熔断
SLOW_RATIO="${SLOW_RATIO:-}"                # 耗时超基线多少倍自动降并发：留空=程序默认(3)，0=关闭
LIMIT="${LIMIT:-}"                          # 只处理前 N 个页面（灰度试跑）：留空=全部
EXCLUDE="${EXCLUDE:-}"                     # 排除 URL 包含这些子串的页面，逗号分隔，例如 /ads,/members
NO_LEARN="${NO_LEARN:-0}"                  # 1=关闭毒页面自动学习（默认开启）
RECHECK="${RECHECK:-0}"                     # 预热后复检等待时长，例如 30s / 2m；0=不复检
LASTMOD="${LASTMOD:-0}"                     # 只预热最近变更的页面，例如 24h；0=全部
JSON_OUT="${JSON_OUT:-}"                    # JSON 报告路径，留空则不生成
MAX_ERROR_RATE="${MAX_ERROR_RATE:-0}"       # 错误率阈值(%)，超过则退出码 3；0=不检查
VERIFY_SSL="${VERIFY_SSL:-0}"               # 1=校验 SSL 证书
EXTRA_ARGS="${EXTRA_ARGS:-}"                # 追加参数，例如 "-host=10.0.0.5:80"

LOG_FILE="${LOG_FILE:-$SCRIPT_DIR/pre-cache.log}"
CRON_LOG="${CRON_LOG:-$SCRIPT_DIR/cron.log}"
LOG_MAX_BYTES="${LOG_MAX_BYTES:-5242880}"   # cron.log 超过 5MB 自动轮转
LOCK_FILE="${LOCK_FILE:-$SCRIPT_DIR/.precache.lock}"
LOCK_DIR="${LOCK_FILE}.d"

# ---------------------------------------------------------------------------
# 配置自检：SITEMAP_URL 还是占位地址时直接报错
# 否则会去抓 yoursite.com，报告看起来"跑完了"，实际预热的是别人的域名
# ---------------------------------------------------------------------------
if [[ "$SITEMAP_URL" == *"yoursite.com"* ]]; then
    cat >&2 <<EOF
错误: SITEMAP_URL 还是占位地址，没有配置真实站点
      当前值: $SITEMAP_URL

      请在 crontab 里通过环境变量指定（推荐），或改脚本第 17 行的默认值：

        SITEMAP_URL="https://www.example.com/sitemap.xml"
EOF
    exit 1
fi

# ---------------------------------------------------------------------------
# 选择可执行文件
# ---------------------------------------------------------------------------
detect_binary() {
    local os arch
    os="$(uname -s)"
    arch="$(uname -m)"

    if [[ "$os" == "Darwin" ]]; then
        # 仓库里的 -amd64/-arm64 是 Linux 产物，macOS 需本地 go build 出 ./pre-caching
        if [[ -x "./pre-caching" ]]; then
            echo "./pre-caching"
            return 0
        fi
        echo "错误: macOS 上找不到 ./pre-caching，请先执行: go build -o pre-caching main.go" >&2
        return 1
    fi

    if [[ "$os" != "Linux" ]]; then
        echo "错误: 不支持的操作系统 $os" >&2
        return 1
    fi

    case "$arch" in
        x86_64|amd64)   echo "./pre-caching-amd64" ;;
        aarch64|arm64)  echo "./pre-caching-arm64" ;;
        *) echo "错误: 不支持的 CPU 架构 $arch" >&2; return 1 ;;
    esac
}

EXECUTABLE="$(detect_binary)" || exit 1

if [[ ! -f "$EXECUTABLE" ]]; then
    echo "错误: 找不到可执行文件 $EXECUTABLE" >&2
    exit 1
fi
chmod +x "$EXECUTABLE" 2>/dev/null

# ---------------------------------------------------------------------------
# 版本自检
# 用 -version 的输出来判定，而不是解析 -h 的 usage 文本：
#   - 解析 usage 依赖参数名，参数一改就失效
#   - `cmd | grep -q` 在 pipefail 下可能因 SIGPIPE 误判为失败
# 脚本会传 -retry/-rate/-json 等 v2.0 才有的参数，旧版二进制遇到未知参数
# 只会打一行 "flag provided but not defined" 然后退出 2，很难定位。
# ---------------------------------------------------------------------------
VERSION_LINE="$("$EXECUTABLE" -version 2>&1 || true)"
VERSION_LINE="${VERSION_LINE%%$'\n'*}"

if [[ "$VERSION_LINE" != "pre-caching 2."* ]]; then
    FILE_INFO="$(ls -l "$EXECUTABLE" 2>/dev/null | awk '{print $5" 字节, 修改时间 "$6" "$7" "$8}')"
    cat >&2 <<EOF
错误: $EXECUTABLE 不是 v2.0+ 版本，当前脚本无法使用它
      -version 实际输出: ${VERSION_LINE:-<空>}
      文件信息: ${FILE_INFO:-<无法读取>}
      文件类型: $(file -b "$EXECUTABLE" 2>/dev/null | cut -c1-60)

      当前脚本需要 pre-caching v2.0 及以上版本。请在部署目录下执行：

        ls -l $EXECUTABLE
        ./${EXECUTABLE#./} -version      # 期望输出: pre-caching 2.0.0

      若上面显示的还是旧内容，说明上传没有覆盖成功（常见原因：上传到了
      另一个目录、文件名不一致、或旧进程仍占用文件）。重新编译并上传：

        GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o pre-caching-amd64 main.go
        GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o pre-caching-arm64 main.go
EOF
    exit 1
fi

echo "二进制版本: $VERSION_LINE"

# ---------------------------------------------------------------------------
# 防重叠执行：上一次还没跑完就跳过本次，避免压力叠加
# ---------------------------------------------------------------------------
acquire_lock() {
    if command -v flock >/dev/null 2>&1; then
        exec 9>"$LOCK_FILE"
        if ! flock -n 9; then
            echo "$(date '+%F %T') 上一次预缓存任务仍在运行，本次跳过"
            exit 0
        fi
        return 0
    fi

    # 无 flock（如 macOS）时用 mkdir 原子性加锁
    if ! mkdir "$LOCK_DIR" 2>/dev/null; then
        local owner=""
        [[ -f "$LOCK_DIR/pid" ]] && owner="$(cat "$LOCK_DIR/pid" 2>/dev/null)"

        # 持有者进程仍存活 → 本次跳过
        if [[ -n "$owner" ]] && kill -0 "$owner" 2>/dev/null; then
            # 注意：变量后紧跟中文/全角字符时必须用 ${var} 包裹，
            # 否则在 cron 的 C locale 下 bash 会把多字节字符的首字节当成变量名的一部分
            echo "$(date '+%F %T') 上一次预缓存任务（PID ${owner}）仍在运行，本次跳过"
            exit 0
        fi

        # 无 PID 记录且锁超过 3 小时，或 PID 已消失 → 判定为残留锁
        if [[ -z "$owner" && -z "$(find "$LOCK_DIR" -maxdepth 0 -mmin +180 2>/dev/null)" ]]; then
            echo "$(date '+%F %T') 上一次预缓存任务仍在运行，本次跳过"
            exit 0
        fi

        echo "$(date '+%F %T') 清理残留锁（PID ${owner:-unknown} 已不存在）"
        rm -rf "$LOCK_DIR"
        mkdir "$LOCK_DIR" 2>/dev/null || exit 0
    fi
    echo $$ > "$LOCK_DIR/pid"
    trap 'rm -rf "$LOCK_DIR"' EXIT INT TERM
}

# ---------------------------------------------------------------------------
# 日志轮转
# ---------------------------------------------------------------------------
rotate_log() {
    [[ -f "$CRON_LOG" ]] || return 0
    local size
    size=$(wc -c < "$CRON_LOG" 2>/dev/null | tr -d ' ')
    [[ -z "$size" ]] && return 0
    if (( size > LOG_MAX_BYTES )); then
        mv -f "$CRON_LOG" "${CRON_LOG}.1"
        echo "$(date '+%F %T') 日志已轮转: ${CRON_LOG}.1" >> "$CRON_LOG"
    fi
}

# ---------------------------------------------------------------------------
# 执行
# ---------------------------------------------------------------------------
acquire_lock
rotate_log

echo "========================================="
echo "开始执行预缓存任务: $(date '+%F %T')"
echo "可执行文件: $EXECUTABLE"
echo "站点地图:   $SITEMAP_URL"

ARGS=(
    --sitemap="$SITEMAP_URL"
    --size="$CONCURRENCY"
    --timeout="$TIMEOUT"
    --delay="$DELAY"
    --retry="$RETRY"
    --log="$LOG_FILE"
)

[[ -n "$CACHE_HEADER" ]]        && ARGS+=(--cacheheader="$CACHE_HEADER")
[[ -n "$RATE" ]]               && ARGS+=(--rate="$RATE")
[[ -n "$MAX_FAIL" ]]           && ARGS+=(--max-fail="$MAX_FAIL")
[[ -n "$SLOW_RATIO" ]]         && ARGS+=(--slow-ratio="$SLOW_RATIO")
[[ -n "$LIMIT" ]]              && ARGS+=(--limit="$LIMIT")
[[ -n "$EXCLUDE" ]]            && ARGS+=(--exclude="$EXCLUDE")
[[ "$NO_LEARN" == "1" ]]       && ARGS+=(--no-learn)
[[ "$RECHECK" != "0" ]]        && ARGS+=(--recheck="$RECHECK")
[[ "$LASTMOD" != "0" ]]        && ARGS+=(--lastmod="$LASTMOD")
[[ -n "$JSON_OUT" ]]           && ARGS+=(--json="$JSON_OUT")
[[ "$MAX_ERROR_RATE" != "0" ]] && ARGS+=(--max-error-rate="$MAX_ERROR_RATE")
[[ "$VERIFY_SSL" == "1" ]]     && ARGS+=(--verify)
[[ -n "$EXTRA_ARGS" ]]         && ARGS+=($EXTRA_ARGS)

"$EXECUTABLE" "${ARGS[@]}"
EXIT_CODE=$?

echo "========================================="
echo "预缓存任务结束: $(date '+%F %T')，退出码: $EXIT_CODE"

case "$EXIT_CODE" in
    0)   ;;
    3)   echo "警告: 错误率超过阈值，请检查站点是否有大量 5xx / 超时" ;;
    4)   echo "警告: 已触发熔断（连续失败过多），目标站点疑似过载，请降低 RATE 后重试" ;;
    130) echo "提示: 任务被信号中断" ;;
    *)   echo "错误: 任务异常退出" ;;
esac

exit "$EXIT_CODE"
