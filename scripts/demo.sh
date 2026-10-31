#!/usr/bin/env bash
# Phase 0 验收演示脚本（macOS / Linux）
# 用法：./scripts/demo.sh
set -euo pipefail

PORT="${PORT:-18099}"
BASE="http://127.0.0.1:${PORT}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

step() { printf '\n=== %s ===\n' "$1"; }
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

if command -v jq >/dev/null 2>&1; then
  pick() { jq -r "$1"; }
else
  pick() { jget "json.dumps($1)"; }
fi

go run ./cmd/server -addr ":${PORT}" -seed >/tmp/ziniao-demo.log 2>&1 &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null || true' EXIT

step "等待服务启动"
for _ in $(seq 1 60); do
  if curl -sf "${BASE}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done

post() { curl -s -X POST "${BASE}$1" -H 'Content-Type: application/json' -d "$2"; }
post_auth() { curl -s -X POST "${BASE}$1" -H "Authorization: Bearer $2" -H 'Content-Type: application/json' -d "$3"; }
auth() { curl -s "${BASE}$1" -H "Authorization: Bearer $2"; }

step "1) 登录老板账号（seed: demo / Zn@12345678）"
OWNER=$(post /api/v1/auth/login '{"username":"demo","password":"Zn@12345678"}')
TOKEN=$(echo "$OWNER" | pick '.token')
echo "tenant=$(echo "$OWNER" | pick '.tenant_id')"

step "2) 写操作：新建店铺"
SHOP=$(post_auth /api/v1/shops "$TOKEN" '{"name":"演示店铺-亚马逊美国站","platform":"Amazon","site":"US","url":"/shops"}')
echo "shop id=$(echo "$SHOP" | pick '.id')"

sleep 1

step "3) 审计日志：这次写操作必然留痕"
auth "/api/v1/audit/logs?action=shop.create" "$TOKEN" | pick '.items[0] | {action, shop_id, terminal_ip, hash}'

step "4) 哈希链完整性校验"
auth /api/v1/audit/verify "$TOKEN" | pick '{ok, checked}'

step "5) 财务角色：字段级遮蔽（服务端已打码）"
FIN_TOKEN=$(post /api/v1/auth/login '{"username":"finance","password":"Zn@12345678"}' | pick '.token')
auth /api/v1/shops "$FIN_TOKEN" | pick '.items[0] | {name, profit, masked_fields}'

step "6) DOM 注入层策略（第三层）"
auth "/api/v1/iam/dom-policy?path=/finance" "$FIN_TOKEN" | pick '{deny_selectors, mask_selectors}'

step "7) 明文导出必须二次审批"
curl -s -o /tmp/ziniao-export.csv -w 'HTTP %{http_code}\n' "${BASE}/api/v1/audit/logs/export" -H "Authorization: Bearer ${TOKEN}"

step "演示完成（日志：/tmp/ziniao-demo.log）"
