#!/bin/sh
# 打包后的冒烟测试：在本机的系统和 CPU 上运行刚打好的包。package 工作流在五种架构上各跑一次
# （Windows x64、macOS Intel / Apple Silicon、Linux x64 / ARM64），确认每个包里的程序都能启动：
#   modbus-cli   对内置模拟器读、写并回读
#   Web 版       （Linux）启动并打开页面
#   桌面版       （Windows、macOS）程序是本机架构，-version 能运行（CI 上没有显卡，不开窗口）；
#                Windows 兼容版用软件渲染，真的打开一次窗口
# 用法：build/smoke.sh（包先放在 dist/；版本号取自 cmd/modbus-ai/main.go，也可用 VERSION=x.y.z 指定）
set -eu
cd "$(dirname "$0")/.."

SOURCE_VERSION=$(sed -n 's/^var version = "\([0-9.]*\)".*/\1/p' cmd/modbus-ai/main.go)
[ -n "$SOURCE_VERSION" ] || { echo "cmd/modbus-ai/main.go 里没找到版本号"; exit 1; }
VERSION=${VERSION:-$SOURCE_VERSION}
[ "$VERSION" = "$SOURCE_VERSION" ] || { echo "VERSION=$VERSION 与源码版本 $SOURCE_VERSION 不一致"; exit 1; }
DIST=dist/v$VERSION
WORK=$(mktemp -d)
EXE=
case "$(uname -s)" in
Linux) SYS=linux ;;
Darwin) SYS=macos ;;
MINGW* | MSYS* | CYGWIN*) SYS=windows EXE=.exe ;;
*) echo "不认识的系统 $(uname -s)"; exit 1 ;;
esac
case "$(uname -m)" in
x86_64 | amd64) ARCH=x64 ;;
arm64 | aarch64) ARCH=arm64 ;;
*) echo "不认识的 CPU $(uname -m)"; exit 1 ;;
esac
echo "== 冒烟测试 $VERSION · $SYS $ARCH"

PIDS=
cleanup() {
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	[ "$SYS" = macos ] && hdiutil detach -quiet "$WORK/dmg" 2>/dev/null || true
}
trap cleanup EXIT

fail() { echo "冒烟测试失败：$*"; exit 1; }
# expect 检查输出里有某段文字
expect() { case "$2" in *"$1"*) ;; *) fail "期望输出里有 $1，实际输出：$2" ;; esac; }
unpack() {
	case "$1" in
	*.zip) if command -v unzip >/dev/null; then unzip -q "$1" -d "$2"; else
		powershell -NoProfile -Command "Expand-Archive -LiteralPath '$(cygpath -w "$1")' -DestinationPath '$(cygpath -w "$2")'"; fi ;;
	*) tar -xzf "$1" -C "$2" ;;
	esac
}

# 内置模拟器（换热站点表，数据不变）
go build -o "$WORK/modbus-sim$EXE" ./cmd/modbus-sim
"$WORK/modbus-sim$EXE" -listen 127.0.0.1:15020 -static &
PIDS="$PIDS $!"

# modbus-cli
PKG="$DIST/modbus-cli-$VERSION-$SYS-$ARCH"
[ "$SYS" = windows ] && PKG="$PKG.zip" || PKG="$PKG.tar.gz"
[ -f "$PKG" ] || fail "没有 $PKG"
unpack "$PKG" "$WORK"
CLI="$WORK/modbus-cli-$VERSION-$SYS-$ARCH/modbus-cli$EXE"
expect "modbus-cli $VERSION" "$("$CLI" version)"
i=0
until "$CLI" -target 127.0.0.1:15020 -timeout 500ms read 40001 1 >/dev/null 2>&1; do
	i=$((i + 1))
	[ $i -lt 40 ] || fail "模拟器没有启动"
	sleep 0.5
done
OUT=$("$CLI" -target 127.0.0.1:15020 -json -type float32 -order CDAB write 40347 16.5)
expect '"verified": true' "$OUT"
OUT=$("$CLI" -target 127.0.0.1:15020 -json -type float32 -order CDAB read 40347 2)
expect '"value": "16.5"' "$OUT"
echo "modbus-cli 通过：$PKG"

case "$SYS" in
linux)
	PKG="$DIST/ModbusAIStudio-Web-$VERSION-Linux-$ARCH.tar.gz"
	[ -f "$PKG" ] || fail "没有 $PKG"
	unpack "$PKG" "$WORK"
	WEB="$WORK/ModbusAIStudio-Web-$VERSION-Linux-$ARCH/modbus-web"
	expect "$VERSION" "$("$WEB" -version)"
	"$WEB" -listen 127.0.0.1:18502 -data "$WORK/web" -no-record >"$WORK/web.log" 2>&1 &
	PIDS="$PIDS $!"
	i=0
	until PAGE=$(curl -fsS http://127.0.0.1:18502/ 2>/dev/null); do
		i=$((i + 1))
		[ $i -lt 40 ] || fail "Web 版没有启动：$(cat "$WORK/web.log")"
		sleep 0.5
	done
	expect "<html" "$PAGE"
	echo "Web 版通过：$PKG"
	;;
windows)
	PKG="$DIST/ModbusAIStudio-$VERSION-Windows-x64.zip"
	[ -f "$PKG" ] || fail "没有 $PKG"
	unpack "$PKG" "$WORK"
	expect "$VERSION" "$("$WORK/ModbusAIStudio-$VERSION-Windows-x64/ModbusAIStudio.exe" -version)"
	echo "桌面版通过：$PKG"
	# 兼容版用 Mesa 软件渲染，没有显卡也能开窗口：真的启动一次，等日志（在程序目录的 data 里）写出窗口已显示
	PKG="$DIST/ModbusAIStudio-$VERSION-Windows-x64-Compat.zip"
	[ -f "$PKG" ] || fail "没有 $PKG"
	unpack "$PKG" "$WORK"
	DIR="$WORK/ModbusAIStudio-$VERSION-Windows-x64-Compat"
	expect "$VERSION" "$("$DIR/ModbusAIStudio.exe" -version)"
	"$DIR/ModbusAIStudio.exe" &
	PIDS="$PIDS $!"
	LOG="$DIR/data/ModbusAIStudio/app.log"
	i=0
	until grep -q "window shown" "$LOG" 2>/dev/null; do
		i=$((i + 1))
		[ $i -lt 240 ] || fail "兼容版两分钟内没有打开窗口：$(cat "$LOG" 2>/dev/null)"
		sleep 0.5
	done
	grep -q "startup render: Mesa llvmpipe" "$LOG" || fail "兼容版没有用 Mesa 软件渲染：$(cat "$LOG")"
	echo "兼容版通过：$PKG（$(grep -o 'window shown: .*' "$LOG")）"
	;;
macos)
	NAME=AppleSilicon WANT=arm64
	[ "$ARCH" = x64 ] && NAME=Intel WANT=x86_64
	PKG="$DIST/ModbusAIStudio-$VERSION-macOS-$NAME.dmg"
	[ -f "$PKG" ] || fail "没有 $PKG"
	hdiutil attach -quiet -nobrowse -readonly -mountpoint "$WORK/dmg" "$PKG"
	BIN=$(ls -d "$WORK"/dmg/*.app)/Contents/MacOS/modbus-ai
	[ "$(lipo -archs "$BIN")" = "$WANT" ] || fail "$BIN 是 $(lipo -archs "$BIN")，应为 $WANT"
	expect "$VERSION" "$("$BIN" -version)"
	echo "桌面版通过：$PKG"
	;;
esac
echo "== 全部通过"
