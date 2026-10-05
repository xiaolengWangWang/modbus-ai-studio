#!/bin/sh
# 在 macOS / Linux 上交叉编译 Windows x64 绿色版：zip 解压即用，不用安装。
# 需要 mingw-w64（brew install mingw-w64），界面（OpenGL）和报文记录（SQLite）都要 cgo。
# 用法：VERSION=0.10.2 build/windows.sh
# 图标各尺寸取自 assets/icon/windows/，zip 里的说明模板在 platform/windows/。
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-0.10.2}
DIST=dist
STAGE="$DIST/ModbusAIStudio-$VERSION-Windows-x64"
ZIP="$STAGE.zip"
CC_WIN=x86_64-w64-mingw32-gcc
command -v "$CC_WIN" >/dev/null || { echo "缺少 $CC_WIN，先执行 brew install mingw-w64"; exit 1; }

rm -rf "$STAGE" "$ZIP"
mkdir -p "$STAGE"

# 图标、版本信息和清单（高 DPI、普通权限运行）编进 exe。
# 资源名 GLFW_ICON 由 GLFW 用作窗口标题栏/任务栏图标，也供 Explorer 显示。
SYSO=cmd/modbus-ai/rsrc_windows_amd64.syso
trap 'rm -f "$SYSO"' EXIT
run_winres() {
	if [ -n "${GO_WINRES:-}" ]; then
		"$GO_WINRES" "$@"
	else
		GOOS= GOARCH= go run github.com/tc-hib/go-winres@v0.3.3 "$@"
	fi
}
(cd cmd/modbus-ai && run_winres make \
	--arch amd64 --in ../../assets/icon/windows/winres.json \
	--product-version "$VERSION" --file-version "$VERSION")

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
sed -e "s/{{VERSION}}/$VERSION/g" -e 's/$/\r/' platform/windows/README.txt >>"$STAGE/README.txt"

(cd "$DIST" && zip -qr "$(basename "$ZIP")" "$(basename "$STAGE")")
echo "  $ZIP"
shasum -a 256 "$ZIP"
