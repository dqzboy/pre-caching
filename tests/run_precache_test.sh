#!/usr/bin/env bash
# 隔离测试执行脚本，不访问任何站点，也不使用真实二进制。
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/precache-shell-test.XXXXXX")"
trap 'rm -rf "$TEST_ROOT"' EXIT
mkdir -p "$TEST_ROOT/fixture space/dist" "$TEST_ROOT/bin"
FIXTURE="$TEST_ROOT/fixture space"
cp "$REPO_DIR/run_precache.sh" "$FIXTURE/"

cat > "$TEST_ROOT/bin/uname" <<'SH'
#!/usr/bin/env bash
case "${1-}" in
    -s) echo Linux ;;
    -m) echo x86_64 ;;
esac
SH
cat > "$FIXTURE/dist/pre-caching-amd64" <<'SH'
#!/usr/bin/env bash
if [[ "${1-}" == -version ]]; then
    echo "pre-caching ${FAKE_VERSION:-2.6.0}"
    exit 0
fi
printf '%s\n' "$@" > "$CAPTURE_ARGS"
echo CURRENT_RUN_OUTPUT
if [[ "${FAKE_WAIT:-0}" == 1 ]]; then
    trap 'echo stopped > "$STOPPED_FILE"; exit 130' TERM
    echo $$ > "$CHILD_PID_FILE"
    while :; do sleep 0.1; done
fi
exit "${FAKE_EXIT:-0}"
SH
chmod +x "$TEST_ROOT/bin/uname" "$FIXTURE/dist/pre-caching-amd64"
export PATH="$TEST_ROOT/bin:$PATH"
export CAPTURE_ARGS="$TEST_ROOT/args"
export SITEMAP_URL="http://127.0.0.1/sitemap.xml"
export LOG_FILE=none
export ENV_FILE="$FIXTURE/precache.env"
export LOCK_FILE="$FIXTURE/.test.lock"
export CRON_LOG="$FIXTURE/cron.log"

cat > "$ENV_FILE" <<'SH'
CACHE_HEADER=x-cache
SLOW_THRESHOLD=3s
LEARN_FILE=/configured/slow-pages.txt
SH

fail() { echo "FAIL: $*" >&2; exit 1; }
contains_arg() {
    local expected="$1" arg
    while IFS= read -r arg; do
        [[ "$arg" == "$expected" ]] && return 0
    done < "$CAPTURE_ARGS"
    return 1
}
run_script() { bash "$FIXTURE/run_precache.sh"; }

# Linux 应能从 dist 找到二进制；显式空字符串应覆盖配置文件。
CACHE_HEADER= SLOW_THRESHOLD=5s LEARN_FILE="$FIXTURE/custom slow.txt" run_script > "$TEST_ROOT/output" 2>&1
contains_arg --cacheheader= || fail "empty CACHE_HEADER override lost"
contains_arg --size=1 || fail "safe concurrency default lost"
contains_arg --delay=0 || fail "unnecessary per-page delay restored"
contains_arg --slow-threshold=5s || fail "slow threshold override lost"
contains_arg "--learn-file=$FIXTURE/custom slow.txt" || fail "learn path with spaces lost"

# 未显式覆盖时从配置文件读取。
run_script > "$TEST_ROOT/output" 2>&1
contains_arg --cacheheader=x-cache || fail "cache header configuration lost"
contains_arg --slow-threshold=3s || fail "slow threshold configuration lost"

# 拒绝缺少新参数的旧二进制，支持后续主版本。
if FAKE_VERSION=2.5.0 run_script > "$TEST_ROOT/output" 2>&1; then
    fail "outdated binary accepted"
fi
FAKE_VERSION=3.0.0 run_script > "$TEST_ROOT/output" 2>&1
FAKE_VERSION=2.10.0 run_script > "$TEST_ROOT/output" 2>&1

# 锁文件/目录不可创建是配置错误，不能被当成另一个任务仍在运行。
if LOCK_FILE="$TEST_ROOT/missing/lock" run_script > "$TEST_ROOT/output" 2>&1; then
    fail "invalid lock path silently skipped"
fi

# 二进制的状态码应原样返回。
if FAKE_EXIT=4 run_script > "$TEST_ROOT/output" 2>&1; then
    fail "binary exit code lost"
else
    [[ $? == 4 ]] || fail "wrong binary exit code"
fi

# 模拟 cron 在启动前打开日志；轮转后当前输出应写入新文件。
echo OLD_LOG > "$CRON_LOG"
LOG_MAX_BYTES=1 run_script >> "$CRON_LOG" 2>&1
[[ "$(cat "$CRON_LOG")" == *CURRENT_RUN_OUTPUT* ]] || fail "current output stayed in rotated file"
[[ "$(cat "$CRON_LOG.1")" != *CURRENT_RUN_OUTPUT* ]] || fail "rotated log contains current run"

# 持锁期间第二次运行应跳过；TERM 应转发到子进程并等待其退出。
export CHILD_PID_FILE="$TEST_ROOT/child.pid"
export STOPPED_FILE="$TEST_ROOT/stopped"
FAKE_WAIT=1 bash "$FIXTURE/run_precache.sh" > "$TEST_ROOT/running-output" 2>&1 &
SCRIPT_PID=$!
for ((i=0; i<100; i++)); do
    [[ -f "$CHILD_PID_FILE" ]] && break
    sleep 0.05
done
if [[ ! -f "$CHILD_PID_FILE" ]]; then
    kill -TERM "$SCRIPT_PID" 2>/dev/null || true
    fail "child did not start"
fi
run_script > "$TEST_ROOT/locked-output" 2>&1
[[ "$(cat "$TEST_ROOT/locked-output")" == *仍在运行* ]] || fail "overlap lock failed"
kill -TERM "$SCRIPT_PID"
if wait "$SCRIPT_PID"; then
    fail "TERM exit code lost"
else
    [[ $? == 130 ]] || fail "TERM must exit 130"
fi
[[ -f "$STOPPED_FILE" ]] || fail "child did not receive TERM"
if kill -0 "$(cat "$CHILD_PID_FILE")" 2>/dev/null; then
    fail "child left running after script exited"
fi
[[ ! -d "${LOCK_FILE}.d" ]] || fail "fallback lock not released"

echo "PASS: wrapper configuration, version, dist, log rotation, locking and signal checks"
