#!/bin/sh
# 构建 macOS 桌面应用：Intel 与 Apple Silicon 各一个 .app 和 DMG，ad-hoc 签名。
# 用法：build/macos.sh（版本号取自 cmd/modbus-ai/main.go，也可用 VERSION=x.y.z 指定）
# 图标取自 assets/icon/AppIcon.png，Info.plist 模板在 platform/macos/。
set -eu
cd "$(dirname "$0")/.."

SOURCE_VERSION=$(sed -n 's/^var version = "\([0-9.]*\)".*/\1/p' cmd/modbus-ai/main.go)
[ -n "$SOURCE_VERSION" ] || { echo "cmd/modbus-ai/main.go 里没找到版本号"; exit 1; }
VERSION=${VERSION:-$SOURCE_VERSION}
[ "$VERSION" = "$SOURCE_VERSION" ] || { echo "VERSION=$VERSION 与源码版本 $SOURCE_VERSION 不一致"; exit 1; }
APP="Modbus AI Studio"
DIST=dist/v$VERSION
export CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=12.0

mkdir -p "$DIST"
# 图标：1024 PNG → iconset → icns
ICON=assets/icon/AppIcon.png
ICONSET="$DIST/AppIcon.iconset"
rm -rf "$ICONSET" && mkdir -p "$ICONSET"
for s in 16 32 128 256 512; do
	sips -z $s $s "$ICON" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
	s2=$((s * 2))
	sips -z $s2 $s2 "$ICON" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$DIST/AppIcon.icns"

for arch in amd64 arm64; do
	case $arch in
	amd64) name=Intel ;;
	arm64) name=AppleSilicon ;;
	esac
	stage="$DIST/$name"
	app="$stage/$APP.app"
	rm -rf "$stage"
	mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
	echo "编译 $name ($arch)…"
	GOARCH=$arch CC="clang -arch $([ $arch = amd64 ] && echo x86_64 || echo arm64)" \
		go build -trimpath -tags no_emoji -ldflags "-s -w -X main.version=$VERSION" -o "$app/Contents/MacOS/modbus-ai" ./cmd/modbus-ai
	cp "$DIST/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
	sed "s/{{VERSION}}/$VERSION/g" platform/macos/Info.plist >"$app/Contents/Info.plist"
	codesign --force --deep --sign - "$app"
	ln -s /Applications "$stage/Applications"
	dmg="$DIST/ModbusAIStudio-$VERSION-macOS-$name.dmg"
	rm -f "$dmg"
	# ULMO（LZMA）比默认的 UDZO（zlib）小约三成，macOS 10.15 起能打开，部署目标是 12.0
	hdiutil create -volname "$APP $VERSION" -srcfolder "$stage" -ov -format ULMO "$dmg" >/dev/null
	parity="$dmg-differences.md"
	sed -e "s/{{VERSION}}/$VERSION/g" -e "s/{{PACKAGE}}/$(basename "$dmg")/g" docs/platform-parity/macos.md >"$parity"
	if grep -q E-Safe "$parity" || grep -F -q -e '{{' -e '}}' "$parity"; then
		echo "$parity 不是完整的明文差异说明"; exit 1
	fi
	echo "  $app"
	echo "  $dmg"
	echo "  $parity"
done
shasum -a 256 "$DIST"/*.dmg
