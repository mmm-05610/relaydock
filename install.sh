#!/usr/bin/env sh
# RelayDock 一键安装：构建单二进制 + 生成 .env + systemd 服务（可选）
set -e
cd "$(dirname "$0")"

echo "==> 构建前端"
(cd web && npm ci && npm run build)

echo "==> 构建网关"
(cd gateway && CGO_ENABLED=0 go build -o ../bin/relaydock ./cmd/gateway)

echo "==> 生成配置"
if [ ! -f .env ]; then
  MASTER=$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  PANEL=$(openssl rand -hex 8 2>/dev/null || head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')
  cat > .env << ENV
PANEL_PASSWORD=$PANEL
GATEWAY_MASTER_KEY=$MASTER
ENV
  echo "    .env 已生成（PANEL_PASSWORD=$PANEL）"
fi

cat << TIP

安装完成：
  二进制      ./bin/relaydock
  启动        source .env && ./bin/relaydock   （工作目录需含 gateway/config.yaml 或已挂载）
  数据        ./data/relaydock.db（SQLite，自动建表）
  控制台      http://localhost:8080
TIP
