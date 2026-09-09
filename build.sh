#!/usr/bin/env sh
# 构建：前端 dist → 内嵌 → 单二进制
set -e
cd "$(dirname "$0")"
VERSION="${1:-dev}"

echo "==> 构建前端"
(cd web && npm ci --silent && npm run build --silent)

echo "==> 拷贝 dist 进内嵌目录"
rm -rf gateway/internal/console/dist
mkdir -p gateway/internal/console
cp -r web/dist gateway/internal/console/dist

echo "==> 构建网关（内嵌控制台的单二进制）"
(cd gateway && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X gateway/internal/gateway.Version=$VERSION" -o ../bin/relaydock ./cmd/gateway)

echo "✅ bin/relaydock 构建完成（版本 $VERSION，控制台已内嵌）"
