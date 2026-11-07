#!/usr/bin/env bash
# AI 数字员工 (dsh) Docker 镜像构建脚本
# 用法: ./build/build.sh [version] [proxy...]
#   代理参数（可选，仅构建时使用，不写入镜像）:
#     http_proxy=<url>  https_proxy=<url>  no_proxy=<url>
set -euo pipefail

BUILD_START=$(date +%s)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-dp-agt-dsh}"

usage() {
  cat <<'EOF'
用法: ./build/build.sh [version] [proxy...]

参数说明:
  version                 镜像版本号（位置参数）
  -h | --help             显示本帮助并退出

代理参数（可选，不带 = 时继承环境变量，仅构建时使用，不写入镜像）:
  http_proxy=<url>        设置 HTTP 代理
  https_proxy=<url>       设置 HTTPS 代理
  no_proxy=<url>          设置不走代理的地址列表

示例:
  ./build/build.sh 0.1.0 http_proxy=http://127.0.0.1:7890
  VERSION=0.1.0 ./build/build.sh
EOF
}

# ── 参数解析 ──
TAG_VERSION=""
HTTP_PROXY_VAL=""
HTTPS_PROXY_VAL=""
NO_PROXY_VAL=""

for arg in "$@"; do
  case "$arg" in
    http_proxy=*|HTTP_PROXY=*)   HTTP_PROXY_VAL="${arg#*=}" ;;
    https_proxy=*|HTTPS_PROXY=*) HTTPS_PROXY_VAL="${arg#*=}" ;;
    no_proxy=*|NO_PROXY=*)       NO_PROXY_VAL="${arg#*=}" ;;
    -h|--help|help)              usage; exit 0 ;;
    -*)                          echo "WARNING: 未知参数 '$arg'，已忽略（用 -h 查看用法）" ;;
    *)                           TAG_VERSION="$arg" ;;
  esac
done

# 版本号：$VERSION env > 位置参数 > "0.1.0"
TAG_VERSION="${TAG_VERSION:-${VERSION:-0.1.0}}"

# 不带 = 时继承环境变量
HTTP_PROXY_VAL="${HTTP_PROXY_VAL:-${HTTP_PROXY:-${http_proxy:-}}}"
HTTPS_PROXY_VAL="${HTTPS_PROXY_VAL:-${HTTPS_PROXY:-${https_proxy:-}}}"
NO_PROXY_VAL="${NO_PROXY_VAL:-${NO_PROXY:-${no_proxy:-}}}"

# 自动补全 scheme（裸 IP:端口 → http://IP:端口）
add_scheme() {
  local v="$1"
  [[ -z "$v" || "$v" == *"://"* ]] && { printf '%s' "$v"; return; }
  printf 'http://%s' "$v"
}
HTTP_PROXY_VAL="$(add_scheme "$HTTP_PROXY_VAL")"
HTTPS_PROXY_VAL="$(add_scheme "$HTTPS_PROXY_VAL")"

FULL_TAG="${IMAGE_NAME}:${TAG_VERSION}"

echo "============================================"
echo " Building ${FULL_TAG}"
echo "============================================"
if [[ -n "$HTTP_PROXY_VAL" || -n "$HTTPS_PROXY_VAL" ]]; then
  echo " 代理 (仅构建时): http=$HTTP_PROXY_VAL https=$HTTPS_PROXY_VAL no_proxy=$NO_PROXY_VAL"
fi
echo ""

# ── 前置检查：dsh 运行时二进制是否已就位 ──
DSH_BIN="$PROJECT_ROOT/bin/deepseek-harness-sdk-runtime-linux-x64"
DSH_RG="$PROJECT_ROOT/bin/deepseek-harness-sdk-runtime-linux-x64-rg"
if [[ ! -x "$DSH_BIN" || ! -x "$DSH_RG" ]]; then
  echo "ERROR: dsh 运行时二进制缺失"
  echo "       请先运行: ./fetch-dsh-runtime.sh"
  echo "       或手动放置到 bin/ 目录"
  exit 1
fi
echo "  dsh 运行时已就位: $(du -h "$DSH_BIN" | cut -f1)"
echo ""

# ---- Docker build args ----
DOCKER_BUILD_ARGS=()
if [[ -n "$HTTP_PROXY_VAL" || -n "$HTTPS_PROXY_VAL" ]]; then
  DOCKER_BUILD_ARGS=(
    --build-arg "HTTP_PROXY=$HTTP_PROXY_VAL"
    --build-arg "HTTPS_PROXY=$HTTPS_PROXY_VAL"
    --build-arg "NO_PROXY=$NO_PROXY_VAL"
  )
fi

# ---- 检查本地基础镜像 ----
if ! docker image inspect ubuntu:24.04 >/dev/null 2>&1; then
  echo "ERROR: 本地缺少 ubuntu:24.04 基础镜像"
  echo "       请先: docker pull ubuntu:24.04"
  exit 1
fi

# ---- HTTPS certs ----
# 服务强制 HTTPS，证书必须存在；缺少 openssl 时直接报错退出。
# 自签名证书生成一次后复用，与 dp-agt build.sh 参数一致。
CERTS_DIR="$PROJECT_ROOT/deploy/certs"
CERT_FILE="$CERTS_DIR/cert.pem"
KEY_FILE="$CERTS_DIR/key.pem"
mkdir -p "$CERTS_DIR"

if [[ ! -f "$CERT_FILE" || ! -f "$KEY_FILE" ]]; then
  if ! command -v openssl &>/dev/null; then
    echo "[build] ERROR: 未找到 openssl，无法生成 HTTPS 证书（服务强制 HTTPS）" >&2
    exit 1
  fi
  echo "[build] 生成自签名 HTTPS 证书 (RSA-2048, 10年)"
  openssl req -x509 -newkey rsa:2048 -sha256 -days 3650 -nodes \
    -keyout "$KEY_FILE" \
    -out "$CERT_FILE" \
    -subj "/C=CN/O=AI数字员工/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
fi
echo "[build] HTTPS 证书已就绪: $CERTS_DIR"

echo "=== Docker buildx build ==="
docker buildx build \
    "${DOCKER_BUILD_ARGS[@]+"${DOCKER_BUILD_ARGS[@]}"}" \
    --platform linux/amd64 \
    -t "${FULL_TAG}" \
    -f "$SCRIPT_DIR/Dockerfile" \
    --load \
    "$PROJECT_ROOT"

# 清理 dangling 镜像
echo ""
echo "=== 清理 dangling 镜像 ==="
docker image prune -f --filter "dangling=true" || true

BUILD_ELAPSED=$(( $(date +%s) - BUILD_START ))
echo "============================================"
echo " Build complete: ${FULL_TAG}"
printf " 总耗时: %dm %ds\n" $(( BUILD_ELAPSED / 60 )) $(( BUILD_ELAPSED % 60 ))
echo "============================================"
echo ""
echo "Run with:"
echo ""
echo "  ./build/deploy.sh"
echo ""
