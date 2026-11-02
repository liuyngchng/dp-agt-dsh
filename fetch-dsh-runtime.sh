#!/usr/bin/env bash
# 下载 dsh 运行时二进制（从 PyPI 提取）
# 用法: ./fetch-dsh-runtime.sh
set -euo pipefail

cd "$(dirname "$0")"

BIN_DIR="./bin"
PLATFORM="manylinux_2_28_x86_64"
BIN_NAME="deepseek-harness-sdk-runtime-linux-x64"
RG_NAME="deepseek-harness-sdk-runtime-linux-x64-rg"

# 已有则跳过
if [ -x "$BIN_DIR/$BIN_NAME" ] && [ -x "$BIN_DIR/$RG_NAME" ] && [ -d "$BIN_DIR/linux-x64" ]; then
    echo "==> dsh 运行时已存在: $BIN_DIR/$BIN_NAME ($(du -h "$BIN_DIR/$BIN_NAME" | cut -f1))"
    exit 0
fi

echo "==> 从 PyPI 下载 deepseek-harness-runtime-bin ..."
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

pip download deepseek-harness-runtime-bin --no-deps -d "$TMPDIR" 2>&1 | tail -1
WHEEL=$(ls "$TMPDIR"/*${PLATFORM}*.whl 2>/dev/null | head -1)

if [ -z "$WHEEL" ]; then
    echo "ERROR: 未找到匹配平台的 wheel 文件"
    echo "      预期: *-${PLATFORM}*.whl"
    ls -la "$TMPDIR/" 2>/dev/null || true
    exit 1
fi

echo "==> 解压 wheel: $(basename "$WHEEL")"
unzip -q "$WHEEL" -d "$TMPDIR/wheel"

echo "==> 复制二进制到 $BIN_DIR/"
mkdir -p "$BIN_DIR"
cp "$TMPDIR/wheel/deepseek_harness_runtime/runtime/$BIN_NAME" "$BIN_DIR/"
cp "$TMPDIR/wheel/deepseek_harness_runtime/runtime/$RG_NAME" "$BIN_DIR/"
cp -r "$TMPDIR/wheel/deepseek_harness_runtime/runtime/linux-x64" "$BIN_DIR/"
chmod +x "$BIN_DIR/$BIN_NAME" "$BIN_DIR/$RG_NAME"

echo "==> 完成: $BIN_DIR/$BIN_NAME ($(du -h "$BIN_DIR/$BIN_NAME" | cut -f1))"