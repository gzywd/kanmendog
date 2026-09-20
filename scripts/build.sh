#!/usr/bin/env bash
# KanmenDog 构建脚本：编译 Go 二进制 + fnpack 打包 .fpk
# v1.9.2：ui/config 改走 fnOS Nginx 反代模式（移除 port 声明），修复网关/HTTPS 访问不了；二进制加 -ldflags="-s -w" 剥离调试符号，版本号从 manifest 自动读取
#
# 用法：./scripts/build.sh
# 前置依赖：
#   - Go 1.21+ 编译环境（用于编译）
#   - fnpack 工具（飞牛官方打包工具，无则自动下载）
set -e

FNPACK_VER="1.2.3"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"

# 从 manifest 读取版本号和应用名
APP_VER="$(grep '^version=' manifest | cut -d= -f2 | tr -d '[:space:]')"
APP_NAME="$(grep '^appname=' manifest | cut -d= -f2 | tr -d '[:space:]')"

GO_SRC="src/kanmendog"
APP_BIN="app/kanmendog"

# ==================== 步骤 1：编译 Go 二进制 ====================
echo "=========================================="
echo "  KanmenDog 构建脚本 v${APP_VER}"
echo "=========================================="
echo ""
echo "[1/3] 编译 Go 二进制..."

if ! command -v go &>/dev/null; then
  echo "错误：未找到 Go 编译环境，请安装 Go 1.21+"
  exit 1
fi

# 确保输出目录存在
mkdir -p app

# 编译（静态链接，兼容 fnOS；-s -w 剥离符号表与调试信息减小体积）
(cd "$GO_SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "../../$APP_BIN" .)

if [ ! -x "$APP_BIN" ]; then
  echo "错误：编译失败，未生成 $APP_BIN"
  exit 1
fi

BIN_SIZE=$(ls -lh "$APP_BIN" | awk '{print $5}')
BIN_TIME=$(stat -c %y "$APP_BIN" | cut -d. -f1)
echo "      编译成功: $APP_BIN ($BIN_SIZE, $BIN_TIME)"

# 同步 UI 资源：src/kanmendog/ui/ → app/ui/（确保 fnpack 打包的 UI 与二进制内嵌 UI 一致）
# 这是关键：fnOS 从 app/ui/ 加载 UI（含 ui/config 入口定义），必须与源码保持一致
echo "[1.5/3] 同步 UI 资源到 app/ui/..."
mkdir -p app/ui
rm -rf app/ui/index.html app/ui/config app/ui/images
cp -r "$GO_SRC/ui/index.html" app/ui/index.html
cp -r "$GO_SRC/ui/config" app/ui/config
cp -r "$GO_SRC/ui/images" app/ui/images
echo "      UI 同步完成：index.html / config / images"

# ==================== 步骤 2：打包 fpk ====================
echo ""
echo "[2/3] 调用 fnpack 打包..."

if [ ! -x ./fnpack ]; then
  echo "      下载 fnpack ${FNPACK_VER} ..."
  curl -fsSL -o fnpack "https://static2.fnnas.com/fnpack/fnpack-${FNPACK_VER}-linux-amd64"
  chmod +x fnpack
fi

./fnpack build

# ==================== 步骤 3：重命名产物 ====================
echo ""
echo "[3/3] 整理输出文件..."

if [ -n "$APP_VER" ] && [ -n "$APP_NAME" ]; then
  OLD_FPK="${APP_NAME}.fpk"
  NEW_FPK="${APP_NAME}-v${APP_VER}.fpk"
  if [ -f "$OLD_FPK" ]; then
    mv "$OLD_FPK" "$NEW_FPK"
    FPK_SIZE=$(ls -lh "$NEW_FPK" | awk '{print $5}')
    echo ""
    echo "=========================================="
    echo "  ✅ 构建完成！"
    echo "  版本: v${APP_VER}"
    echo "  二进制: $APP_BIN ($BIN_SIZE)"
    echo "  包文件: ${HERE}/${NEW_FPK} ($FPK_SIZE)"
    echo "=========================================="
  else
    echo "警告：未找到预期的 fpk 文件 ${OLD_FPK}"
    ls -la *.fpk 2>/dev/null || echo "无 fpk 产物"
  fi
else
  echo "完成。产物见当前目录的 *.fpk"
fi
