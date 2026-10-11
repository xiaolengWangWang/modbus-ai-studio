#!/bin/sh
# 编译 Linux Web 版（modbus-web）：x64 和 ARM64（树莓派 4 / 5、多数 ARM 网关）各打一个 tar.gz，解压即用。
# 不需要 cgo：报文记录用纯 Go 的 SQLite，在 Windows / macOS / Linux 上都能编，编出来是静态链接的单个文件，不挑 glibc 版本。
# 用法：build/linux-web.sh（版本号取自 cmd/modbus-ai/main.go，也可用 VERSION=x.y.z 指定）
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(sed -n 's/^var version = "\([0-9.]*\)".*/\1/p' cmd/modbus-ai/main.go)}
[ -n "$VERSION" ] || { echo "cmd/modbus-ai/main.go 里没找到版本号"; exit 1; }
DIST=dist
mkdir -p "$DIST"

sha256() { if command -v shasum >/dev/null; then shasum -a 256 "$1"; else sha256sum "$1"; fi; }

for ARCH in amd64 arm64; do
	LABEL=$ARCH
	[ "$ARCH" = amd64 ] && LABEL=x64
	NAME="ModbusAIStudio-Web-$VERSION-Linux-$LABEL"
	STAGE="$DIST/$NAME"
	TGZ="$STAGE.tar.gz"
	rm -rf "$STAGE" "$TGZ"
	mkdir -p "$STAGE"

	echo "编译 Linux $LABEL…"
	CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
		-ldflags "-s -w -X main.version=$VERSION" -o "$STAGE/modbus-web" ./cmd/modbus-web

	# 说明直接在这里生成：仓库里的文本模板会被加密软件改成密文。不用 heredoc：在装了加密软件的电脑上，
	# heredoc 经过的临时文件同样会被加密，写出的说明是密文；printf 不经过临时文件。
	printf '%s\n' "Modbus AI Studio Web 版 $VERSION（Linux $LABEL）

在网关或服务器上运行，用浏览器调试 Modbus 设备：连接（TCP / RTU over TCP / ASCII over TCP / 串口 RTU / ASCII）、
读取表、写入并回读验证、协议识别、通信报文监视，收发报文按天记进 SQLite，可在页面上下载。

启动
  chmod +x modbus-web
  ./modbus-web
  终端会打印浏览器地址和访问口令。口令第一次启动时随机生成，保存在数据目录的 web-password 文件里，
  改口令就改这个文件再重启。默认数据目录是 ~/.config/ModbusAIStudio/web，可用 -data 指定。

常用选项（./modbus-web -h 查看全部）
  -listen :8502                       监听地址，只允许本机访问时写 127.0.0.1:8502
  -data /var/lib/modbus-web           数据目录：访问口令和报文记录
  -cert server.crt -key server.key    用 HTTPS 提供服务（跨网访问时建议使用）
  -no-record                          不记录收发报文

串口
  用 root 运行，或把运行的用户加入 dialout 组：sudo usermod -aG dialout <用户名>，重新登录后生效。

开机自动运行（systemd）
  sudo mkdir -p /opt/modbus-web && sudo cp modbus-web /opt/modbus-web/
  sudo tee /etc/systemd/system/modbus-web.service >/dev/null <<'UNIT'
  [Unit]
  Description=Modbus AI Studio Web
  After=network-online.target
  Wants=network-online.target

  [Service]
  ExecStart=/opt/modbus-web/modbus-web -listen :8502 -data /var/lib/modbus-web
  Restart=on-failure

  [Install]
  WantedBy=multi-user.target
  UNIT
  sudo systemctl daemon-reload && sudo systemctl enable --now modbus-web
  查看口令：sudo cat /var/lib/modbus-web/web-password

防火墙
  从别的电脑访问时放行端口，例如 sudo ufw allow 8502/tcp 或 sudo firewall-cmd --add-port=8502/tcp --permanent。" >"$STAGE/README.txt"
	if grep -q E-SafeNet "$STAGE/README.txt" || ! grep -q "$VERSION" "$STAGE/README.txt"; then
		echo "$STAGE/README.txt 不是明文说明（被加密软件加密了？），停止打包"; exit 1
	fi

	# 属主写成 root，解压时不带编译机的用户名。Windows（Git Bash）上编出的文件没有可执行位，
	# 用 GNU tar 的 --mode 显式写上；macOS 的 bsdtar 没有 --mode，chmod 在那里是有效的。
	chmod 755 "$STAGE/modbus-web"
	if tar --version 2>/dev/null | grep -q GNU; then
		tar -C "$DIST" --owner=0 --group=0 --mode=755 -cf "$STAGE.tar" "$NAME/modbus-web"
		tar -C "$DIST" --owner=0 --group=0 --mode=644 -rf "$STAGE.tar" "$NAME/README.txt"
	else
		tar -C "$DIST" --uid 0 --gid 0 -cf "$STAGE.tar" "$NAME"
	fi
	gzip -9 "$STAGE.tar"
	rm -rf "$STAGE"
	echo "  $TGZ"
	sha256 "$TGZ"
done
