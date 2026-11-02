#!/usr/bin/env bash
# AI 数字员工 (dsh) deploy script
# Usage: ./build/deploy.sh [start|stop|restart|logs|status]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

IMAGE="${IMAGE:-dp-agt-dsh:0.1.0}"
CONTAINER="${CONTAINER:-dp-agt-dsh}"
HOST_PORT="${HOST_PORT:-20001}"
DATA_DIR="${DATA_DIR:-$PROJECT_ROOT/data}"
NETWORK="${NETWORK:-llm_net}"

echo "=== ensure data dir exists ==="
mkdir -p "$DATA_DIR"
mkdir -p "$DATA_DIR/tasks" "$DATA_DIR/logs" "$DATA_DIR/dsh-home"

echo "=== detect host uid/gid ==="
HOST_UID=$(id -u)
HOST_GID=$(id -g)

echo "=== check cfg.yml ==="
if [ ! -f "$DATA_DIR/cfg.yml" ]; then
  echo "[deploy] cfg.yml 不存在，从模板复制..."
  if [ -f "$PROJECT_ROOT/data/cfg.yml.template" ]; then
    cp "$PROJECT_ROOT/data/cfg.yml.template" "$DATA_DIR/cfg.yml"
    echo "[deploy] 请编辑 $DATA_DIR/cfg.yml 后再启动"
  fi
fi

echo "=== check cfg.csv.template ==="
if [ ! -f "$DATA_DIR/cfg.csv.template" ]; then
  cp "$PROJECT_ROOT/data/cfg.csv.template" "$DATA_DIR/cfg.csv.template" 2>/dev/null || true
  echo "[deploy] cfg.csv.template 已复制到数据目录"
fi

echo "=== ensure network exists ==="
if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
  echo "[deploy] Creating network: $NETWORK"
  docker network create "$NETWORK"
fi

echo "=== detect local IP ==="
LOCAL_IP=$(ip -4 addr show scope global | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | head -1)
if [ -z "$LOCAL_IP" ]; then
  LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
fi

case "${1:-start}" in
  start)
    echo "=== check image ==="
    if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
      echo "[deploy] Image not found: $IMAGE"
      echo "[deploy] Run: ./build/build.sh"
      exit 1
    fi

    echo "=== stop existing container ==="
    if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
      docker stop "$CONTAINER" 2>/dev/null || true
      docker rm "$CONTAINER" 2>/dev/null || true
    fi

    echo "[deploy] Starting container: $CONTAINER"
    docker run -d \
      --name "$CONTAINER" \
      --user "$HOST_UID:$HOST_GID" \
      --network "$NETWORK" \
      -p "$HOST_PORT:20001" \
      -v "$DATA_DIR:/app/data" \
      --restart unless-stopped \
      "$IMAGE"

    PROTO="http"
    echo ""
    echo "[deploy] 等待服务启动（最多 30 秒）..."
    echo ""
    if [ -n "$LOCAL_IP" ]; then
      echo "  管理后台:  $PROTO://$LOCAL_IP:$HOST_PORT"
      echo "  健康检查:  $PROTO://$LOCAL_IP:$HOST_PORT/health"
      echo "  业务接口:  POST $PROTO://$LOCAL_IP:$HOST_PORT/api/v1/<path>"
    else
      echo "  管理后台:  $PROTO://localhost:$HOST_PORT"
      echo "  健康检查:  $PROTO://localhost:$HOST_PORT/health"
      echo "  业务接口:  POST $PROTO://localhost:$HOST_PORT/api/v1/<path>"
    fi
    echo ""

    STARTED=0
    for ((i = 0; i < 30; i++)); do
      if docker logs "$CONTAINER" 2>&1 | grep -qE 'listening on'; then
        STARTED=1
        break
      fi
      sleep 1
    done

    if [ "$STARTED" -eq 1 ]; then
      echo "[deploy] ✓ 服务已启动（端口 $HOST_PORT），Ctrl+C 退出日志..."
    else
      echo "[deploy] ⚠ 30 秒内未检测到启动完成，继续等待..."
    fi
    echo ""
    docker logs -f --tail=0 "$CONTAINER"
    ;;

  stop)
    echo "[deploy] Stopping container: $CONTAINER"
    docker stop "$CONTAINER" || true
    docker rm "$CONTAINER" || true
    echo "[deploy] Stopped"
    ;;

  restart)
    "$0" stop
    "$0" start
    ;;

  logs)
    docker logs -f --tail=100 "$CONTAINER"
    ;;

  status)
    if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
      echo "Container: $CONTAINER"
      docker inspect "$CONTAINER" --format 'Status: {{.State.Status}} | Port: {{(index .NetworkSettings.Ports "20001/tcp" 0).HostPort}} | Started: {{.State.StartedAt}}'
    else
      echo "Container not running"
    fi
    ;;

  *)
    echo "Usage: $0 [start|stop|restart|logs|status]"
    exit 1
    ;;
esac