# syntax=docker/dockerfile:1
# AI 数字员工 (dsh) — 多阶段构建
# 基于 ubuntu:24.04，用 docker buildx 构建

# ---------- 阶段 1: 构建 Go 程序 ----------
FROM ubuntu:24.04 AS builder

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        golang-go \
        git \
        gcc \
        libc6-dev \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# 先复制 go.mod / go.sum，利用缓存
COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download || true

# 复制源码
COPY . .

# 构建（CGO 需要 gcc，SQLite 走纯 Go 驱动则不需要；这里保留 CGO 兼容）
RUN GOPROXY=https://goproxy.cn,direct CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/dp-agt-dsh ./cmd/dp-agt-dsh

# ---------- 阶段 2: 运行时 ----------
FROM ubuntu:24.04

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        jq \
        libstdc++6 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 复制 Go 二进制
COPY --from=builder /out/dp-agt-dsh /app/dp-agt-dsh

# 复制 dsh 运行时二进制 + sidecar
COPY bin/deepseek-harness-sdk-runtime-linux-x64 /app/bin/deepseek-harness-sdk-runtime-linux-x64
COPY bin/deepseek-harness-sdk-runtime-linux-x64-rg /app/bin/deepseek-harness-sdk-runtime-linux-x64-rg
COPY bin/linux-x64 /app/bin/linux-x64

# 复制提示词 + 前端
COPY prompts/ /app/prompts/
COPY web/ /app/web/

# 数据目录（容器挂载卷）
RUN mkdir -p /app/data/tasks /app/data/logs /app/data/dsh-home

# 复制配置模板
COPY data/cfg.yml.template /app/data/cfg.yml.template
COPY data/cfg.csv.template /app/data/cfg.csv.template

EXPOSE 20001

ENV DATA_DIR=/app/data

CMD ["/app/dp-agt-dsh"]