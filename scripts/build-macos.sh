#!/bin/sh
# 构建 macOS 桌面应用：Intel 与 Apple Silicon 各一个 .app 和 DMG，ad-hoc 签名。
# 用法：VERSION=0.8.0 scripts/build-macos.sh
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-0.8.0}
APP="Modbus AI Studio"
DIST=dist
export CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=12.0

mkdir -p "$DIST"
# 图标：1024 PNG → iconset → icns
ICONSET="$DIST/AppIcon.iconset"
rm -rf "$ICONSET" && mkdir -p "$ICONSET"
go run ./scripts/icon "$DIST/icon-1024.png" 1024
for s in 16 32 128 256 512; do
	sips -z $s $s "$DIST/icon-1024.png" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
	s2=$((s * 2))
	sips -z $s2 $s2 "$DIST/icon-1024.png" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
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
	cat >"$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>$APP</string>
	<key>CFBundleDisplayName</key><string>$APP</string>
	<key>CFBundleIdentifier</key><string>studio.modbusai.desktop</string>
	<key>CFBundleExecutable</key><string>modbus-ai</string>
	<key>CFBundleIconFile</key><string>AppIcon</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$VERSION</string>
	<key>CFBundleVersion</key><string>$VERSION</string>
	<key>LSMinimumSystemVersion</key><string>12.0</string>
	<key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
EOF
	codesign --force --deep --sign - "$app"
	ln -s /Applications "$stage/Applications"
	dmg="$DIST/ModbusAIStudio-$VERSION-macOS-$name.dmg"
	rm -f "$dmg"
	hdiutil create -volname "$APP $VERSION" -srcfolder "$stage" -ov -format UDZO "$dmg" >/dev/null
	echo "  $app"
	echo "  $dmg"
done
shasum -a 256 "$DIST"/*.dmg
