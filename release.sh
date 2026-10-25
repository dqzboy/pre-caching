#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go)"
if [[ -z "$VERSION" ]]; then
    echo "错误: 无法从 main.go 读取 version 常量" >&2
    exit 1
fi

echo "版本: $VERSION"

UNFMT="$(gofmt -l main.go main_test.go)"
if [[ -n "$UNFMT" ]]; then
    echo "错误: Go 文件未格式化，先执行 gofmt -w main.go main_test.go" >&2
    exit 1
fi
go vet main.go main_test.go
go test -race -cover main.go main_test.go
bash -n run_precache.sh release.sh
bash tests/run_precache_test.sh

mkdir -p dist

echo "编译 linux/amd64 ..."
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/pre-caching-amd64 main.go

echo "编译 linux/arm64 ..."
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/pre-caching-arm64 main.go

echo
for f in dist/pre-caching-amd64 dist/pre-caching-arm64; do
    printf "  %-28s %s\n" "$f" "$(du -h "$f" | cut -f1)"
done
