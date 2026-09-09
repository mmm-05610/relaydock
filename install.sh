#!/usr/bin/env sh
# RelayDock 一键安装：构建单二进制 + 生成 .env + systemd 服务（可选）
set -e
cd "$(dirname "$0")"

echo "==> 构建（前端内嵌 + 网关单二进制）"
./build.sh "${VERSION:-dev}"

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
  二进制      ./bin/relaydock（控制台已内嵌，单文件完整）
  启动        source .env && ./bin/relaydock   （工作目录需含 gateway/config.yaml 或已挂载）
  数据        ./data/relaydock.db（SQLite，自动建表）
  控制台      http://localhost:8080
TIP
