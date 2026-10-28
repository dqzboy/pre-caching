#!/usr/bin/env bash
#
# 预缓存执行脚本（供 cron / systemd timer 调用）
#
# 所有参数都可用环境变量覆盖，例如：
#   SITEMAP_URL="https://blog.example.com/sitemap.xml" CONCURRENCY=1 ./run_precache.sh
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
              MAX_FAIL SLOW_RATIO SLOW_THRESHOLD MAX_LATENCY COOLDOWN MAX_SLOW REST_RATIO LIMIT EXCLUDE NO_LEARN LEARN_FILE \
              LOG_MAX_BYTES LOG_RETENTION_DAYS LOCK_FILE; do
        [[ "${!_v+x}" == x ]] && _PRESET+=("${_v}=${!_v}")
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
CACHE_HEADER="${CACHE_HEADER-x-cache}"      # 显式留空则不统计缓存状态
CONCURRENCY="${CONCURRENCY:-1}"             # 动态站默认单并发
TIMEOUT="${TIMEOUT:-10}"                    # 单请求超时（秒）
DELAY="${DELAY:-1000}"                      # 请求完成后全局最小休息（毫秒）
RATE="${RATE:-}"                           # 全局 QPS 上限：留空=用程序默认(1)，显式设 0=完全不限速
RETRY="${RETRY:-1}"                         # 失败重试次数
MAX_FAIL="${MAX_FAIL:-}"                    # 连续失败多少个请求后熔断：留空=程序默认(3)，0=关闭熔断
SLOW_RATIO="${SLOW_RATIO:-}"                # 耗时超基线多少倍自动降并发：留空=程序默认(3)，0=关闭
SLOW_THRESHOLD="${SLOW_THRESHOLD:-2s}"      # 慢页面榜单阈值，与超时学习和自适应降速无关
MAX_LATENCY="${MAX_LATENCY:-3s}"            # 达到此耗时后全局冷却；0 关闭慢响应和超时保护
COOLDOWN="${COOLDOWN:-10s}"                 # 每次慢响应后全局休息
MAX_SLOW="${MAX_SLOW:-3}"                   # 连续慢响应中止阈值；首次超时立即中止
REST_RATIO="${REST_RATIO:-1}"               # 非 HIT 完成后按请求耗时倍数休息
LIMIT="${LIMIT:-}"                          # 只处理前 N 个页面（灰度试跑）：留空=全部
EXCLUDE="${EXCLUDE:-}"                     # 排除 URL 包含这些子串的页面，逗号分隔，例如 /ads,/members
NO_LEARN="${NO_LEARN:-0}"                  # 1=关闭毒页面自动学习（默认开启）
LEARN_FILE="${LEARN_FILE:-$SCRIPT_DIR/slow-pages.txt}" # 与二进制的存放目录无关
RECHECK="${RECHECK:-0}"                     # 预热后复检等待时长，例如 30s / 2m；0=不复检
LASTMOD="${LASTMOD:-0}"                     # 只预热最近变更的页面，例如 24h；0=全部
JSON_OUT="${JSON_OUT:-}"                    # JSON 报告路径，留空则不生成
MAX_ERROR_RATE="${MAX_ERROR_RATE:-0}"       # 错误率阈值(%)，超过则退出码 3；0=不检查
VERIFY_SSL="${VERIFY_SSL:-0}"               # 1=校验 SSL 证书
EXTRA_ARGS="${EXTRA_ARGS:-}"                # 追加参数，例如 "-host=10.0.0.5:80"

LOG_FILE="${LOG_FILE:-$SCRIPT_DIR/pre-cache.log}"
CRON_LOG="${CRON_LOG:-$SCRIPT_DIR/cron.log}"
LOG_MAX_BYTES="${LOG_MAX_BYTES:-5242880}"   # 两种日志每天或达到 5MiB 轮转，仅保留 .1
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-7}" # 清理超过此天数未修改的日志文件
LOCK_FILE="${LOCK_FILE:-$SCRIPT_DIR/.precache.lock}"
LOCK_DIR="${LOCK_FILE}.d"

if [[ ! "$LOG_MAX_BYTES" =~ ^[0-9]{1,15}$ ]] || (( 10#$LOG_MAX_BYTES < 1 )) ||
   [[ ! "$LOG_RETENTION_DAYS" =~ ^[0-9]{1,5}$ ]] ||
   (( 10#$LOG_RETENTION_DAYS < 1 || 10#$LOG_RETENTION_DAYS > 36500 )); then
    echo "错误: LOG_MAX_BYTES 必须是正整数，LOG_RETENTION_DAYS 必须在 1 到 36500 之间" >&2
    exit 1
fi
LOG_MAX_BYTES=$((10#$LOG_MAX_BYTES))
LOG_RETENTION_DAYS=$((10#$LOG_RETENTION_DAYS))

# ---------------------------------------------------------------------------
# 配置自检：SITEMAP_URL 还是占位地址时直接报错
# 否则会去抓 yoursite.com，报告看起来"跑完了"，实际预热的是别人的域名
# ---------------------------------------------------------------------------
if [[ "$SITEMAP_URL" == *"yoursite.com"* ]]; then
    cat >&2 <<EOF
错误: SITEMAP_URL 还是占位地址，没有配置真实站点
      当前值: $SITEMAP_URL

      请在 precache.env 中配置 SITEMAP_URL，或通过环境变量指定：

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

    if [[ "$os" != "Linux" ]]; then
        echo "错误: 仅支持 Linux，当前系统为 $os" >&2
        return 1
    fi

    local binary
    case "$arch" in
        x86_64|amd64)   binary="pre-caching-amd64" ;;
        aarch64|arm64)  binary="pre-caching-arm64" ;;
        *) echo "错误: 不支持的 CPU 架构 $arch" >&2; return 1 ;;
    esac
    local candidate
    for candidate in "./$binary" "./dist/$binary"; do
        if [[ -f "$candidate" ]]; then
            echo "$candidate"
            return 0
        fi
    done
    echo "错误: 找不到 ./$binary 或 ./dist/$binary，请先执行 ./release.sh" >&2
    return 1
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
# 脚本会传 -log-retention-days 等 v2.7.1 才有的参数，旧版二进制遇到未知参数
# 只会打一行 "flag provided but not defined" 然后退出 2，很难定位。
# ---------------------------------------------------------------------------
VERSION_LINE="$("$EXECUTABLE" -version 2>&1 || true)"
VERSION_LINE="${VERSION_LINE%%$'\n'*}"

version_supported() {
    local major minor patch
    if [[ "$VERSION_LINE" =~ ^pre-caching\ ([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
        major="${BASH_REMATCH[1]}"
        minor="${BASH_REMATCH[2]}"
        patch="${BASH_REMATCH[3]}"
        (( 10#$major > 2 || (10#$major == 2 && (10#$minor > 7 || (10#$minor == 7 && 10#$patch >= 1))) ))
        return $?
    fi
    return 1
}

if ! version_supported; then
    FILE_INFO="$(ls -l "$EXECUTABLE" 2>/dev/null | awk '{print $5" 字节, 修改时间 "$6" "$7" "$8}')"
    cat >&2 <<EOF
错误: $EXECUTABLE 版本过旧或格式无效，当前脚本需要 v2.7.1 及以上版本
      -version 实际输出: ${VERSION_LINE:-<空>}
      文件信息: ${FILE_INFO:-<无法读取>}
      文件类型: $(file -b "$EXECUTABLE" 2>/dev/null | cut -c1-60)

      当前脚本使用日志轮转参数，需要 pre-caching v2.7.1 及以上版本。请在部署目录下执行：

        ls -l $EXECUTABLE
        ./${EXECUTABLE#./} -version      # 期望输出: pre-caching 2.7.1 或更新版本

      若上面显示的还是旧内容，说明上传没有覆盖成功（常见原因：上传到了
      另一个目录、文件名不一致、或旧进程仍占用文件）。重新编译并上传：

        GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o pre-caching-amd64 main.go
        GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o pre-caching-arm64 main.go
EOF
    exit 1
fi

# ---------------------------------------------------------------------------
# 防重叠执行：上一次还没跑完就跳过本次，避免压力叠加
# ---------------------------------------------------------------------------
acquire_lock() {
    if command -v flock >/dev/null 2>&1; then
        if ! exec 9>"$LOCK_FILE"; then
            echo "错误: 无法打开锁文件 $LOCK_FILE，请检查目录和权限" >&2
            exit 1
        fi
        if ! flock -n 9; then
            echo "$(date '+%F %T') 上一次预缓存任务仍在运行，本次跳过"
            exit 0
        fi
        return 0
    fi

    # 精简 Linux 系统无 flock 时，用 mkdir 原子性加锁
    if ! mkdir "$LOCK_DIR" 2>/dev/null; then
        if [[ ! -d "$LOCK_DIR" ]]; then
            echo "错误: 无法创建锁目录 $LOCK_DIR，请检查目录和权限" >&2
            exit 1
        fi
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
    trap 'rm -rf "$LOCK_DIR"' EXIT
}

# ---------------------------------------------------------------------------
# 日志轮转
# ---------------------------------------------------------------------------
rotate_log() {
    local expired_minutes=$((LOG_RETENTION_DAYS * 1440))
    # cron 会在脚本启动前打开追加描述符，因此只截断当前文件，不能删除或改名。
    if [[ -f "${CRON_LOG}.1" ]]; then
        find "${CRON_LOG}.1" -type f -mmin "+$expired_minutes" -exec rm -f -- {} \; || return 1
    fi
    [[ -f "$CRON_LOG" ]] || return 0
    if [[ -n "$(find "$CRON_LOG" -type f -mmin "+$expired_minutes" -print -quit)" ]]; then
        : > "$CRON_LOG" || return 1
        return 0
    fi
    local size previous_day
    size=$(wc -c < "$CRON_LOG" 2>/dev/null | tr -d ' ')
    [[ -z "$size" ]] && return 0
    previous_day="$(find "$CRON_LOG" ! -newermt "$(date '+%F')" -print -quit)"
    if (( size > 0 )) && { (( size >= LOG_MAX_BYTES )) || [[ -n "$previous_day" ]]; }; then
        # 保留当前文件的 inode，使 cron 提前打开的追加描述符继续写当前日志。
        if ! cp -pf "$CRON_LOG" "${CRON_LOG}.1"; then
            echo "警告: 无法轮转日志 $CRON_LOG" >&2
            return 1
        fi
        : > "$CRON_LOG" || return 1
        echo "$(date '+%F %T') 日志已轮转: ${CRON_LOG}.1" >> "$CRON_LOG"
    fi
}

# ---------------------------------------------------------------------------
# 执行
# ---------------------------------------------------------------------------
acquire_lock
rotate_log

echo "二进制版本: $VERSION_LINE"
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
    --log-max-bytes="$LOG_MAX_BYTES"
    --log-retention-days="$LOG_RETENTION_DAYS"
    --slow-threshold="$SLOW_THRESHOLD"
    --max-latency="$MAX_LATENCY"
    --cooldown="$COOLDOWN"
    --max-slow="$MAX_SLOW"
    --rest-ratio="$REST_RATIO"
    --learn-file="$LEARN_FILE"
)

ARGS+=(--cacheheader="$CACHE_HEADER")
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

CHILD_PID=""
handle_signal() {
    trap '' INT TERM
    if [[ -n "$CHILD_PID" ]]; then
        kill -TERM "$CHILD_PID" 2>/dev/null || true
        wait "$CHILD_PID" 2>/dev/null || true
    fi
    exit 130
}
trap handle_signal INT TERM
"$EXECUTABLE" "${ARGS[@]}" &
CHILD_PID=$!
wait "$CHILD_PID"
EXIT_CODE=$?
trap - INT TERM

echo "========================================="
echo "预缓存任务结束: $(date '+%F %T')，退出码: $EXIT_CODE"

case "$EXIT_CODE" in
    0)   ;;
    3)   echo "警告: 首轮或复检的服务不可用率超过阈值，请检查 5xx / 429 / 超时" ;;
    4)   echo "警告: 已触发压力保护（超时 / 连续慢响应 / 连续失败），请先确认网站和 PHP 请求恢复，勿立即重跑" ;;
    130) echo "提示: 任务被信号中断" ;;
    *)   echo "错误: 任务异常退出" ;;
esac

exit "$EXIT_CODE"
