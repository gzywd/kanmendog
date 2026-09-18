#!/usr/bin/env bash
# 下载 fnpack 并打包 .fpk（项目根目录需含 manifest）
set -e

VER="1.2.3"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"

if [ ! -x ./fnpack ]; then
  echo "下载 fnpack ${VER} ..."
  curl -fsSL -o fnpack "https://static2.fnnas.com/fnpack/fnpack-${VER}-linux-amd64"
  chmod +x fnpack
fi

echo "开始打包 ..."
./fnpack build
echo "完成。产物见当前目录的 *.fpk"
