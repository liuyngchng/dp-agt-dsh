#!/usr/bin/env bash
# 构建 AI 数字员工 (dsh) 镜像
# 使用 docker buildx 多阶段构建，基于 ubuntu:24.04
set -euo pipefail

IMAGE_NAME="${IMAGE_NAME:-dp-agt-dsh}"
IMAGE_TAG="${IMAGE_TAG:-latest}"

cd "$(dirname "$0")/.."

echo "==> 构建镜像 ${IMAGE_NAME}:${IMAGE_TAG}"
echo "==> 使用 docker buildx (平台: linux/amd64)"

docker buildx build \
    --platform linux/amd64 \
    --load \
    -t "${IMAGE_NAME}:${IMAGE_TAG}" \
    .

echo "==> 构建完成: ${IMAGE_NAME}:${IMAGE_TAG}"
echo ""
echo "==> 运行:"
echo "    docker run -d --name dp-agt-dsh \\"
echo "      -p 20001:20001 \\"
echo "      -v \$(pwd)/data:/app/data \\"
echo "      ${IMAGE_NAME}:${IMAGE_TAG}"