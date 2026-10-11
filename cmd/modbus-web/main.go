// modbus-web 是 Linux Web 版：在网关或服务器上运行，用浏览器调试 Modbus 设备。
// 不需要图形界面，也不需要 cgo：CGO_ENABLED=0 交叉编译出单个可执行文件，报文记录用纯 Go 的 SQLite。
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/web"
)

// version 在打包时用 -ldflags "-X main.version=…" 覆盖；没覆盖时取 go build 记下的版本。
var version = ""

const usage = `用法：modbus-web [选项]

在本机启动 Web 服务，用浏览器打开打印出的地址，输入访问口令后调试 Modbus 设备。
口令第一次启动时随机生成，保存在数据目录的 web-password 文件里；改口令就改这个文件再重启。
收发报文按天记在数据目录的 records 下，可在页面“记录文件”里下载，桌面版“历史记录”能直接打开。

示例：
  modbus-web
  modbus-web -listen :8502 -data /var/lib/modbus-web
  modbus-web -cert server.crt -key server.key      通过 HTTPS 提供服务

选项：`

func main() {
	listen := flag.String("listen", ":8502", "监听地址；只允许本机访问时写 127.0.0.1:8502")
	dataDir := flag.String("data", defaultDataDir(), "数据目录：访问口令和报文记录")
	cert := flag.String("cert", "", "HTTPS 证书文件（PEM），和 -key 一起用")
	key := flag.String("key", "", "HTTPS 私钥文件（PEM）")
	noRecord := flag.Bool("no-record", false, "不记录收发报文")
	showVersion := flag.Bool("version", false, "显示版本号")
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	if *showVersion {
		fmt.Println(appVersion())
		return
	}
	if (*cert == "") != (*key == "") {
		fail("-cert 和 -key 要一起给")
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fail("创建数据目录失败：%v", err)
	}
	password, err := loadPassword(filepath.Join(*dataDir, "web-password"))
	if err != nil {
		fail("读取访问口令失败：%v", err)
	}
	var rec *recorder.Recorder
	if !*noRecord {
		if rec, err = recorder.OpenDaily(filepath.Join(*dataDir, "records")); err != nil {
			fail("打开报文记录失败：%v（加 -no-record 可以不记录）", err)
		}
	}
	srv, err := web.New(web.Options{Version: appVersion(), Password: password, Recorder: rec, Secure: *cert != ""})
	if err != nil {
		fail("%v", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fail("监听 %s 失败：%v", *listen, err)
	}

	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	fmt.Printf("Modbus AI Studio Web %s（Ctrl+C 退出）\n\n", appVersion())
	for _, u := range urls(scheme, ln.Addr().(*net.TCPAddr)) {
		fmt.Println("  浏览器打开", u)
	}
	fmt.Printf("\n  访问口令 %s\n  数据目录 %s\n", password, *dataDir)
	if *cert == "" && !ln.Addr().(*net.TCPAddr).IP.IsLoopback() {
		fmt.Println("\n  注意：HTTP 明文传输口令，只在可信的内网使用；跨网访问请加 -cert / -key 用 HTTPS。")
	}
	fmt.Println()

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() {
		if *cert != "" {
			errc <- hs.ServeTLS(ln, *cert, *key)
		} else {
			errc <- hs.Serve(ln)
		}
	}()
	exit := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Printf("Web 服务停止：%v", err)
		exit = 1
	}
	stop()
	// 先停掉读取表和推送（推送连接不会自己结束），再等正在处理的请求，最后写完报文记录的缓冲。
	srv.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	hs.Shutdown(shutdown)
	cancel()
	if rec != nil {
		if err := rec.Close(); err != nil {
			log.Printf("报文记录保存失败：%v", err)
			exit = 1
		}
	}
	os.Exit(exit)
}

func appVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}

// defaultDataDir 是 ~/.config/ModbusAIStudio/web；取不到用户目录时（例如 systemd 下没有 HOME）用当前目录下的 modbus-web-data。
func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "ModbusAIStudio", "web")
	}
	return "modbus-web-data"
}

// loadPassword 读口令文件，没有或为空时随机生成一个并保存（只有本用户能读）。
func loadPassword(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if pw := strings.TrimSpace(string(data)); pw != "" {
		return pw, nil
	}
	pw := rand.Text()[:16]
	return pw, os.WriteFile(path, []byte(pw+"\n"), 0o600)
}

// urls 是浏览器能打开的地址。监听全部网卡时列出本机每个 IPv4 地址，方便在别的电脑上访问。
func urls(scheme string, addr *net.TCPAddr) []string {
	port := fmt.Sprint(addr.Port)
	if !addr.IP.IsUnspecified() {
		return []string{fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(addr.IP.String(), port))}
	}
	out := []string{fmt.Sprintf("%s://127.0.0.1:%s", scheme, port)}
	ifaces, _ := net.InterfaceAddrs()
	for _, a := range ifaces {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, fmt.Sprintf("%s://%s:%s", scheme, ipn.IP, port))
		}
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
