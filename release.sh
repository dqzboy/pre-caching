#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

usage() {
    echo "用法: ./release.sh [版本号]"
    echo "不带参数进入编译菜单；传入版本号则直接编译全部架构，例如 ./release.sh 2.7.3"
}

# 交互终端使用 Readline，让退格、Delete 和方向键正常编辑输入。
# 管道输入仍用普通 read，便于自动构建。
read_input() {
    local prompt="$1" target="$2"
    if [[ -t 0 ]]; then
        IFS= read -e -r -p "$prompt" "$target"
    else
        printf '%s' "$prompt"
        IFS= read -r "$target"
    fi
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
fi
if (( $# > 1 )); then
    usage >&2
    exit 1
fi

RELEASE_ARCHES=(amd64 arm64)
if (( $# == 0 )); then
    printf '\n预缓存工具 · 发布构建\n\n'
    echo "  1. 编译全部架构（amd64 + arm64，默认）"
    echo "  2. 仅编译 amd64"
    echo "  3. 仅编译 arm64"
    echo "  0. 退出"
    echo
    while true; do
        if ! read_input '请选择（回车编译全部）: ' RELEASE_CHOICE; then
            echo >&2
            echo "错误: 未读取到菜单选项" >&2
            exit 1
        fi
        case "${RELEASE_CHOICE:-1}" in
            1) break ;;
            2) RELEASE_ARCHES=(amd64); break ;;
            3) RELEASE_ARCHES=(arm64); break ;;
            0) echo "已退出"; exit 0 ;;
            *) echo "请输入 0、1、2 或 3" ;;
        esac
    done
fi

while true; do
    if (( $# == 1 )); then
        RELEASE_NUMBER="$1"
    else
        if ! read_input '请输入发布版本号（例如 2.7.3）: ' RELEASE_NUMBER; then
            echo >&2
            echo "错误: 未读取到版本号" >&2
            exit 1
        fi
    fi
    # 输入无需 v；兼容粘贴完整版本，发布时统一添加一个 v 前缀。
    RELEASE_NUMBER="${RELEASE_NUMBER#v}"
    if [[ "$RELEASE_NUMBER" =~ ^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$ ]]; then
        break
    fi
    echo "版本号格式应为三段数字，例如 2.7.3" >&2
    (( $# == 0 )) || exit 1
done
RELEASE_VERSION="v${RELEASE_NUMBER}"
echo
echo "发布版本: $RELEASE_VERSION"

UNFMT="$(gofmt -l main.go main_test.go)"
if [[ -n "$UNFMT" ]]; then
    echo "错误: Go 文件未格式化，先执行 gofmt -w main.go main_test.go" >&2
    exit 1
fi
go vet main.go main_test.go
go test -race -cover main.go main_test.go
bash -n run_precache.sh release.sh

mkdir -p dist

for RELEASE_ARCH in "${RELEASE_ARCHES[@]}"; do
    echo "编译 linux/$RELEASE_ARCH ..."
    GOOS=linux GOARCH="$RELEASE_ARCH" go build -trimpath \
        -ldflags="-s -w -X main.version=$RELEASE_VERSION" \
        -o "dist/pre-caching-$RELEASE_ARCH" main.go
done

echo
echo "构建完成: $RELEASE_VERSION"
for RELEASE_ARCH in "${RELEASE_ARCHES[@]}"; do
    RELEASE_FILE="dist/pre-caching-$RELEASE_ARCH"
    printf '  %-28s %s\n' "$RELEASE_FILE" "$(du -h "$RELEASE_FILE" | cut -f1)"
done
