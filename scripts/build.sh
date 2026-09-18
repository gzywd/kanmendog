#!/usr/bin/env bash
# 下载 fnpack 并打包 .fpk（项目根目录需含 manifest）
# v1.3.1：输出文件名带版本号
set -e

VER="1.2.3"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"

# 从 manifest 读取版本号
APP_VER="$(grep '^version=' manifest | cut -d= -f2 | tr -d '[:space:]')"
APP_NAME="$(grep '^appname=' manifest | cut -d= -f2 | tr -d '[:space:]')"

if [ ! -x ./fnpack ]; then
  echo "下载 fnpack ${VER} ..."
  curl -fsSL -o fnpack "https://static2.fnnas.com/fnpack/fnpack-${VER}-linux-amd64"
  chmod +x fnpack
fi

echo "开始打包 ..."
./fnpack build

# 重命名为带版本号的文件名
if [ -n "$APP_VER" ] && [ -n "$APP_NAME" ]; then
  OLD_FPK="${APP_NAME}.fpk"
  NEW_FPK="${APP_NAME}-v${APP_VER}.fpk"
  if [ -f "$OLD_FPK" ]; then
    mv "$OLD_FPK" "$NEW_FPK"
    echo "完成。产物: ${HERE}/${NEW_FPK}"
  else
    echo "警告：未找到预期的 fpk 文件 ${OLD_FPK}"
    ls -la *.fpk 2>/dev/null || echo "无 fpk 产物"
  fi
else
  echo "完成。产物见当前目录的 *.fpk（无法从 manifest 读取版本号，使用默认文件名）"
fi
