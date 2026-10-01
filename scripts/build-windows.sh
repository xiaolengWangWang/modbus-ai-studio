#!/bin/sh
# 在 macOS / Linux 上交叉编译 Windows x64 绿色版：zip 解压即用，不用安装。
# 需要 mingw-w64（brew install mingw-w64），界面（OpenGL）和报文记录（SQLite）都要 cgo。
# 用法：VERSION=0.8.2 scripts/build-windows.sh
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-0.8.2}
DIST=dist
STAGE="$DIST/ModbusAIStudio-$VERSION-Windows-x64"
ZIP="$STAGE.zip"
CC_WIN=x86_64-w64-mingw32-gcc
command -v "$CC_WIN" >/dev/null || { echo "缺少 $CC_WIN，先执行 brew install mingw-w64"; exit 1; }

rm -rf "$STAGE" "$ZIP"
mkdir -p "$STAGE"

# 图标、版本信息和清单（高 DPI、普通权限运行）编进 exe
go run ./scripts/icon "$DIST/icon-1024.png" 1024
SYSO=cmd/modbus-ai/rsrc_windows_amd64.syso
trap 'rm -f "$SYSO"' EXIT
(cd cmd/modbus-ai && GOOS= GOARCH= go run github.com/tc-hib/go-winres@v0.3.3 simply \
	--arch amd64 --icon "../../$DIST/icon-1024.png" --manifest gui \
	--product-name "Modbus AI Studio" --file-description "Modbus AI Studio" \
	--product-version "$VERSION" --file-version "$VERSION" \
	--original-filename ModbusAIStudio.exe --copyright "Copyright (c) 2026 xiaolengWangWang")

echo "编译 Windows x64…"
export GOOS=windows GOARCH=amd64
# -static：把 MinGW 运行库静态链接进去，干净的 Windows 上不缺 libwinpthread 之类的 DLL
CGO_ENABLED=1 CC=$CC_WIN go build -trimpath \
	-ldflags "-s -w -H windowsgui -X main.version=$VERSION -extldflags=-static" \
	-o "$STAGE/ModbusAIStudio.exe" ./cmd/modbus-ai
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$STAGE/modbus-sim.exe" ./cmd/modbus-sim
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$STAGE/modbus-cli.exe" ./cmd/modbus-cli
unset GOOS GOARCH

# 说明文件用 UTF-8 BOM + CRLF，记事本打开不乱码
printf '\357\273\277' >"$STAGE/README.txt"
sed 's/$/\r/' <<EOF >>"$STAGE/README.txt"
Modbus AI Studio ${VERSION}（Windows x64 绿色版）

双击 ModbusAIStudio.exe 运行，不用安装。
- 系统要求：Windows 10 / 11 64 位。不支持 Windows 7 / 8。
- 需要显卡支持 OpenGL 2.1，不支持时程序会弹窗说明。处理办法：下载 Mesa3D 软件渲染版的
  opengl32.dll（https://github.com/pal1000/mesa-dist-win 的 x64 目录），放到 exe 同一目录再运行。
- 报文记录保存在 %AppData%\\ModbusAIStudio\\packets.db，保留 7 天。
- modbus-sim.exe 是命令行模拟从站（换热站示例点表），modbus-cli.exe 是命令行主站，
  在命令行里加 -h 查看用法。

项目主页：https://github.com/xiaolengWangWang/modbus-ai-studio
EOF

(cd "$DIST" && zip -qr "$(basename "$ZIP")" "$(basename "$STAGE")")
echo "  $ZIP"
shasum -a 256 "$ZIP"
