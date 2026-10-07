#!/bin/sh
# 构建 macOS 桌面应用：Intel 与 Apple Silicon 各一个 .app 和 DMG，ad-hoc 签名。
# 用法：VERSION=0.11.3 build/macos.sh
# 图标取自 assets/icon/AppIcon.png，Info.plist 模板在 platform/macos/。
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-0.11.3}
APP="Modbus AI Studio"
DIST=dist
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
		go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$app/Contents/MacOS/modbus-ai" ./cmd/modbus-ai
	cp "$DIST/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
	sed "s/{{VERSION}}/$VERSION/g" platform/macos/Info.plist >"$app/Contents/Info.plist"
	codesign --force --deep --sign - "$app"
	ln -s /Applications "$stage/Applications"
	dmg="$DIST/ModbusAIStudio-$VERSION-macOS-$name.dmg"
	rm -f "$dmg"
	hdiutil create -volname "$APP $VERSION" -srcfolder "$stage" -ov -format UDZO "$dmg" >/dev/null
	echo "  $app"
	echo "  $dmg"
done
shasum -a 256 "$DIST"/*.dmg
