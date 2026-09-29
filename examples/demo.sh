#!/usr/bin/env bash
# 启动一个服务端和三个本地虚拟客户端，发布多个整份配置版本并观察状态。
set -euo pipefail
cd "$(dirname "$0")/.."

DATA=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT

go run ./cmd/server -addr 127.0.0.1:18080 -data "$DATA/server.json" &
sleep 1
for i in 1 2 3; do
  port=$((19000 + i))
  go run ./cmd/client -id "c$i" -addr "127.0.0.1:$port" -server http://127.0.0.1:18080 -data "$DATA/c$i" &
  sleep 0.2
done
sleep 1

publish() {
  curl -sX POST 127.0.0.1:18080/publish \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$1\",\"sequence\":$2,\"content\":{\"v\":$2}}"
  echo
}

publish v1 1
sleep 1
publish v2 2
sleep 1
publish v3 3
sleep 1

echo '== 状态 =='
curl -s 127.0.0.1:18080/status
echo
echo '== 对账（回连客户端 /actual）=='
curl -sX POST 127.0.0.1:18080/reconcile
echo
