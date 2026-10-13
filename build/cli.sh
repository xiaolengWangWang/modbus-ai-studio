#!/bin/sh
# 编译命令行工具 modbus-cli：Windows x64、macOS Intel / Apple Silicon、Linux x64 / ARM64 各打一个包，解压即用。
# 不需要 cgo，五个包都在一台机器上交叉编译。包里的 README.txt 是 modbus-cli manual 的输出，和程序里的说明一字不差。
# 文件名是小写的 modbus-cli-x.y.z-<系统>-<架构>：桌面版检查更新按“-Windows-x64.zip”这类结尾找安装包，
# CLI 的包不能和它同结尾，否则已经装好的旧版本会把 CLI 的包当成更新下载。
# 用法：build/cli.sh（版本号取自 cmd/modbus-ai/main.go，也可用 VERSION=x.y.z 指定）
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(sed -n 's/^var version = "\([0-9.]*\)".*/\1/p' cmd/modbus-ai/main.go)}
[ -n "$VERSION" ] || { echo "cmd/modbus-ai/main.go 里没找到版本号"; exit 1; }
DIST=dist
mkdir -p "$DIST"
LDFLAGS="-s -w -X main.version=$VERSION"

sha256() { if command -v shasum >/dev/null; then shasum -a 256 "$1"; else sha256sum "$1"; fi; }

MANUAL="$DIST/modbus-cli-$VERSION-manual.txt"
go run -ldflags "$LDFLAGS" ./cmd/modbus-cli manual >"$MANUAL"
if grep -q E-SafeNet "$MANUAL" || ! grep -q "modbus-cli $VERSION" "$MANUAL"; then
	echo "$MANUAL 不是明文说明（被加密软件加密了？），停止打包"; exit 1
fi

for TARGET in windows/amd64 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
	GOOS=${TARGET%/*}
	GOARCH=${TARGET#*/}
	SYS=$GOOS
	[ "$GOOS" = darwin ] && SYS=macos
	ARCH=$GOARCH
	[ "$GOARCH" = amd64 ] && ARCH=x64
	NAME="modbus-cli-$VERSION-$SYS-$ARCH"
	STAGE="$DIST/$NAME"
	rm -rf "$STAGE" "$STAGE.zip" "$STAGE.tar.gz"
	mkdir -p "$STAGE"

	echo "编译 $NAME…"
	EXE=modbus-cli
	[ "$GOOS" = windows ] && EXE=modbus-cli.exe
	CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -trimpath -ldflags "$LDFLAGS" -o "$STAGE/$EXE" ./cmd/modbus-cli

	if [ "$GOOS" = windows ]; then
		# 记事本按 UTF-8 BOM + CRLF 显示中文和换行最稳
		{ printf '\357\273\277'; sed 's/$/\r/' "$MANUAL"; } >"$STAGE/README.txt"
		PKG="$STAGE.zip"
		(cd "$DIST" && zip -q -X -r "$NAME.zip" "$NAME")
	else
		cp "$MANUAL" "$STAGE/README.txt"
		PKG="$STAGE.tar.gz"
		# 属主写成 root；GNU tar 用 --mode 写上可执行位（Windows 上编出的文件没有），bsdtar 没有 --mode，chmod 有效
		chmod 755 "$STAGE/$EXE"
		if tar --version 2>/dev/null | grep -q GNU; then
			tar -C "$DIST" --owner=0 --group=0 --mode=755 -cf "$STAGE.tar" "$NAME/$EXE"
			tar -C "$DIST" --owner=0 --group=0 --mode=644 -rf "$STAGE.tar" "$NAME/README.txt"
		else
			tar -C "$DIST" --uid 0 --gid 0 -cf "$STAGE.tar" "$NAME"
		fi
		gzip -9 "$STAGE.tar"
	fi
	rm -rf "$STAGE"
	echo "  $PKG"
	sha256 "$PKG"
done
rm -f "$MANUAL"
