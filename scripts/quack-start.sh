#!/bin/bash
# quack-start.sh — 一键启动 DuckDB Quack 服务
set -e

DATA_DIR="${1:-./quack-data}"
TOKEN="${2:-quack_$(date +%s)}"
PORT="${3:-9491}"
DB="${DATA_DIR}/default.db"

mkdir -p "$DATA_DIR"

echo "🚀 启动 Quack 服务"
echo "   数据库: $DB"
echo "   端口:   $PORT"
echo "   Token:  $TOKEN"
echo ""

# 清理旧进程
pkill -f "duckdb.*quack\|duckdb.*$PORT" 2>/dev/null || true
sleep 1

# 启动 DuckDB + Quack（保持进程存活）
(echo "LOAD quack; CALL quack_serve('quack:localhost:$PORT', token := '$TOKEN');" && cat) | \
    duckdb "$DB" > /dev/null 2>&1 &

sleep 2

# 验证
if ! ss -tlnp 2>/dev/null | grep -q ":$PORT"; then
    echo "❌ 启动失败，端口 $PORT 无监听"
    exit 1
fi

echo "✅ Quack 服务运行中"
echo ""
echo "📋 连接命令:"
echo ""
echo "  duckdb -c \""
echo "    LOAD quack;"
echo "    CREATE SECRET (TYPE QUACK, TOKEN '$TOKEN');"
echo "    ATTACH 'quack:localhost:$PORT' AS remote;"
echo "  \""
echo ""
echo "🛑 停止: pkill -f 'duckdb.*$DB'"
echo "📁 Token 已保存: $DATA_DIR/.token"
echo "$TOKEN" > "$DATA_DIR/.token"
echo "$(ps aux | grep "duckdb.*$DB" | grep -v grep | awk '{print $2}')" > "$DATA_DIR/.pid"
