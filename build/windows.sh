#!/bin/sh
# 在 macOS / Linux 上交叉编译 Windows x64 绿色版：zip 解压即用，不用安装。打两个包：
#   ModbusAIStudio-x.y.z-Windows-x64.zip         普通版，用显卡的 OpenGL
#   ModbusAIStudio-x.y.z-Windows-x64-Compat.zip  兼容版：go-legacy-win7 编译（Win7/8/8.1 也能运行），
#                                                 带 Mesa 软件渲染（不依赖显卡），数据放在程序目录的 data 文件夹
# 需要 mingw-w64（brew install mingw-w64），界面（OpenGL）和报文记录（SQLite）都要 cgo；
# 兼容版还要 7z（apt install p7zip-full / brew install sevenzip）解开 Mesa 的 .7z。
# 用法：build/windows.sh（版本号取自 cmd/modbus-ai/main.go，也可用 VERSION=x.y.z 指定）
# 图标各尺寸取自 assets/icon/windows/，zip 里的说明模板在 platform/windows/。
set -eu
cd "$(dirname "$0")/.."

SOURCE_VERSION=$(sed -n 's/^var version = "\([0-9.]*\)".*/\1/p' cmd/modbus-ai/main.go)
[ -n "$SOURCE_VERSION" ] || { echo "cmd/modbus-ai/main.go 里没找到版本号"; exit 1; }
VERSION=${VERSION:-$SOURCE_VERSION}
[ "$VERSION" = "$SOURCE_VERSION" ] || { echo "VERSION=$VERSION 与源码版本 $SOURCE_VERSION 不一致"; exit 1; }
DIST=dist/v$VERSION
STAGE="$DIST/ModbusAIStudio-$VERSION-Windows-x64"
CSTAGE="$STAGE-Compat"
CC_WIN=x86_64-w64-mingw32-gcc
command -v "$CC_WIN" >/dev/null || { echo "缺少 $CC_WIN，先执行 brew install mingw-w64"; exit 1; }

# 兼容版用的工具链和 Mesa：固定版本和 SHA-256，下载后缓存，换版本时一起改
GO_WIN7=1.26.9-1 # https://github.com/thongtech/go-legacy-win7/releases
MESA=26.2.4      # https://github.com/pal1000/mesa-dist-win/releases
MESA_SHA256=351fc8c8b695878ffb3eaa044b3ead08672a48b1a045e3c3e3975811df0f6695
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}/modbus-ai-studio

rm -rf "$STAGE" "$STAGE.zip" "$CSTAGE" "$CSTAGE.zip"
mkdir -p "$STAGE" "$CSTAGE" "$CACHE"

# fetch 下载 url 到缓存目录的 name，核对 SHA-256；缓存里已有且校验对的不再下载。
fetch() {
	url=$1 sum=$2 out="$CACHE/$3"
	if [ ! -f "$out" ] || [ "$(shasum -a 256 "$out" | cut -d' ' -f1)" != "$sum" ]; then
		echo "下载 ${url}…"
		curl -fsSL --retry 3 -o "$out.part" "$url"
		got=$(shasum -a 256 "$out.part" | cut -d' ' -f1)
		[ "$got" = "$sum" ] || { echo "$3 的 SHA-256 是 $got，应为 $sum"; exit 1; }
		mv "$out.part" "$out"
	fi
}

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

# build 用 GOROOT 为 $1 的 Go 工具链编译 exe 到 $2，$3 是额外的编译标签，再放说明、打 zip。
# GOROOT 写明（本机设过 GOROOT 环境变量时会用错编译器），GOTOOLCHAIN=local 不让 go.mod 换成别的工具链。
# -static：把 MinGW 运行库静态链接进去，干净的 Windows 上不缺 libwinpthread 之类的 DLL
# no_emoji：不带 Fyne 自带的 4 MB 彩色 emoji 字体，emoji 用系统字体显示
build() {
	GOROOT=$1 GOTOOLCHAIN=local GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=$CC_WIN "$1/bin/go" build -trimpath -tags "no_emoji$3" \
		-ldflags "-s -w -H windowsgui -X main.version=$VERSION -extldflags=-static" \
		-o "$2/ModbusAIStudio.exe" ./cmd/modbus-ai
	# 新版 MinGW 会自动链接 default-manifest.o，可能覆盖高 DPI 清单。
	# 链接后重写资源，确保最终 exe 使用项目的清单、图标和版本信息。
	run_winres patch --no-backup --in assets/icon/windows/winres.json \
		--product-version "$VERSION" --file-version "$VERSION" "$2/ModbusAIStudio.exe"
	# 说明文件用 UTF-8 BOM + CRLF，记事本打开不乱码
	printf '\357\273\277' >"$2/README.txt"
	sed -e "s/{{VERSION}}/$VERSION/g" -e 's/$/\r/' platform/windows/README.txt >>"$2/README.txt"
	# Check the exact packaged bytes before uploading: encrypted/invalid templates
	# must never produce an unreadable installer README.
	python3 - "$2/README.txt" "$VERSION" <<'PY'
import pathlib
import sys

text = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8-sig")
assert "E-Safe" not in text and "\0" not in text
assert sys.argv[2] in text and "50 MB" in text
assert "{{VERSION}}" not in text
PY
	(cd "$DIST" && zip -9qr "$(basename "$2").zip" "$(basename "$2")")
	echo "  $2.zip"
	shasum -a 256 "$2.zip"
}

echo "编译 Windows x64 普通版…"
build "$(go env GOROOT)" "$STAGE" ""

echo "准备兼容版的 Go 工具链和 Mesa…"
case "$(uname -s)-$(uname -m)" in
Linux-x86_64) HOST=linux_amd64 HOST_SHA256=31d6c5778d069fcc027dd4b3eb1f26f089579b98cccc07ac54da038124ece99a ;;
Linux-aarch64) HOST=linux_arm64 HOST_SHA256=07365f00605a71346d2179088ae77e1ed1a847db7778af5eabc8a52ff28eb9a3 ;;
Darwin-arm64) HOST=darwin_arm64 HOST_SHA256=5755a45b507111b1e3583427a3d324ff893e42d254dea42075bb39a8f6018a28 ;;
Darwin-x86_64) HOST=darwin_amd64 HOST_SHA256=a3c425ed25811fe196c9c59fee60305dd0501f53de4a6a8be99ef410981220ce ;;
*) echo "go-legacy-win7 没有 $(uname -s) $(uname -m) 的版本"; exit 1 ;;
esac
fetch "https://github.com/thongtech/go-legacy-win7/releases/download/v$GO_WIN7/go-legacy-win7-$GO_WIN7.$HOST.tar.gz" \
	"$HOST_SHA256" "go-legacy-win7-$GO_WIN7.$HOST.tar.gz"
WIN7_GOROOT="$CACHE/go-legacy-win7-$GO_WIN7.$HOST/go-legacy-win7"
if [ ! -x "$WIN7_GOROOT/bin/go" ]; then
	rm -rf "$CACHE/go-legacy-win7-$GO_WIN7.$HOST"
	mkdir -p "$CACHE/go-legacy-win7-$GO_WIN7.$HOST"
	tar -xzf "$CACHE/go-legacy-win7-$GO_WIN7.$HOST.tar.gz" -C "$CACHE/go-legacy-win7-$GO_WIN7.$HOST"
fi
fetch "https://github.com/pal1000/mesa-dist-win/releases/download/$MESA/mesa3d-$MESA-release-msvc.7z" \
	"$MESA_SHA256" "mesa3d-$MESA-release-msvc.7z"
# opengl32.dll 只是个壳，真正的渲染器在 libgallium_wgl.dll，两个都要放在 exe 旁边
SEVENZIP=$(command -v 7z || command -v 7zz || true)
[ -n "$SEVENZIP" ] || { echo "缺少 7z，先执行 apt install p7zip-full 或 brew install sevenzip"; exit 1; }
"$SEVENZIP" e -y -bd -o"$CSTAGE" "$CACHE/mesa3d-$MESA-release-msvc.7z" x64/opengl32.dll x64/libgallium_wgl.dll >/dev/null
sed -e "s/{{MESA_VERSION}}/$MESA/g" -e 's/$/\r/' platform/windows/Mesa-LICENSE.txt >"$CSTAGE/Mesa-LICENSE.txt"

echo "编译 Windows x64 兼容版…"
build "$WIN7_GOROOT" "$CSTAGE" ",compat"
# 官方 Go 1.21 起的运行时在 Win7 上起不来；go-legacy-win7 的运行时会退回 RtlGenRandom（SystemFunction036）
grep -q SystemFunction036 "$CSTAGE/ModbusAIStudio.exe" || { echo "兼容版不是用 go-legacy-win7 编译的"; exit 1; }
